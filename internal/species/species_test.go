package species

import (
	"context"
	"errors"
	"testing"
)

// searchLike is where a caller's own characters start being interpreted, so it
// is tested on its own rather than only through a query.
//
// The cases that matter are the ones where an unescaped term would return a
// catalogue the caller did not ask for. A term of "%" is the sharpest: unescaped
// it matches every row, which is a plausible-looking response rather than an
// obvious failure, and a test asserting only "no results for 100%" would pass
// either way because nothing in the catalogue contains the digits 100.
func TestSearchLikeEscapesWildcards(t *testing.T) {
	cases := []struct{ term, want string }{
		{"buffalo", "%buffalo%"},
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

// An escaped term must not behave as a wildcard. Asserted on the pattern rather
// than on the query because that is where the behaviour is decided, and because
// a test needing a database to notice that "%" matches everything is a test that
// will be skipped exactly when it is most needed.
func TestAnEscapedTermCannotMatchEverything(t *testing.T) {
	p := searchLike("%")
	if p == "%" {
		t.Fatal(`searchLike("%") returned a bare wildcard, which matches every row`)
	}
	if p != `%\%%` {
		t.Fatalf("searchLike(%%) = %q", p)
	}
}

type fakeStore struct {
	list  []Species
	get   map[string]Species
	seen  string
	fails error
}

func (f *fakeStore) List(_ context.Context, search string) ([]Species, error) {
	f.seen = search
	return f.list, f.failures()
}

func (f *fakeStore) Get(_ context.Context, code string) (Species, error) {
	sp, ok := f.get[code]
	if !ok {
		return Species{}, ErrNotFound
	}
	return sp, f.failures()
}

func (f *fakeStore) failures() error { return f.fails }

func TestListTrimsWhitespaceAndTreatsBlankAsEverything(t *testing.T) {
	st := &fakeStore{list: []Species{{Code: "LEOP"}}}
	svc := New(st)

	// A search box the user cleared must not become a search for nothing. If it
	// did, a device building its offline bundle would send an empty term and get
	// an empty catalogue, and would cache that.
	for _, term := range []string{"", "   ", "\t\n"} {
		if _, err := svc.List(context.Background(), term); err != nil {
			t.Fatalf("list(%q): %v", term, err)
		}
		if st.seen != "" {
			t.Fatalf("list(%q) searched for %q, want everything", term, st.seen)
		}
	}

	if _, err := svc.List(context.Background(), "  buffalo  "); err != nil {
		t.Fatal(err)
	}
	if st.seen != "buffalo" {
		t.Fatalf("searched for %q, want the term trimmed to %q", st.seen, "buffalo")
	}
}

// A device that has cached a code will send it back exactly as it holds it.
// Codes are stored uppercase, so a lowercase code is the ordinary case of a
// device that got the case wrong somewhere, and answering "not found" for it
// would be a catalogue that appears to have lost the species.
func TestGetNormalisesCaseAndWhitespace(t *testing.T) {
	st := &fakeStore{get: map[string]Species{"LEOP": {Code: "LEOP", CommonName: "Leopard"}}}
	svc := New(st)

	for _, code := range []string{"LEOP", "leop", "  LeOp  "} {
		sp, err := svc.Get(context.Background(), code)
		if err != nil {
			t.Fatalf("get(%q): %v", code, err)
		}
		if sp.Code != "LEOP" {
			t.Fatalf("get(%q) returned %q", code, sp.Code)
		}
	}
}

func TestGetReportsAnAbsentCodeAsNotFound(t *testing.T) {
	svc := New(&fakeStore{get: map[string]Species{}})

	// An empty code is refused here rather than sent to the database, because
	// "no code was given" and "no such code" are different answers and the
	// handler phrases them differently.
	for _, code := range []string{"", "   "} {
		if _, err := svc.Get(context.Background(), code); !NotFound(err) {
			t.Fatalf("get(%q) = %v, want NotFound", code, err)
		}
	}
	if _, err := svc.Get(context.Background(), "ZZZZ"); !NotFound(err) {
		t.Fatalf("got %v, want NotFound", err)
	}
}

// NotFound must be about this package's own error. A driver error that happens
// to be "no rows" would otherwise be reported to a client as "no such species",
// which is an answer, and it would be a wrong one.
func TestNotFoundIsFalseForOtherErrors(t *testing.T) {
	if NotFound(errors.New("connection refused")) {
		t.Fatal("NotFound matched an unrelated error")
	}
	if NotFound(nil) {
		t.Fatal("NotFound matched nil")
	}
}
