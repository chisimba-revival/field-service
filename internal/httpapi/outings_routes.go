package httpapi

import (
	"net/http"

	"field-service/internal/outings"
)

// OutingsRoutes serves the outings catalogue.
type OutingsRoutes struct {
	guard   *Guard
	outings *outings.Store
}

// NewOutingsRoutes builds the outings routes.
func NewOutingsRoutes(guard *Guard, outings *outings.Store) *OutingsRoutes {
	return &OutingsRoutes{guard: guard, outings: outings}
}

// Serve handles GET /api/v1/outings and GET /api/v1/outings/{id}.
// The mux uses Go 1.22+ method-based routing. Both patterns are registered
// on an internal mux so the handler receives the full original URL.
func (r *OutingsRoutes) Serve() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /api/v1/outings", r.guard.Serve(r.listHandler()))
	mux.Handle("GET /api/v1/outings/{id}", r.guard.Serve(r.getHandler()))
	return mux
}

// listHandler returns all outings visible to the caller's grants.
func (r *OutingsRoutes) listHandler() Handler {
	return func(w http.ResponseWriter, req *http.Request, caller Caller) error {
		result, err := r.outings.List(req.Context(), caller.Grants())
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal_error", "Could not read outings.")
			return nil
		}

		writeJSON(w, http.StatusOK, map[string]any{
			"data": map[string]any{
				"outings": result,
			},
		})
		return nil
	}
}

// getHandler returns one outing by id, only if it is in a context the caller may read.
func (r *OutingsRoutes) getHandler() Handler {
	return func(w http.ResponseWriter, req *http.Request, caller Caller) error {
		id := req.PathValue("id")
		if id == "" {
			return problem(w, http.StatusNotFound, "unknown_outing",
				"No outing was named.")
		}

		result, err := r.outings.Get(req.Context(), caller.Grants(), id)
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal_error", "Could not read outing.")
			return nil
		}
		if result == nil {
			// Same answer for unknown id and ungranted context, so the endpoint
			// cannot be used to learn which outings exist in other contexts.
			return problem(w, http.StatusNotFound, "unknown_outing",
				"There is no outing with that identifier.")
		}

		writeJSON(w, http.StatusOK, result)
		return nil
	}
}
