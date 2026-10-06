// Package verify applies a mentor's answer to a log book entry.
//
// It is a separate package from push because it is a different kind of request
// arriving a different way. Push carries a trainee's offline batch through the
// person door, and every rule in it is written around a token that names an
// active write context. This arrives through the service door, with no context
// on the token at all, and the context comes in the request instead.
//
// That difference is why the rules live here rather than being added to push. A
// verification that quietly reused push's context handling would be validating
// itself against a token it does not have.
package verify

import (
	"context"
	"strings"
)

// Outcome is what a mentor concluded. It is deliberately not a free string: the
// three answers below are the only ones that mean something, and a fourth would
// be a status the rest of the system has no handling for.
type Outcome string

const (
	// OutcomeVerified is a mentor committing to the claim as it stands.
	OutcomeVerified Outcome = "verified"
	// OutcomeRejected is examined and found wanting.
	OutcomeRejected Outcome = "rejected"
	// OutcomeNeedsReview is examined without a verdict. It exists so a mentor can
	// be honest about uncertainty rather than being pushed into a decision they
	// have not earned.
	OutcomeNeedsReview Outcome = "needs_review"
)

func validOutcome(o Outcome) bool {
	switch o {
	case OutcomeVerified, OutcomeRejected, OutcomeNeedsReview:
		return true
	}
	return false
}

// Request is one mentor's answer to one entry.
//
// Context and Mentor arrive here rather than being read off the token, because a
// service token names no user and carries no context. The microservice checks
// both against its own state and its own rules; it does not take them on trust.
type Request struct {
	OperationID string  `json:"operation_id"`
	EntryID     string  `json:"entry_id"`
	Context     string  `json:"context"`
	Mentor      string  `json:"mentor"`
	Outcome     Outcome `json:"outcome"`

	// Corrections. A nil field means "leave it as the trainee recorded it", which
	// is different from setting it to the empty string.
	SpeciesCode *string `json:"species_code"`
	Count       *int    `json:"count"`

	// Notes is the mentor's reasoning. It is teaching content and is required by
	// CorrectionReason's rule below whenever the answer changes the claim.
	Notes string `json:"verification_notes"`

	// CorrectionReason is required whenever SpeciesCode or Count is supplied. It
	// is a separate field from Notes on purpose: one is the explanation of the
	// change and the other is the conversation, and merging them means a later
	// edit to the conversation can quietly erase the reason the trainee's claim
	// was overruled.
	CorrectionReason string `json:"correction_reason"`

	// BaseRevision is the revision the mentor believed they were answering. It is
	// required, because a verification against a revision the mentor never saw is
	// an authority decision about a record they did not read.
	BaseRevision *int64 `json:"base_revision"`
}

// Result is the outcome as the client is told.
type Result struct {
	OperationID  string `json:"operation_id"`
	EntryID      string `json:"entry_id"`
	Outcome      string `json:"outcome"`
	ErrorCode    string `json:"error_code,omitempty"`
	NewRevision  *int64 `json:"new_revision,omitempty"`
	VerifiedBy   string `json:"verified_by,omitempty"`
	VerifiedAt   string `json:"verified_at,omitempty"`
	ClientReason string `json:"-"`
}

// Store applies a decision and reads the state it depends on.
type Store interface {
	// Verify applies req, or explains why it did not.
	Verify(ctx context.Context, req Request) (Result, error)
}

// Service applies mentor decisions.
type Service struct{ store Store }

// New builds a service over a store.
func New(store Store) *Service { return &Service{store: store} }

// Verify validates and applies.
//
// The order is deliberate: every refusal is decided before the store is touched,
// so a malformed request cannot half-apply. A verification that wrote a status
// and then failed on a missing reason would leave an entry verified by a mentor
// who never gave one, and the reason is the whole teaching content of the row.
func (s *Service) Verify(ctx context.Context, req Request) (Result, error) {
	if err := check(req); err != nil {
		return refusal(req, err), nil
	}
	return s.store.Verify(ctx, req)
}

// RefusedError is a client mistake, described by a stable code.
type RefusedError struct{ Code string }

func (e *RefusedError) Error() string { return "verify: " + e.Code }

func refuse(code string) error { return &RefusedError{Code: code} }

// check applies the rules that need no database.
//
// It is a separate function so it can be exercised without one, and so the store
// can be handed a request already known to be well-formed. Every code here is a
// decision a mentor can act on.
func check(req Request) error {
	if !isUUID(req.OperationID) {
		// The client's own idempotency key. Without it a retry after a dropped
		// response is a second verification of an entry the mentor already
		// answered, and the entry would gain a revision for a decision made once.
		return refuse("verify_without_operation_id")
	}
	if !isUUID(req.EntryID) {
		return refuse("verify_without_entry_id")
	}
	if req.Context == "" {
		return refuse("verify_without_context")
	}
	if strings.TrimSpace(req.Mentor) == "" {
		// A verification names who made it. An unattributed judgement cannot be
		// reviewed, appealed, or learned from.
		return refuse("verify_without_mentor")
	}
	if !validOutcome(req.Outcome) {
		// Refused rather than defaulted. Defaulting to needs_review would make a
		// typo look like a mentor being cautious, which is a different statement
		// about a trainee's work than the one that was made.
		return refuse("unknown_outcome")
	}
	if req.BaseRevision == nil {
		return refuse("verify_without_base_revision")
	}
	// The rule that makes this an authority decision rather than an edit.
	if corrects(req) && strings.TrimSpace(req.CorrectionReason) == "" {
		return refuse("correction_without_reason")
	}
	return nil
}

// corrects reports whether the request changes the trainee's claim.
func corrects(req Request) bool {
	return req.SpeciesCode != nil || req.Count != nil
}

func refusal(req Request, err error) Result {
	code := ""
	if re, ok := err.(*RefusedError); ok {
		code = re.Code
	}
	return Result{
		OperationID: req.OperationID,
		EntryID:     req.EntryID,
		Outcome:     "refused",
		ErrorCode:   code,
	}
}

// isUUID reports whether s is a canonical uuid.
//
// Length and character position rather than a parse: the only thing the database
// would accept is a uuid, so anything else is a client mistake to be described
// rather than passed down to be reported as a syntax error.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			isHex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}
