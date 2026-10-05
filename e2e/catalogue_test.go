//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
)

// The catalogue routes, exercised against the real service, the real database
// and the real guard.
//
// These are here rather than only in internal/species because the defects found
// so far in this package have all been in the SQL or in the wiring, and a test
// with a fake store cannot see either. Two of the three bugs in the catalogue
// were found by running curl at a real server and reading the answer: a LIKE
// pattern that was parameterised but not escaped, and a search that matched a
// name but not a code while its own comment said it matched both.
//
// The shape of each assertion below is dictated by how the corresponding defect
// survived. The escaping is asserted through a search whose answer differs
// sharply escaped and unescaped. The code search is asserted with a term that
// appears in no description, so it can only match through the code column.

// catalogueGET fetches a catalogue path with a token.
func (r *rig) catalogueGET(tok, path string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest("GET", r.server.URL+path, nil)
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return r.do(req)
}

type speciesEnvelope struct {
	Species []struct {
		Code           string `json:"code"`
		CommonName     string `json:"common_name"`
		ScientificName string `json:"scientific_name"`
		Description    string `json:"description"`
	} `json:"species"`
}

func decodeCatalogue(t *testing.T, body []byte) speciesEnvelope {
	t.Helper()
	var env speciesEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decoding catalogue response %q: %v", body, err)
	}
	return env
}

func searchCatalogue(t *testing.T, r *rig, tok, term string) []string {
	t.Helper()
	code, body := r.catalogueGET(tok, "/api/v1/species?q="+url.QueryEscape(term))
	if code != http.StatusOK {
		t.Fatalf("search %q: status %d, body %s", term, code, body)
	}
	env := decodeCatalogue(t, body)
	out := make([]string, 0, len(env.Species))
	for _, sp := range env.Species {
		out = append(out, sp.Code)
	}
	return out
}

// The catalogue is served, behind the guard, with the descriptions intact.
//
// This is the reason the package exists at all: a device cannot bundle an
// offline cache of species it cannot fetch.
func TestTheWholeCatalogueIsServedBehindTheGuard(t *testing.T) {
	r := start(t)
	tok := r.token(0, r.ensureDrive("Alpha"))

	if code, _ := r.catalogueGET("", "/api/v1/species"); code == http.StatusOK {
		t.Fatal("the catalogue was served without a token")
	}

	code, body := r.catalogueGET(tok, "/api/v1/species")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	env := decodeCatalogue(t, body)
	if len(env.Species) == 0 {
		t.Fatal("the catalogue came back empty")
	}
	for _, sp := range env.Species {
		if sp.Code == "" || sp.CommonName == "" || sp.Description == "" {
			t.Fatalf("entry %+v is missing something a device must cache", sp)
		}
	}
}

// Searching by code is the case that was broken, and it is the one a caller is
// most likely to use: the code is printed on the sighting they are looking at.
func TestTheCatalogueIsSearchableByCode(t *testing.T) {
	r := start(t)
	tok := r.token(0, r.ensureDrive("Alpha"))

	all := searchCatalogue(t, r, tok, "")
	if len(all) < 5 {
		t.Fatalf("expected the reserve's catalogue, got %d entries: %v", len(all), all)
	}

	// Lowercase on purpose. A device holding a cached code will send it back
	// exactly as it holds it, and a catalogue that loses a species to a case
	// difference looks like one that has forgotten it.
	for _, term := range []string{"LEOP", "leop"} {
		got := searchCatalogue(t, r, tok, term)
		if len(got) != 1 || got[0] != "LEOP" {
			t.Fatalf("search %q returned %v, want [LEOP]", term, got)
		}
	}
}

// A wildcard a caller typed must not behave as a wildcard.
//
// Asserted with a bare "%", because that is the term whose escaped and
// unescaped answers differ most sharply: unescaped it matches every row, which
// is a plausible-looking catalogue rather than an obvious failure. Asserting
// "100% matches nothing" would pass either way, since nothing in the catalogue
// contains the digits 100 — a test that cannot tell the two implementations
// apart is a test of neither.
func TestASearchWildcardIsEscapedNotHonoured(t *testing.T) {
	r := start(t)
	tok := r.token(0, r.ensureDrive("Alpha"))

	total := len(searchCatalogue(t, r, tok, ""))
	if total == 0 {
		t.Fatal("the catalogue is empty, so this test could not tell the two behaviours apart")
	}

	for _, term := range []string{"%", "_", "LE%P", "L_EP"} {
		got := searchCatalogue(t, r, tok, term)
		if len(got) != 0 {
			t.Fatalf("search %q returned %v, want nothing: the wildcard was honoured",
				term, got)
		}
		if len(got) == total {
			t.Fatalf("search %q returned the whole catalogue", term)
		}
	}
}

// Searching by description, because a trainee looking for "grazing" is as likely
// to want the white rhino as one typing "rhino".
func TestTheCatalogueIsSearchableByDescription(t *testing.T) {
	r := start(t)
	tok := r.token(0, r.ensureDrive("Alpha"))

	got := searchCatalogue(t, r, tok, "grazing")
	if len(got) != 1 || got[0] != "WHRI" {
		t.Fatalf(`search "grazing" returned %v, want [WHRI]`, got)
	}
}

// One entry, and an absent code answered as absent rather than as a fault.
//
// The code is requested lowercase, so the normalisation the service does is
// covered here too: a client that cached "leop" must get the leopard, not a 404.
func TestASingleSpeciesIsServedAndAnAbsentCodeIsNotFound(t *testing.T) {
	r := start(t)
	tok := r.token(0, r.ensureDrive("Alpha"))

	code, body := r.catalogueGET(tok, "/api/v1/species/leop")
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	var one struct {
		Code           string `json:"code"`
		ScientificName string `json:"scientific_name"`
	}
	if err := json.Unmarshal(body, &one); err != nil {
		t.Fatalf("decoding %q: %v", body, err)
	}
	if one.Code != "LEOP" || one.ScientificName == "" {
		t.Fatalf("got %+v", one)
	}

	// A code the catalogue does not hold, in the format one it would.
	if code, _ := r.catalogueGET(tok, "/api/v1/species/ZZZZ"); code != http.StatusNotFound {
		t.Fatalf("absent code gave status %d, want 404", code)
	}
	// And one that could not be a code at all, which must not reach the database
	// as a lookup that could match something.
	if code, _ := r.catalogueGET(tok, "/api/v1/species/LEOP/extra"); code != http.StatusNotFound {
		t.Fatalf("a code with a slash in it gave status %d, want 404", code)
	}
}
