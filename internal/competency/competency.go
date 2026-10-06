// Package competency serves the competency catalogue.
//
// Separate from species because it is not the same problem: species are looked up
// one at a time to check a code, while competencies are listed whole and filtered,
// because a mentor choosing what to assess on an outing is choosing from a set.
//
// The catalogue is reference data, so there is no context and nothing to isolate,
// for the same reason the species catalogue has none.
package competency

import (
	"context"
	"strconv"
	"strings"
)

// MaxCodeLen bounds the code a caller may send. Longer cannot exist in the
// catalogue, so a longer one is a client mistake rather than a lookup.
const MaxCodeLen = 16

// Competency is one catalogue entry.
//
// Retired is carried on the entry rather than expressed by its absence, because a
// client asking "was this trainee assessed on this?" needs a retired row to come
// back and mean yes. An absent row is indistinguishable from one never taught,
// and a client that confuses the two tells a trainee they were never assessed
// when they were.
type Competency struct {
	Code        string `json:"code"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Level       int    `json:"level"`
	Description string `json:"description,omitempty"`
	Retired     bool   `json:"retired"`
}

// Query narrows a catalogue listing.
type Query struct {
	// Category is matched exactly. It is a closed vocabulary the catalogue
	// defines, so a substring match would answer a question nobody asked.
	Category string
	// Level, when non-zero, matches that level exactly.
	Level int
	// Search is a case-insensitive substring over code, name and description.
	Search string
	// CurrentOnly excludes retired entries. A client building a new sign-off
	// wants this; a client reading a trainee's history does not.
	CurrentOnly bool
}

// Store reads the catalogue.
type Store interface {
	List(ctx context.Context, q Query) ([]Competency, error)
	Get(ctx context.Context, code string) (Competency, error)
}

// Service answers catalogue questions.
type Service struct{ store Store }

// New builds a service over a store.
func New(store Store) *Service { return &Service{store: store} }

// List returns the catalogue under a query.
func (s *Service) List(ctx context.Context, q Query) ([]Competency, error) {
	q.Search = strings.TrimSpace(q.Search)
	// A blank category means "no filter", not "the category that is blank". The
	// catalogue rejects a blank category on insert, so treating one as a filter
	// would be an empty result the caller could not distinguish from a real one.
	q.Category = strings.TrimSpace(q.Category)
	return s.store.List(ctx, q)
}

// Get returns one entry, or ErrNotFound.
//
// A retired entry is found, not reported missing. That is the whole reason the
// catalogue retires rather than deletes, and a lookup that hid retired rows would
// undo it at the only place a client looks.
func (s *Service) Get(ctx context.Context, code string) (Competency, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return Competency{}, ErrNotFound
	}
	return s.store.Get(ctx, code)
}

// ErrNotFound means the catalogue holds no such code.
var ErrNotFound = errNotFound{}

type errNotFound struct{}

func (errNotFound) Error() string { return "competency: no such code" }

// NotFound reports whether err is ErrNotFound.
func NotFound(err error) bool {
	_, ok := err.(errNotFound)
	return ok
}

// ParseLevel reads a level from a query string.
//
// A malformed level is an error rather than zero, because zero is this type's
// "no filter" and a caller who meant level 2 would otherwise be shown the whole
// catalogue — which looks like the filter working and is the opposite.
func ParseLevel(raw string) (int, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if n < 1 || n > 5 {
		return 0, errLevelRange
	}
	return n, nil
}

type levelRange struct{}

func (levelRange) Error() string { return "competency: level must be 1 to 5" }

var errLevelRange = levelRange{}

// BadLevel reports whether err came from ParseLevel.
func BadLevel(err error) bool {
	_, ok := err.(levelRange)
	return ok
}
