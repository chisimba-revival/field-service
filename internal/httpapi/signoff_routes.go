package httpapi

// The sign-off endpoints.
//
// One route for now: creating a draft. Submission and review are separate acts
// with separate permissions, and a route that did all three at once would let a
// trainee file and approve a sign-off in one call — which is the thing the
// contract's split exists to prevent.

import (
	"encoding/json"
	"errors"
	"net/http"

	"field-service/internal/signoff"
)

// MaxSignoffBody bounds a sign-off submission.
//
// Larger than the sync body because a sign-off legitimately carries evidence
// prose for up to twenty competencies, and a body too small to hold a mentor's
// reasoning would have them trim the reasoning rather than the metadata.
const MaxSignoffBody = 64 << 10

// Signoffs holds what the sign-off routes need.
type Signoffs struct {
	Signoffs *signoff.Service
	Log      Logger
}

// NewSignoffRoutes builds the sign-off routes, already wrapped in the guard.
func NewSignoffRoutes(g *Guard, s *Signoffs) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/signoffs", g.Serve(s.createHandler()))
	return mux
}

func (s *Signoffs) createHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var req signoff.Request
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxSignoffBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return problem(w, http.StatusRequestEntityTooLarge, "body_too_large",
					"That sign-off is too long to accept.")
			}
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That sign-off could not be read. The field names must match exactly.")
		}
		// A second document in one body is refused rather than ignored, for the
		// reason it is everywhere else here: a client that sent two would be told
		// its first was recorded.
		if dec.More() {
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That body carried more than one sign-off.")
		}

		// A refusal is a client mistake and is answered with its own code; a fault
		// is logged and answered without description. An error is never a refusal,
		// so the two are distinguished here rather than guessed at by the store.
		rec, err := s.Signoffs.Create(r.Context(), c.Subject(), c.ActiveContext(), req)
		if err != nil {
			var ref *signoff.Refusal
			if errors.As(err, &ref) {
				return problem(w, statusForRefusal(ref.Code), ref.Code, ref.Message)
			}
			return s.internal(w, r, err)
		}

		return writeJSON(w, http.StatusCreated, rec)
	}
}

// statusForRefusal maps a refusal to a status.
//
// The default is 422, because a refusal is a well-formed request that asks for
// something this service will not do. The one exception is a caller with no write
// context, which is 403: there is nothing wrong with what they asked for, and
// answering 422 would tell them to fix a request that is already correct.
//
// Every other refusal shares the default deliberately. Distinguishing them would
// let a caller enumerate which ids exist, and rule 14's list of refusals exists
// to be legible to the person showing the message — not to be a lookup table.
func statusForRefusal(code string) int {
	if code == "no_write_context" {
		return http.StatusForbidden
	}
	return http.StatusUnprocessableEntity
}

// internal reports a fault to the log and answers without describing it.
func (s *Signoffs) internal(w http.ResponseWriter, r *http.Request, err error) error {
	if s.Log != nil {
		// The request, never nil — the same reason the other handlers pass it. A
		// logger reading r.Method would dereference nil and the panic would cost
		// the client the very error this exists to report.
		s.Log.Refused(r, "handler_failed", err)
	}
	return problem(w, http.StatusInternalServerError, "internal_error",
		"That did not go through. Nothing was changed.")
}
