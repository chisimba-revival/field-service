package httpapi

// The species catalogue endpoints.
//
// Two read-only routes, both behind the guard and both taking nothing from the
// request body. They sit in this package rather than beside the sync routes
// because they share the guard, the bounded-response helpers and the rule that a
// fault is logged rather than described — not because they are part of syncing.
// They are not, and a device building its offline catalogue bundle fetches these
// with the same token it uses for everything else.

import (
	"net/http"
	"strings"

	"field-service/internal/species"
)

// Catalogue holds what the species routes need.
type Catalogue struct {
	Catalogue *species.Service
	Log       Logger
}

// NewCatalogueRoutes builds the species routes, each already wrapped in the guard.
func NewCatalogueRoutes(g *Guard, cat *Catalogue) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/species", g.Serve(cat.listHandler()))
	mux.Handle("/api/v1/species/", g.Serve(cat.getHandler()))
	return mux
}

func (s *Catalogue) listHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, _ Caller) error {
		// The search is a query parameter rather than a body, so it is a GET with
		// no body at all. That is why decode() is not involved: there is nothing
		// to bound and nothing to type-check.
		list, err := s.Catalogue.List(r.Context(), r.URL.Query().Get("q"))
		if err != nil {
			return s.internal(w, r, err)
		}
		// Always an array, never null. A device building an offline bundle has to
		// distinguish "the catalogue is empty" from "the response was not what I
		// expected", and a nil slice marshals to null — so an empty catalogue
		// would arrive as something that is not a list.
		if list == nil {
			list = []species.Species{}
		}
		return writeJSON(w, http.StatusOK, map[string]any{"species": list})
	}
}

func (s *Catalogue) getHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, _ Caller) error {
		// The code is the tail of the path. r.URL.Path is used rather than the
		// raw pattern so a code arriving percent-encoded is decoded once by the
		// URL parser, and anything with a further slash in it is not a code.
		code := strings.TrimPrefix(r.URL.Path, "/api/v1/species/")
		if code == "" || strings.Contains(code, "/") {
			return problem(w, http.StatusNotFound, "unknown_species",
				"No species code was given.")
		}

		sp, err := s.Catalogue.Get(r.Context(), code)
		switch {
		case err == nil:
			return writeJSON(w, http.StatusOK, sp)
		case species.NotFound(err):
			// Not found rather than refused. A code the catalogue does not hold
			// is an answer, and it is the same answer whether it was never there
			// or was never going to be — so the response says nothing about which
			// of the catalogue a caller has been shown.
			return problem(w, http.StatusNotFound, "unknown_species",
				"No species with that code is in the catalogue.")
		default:
			return s.internal(w, r, err)
		}
	}
}

// internal reports a fault to the log and answers without describing it.
//
// Same reasoning as the sync routes: a database error names tables, columns and
// constraints, which is a schema handed to whoever can read the response.
func (s *Catalogue) internal(w http.ResponseWriter, r *http.Request, err error) error {
	if s.Log != nil {
		// The request, never nil — the same reason Sync.internal passes it. A
		// logger reading r.Method dereferences it, and the panic would cost the
		// client the very error this function exists to report.
		s.Log.Refused(r, "handler_failed", err)
	}
	return problem(w, http.StatusInternalServerError, "internal_error",
		"That did not go through. Nothing was changed.")
}
