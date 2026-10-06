package httpapi

// The competency catalogue endpoints.
//
// Read-only and behind the guard, like the species routes. Two of them, because
// a client doing two different jobs needs two different questions answered: the
// whole catalogue for building a sign-off, and one entry when it has a code in
// hand.
//
// They live in this package rather than beside the sign-off routes because they
// share the guard, the bounded-response helpers and the rule that a fault is
// logged rather than described.

import (
	"net/http"
	"strings"

	"field-service/internal/competency"
)

// Competencies holds what the competency routes need.
type Competencies struct {
	Competencies *competency.Service
	Log          Logger
}

// NewCompetencyRoutes builds the competency routes, each already wrapped in the guard.
func NewCompetencyRoutes(g *Guard, c *Competencies) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/competencies", g.Serve(c.listHandler()))
	mux.Handle("/api/v1/competencies/", g.Serve(c.getHandler()))
	return mux
}

func (s *Competencies) listHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, _ Caller) error {
		params := r.URL.Query()

		level, err := competency.ParseLevel(params.Get("level"))
		if err != nil {
			// Refused, not ignored. Level 0 is this type's "no filter", so a
			// caller who meant level 2 and mistyped it would otherwise be shown
			// the whole catalogue — which looks like the filter working.
			return problem(w, http.StatusBadRequest, "bad_level",
				"That is not a level. Levels run from 1 to 5.")
		}

		// current=true excludes retired entries. Absent or anything else means
		// include them, because the commonest reader is a trainee's history and
		// hiding a retired entry there tells them they were never assessed.
		currentOnly := false
		if v := strings.TrimSpace(params.Get("current")); v != "" {
			currentOnly = !(strings.EqualFold(v, "false") || v == "0")
		}

		list, err := s.Competencies.List(r.Context(), competency.Query{
			Category:    params.Get("category"),
			Level:       level,
			Search:      params.Get("q"),
			CurrentOnly: currentOnly,
		})
		if err != nil {
			return s.internal(w, r, err)
		}
		if list == nil {
			list = []competency.Competency{}
		}
		return writeJSON(w, http.StatusOK, map[string]any{"competencies": list})
	}
}

func (s *Competencies) getHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, _ Caller) error {
		code := strings.TrimPrefix(r.URL.Path, "/api/v1/competencies/")
		if code == "" || strings.Contains(code, "/") {
			return problem(w, http.StatusNotFound, "unknown_competency",
				"No competency code was given.")
		}
		if len(code) > competency.MaxCodeLen {
			return problem(w, http.StatusNotFound, "unknown_competency",
				"No competency with that code is in the catalogue.")
		}

		c, err := s.Competencies.Get(r.Context(), code)
		switch {
		case err == nil:
			return writeJSON(w, http.StatusOK, c)
		case competency.NotFound(err):
			return problem(w, http.StatusNotFound, "unknown_competency",
				"No competency with that code is in the catalogue.")
		default:
			return s.internal(w, r, err)
		}
	}
}

func (s *Competencies) internal(w http.ResponseWriter, r *http.Request, err error) error {
	if s.Log != nil {
		s.Log.Refused(r, reasonHandlerFailed, err)
	}
	return problem(w, http.StatusInternalServerError, "internal_error",
		"That did not go through. Nothing was changed.")
}
