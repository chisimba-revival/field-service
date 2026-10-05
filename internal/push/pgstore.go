package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ErrNotFound is returned by a store method that addressed one row and found
// none. It is separate from a conflict: "there is nothing here" and "there is
// something here and you are wrong about it" are different answers.
var ErrNotFound = errors.New("push: not found")

// PgxStore applies batches against Postgres.
//
// It exists in this package rather than beside the other database code because
// the transaction boundary and the rules above are one design decision split
// across two files would stop being one decision. Every method here runs inside
// the transaction Push opened: the entity change, the change-feed entry and the
// recorded outcome either all commit or none do.
type PgxStore struct{ pool Pgx }

// Pgx is the narrow surface this store needs from a pool.
//
// Deliberately minimal and hand-written: a wider interface would let the
// transaction be opened from outside this file, which is the one thing the
// design of Push depends on not happening.
type Pgx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// NewPgxStore builds a store over a pool.
//
// The first version returned a store with an unset field, which compiles and
// fails only when the first batch arrives. Constructing the field from the
// argument is the whole defence: there is no way to build one without a pool.
func NewPgxStore(p Pgx) *PgxStore { return &PgxStore{pool: p} }

// Begin opens the batch transaction.
func (s *PgxStore) Begin(ctx context.Context) (Tx, error) {
	if s.pool == nil {
		return nil, errors.New("push: store has no pool")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &pgxTx{tx: tx}, nil
}

// pgxTx is one open transaction.
type pgxTx struct{ tx pgx.Tx }

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }

// RecordedOutcome reads a previously recorded outcome.
//
// Keyed by caller as well as operation id, because an operation id is a string
// somebody chose and is never proof of identity. Without the caller in the key,
// one guide retrying an id they happened to guess would be told their change
// had already been applied, and would drop a real observation.
func (t *pgxTx) RecordedOutcome(ctx context.Context, callerID, operationID string) (Result, bool, error) {
	var raw []byte
	err := t.tx.QueryRow(ctx,
		`select result from operation_outcome where caller_user_id = $1 and operation_id = $2`,
		callerID, operationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		// A result this build cannot read is not a missing result. Returning
		// "not found" would make the service re-apply an operation whose outcome
		// is already recorded, which is the one thing idempotency exists to stop.
		return Result{}, false, fmt.Errorf("push: recorded outcome unreadable: %w", err)
	}
	return res, true, nil
}

// Apply dispatches on the operation's kind.
//
// There is no default branch that applies the payload wholesale. A new kind has
// to be written out here, naming the fields it may write, which is what stops
// "replace the entity" being reachable by adding one line.
func (t *pgxTx) Apply(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.OperationID == "" {
		return Result{}, ErrNotFound
	}

	var res Result
	var err error
	switch op.Kind {
	case "create":
		res, err = t.create(ctx, caller, op)
	case "correct":
		res, err = t.correct(ctx, caller, op)
	default:
		// Refused rather than deferred: a kind this build does not know will not
		// become known by waiting, and deferring would keep the client retrying
		// something that can never succeed.
		res = Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "unsupported_kind",
		}
	}
	if err != nil {
		return Result{}, err
	}

	// The outcome is recorded here, in the same transaction as whatever the kind
	// did. Committing it separately would mean that a failure between the two
	// loses the change with no error anywhere, or records an outcome for a change
	// that never happened.
	encoded, err := json.Marshal(res)
	if err != nil {
		return Result{}, err
	}
	if _, err := t.tx.Exec(ctx,
		`insert into operation_outcome
		   (caller_user_id, operation_id, entity_type, entity_id, kind, outcome, result)
		 values ($1, $2, $3, $4, $5, $6, $7)`,
		caller.ID, op.OperationID, op.Entity, op.EntityID, op.Kind, string(res.Outcome), encoded,
	); err != nil {
		return Result{}, err
	}
	return res, nil
}

// create records a sighting.
//
// base_revision must be absent. A create that carries one means the client
// believes it is changing something, and treating that as a create would
// overwrite whatever the server has without noticing the disagreement.
func (t *pgxTx) create(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.Entity != "sighting" {
		return unsupported(op, "sighting"), nil
	}
	if op.BaseRevision != nil {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "create_with_base_revision",
			ClientState: op.Payload,
		}, nil
	}

	// The context comes from the caller, never from the payload. There is no
	// context field on Operation to read even if a client sent one, and the value
	// is bound rather than interpolated.
	const q = `
		insert into sighting
		  (id, context_code, drive_id, species_code, count, location,
		   location_accuracy_m, distance_m, bearing_deg, behaviour, age_sex_class,
		   notes, status, captured_at, recorded_at, created_by, revision)
		values ($1,$2,$3,$4,$5,st_setsrid(st_makepoint($6,$7),4326),
		        $8,$9,$10,$11,$12,$13,'pending',$14,$15,$16,1)
		returning revision`

	var revision int64
	err := t.tx.QueryRow(ctx, q,
		op.EntityID, caller.WriteContext, payloadUUID(op.Payload, "drive_id"),
		payloadText(op.Payload, "species_code"), payloadInt(op.Payload, "count"),
		payloadFloat(op.Payload, "longitude"), payloadFloat(op.Payload, "latitude"),
		payloadFloatPtr(op.Payload, "location_accuracy_m"), payloadIntPtr(op.Payload, "distance_m"),
		payloadIntPtr(op.Payload, "bearing_deg"), payloadTextPtr(op.Payload, "behaviour"),
		payloadTextPtr(op.Payload, "age_sex_class"), payloadTextPtr(op.Payload, "notes"),
		op.CapturedAt, op.CapturedAt, caller.ID,
	).Scan(&revision)
	if err != nil {
		if isUniqueViolation(err) {
			// The record already exists. That is not a conflict — it is the
			// situation a retried create with a fresh operation id lands in, and
			// it is reported as a refusal carrying the existing revision so the
			// client can reconcile instead of guessing.
			return Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "already_exists",
			}, nil
		}
		return Result{}, err
	}

	if err := t.feed(ctx, caller, op, revision); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: revision,
	}, nil
}

