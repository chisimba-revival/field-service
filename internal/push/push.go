// Package push applies what a field device recorded while it had no signal.
//
// This is the heart of the service. The contract's framing decides every
// awkward decision below: "The system's most valuable output is not a record but
// a correction." Everything here exists to make a correction possible months
// later, from a device that was offline, against a server that has moved on —
// and to never silently discard what the trainee wrote in the meantime.
//
// Four rules shape the code, and each one is a place where the obvious
// implementation is wrong.
//
// An operation is an INTENT, not a replacement. `end` carries an end time and a
// summary, not the whole drive as the client last saw it. Replacing the entity
// would overwrite fields the client never intended to touch, which is a conflict
// dressed up as a successful write. So each kind names the fields it may write,
// and there is no "replace everything" kind to reach for by mistake.
//
// Idempotency is keyed by OPERATION id, never entity id, and scoped to the
// caller. The contract is explicit: "retrying a start must not be mistaken for
// retrying the drive's creation." An operation id is also never proof of
// identity — it is a string somebody chose — so the outcome row is keyed by
// caller as well.
//
// Replay returns the ORIGINALLY RECORDED outcome, including a rejection. If a
// client retries an operation that was refused, handing it a fresh evaluation
// would let a second attempt succeed by being different, which is not what
// "retry" means. The outcome row is written in the same transaction as the
// entity change: committing it first and then failing the write would lose the
// observation with no error anywhere.
//
// Conflicts are refused, never merged, and the client's version is preserved
// alongside the server's. "The server never merges and never discards the
// client's edit." The client decides what to keep.
package push

import (
	"context"
	"errors"
	"time"
)

// Outcome is what happened to one operation.
type Outcome string

const (
	// OutcomeApplied means the operation changed something.
	OutcomeApplied Outcome = "applied"
	// OutcomeNoop means it had already been applied. Reported distinctly from
	// applied so a client can stop retrying without being told it did something.
	OutcomeNoop Outcome = "noop"
	// OutcomeDeferred means the service could not decide now. The client keeps
	// the operation; nothing was written.
	OutcomeDeferred Outcome = "deferred"
	// OutcomeRefused means the operation conflicts with the server's state, or
	// violates a rule. The client's submitted values are returned beside the
	// server's and both are kept.
	OutcomeRefused Outcome = "refused"
	// OutcomeUnknownOperation means an operation id presented after its recorded
	// outcome expired. Deliberately distinct from noop: re-applying it would be a
	// guess, and the contract requires refusal rather than silent reapplication.
	OutcomeUnknownOperation Outcome = "unknown_operation"
)

// terminal reports whether a retry must return this outcome rather than
// re-evaluating.
//
// Applied and refused are both terminal, and for different reasons: applied
// because redoing it would double the effect, refused because a client that
// changed the payload and resent it must get the original answer back. Deferred
// is NOT terminal — nothing was written, so there is nothing to remember.
func (o Outcome) terminal() bool {
	return o == OutcomeApplied || o == OutcomeRefused || o == OutcomeUnknownOperation
}

var (
	// ErrNoCaller is returned when a batch carries no caller.
	//
	// The outcome table is keyed by caller because an operation id is not proof
	// of identity. A batch with no caller therefore cannot be recorded at all,
	// and saying so up front beats writing an outcome row nobody can read.
	ErrNoCaller = errors.New("push: no caller")

	// ErrNoWriteContext is returned when the caller's token names no active
	// context.
	//
	// Refused rather than defaulted. Defaulting would mean inventing a context,
	// and a record written into an invented context is invisible to every guide
	// who actually holds that grant.
	ErrNoWriteContext = errors.New("push: token carries no active context")

	// ErrNoOperations is returned for an empty batch.
	//
	// Rejected rather than answered with an empty success, because a client
	// sending nothing has usually mis-built its queue, and an empty success would
	// hide that until the data it meant to send had aged out.
	ErrNoOperations = errors.New("push: batch carries no operations")
)

// Operation is one entry of a batch, as the client sent it.
type Operation struct {
	// OperationID is the retry identity. The client mints it before the first
	// attempt, so a crash mid-send does not lose the ability to retry.
	OperationID string `json:"operation_id"`
	Entity      string `json:"entity"`
	Kind        string `json:"kind"`
	EntityID    string `json:"entity_id"`

	// BaseRevision is the revision the client held when it made this change. It
	// is what makes a conflict detectable; nil means the client believes the
	// record does not exist yet.
	BaseRevision *int64 `json:"base_revision"`

	CapturedAt time.Time      `json:"captured_at"`
	Payload    map[string]any `json:"payload"`
	DependsOn  []string       `json:"depends_on,omitempty"`

	// There is deliberately no context_code field. The contract says writes do
	// not accept a caller-supplied context: a record goes into the caller's
	// active context. A client cannot write into a context it was not granted,
	// because there is nowhere to put one — which is the property being relied
	// on, not an omission.
}

// Result is one operation's outcome, as the client receives it.
type Result struct {
	OperationID string  `json:"operation_id"`
	Entity      string  `json:"entity"`
	EntityID    string  `json:"entity_id"`
	Outcome     Outcome `json:"outcome"`

	// NewRevision is set when something was applied, so the client can update
	// its local row without a second round trip.
	NewRevision int64 `json:"new_revision,omitempty"`

	// ServerRevision is the revision the server actually holds, sent with a
	// conflict. The contract requires the current server state and revision
	// alongside the client's submitted values so the two can be compared; a
	// client told only that a conflict occurred has nothing to decide with.
	ServerRevision int64 `json:"server_revision,omitempty"`

	// ErrorCode is a stable machine-readable word. The client branches on it;
	// humans never see it, because a raw code on a phone at a waterhole is a
	// shrug rather than a message.
	ErrorCode string `json:"error_code,omitempty"`

	// ServerState and ClientState are both present on a conflict, and both are
	// kept. "The client decides what to keep."
	ServerState map[string]any `json:"server_state,omitempty"`
	ClientState map[string]any `json:"client_state,omitempty"`
}

