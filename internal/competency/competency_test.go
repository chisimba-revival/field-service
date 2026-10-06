package competency

import (
	"context"
	"errors"
	"testing"
)

// searchLike is where a caller's own characters start being interpreted, so it is
// tested on its own rather than through a query.
//
// The sharpest case is a bare "%": unescaped it matches every row, which is a
// plausible-looking catalogue rather than an obvious failure. Asserting that
// "100% matches nothing" would pass either way, because nothing in the catalogue
// contains the digits 100 — a test that cannot tell the two implementations
// apart is a test of neither. That was learned on the species catalogue.
func TestSearchLikeEscapesWildcards(t *testing.T) {
	cases := []struct{ term, want string }{
		{"compass", "%compass%"},
		{"", "%%"},
		{"%", `%\%%`},
		{"_", `%\_%`},
		{"100%", `%100\%%`},
		{"a_b", `%a\_b%`},
		{`back\slash`, `%back\\slash%`},
	}
	for _, c := range cases {
		if got := searchLike(c.term); got != c.want {
			t.Errorf("searchLike(%q) = %q, want %q", c.term, got, c.want)
		}
	}
}

func TestAnEscapedTermCannotMatchEverything(t *testing.T) {
	if p := searchLike("%"); p == "%" {
		t.Fatal(`searchLike("%") returned a bare wildcard, which matches every row`)
	}
	if p := searchLike("_"); p == "%_" {
		t.Fatal(`searchLike("_") returned a bare single-character wildcard`)
	}
}

type fakeStore struct {
	list  []Competency
	get   map[string]Competency
	seen  Query
	fails error
}

func (f *fakeStore) List(_ context.Context, q Query) ([]Competency, error) {
	f.seen = q
	return f.list, f.fails
}

func (f *fakeStore) Get(_ context.Context, code string) (Competency, error) {
	c, ok := f.get[code]
	if !ok {
		return Competency{}, ErrNotFound
	}
	return c, f.fails
}

func TestListNormalisesItsQuery(t *testing.T) {
	st := &fakeStore{list: []Competency{{Code: "NAVMAP"}}}
	svc := New(st)

	// Whitespace is trimmed off the search and the category. A blank category
	// means "no filter": the catalogue rejects a blank category on insert, so
	// treating one as a filter would be an empty result indistinguishable from a
	// real one.
	if _, err := svc.List(context.Background(), Query{Search: "  compass  ", Category: "  "}); err != nil {
		t.Fatal(err)
	}
	if st.seen.Search != "compass" {
		t.Fatalf("searched for %q, want trimmed to %q", st.seen.Search, "compass")
	}
	if st.seen.Category != "" {
		t.Fatalf("category %q, want empty so it means no filter", st.seen.Category)
	}
}

// Retired entries come back by default. This is the property that makes retirement
// rather than deletion worth doing, and it is a property of the default rather
// than of a flag, so a test that set the flag would miss it.
func TestRetiredEntriesAreIncludedUnlessExplicitlyExcluded(t *testing.T) {
	st := &fakeStore{list: []Competency{
		{Code: "SAFBIG", Retired: false},
		{Code: "OLDONE", Retired: true},
	}}
	svc := New(st)

	if _, err := svc.List(context.Background(), Query{}); err != nil {
		t.Fatal(err)
	}
	if st.seen.CurrentOnly {
		t.Fatal("the default listing excluded retired entries; a trainee's history would show a gap where they were assessed")
	}

	if _, err := svc.List(context.Background(), Query{CurrentOnly: true}); err != nil {
		t.Fatal(err)
	}
	if !st.seen.CurrentOnly {
		t.Fatal("current=true did not reach the store")
	}
}

// A device holding a cached code sends it back as it holds it, so case is
// normalised on the way in.
func TestGetNormalisesCaseAndWhitespace(t *testing.T) {
	st := &fakeStore{get: map[string]Competency{"NAVMAP": {Code: "NAVMAP", Name: "Map and compass"}}}
	svc := New(st)

	for _, code := range []string{"NAVMAP", "navmap", "  NavMap  "} {
		c, err := svc.Get(context.Background(), code)
		if err != nil {
			t.Fatalf("get(%q): %v", code, err)
		}
		if c.Code != "NAVMAP" {
			t.Fatalf("get(%q) returned %q", code, c.Code)
		}
	}
}

func TestGetReportsAnAbsentCodeAsNotFound(t *testing.T) {
	svc := New(&fakeStore{get: map[string]Competency{}})

	for _, code := range []string{"", "   ", "NOPE"} {
		if _, err := svc.Get(context.Background(), code); !NotFound(err) {
			t.Fatalf("get(%q) = %v, want NotFound", code, err)
		}
	}
}

// NotFound must be about this package's own error. A driver error that happens to
// be "no rows" would otherwise be reported as "no such competency", which is an
// answer, and it would be a wrong one.
func TestNotFoundIsFalseForOtherErrors(t *testing.T) {
	if NotFound(errors.New("connection refused")) {
		t.Fatal("NotFound matched an unrelated error")
	}
	if NotFound(nil) {
		t.Fatal("NotFound matched nil")
	}
}

// A malformed level is refused rather than read as "no filter". Level 0 is this
// type's no-filter value, so ignoring a typo would show the whole catalogue —
// which looks exactly like the filter working.
func TestParseLevelRefusesRatherThanIgnoringATypo(t *testing.T) {
	if got, err := ParseLevel(""); err != nil || got != 0 {
		t.Fatalf(`ParseLevel("") = %d, %v; want 0, nil`, got, err)
	}
	if got, err := ParseLevel(" 2 "); err != nil || got != 2 {
		t.Fatalf(`ParseLevel(" 2 ") = %d, %v; want 2, nil`, got, err)
	}
	// Two kinds of wrong, kept apart deliberately.
	//
	// "two", "2.5" and "2abc" are not numbers at all, so they are a parse
	// failure. "0", "6" and "-1" are numbers that are not levels, so they are a
	// range failure. Both are refused and both come back as zero, which is the
	// property that matters — zero is this type's no-filter value, so an error
	// that returned anything else would be a filter nobody asked for. The handler
	// answers both the same way, because a caller who sent "6" needs to know the
	// level was wrong and gains nothing from being told which way.
	for _, raw := range []string{"two", "2.5", "2abc", " ", "1 2"} {
		got, err := ParseLevel(raw)
		if err == nil && raw != " " {
			t.Errorf("ParseLevel(%q) = %d, nil; want an error", raw, got)
		}
		if got != 0 {
			t.Errorf("ParseLevel(%q) = %d alongside an error; want 0 so it cannot be read as a level", raw, got)
		}
		if BadLevel(err) {
			t.Errorf("ParseLevel(%q) reported a range error; it is not a number", raw)
		}
	}
	for _, raw := range []string{"0", "6", "-1", "99"} {
		got, err := ParseLevel(raw)
		if err == nil {
			t.Errorf("ParseLevel(%q) = %d, nil; want an error", raw, got)
			continue
		}
		if got != 0 {
			t.Errorf("ParseLevel(%q) = %d alongside an error; want 0", raw, got)
		}
		if !BadLevel(err) {
			t.Errorf("ParseLevel(%q) is a number out of range but is not a level-range error", raw)
		}
	}
}
