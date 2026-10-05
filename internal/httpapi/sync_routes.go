package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"field-service/internal/pull"
	"field-service/internal/push"
)

// This is where "login is not authorisation" stops being architecture and
// becomes endpoints. Every route below is registered through the guard, so a
// handler cannot be reached without a caller having been established first —
// not by convention, but by there being no way to register one that is not
// wrapped.

// MaxBodyBytes bounds a request body.
//
// An unbounded read is a memory lever held by anyone who can reach the port, and
// a push batch is the largest thing a client sends. The bound is generous enough
// that a device carrying a large queued batch will not hit it, and small enough
// that exhausting the service takes deliberate effort rather than one careless
// request.
const MaxBodyBytes = 4 << 20 // 4 MiB

// Sync holds what the sync routes need.
type Sync struct {
	Push PushService
	// Pull answers a pull for a caller. It is a function rather than a service
	// because a pull must be bound to the transaction carrying the caller's
	// grants: those settings are transaction-local, so a service built once over
	// a pooled connection would find none of them and read nothing.
	Pull func(ctx context.Context, caller string, cursor string, q Querier) (pull.Page, error)
	// Session supplies that transaction. It is the guard's own session, so there
	// is one place that decides what a caller may read.
	Session Session
	// Log receives the detail of any fault. Not optional in practice: the client
	// is told a sentence and the operator is told the reason, and the second half
	// is what makes the first half safe to give away.
	Log Logger
}

// PushService applies a batch.
//
// An interface, not the concrete service, and the reason is a test that was
// quietly measuring nothing. The single most important thing this file decides is
// where a write's context comes from: the token, never the request. With a
// concrete *push.Service there was no way to observe the Caller the handler
// built, so the only test available built one itself — which proves the test's
// copy of the decision correct and leaves the real one unguarded. Falsifying it
// by reading the context out of the body produced zero failures.
type PushService interface {
	Push(ctx context.Context, caller push.Caller, ops []push.Operation) ([]push.Result, error)
}

// NewSyncRoutes builds the two sync routes, each already wrapped in the guard.
func NewSyncRoutes(g *Guard, sync *Sync) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/v1/sync/push", g.Serve(sync.pushHandler()))
	mux.Handle("/api/v1/sync/pull", g.Serve(sync.pullHandler()))
	return mux
}

type pushRequest struct {
	Operations []push.Operation `json:"operations"`
}

type pullRequest struct {
	Cursor string `json:"cursor"`
}

func (s *Sync) pushHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var body pushRequest
		if err := decode(w, r, &body); err != nil {
			return err
		}
		if len(body.Operations) == 0 {
			return problem(w, http.StatusBadRequest, "no_operations",
				"The batch carries no operations.")
		}

		// The write context is the token's active context, never a value from the
		// request. Operation has no context field for a client to fill in, so
		// there is nowhere for an ungranted context to arrive from.
		results, err := s.Push.Push(r.Context(), push.Caller{
			ID:           c.Subject(),
			WriteContext: c.ActiveContext(),
		}, body.Operations)
		if err != nil {
			// Nothing was applied, and the client needs to know that rather than
			// infer it from results that look successful. The reason goes to the
			// log: a Postgres error names tables, columns and constraints, which
			// is a schema handed to whoever can read the response.
			return s.internal(w, r, err)
		}
		return writeJSON(w, http.StatusOK, map[string]any{"results": results})
	}
}

func (s *Sync) pullHandler() Handler {
	return func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var body pullRequest
		// An empty body is a first pull, which is ordinary rather than an error,
		// so a missing body is fine. Only a body that will not decode is not.
		if r.ContentLength != 0 {
			if err := decode(w, r, &body); err != nil {
				return err
			}
		}

		var page pull.Page
		err := s.Session.Querier(r.Context(), c.Subject(), c.Grants(),
			func(q Querier) error {
				var err error
				page, err = s.Pull(r.Context(), c.Subject(), body.Cursor, q)
				return err
			})
		if err != nil {
			return s.internal(w, r, err)
		}
		return writeJSON(w, http.StatusOK, page)
	}
}

// decode reads a bounded, strictly-typed body.
//
// Unknown fields are refused rather than ignored. A client sending entityId
// instead of entity_id would otherwise be accepted, its change silently dropped,
// and the operation reported as applied — the worst possible outcome, because
// the trainee walks away believing it was recorded.
//
// The strictness is on the wire format only. It costs a client nothing to send
// exactly what the contract names, and it turns a typo into a refusal at the
// boundary rather than a record that disagrees with what was written.
func decode(w http.ResponseWriter, r *http.Request, into any) error {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return problem(w, http.StatusUnsupportedMediaType, "unsupported_media_type",
			"Send this as JSON.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()

	if err := dec.Decode(into); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return problem(w, http.StatusRequestEntityTooLarge, "body_too_large",
				"The batch is larger than this service accepts.")
		}
		return problem(w, http.StatusBadRequest, "unreadable_body",
			"That request could not be read. Check the field names against the contract.")
	}
	// Two documents in one body is a client bug, not a batch.
	if dec.More() {
		return problem(w, http.StatusBadRequest, "unreadable_body",
			"Send one request, not several.")
	}
	return nil
}

// writeJSON writes a response with the content type a client needs in order to
// parse it, and marked uncacheable — a device behind a shared proxy must not be
// served somebody else's page of changes.
func writeJSON(w http.ResponseWriter, status int, body any) error {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// An encode failure means the connection is already broken, so there is
	// nobody left to tell; the status line is already out. Returning the error
	// lets the caller log it.
	return json.NewEncoder(w).Encode(body)
}

// problem is a client-visible error: a status, a stable code to branch on, and a
// sentence a person could read.
//
// The code is for the client and the sentence is for the person holding the
// phone. Returning the code as the message would be a shrug.
// problem writes a client-visible error and returns ErrAnswered.
//
// The sentinel is the whole point, and its absence was a bug found only by
// running the service. Returning writeJSON's error means returning nil on
// success — and a handler that treats a non-nil error as "stop" cannot tell
// "refused, already answered" from "carried on". So a request with the wrong
// content type was answered with 415 and then processed anyway, writing a
// second body whose status line Go discarded with a "superfluous
// WriteHeader" warning, and the client received a truncated response.
//
// A refusal has to be distinguishable from a success, or the handler keeps
// going. The guard recognises this sentinel and adds nothing to the response.
func problem(w http.ResponseWriter, status int, code, message string) error {
	if err := writeJSON(w, status, map[string]any{"error": code, "message": message}); err != nil {
		return err
	}
	return ErrAnswered
}

// internal reports a fault to the log and describes none of it to the client.
//
// The split matters. A Postgres error names tables, columns and constraint names,
// and that is a schema handed to whoever can read the response. So the client
// gets a status and a sentence, and the log gets the error — which is why the
// error is actually logged rather than assigned to a blank identifier, as the
// first version of this function did while a comment beside it claimed the
// detail reached the log.
func (s *Sync) internal(w http.ResponseWriter, r *http.Request, err error) error {
	if s.Log != nil {
		// The request, never nil. Passing nil was a shortcut because this
		// function had been given only the writer, and it cost a panic: a
		// logger that reads r.Method dereferenced it, the handler died mid
		// response, and the client got nothing at all — so the very error this
		// function exists to report was the one thing never reported.
		s.Log.Refused(r, "handler_failed", err)
	}
	return problem(w, http.StatusInternalServerError, "internal_error",
		"That did not go through. Nothing was changed.")
}