// Caller is who is pushing, and where a new record goes.
//
// This is a value rather than a set of strings so that the rule about context
// cannot be got wrong by forgetting a parameter. Rule 11 needs both a scope and
// a grant in a context; writes then go into the caller's ACTIVE context, which
// is a third thing again. Handing a push three loose strings invites the caller
// to pass the grant set where the write context belongs, and the result would be
// records written into a context the token never named.
type Caller struct {
	// ID is the Chisimba user id, unchanged. Rule 2 is explicit: no surrogate
	// keys and no re-mapping.
	ID string

	// WriteContext is the token's active context. A new record goes here and
	// nowhere else. Operation has no context field at all, so a client cannot
	// write into a context it was not granted — there is nowhere to put one,
	// which is the property being relied on rather than an omission.
	WriteContext string
}

// usable reports whether this caller can be recorded at all.
//
// Both fields are required and for different reasons. A caller with no id
// cannot own an outcome row. A caller with no active context cannot be granted
// one either: the contract has a record's context come from the token, so a
// token carrying none is not a caller who may write.
func (c Caller) usable() bool { return c.ID != "" && c.WriteContext != "" }

// Store is the database work a push needs.
//
// Declared as an interface so the rules above can be tested without Postgres,
// and so the transaction boundary is visible in one place: the transaction is
// opened by the Store and handed to every call, which is what lets an entity
// change and its recorded outcome commit together or not at all.
type Store interface {
	// Begin opens the transaction the whole batch runs in.
	Begin(ctx context.Context) (Tx, error)
}

// Tx is one open transaction. Every method takes it as its first argument
// because a Store that opened its own transaction per call could not promise
// that an entity change and its outcome row commit together — which is the
// single most important guarantee in this file.
type Tx interface {
	// RecordedOutcome returns the outcome previously recorded for this caller
	// and operation id, or false if there is none.
	RecordedOutcome(ctx context.Context, callerID, operationID string) (Result, bool, error)

	// Apply performs one operation and records its outcome. Both writes MUST
	// happen in this transaction.
	Apply(ctx context.Context, caller Caller, op Operation) (Result, error)

	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// Service applies batches.
type Service struct {
	store Store
}

// New builds a service over a store.
func New(store Store) *Service { return &Service{store: store} }

// Push applies a batch and returns one result per operation, in the order sent.
//
// Results come back in request order rather than grouped by outcome, because a
// client matching them by position is far simpler than one matching by operation
// id, and the batch is bounded by the contract anyway.
//
// A batch is NOT all-or-nothing. One refused operation does not discard the
// others: a device that recorded four sightings and had one refused needs three
// of them to survive. The thing that must never be half-done is a single
// operation — its entity change and its outcome row — which is why both live in
// the transaction this method opens.
func (s *Service) Push(ctx context.Context, caller Caller, ops []Operation) ([]Result, error) {
	if caller.ID == "" {
		return nil, ErrNoCaller
	}
	if caller.WriteContext == "" {
		return nil, ErrNoWriteContext
	}
	if len(ops) == 0 {
		return nil, ErrNoOperations
	}

	tx, err := s.store.Begin(ctx)
	if err != nil {
		return nil, err
	}
	// Rollback after a successful Commit is harmless in pgx and returns
	// ErrTxClosed, which is not worth reporting; the commit's own error is.
	defer func() { _ = tx.Rollback(ctx) }()

	results := make([]Result, 0, len(ops))
	for _, op := range ops {
		results = append(results, s.one(ctx, tx, caller, op))
	}

	if err := tx.Commit(ctx); err != nil {
		// Nothing is applied, so returning the per-operation results would be a
		// lie: the caller has to be able to tell that the whole batch failed,
		// because otherwise it looks like a batch that was applied.
		return nil, err
	}
	return results, nil
}

// one applies a single operation, honouring a recorded outcome if there is one.
func (s *Service) one(ctx context.Context, tx Tx, caller Caller, op Operation) Result {
	if op.OperationID == "" {
		return Result{
			Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "missing_operation_id",
			ClientState: op.Payload,
		}
	}

	recorded, found, err := tx.RecordedOutcome(ctx, caller.ID, op.OperationID)
	if err != nil {
		// The outcome table could not be read. Deferring rather than refusing is
		// the right direction: this operation may already have been applied, and
		// refusing it would tell the client its change was rejected when it might
		// already be committed. Deferred means "ask again", which is true.
		return deferral(op, "outcome_unreadable")
	}
	if found {
		// The originally recorded outcome, including a rejection. Re-evaluating
		// would let a retry succeed by being different, which is not a retry.
		return recorded
	}

	res, err := tx.Apply(ctx, caller, op)
	if err != nil {
		return deferral(op, "apply_failed")
	}
	return res
}

func deferral(op Operation, code string) Result {
	return Result{
		OperationID: op.OperationID,
		Entity:      op.Entity,
		EntityID:    op.EntityID,
		Outcome:     OutcomeDeferred,
		ErrorCode:   code,
	}
}
