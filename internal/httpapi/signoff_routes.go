package httpapi

// The sign-off endpoints.
//
// Creating a draft, and reading one back. Submission and review are separate acts
// with separate permissions, and a route that did all three at once would let a
// trainee file and approve a sign-off in one call — which is the thing the
// contract's split exists to prevent.
//
// The read is not optional to the rest of it. A sign-off a mentor writes in
// Chisimba is feedback for a trainee, and feedback the trainee cannot fetch is
// not feedback: it is a row the mentor filled in.

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
	// Create a draft. The trainee owns this, and only the trainee may file it.
	mux.Handle("POST /api/v1/signoffs", g.Serve(s.createHandler()))
	// Read one sign-off. The trainee, the mentor, and an administrator in the
	// same context may read; anyone else gets not found, so the endpoint cannot
	// be used to learn which sign-offs exist.
	mux.Handle("GET /api/v1/signoffs/{id}", g.Serve(s.getHandler()))
	// Submit for review. The trainee who created the draft does this; a mentor
	// submitting on their behalf would be a self-assessment.
	mux.Handle("POST /api/v1/signoffs/{id}/submit", g.Serve(s.submitHandler()))
	// Review a submitted sign-off. The mentor who wrote the original does this;
	// the review is itself an assessment with the same evidence requirements.
	mux.Handle("POST /api/v1/signoffs/{id}/review", g.Serve(s.reviewHandler()))
	return mux
}

// getHandler serves one sign-off to a caller entitled to see it.
func (s *Signoffs) getHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		id := r.PathValue("id")
		if id == "" {
			return problem(w, http.StatusNotFound, "unknown_signoff",
				"No sign-off was named.")
		}

		// Whether this caller is an administrator is decided here, where the token
		// is, and reaches the store as a plain boolean. The store cannot read the
		// token and must not be asked to decide it from a string that means
		// something else.
		isAdmin := s.Signoffs.AdminRole != "" && c.HasRole(s.Signoffs.AdminRole)

		rec, err := s.Signoffs.Get(r.Context(), c.Subject(), c.ActiveContext(), isAdmin, id)
		switch {
		case err == nil:
			return writeJSON(w, http.StatusOK, rec)
		case signoff.NotFound(err):
			// The same answer for an unknown id and for somebody else's. Any
			// difference turns this into a way of learning which sign-offs exist,
			// and the id is in the change feed every device in the context holds.
			return problem(w, http.StatusNotFound, "unknown_signoff",
				"There is no sign-off with that identifier.")
		default:
			var ref *signoff.Refusal
			if errors.As(err, &ref) {
				return problem(w, statusForRefusal(ref.Code), ref.Code, ref.Message)
			}
			return s.internal(w, r, err)
		}
	}
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

		// The sign-off is written in the caller's active context. The store gets
		// the full grants list because the RLS policy checks whether the row's
		// context is among the caller's grants — a list of one context here would
		// forbid writing to any other context the caller holds.
		rec, err := s.Signoffs.Create(r.Context(), c.Subject(), c.ActiveContext(), c.Grants(), req)
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

// submitHandler transitions a draft to submitted. The sign-off is now in the
// review queue and the review clock starts.
func (s *Signoffs) submitHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		id := r.PathValue("id")
		if id == "" {
			return problem(w, http.StatusNotFound, "unknown_signoff",
				"No sign-off was named.")
		}

		rec, err := s.Signoffs.Submit(r.Context(), c.Subject(), c.ActiveContext(), id)
		if err != nil {
			var ref *signoff.Refusal
			if errors.As(err, &ref) {
				return problem(w, statusForRefusal(ref.Code), ref.Code, ref.Message)
			}
			return s.internal(w, r, err)
		}

		return writeJSON(w, http.StatusOK, rec)
	}
}

// reviewHandler records a mentor's assessment of a submitted sign-off. The
// review is itself an assessment, so it carries the same evidence requirements
// as the original submission.
func (s *Signoffs) reviewHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		id := r.PathValue("id")
		if id == "" {
			return problem(w, http.StatusNotFound, "unknown_signoff",
				"No sign-off was named.")
		}

		var req signoff.ReviewRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, MaxSignoffBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				return problem(w, http.StatusRequestEntityTooLarge, "body_too_large",
					"That review is too long to accept.")
			}
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That review could not be read. The field names must match exactly.")
		}
		if dec.More() {
			return problem(w, http.StatusBadRequest, "unreadable_body",
				"That body carried more than one review.")
		}

		rec, err := s.Signoffs.Review(r.Context(), c.Subject(), c.ActiveContext(), id, req)
		if err != nil {
			var ref *signoff.Refusal
			if errors.As(err, &ref) {
				return problem(w, statusForRefusal(ref.Code), ref.Code, ref.Message)
			}
			return s.internal(w, r, err)
		}

		return writeJSON(w, http.StatusOK, rec)
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