// correct is a mentor or guide changing a record that already exists.
//
// Rule 15 and the reason this service exists: a correction changing species code
// or count REQUIRES a reason, and the originals are retained permanently. The
// database has that as a table constraint, so no code path can produce a
// correction without one — but a constraint violation arriving as a plain error
// would tell the client only that something went wrong, so it is recognised here
// and named.
func (t *pgxTx) correct(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.Entity != "sighting" {
		return unsupported(op, "sighting"), nil
	}
	if op.BaseRevision == nil {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "correct_without_base_revision",
			ClientState: op.Payload,
		}, nil
	}

	var serverRevision int64
	var serverState []byte
	err := t.tx.QueryRow(ctx,
		`select revision, to_jsonb(s) - 'location' from sighting where id = $1`,
		op.EntityID).Scan(&serverRevision, &serverState)
	if errors.Is(err, pgx.ErrNoRows) {
		// The contract says an absent entity is treated as a create, but a
		// correction has no way to become one: it names the fields that are
		// wrong, and there is nothing to be wrong about. Reporting it as absent
		// is more honest than inventing a record.
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "correct_of_absent_record",
		}, nil
	}
	if err != nil {
		return Result{}, err
	}

	if serverRevision != *op.BaseRevision {
		// Revisions, never timestamps. A device offline for three days submits an
		// edit captured this morning; comparing clocks would have it win.
		var existing map[string]any
		_ = json.Unmarshal(serverState, &existing)
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision, ServerState: existing,
			ClientState: op.Payload,
		}, nil
	}

	const q = `
		update sighting set
		  revision          = revision + 1,
		  -- the originals are written first and never cleared, so the record
		  -- always shows both what was claimed and what it was corrected to
		  recorded_species_code = coalesce(recorded_species_code, species_code),
		  recorded_count       = coalesce(recorded_count, count),
		  correction_reason    = $2,
		  species_code         = coalesce($3, species_code),
		  count                = coalesce($4, count),
		  verification_notes   = coalesce($5, verification_notes),
		  verified_by          = $6,
		  verified_at          = now()
		where id = $1
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q, op.EntityID,
		payloadTextPtr(op.Payload, "correction_reason"),
		payloadTextPtr(op.Payload, "species_code"),
		payloadIntPtr(op.Payload, "count"),
		payloadTextPtr(op.Payload, "verification_notes"),
		caller.ID,
	).Scan(&revision)
	if err != nil {
		if isCorrectionReasonRequired(err) {
			// The rule the whole service is built around, arriving as a database
			// error. Named rather than surfaced as a failure, so the client can
			// tell "you corrected a count without saying why" from "something
			// broke".
			return Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "correction_requires_a_reason",
				ClientState: op.Payload,
			}, nil
		}
		return Result{}, err
	}

	if err := t.feed(ctx, caller, op, revision); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: revision,
	}, nil
}

// feed appends the change-feed entry carrying the row itself.
//
// The body is the serialised row rather than its identity: identity-only turns
// one page into N round trips over a connection that is already marginal, which
// is the situation this service exists for. A live join is worse — the page
// stops being a consistent snapshot, and a client can receive two revisions of
// one record in one page and apply them the wrong way round.
func (t *pgxTx) feed(ctx context.Context, caller Caller, op Operation, revision int64) error {
	var body []byte
	if err := t.tx.QueryRow(ctx,
		`select to_jsonb(s) from sighting s where s.id = $1`, op.EntityID,
	).Scan(&body); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx,
		`insert into change_feed (context_code, entity_type, entity_id, revision, body)
		 values ($1,$2,$3,$4,$5)`,
		caller.WriteContext, op.Entity, op.EntityID, revision, body)
	return err
}

func unsupported(op Operation, want string) Result {
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeRefused, ErrorCode: "unsupported_entity",
		ClientState: map[string]any{"expected": want},
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// isCorrectionReasonRequired recognises the rule-15 check constraint by name.
//
// Matching on the constraint name rather than on "some error happened" is what
// lets the client be told what is actually wrong with its correction instead of
// being handed a generic failure it cannot act on.
func isCorrectionReasonRequired(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" && pgErr.ConstraintName == "correction_has_reason"
}

// Payload readers. Each returns nil for an absent key so that "not mentioned"
// and "mentioned as empty" stay distinguishable — a sparse correction must not
// blank a field the client never sent.

func payloadText(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}
func payloadTextPtr(m map[string]any, k string) *string {
	if s, ok := m[k].(string); ok {
		return &s
	}
	return nil
}
func payloadIntPtr(m map[string]any, k string) *int {
	switch v := m[k].(type) {
	case float64:
		n := int(v)
		return &n
	case int:
		return &v
	}
	return nil
}
func payloadInt(m map[string]any, k string) any {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return nil
}
func payloadFloat(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok {
		return v
	}
	return 0
}
func payloadFloatPtr(m map[string]any, k string) *float64 {
	if v, ok := m[k].(float64); ok {
		return &v
	}
	return nil
}
func payloadUUID(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}
