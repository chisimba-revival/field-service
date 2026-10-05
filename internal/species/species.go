// Package species serves the species catalogue.
//
// The catalogue is reference data rather than field data, which is why it is
// separate from push and pull and why it is a package of its own: nothing here
// writes, nothing here is scoped to a context, and nothing here is filtered by
// the grants a caller holds. Everything that does those things is a different
// kind of problem and lives elsewhere.
//
// The contract specifies the endpoints and, more importantly, the reason the
// microservice serves them rather than Chisimba: "they belong with the field
// data and must be bundled into the offline cache without a second round trip."
// So the whole catalogue is expected to be small enough to send at once, and the
// search parameter here narrows a response for a human at a screen rather than
// paging through anything.
package species

import (
	"context"
	"strings"
)

// MaxCodeLen is the longest a species code may be, matching the catalogue's own
// constraint. A longer one cannot exist, so a longer one in a path is a client
// mistake and is reported as not-found rather than passed to the database.
//
// Four is the format the contract fixes; the bound is left slightly wider
// because rejecting "LEOPARD" as a malformed "LEOP" before the catalogue has
// been consulted would turn a typo into a different answer from the one a
// lookup gives.
const MaxCodeLen = 32

// Species is one catalogue entry.
type Species struct {
	Code           string `json:"code"`
	CommonName     string `json:"common_name"`
	ScientificName string `json:"scientific_name,omitempty"`
	Description    string `json:"description,omitempty"`
}

// Store reads the catalogue.
type Store interface {
	List(ctx context.Context, search string) ([]Species, error)
	Get(ctx context.Context, code string) (Species, error)
}

// Service answers catalogue questions.
type Service struct{ store Store }

// New builds a service over a store.
func New(store Store) *Service { return &Service{store: store} }

// List returns the catalogue, optionally narrowed by a search term.
//
// An empty term returns everything, because that is what a device building its
// offline bundle wants and because a search box that has to be filled in to see
// the data is a search box that will be left empty.
//
// The term is matched against the common name, the scientific name and the
// description, because a trainee looking for "grazing" is as likely to want the
// white rhino as one typing "rhino", and a code they half remember is a third
// case. Whitespace is trimmed and an all-whitespace term is treated as empty
// rather than as a term that matches nothing.
func (s *Service) List(ctx context.Context, search string) ([]Species, error) {
	return s.store.List(ctx, strings.TrimSpace(search))
}

// Get returns one entry.
//
// An unknown code is ErrNotFound rather than an empty entry, so the handler can
// distinguish "no such species" from "a species with no description" — the
// second is a legitimate catalogue state and the first is a client mistake.
func (s *Service) Get(ctx context.Context, code string) (Species, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return Species{}, ErrNotFound
	}
	return s.store.Get(ctx, code)
}

// ErrNotFound means the catalogue holds no such code.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "species: no such code" }

// NotFound reports whether err is ErrNotFound.
func NotFound(err error) bool {
	_, ok := err.(errNotFound)
	return ok
}
