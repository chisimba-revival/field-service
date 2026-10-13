package push

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

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
	return &pgxTx{tx: tx, now: time.Now().UTC}, nil
}

// pgxTx is one open transaction.
type pgxTx struct {
	tx pgx.Tx
	// now is the service's own clock, injectable so a test can tell a
	// service-recorded time from a client-supplied one.
	now func() time.Time
}

func (t *pgxTx) clock() time.Time {
	if t.now == nil {
		return time.Now().UTC()
	}
	return t.now()
}

func (t *pgxTx) Commit(ctx context.Context) error   { return t.tx.Commit(ctx) }
func (t *pgxTx) Rollback(ctx context.Context) error { return t.tx.Rollback(ctx) }
func (t *pgxTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.tx.Exec(ctx, sql, args...)
}

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
		// The entity is dispatched on before the kind is handled further down, so
		// that adding an entity does not mean adding a case to every handler. A
		// create names what it is creating; the handlers below each check that
		// too, and refuse an entity they do not serve.
		switch op.Entity {
		case "outing":
			res, err = t.createOuting(ctx, caller, op)
		case entityLogBookEntry:
			res, err = t.create(ctx, caller, op)
		case entityDangerousGameEncounter:
			res, err = t.createEncounter(ctx, caller, op)
		case entityTrailWaypoint:
			res, err = t.createTrailWaypoint(ctx, caller, op)
		case entityPlannedRoute:
			res, err = t.createPlannedRoute(ctx, caller, op)
		case entityRouteWaypoint:
			res, err = t.createRouteWaypoint(ctx, caller, op)
		default:
			res = Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "unsupported_entity",
				ClientState: map[string]any{
					"expected": []string{"outing", entityLogBookEntry,
						entityDangerousGameEncounter, entityTrailWaypoint},
				},
			}
		}
	case "correct":
		res, err = t.correct(ctx, caller, op)
	case "verify":
		res, err = t.verify(ctx, caller, op)
	case "update":
		switch op.Entity {
		case "outing":
			res, err = t.updateOuting(ctx, caller, op)
		case entityPlannedRoute:
			res, err = t.updatePlannedRoute(ctx, caller, op)
		case entityRouteWaypoint:
			res, err = t.updateRouteWaypoint(ctx, caller, op)
		default:
			res = Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "unsupported_entity_for_update",
				ClientState: map[string]any{
					"expected": []string{"outing", entityPlannedRoute, entityRouteWaypoint},
				},
			}
		}
	case "delete":
		switch op.Entity {
		case entityPlannedRoute:
			res, err = t.deletePlannedRoute(ctx, caller, op)
		case entityRouteWaypoint:
			res, err = t.deleteRouteWaypoint(ctx, caller, op)
		default:
			res = Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "unsupported_entity_for_delete",
				ClientState: map[string]any{
					"expected": []string{entityPlannedRoute, entityRouteWaypoint},
				},
			}
		}
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

// create records a log book entry.
//
// base_revision must be absent. A create that carries one means the client
// believes it is changing something, and treating that as a create would
// overwrite whatever the server has without noticing the disagreement.
func (t *pgxTx) create(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.Entity != entityLogBookEntry {
		return unsupported(op, entityLogBookEntry), nil
	}
	if op.BaseRevision != nil {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "create_with_base_revision",
			ClientState: op.Payload,
		}, nil
	}

	// A payload missing a field it must carry is refused, not attempted.
	//
	// Found by sending a real request with no outing_id: it became the empty
	// string, the insert failed with "invalid input syntax for type uuid", and
	// the whole batch came back as a 500. Every other client mistake here is
	// refused — an unknown kind, a create carrying a base_revision, a correction
	// with no revision — so letting a missing field through to the database was
	// an inconsistency as well as a fault. Worse than the 500 was the direction:
	// one bad operation in a batch of four took all four with it, and the rule is
	// that a batch is not all-or-nothing.
	if !isUUID(payloadUUID(op.Payload, "outing_id")) {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "create_without_outing_id",
			ClientState: op.Payload,
		}, nil
	}

	// An outing that does not exist, or exists in a context this caller cannot
	// write to, is refused here rather than left to the foreign key.
	//
	// Two reasons, and they are different in kind.
	//
	// The first is consistency. Every other client mistake in this file is a
	// refusal — an unknown kind, a create carrying a base_revision, a missing
	// outing_id. An outing that is not there is the same kind of mistake, and it was
	// arriving as a 500 because the constraint violation was the thing reporting
	// it. Worse than the status was the direction: one bad operation in a batch
	// of four took all four with it, and a batch is not all-or-nothing.
	//
	// The second is that this check cannot be left to the database. Push opens
	// its own transaction and never sets app.context_grants, and the runtime it
	// is deployed against is a superuser, which PostgreSQL exempts from
	// row-level security outright. The policies would not have caught this even
	// if the foreign key had been satisfied — so an outing in another context is
	// refused because this code compares the two, not because something
	// downstream was trusted to.
	//
	// The two cases deliberately share one error code. Reporting "no such
	// outing" for one that exists in somebody else's context would turn this
	// into an oracle: a caller could walk the id space and learn which outings
	// other reserves have planned, which is exactly what the grant set exists to
	// withhold.
	known, err := t.outingInContext(ctx, payloadUUID(op.Payload, "outing_id"), caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "unknown_outing",
			ClientState: op.Payload,
		}, nil
	}

	// The context comes from the caller, never from the payload. There is no
	// context field on Operation to read even if a client sent one, and the value
	// is bound rather than interpolated.
	const q = `
		insert into log_book_entry
		  (id, context_code, outing_id, species_code, count, location,
		   location_accuracy_m, distance_m, bearing_deg, behaviour, age_sex_class,
		   notes, status, captured_at, recorded_at, created_by, revision)
		values ($1,$2,$3,$4,$5,st_setsrid(st_makepoint($6,$7),4326),
		        $8,$9,$10,$11,$12,$13,'pending',$14,$15,$16,1)
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID, caller.WriteContext, payloadUUID(op.Payload, "outing_id"),
		payloadText(op.Payload, "species_code"), payloadInt(op.Payload, "count"),
		payloadFloat(op.Payload, "longitude"), payloadFloat(op.Payload, "latitude"),
		payloadFloatPtr(op.Payload, "location_accuracy_m"), payloadIntPtr(op.Payload, "distance_m"),
		payloadIntPtr(op.Payload, "bearing_deg"), payloadTextPtr(op.Payload, "behaviour"),
		payloadTextPtr(op.Payload, "age_sex_class"), payloadTextPtr(op.Payload, "notes"),
		op.CapturedAt, recordedAt(op, t.now()), caller.ID,
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
// outingInContext reports whether an outing exists in the given context.
//
// The context is compared here rather than being relied upon to be enforced
// downstream, because on this path nothing downstream is enforcing it: see the
// note at the refusal above. A true answer means the outing is in that context;
// a false answer means it is not there, or is somewhere this caller cannot see,
// and the caller is told the same thing either way.
func (t *pgxTx) outingInContext(ctx context.Context, outingID, context string) (bool, error) {
	const q = `select exists (
		select 1 from outing where id = $1 and context_code = $2
	)`
	var known bool
	if err := t.tx.QueryRow(ctx, q, outingID, context).Scan(&known); err != nil {
		return false, err
	}
	return known, nil
}

func (t *pgxTx) correct(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.Entity != entityLogBookEntry {
		return unsupported(op, entityLogBookEntry), nil
	}
	if op.BaseRevision == nil {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "correct_without_base_revision",
			ClientState: op.Payload,
		}, nil
	}

	var serverRevision int64
	var serverSpecies, serverCount *string
	var serverNotes *string
	var serverState []byte
	err := t.tx.QueryRow(ctx,
		`select s.revision, s.species_code, s.count::text, s.notes,
		        to_jsonb(s) - 'location'
		   from log_book_entry s where s.id = $1`, op.EntityID).
		Scan(&serverRevision, &serverSpecies, &serverCount, &serverNotes, &serverState)
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

	// Rule 15 is checked HERE, before the update, and not by catching the
	// constraint violation. Postgres aborts the entire transaction the moment a
	// statement fails, so a version of this that attempted the update and turned
	// the error into a refusal would find the batch already dead — every
	// operation after it would fail with "current transaction is aborted", and
	// the refusal would be reported alongside a transaction that could not
	// commit. The check constraint stays as the backstop that no other code path
	// can skip; this is what makes the violation a normal outcome instead of a
	// dead batch.
	newSpecies := payloadTextPtr(op.Payload, "species_code")
	newCount := payloadIntPtr(op.Payload, "count")
	reason := payloadTextPtr(op.Payload, "correction_reason")
	// An amendment is not a field of this store's own invention: the client
	// sends the same keys create() reads, so that the operation which corrects a
	// record and the operation which wrote it speak one vocabulary. Before this
	// was read, a note-only amendment arrived carrying none of the keys above,
	// every coalesce below resolved to null, and the answer the client received
	// was OutcomeApplied — the record was acknowledged as changed and nothing
	// about it had changed. The reason was, in fact, cleared.
	newBehaviour := payloadTextPtr(op.Payload, "behaviour")
	newAgeSexClass := payloadTextPtr(op.Payload, "age_sex_class")
	newNotes := payloadTextPtr(op.Payload, "notes")
	for _, e := range []struct {
		key string
		val *string
	}{
		{"behaviour", newBehaviour},
		{"age_sex_class", newAgeSexClass},
	} {
		if e.val == nil {
			continue
		}
		known, err := t.enumValue(ctx, e.key, *e.val)
		if err != nil {
			return Result{}, err
		}
		if !known {
			return Result{
				OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
				Outcome: OutcomeRefused, ErrorCode: "unknown_" + e.key,
				ServerState: map[string]any{e.key: *e.val},
				ClientState: op.Payload,
			}, nil
		}
	}
	// Counts are compared as ints. They arrive from the database as text because
	// a nullable integer scanned through a driver that has to handle the null
	// case is easier to read that way, and a lexical comparison would decide "10"
	// is less than "9" — the kind of comparison that calls a changed record
	// unchanged and lets a correction through with no reason.
	existingCount, countErr := strconv.Atoi(derefOr(serverCount, ""))
	if countErr != nil {
		existingCount = -1
	}
	changesClaim := (newSpecies != nil && (serverSpecies == nil || *newSpecies != *serverSpecies)) ||
		(newCount != nil && *newCount != existingCount)
	if changesClaim && (reason == nil || *reason == "") {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "correction_requires_a_reason",
			ServerState: map[string]any{
				"species_code": derefOr(serverSpecies, ""),
				"count":        derefOr(serverCount, ""),
			},
			ClientState: op.Payload,
		}, nil
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
		update log_book_entry set
		  revision          = revision + 1,
		  -- the originals are written first and never cleared, so the record
		  -- always shows both what was claimed and what it was corrected to
		  --
		  -- Only a correction that actually moves the claim stamps it. Writing
		  -- the present value as "what was claimed" when nothing was corrected
		  -- asserts that the record was corrected when it was not, and leaves
		  -- recorded_* set beside an empty correction_reason — which
		  -- correction_has_reason refuses, so an amendment carrying nothing but
		  -- a note would be a constraint violation rather than an amendment.
		  recorded_species_code = case
		    when $3::text is not null and $3::text is distinct from species_code
		      then coalesce(recorded_species_code, species_code)
		    else recorded_species_code end,
		  recorded_count       = case
		    when $4::int is not null and $4::int is distinct from count
		      then coalesce(recorded_count, count)
		    else recorded_count end,
		  -- a reason is never cleared by a later amendment that carries none.
		  -- Overwriting it would leave recorded_* set with no reason beside it,
		  -- which is exactly what the check constraint forbids
		  correction_reason    = coalesce($2, correction_reason),
		  species_code         = coalesce($3, species_code),
		  count                = coalesce($4, count),
		  verification_notes   = coalesce($5, verification_notes),
		  behaviour            = coalesce($7, behaviour),
		  age_sex_class        = coalesce($8, age_sex_class),
		  notes                = coalesce($9, notes),
		  verified_by          = $6,
		  verified_at          = now()
		where id = $1
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q, op.EntityID,
		reason, newSpecies, newCount,
		payloadTextPtr(op.Payload, "verification_notes"),
		caller.ID,
		newBehaviour, newAgeSexClass, newNotes,
	).Scan(&revision)
	if err != nil {
		// A constraint violation reaching here means the check above and the
		// constraint disagree, which is a bug in this file rather than a client's
		// mistake. It is reported as an error so it fails the batch loudly: it
		// must not be turned into a deferral, because a deferral tells the client
		// to try again when the truth is that this service is broken.
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

// verify applies a mentor's decision to a log book entry.
//
// It is the person-door equivalent of the service-door verify endpoint.
// The trainee (or mentor on the device) submits a verification outcome
// (verified/rejected/needs_review) with optional corrections to species/count
// and the mentor's reasoning. The service requires a correction_reason when
// species or count is changed. The base revision ensures the mentor read the
// current state of the record.
func (t *pgxTx) verify(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if op.Entity != entityLogBookEntry {
		return unsupported(op, entityLogBookEntry), nil
	}
	if op.BaseRevision == nil {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "verify_without_base_revision",
			ClientState: op.Payload,
		}, nil
	}

	// Validate outcome
	outcomeStr := payloadText(op.Payload, "outcome")
	if outcomeStr != "verified" && outcomeStr != "rejected" && outcomeStr != "needs_review" {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "unknown_outcome",
			ClientState: op.Payload,
		}, nil
	}

	// Mentor is the caller (person token)
	if caller.ID == "" {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "verify_without_mentor",
			ClientState: op.Payload,
		}, nil
	}

	// Verify context - the entry must be in the caller's context
	var currentRevision int64
	var currentSpecies *string
	var currentCount *int
	var currentStatus string
	err := t.tx.QueryRow(ctx,
		`select revision, species_code, count, status::text
		   from log_book_entry
		  where id = $1 and context_code = $2 and deleted_at is null`,
		op.EntityID, caller.WriteContext).
		Scan(&currentRevision, &currentSpecies, &currentCount, &currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "unknown_log_book_entry",
		}, nil
	}
	if err != nil {
		return Result{}, err
	}

	// Check base revision matches
	if *op.BaseRevision != currentRevision {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: currentRevision,
			ClientState:    op.Payload,
		}, nil
	}

	// Cannot re-verify an already decided entry
	if currentStatus != "pending" && currentStatus != "needs_review" {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "already_decided",
			ClientState: op.Payload,
		}, nil
	}

	// Rule 15: correction requires reason
	newSpecies := payloadTextPtr(op.Payload, "species_code")
	newCount := payloadIntPtr(op.Payload, "count")
	reason := payloadTextPtr(op.Payload, "correction_reason")
	changesClaim := (newSpecies != nil && (currentSpecies == nil || *newSpecies != *currentSpecies)) ||
		(newCount != nil && *newCount != derefOrInt(currentCount, -1))
	if changesClaim && (reason == nil || *reason == "") {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "correction_requires_a_reason",
			ServerState: map[string]any{
				"species_code": derefOr(currentSpecies, ""),
				"count":        derefOrInt(currentCount, -1),
			},
			ClientState: op.Payload,
		}, nil
	}

	// Record what was claimed before correction
	var recordedSpecies *string
	var recordedCount *int
	if newSpecies != nil || newCount != nil {
		recordedSpecies = currentSpecies
		recordedCount = currentCount
	}

	// Apply the verification
	const q = `
		update log_book_entry set
		  revision          = revision + 1,
		  status            = $3::sighting_status,
		  verified_by       = $4,
		  verified_at       = $5,
		  verification_notes = nullif($6, ''),
		  correction_reason = coalesce(nullif($7, ''), correction_reason),
		  species_code      = $8,
		  count             = $9,
		  recorded_species_code = coalesce($10, recorded_species_code),
		  recorded_count    = coalesce($11, recorded_count),
		  revision          = $12
		where id = $1 and context_code = $2
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID, caller.WriteContext,
		payloadText(op.Payload, "outcome"),
		caller.ID,
		time.Now().UTC(),
		payloadTextPtr(op.Payload, "verification_notes"),
		payloadTextPtr(op.Payload, "correction_reason"),
		newSpecies, newCount,
		recordedSpecies, recordedCount,
		currentRevision+1,
	).Scan(&revision)
	if err != nil {
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
// stops being a consistent snapshot, and a client can receive two revisions of
// one record in one page and apply them the wrong way round.
func (t *pgxTx) feed(ctx context.Context, caller Caller, op Operation, revision int64) error {
	// The table is chosen from the entity rather than assumed to be the log book.
	//
	// Reading the log book for every operation meant a create of anything else
	// returned no row, and a no-row error inside a transaction poisons it: the
	// failure surfaced as "commit unexpectedly resulted in rollback" on an
	// operation that had been applied, which is the worst of both — a client told
	// the write failed, retrying forever, against a row that was there all along.
	table, ok := feedTables[op.Entity]
	if !ok {
		return fmt.Errorf("push: no change-feed table for entity %q", op.Entity)
	}
	var body []byte
	if err := t.tx.QueryRow(ctx,
		`select to_jsonb(t) from `+table+` t where t.id = $1`, op.EntityID,
	).Scan(&body); err != nil {
		return err
	}
	_, err := t.tx.Exec(ctx,
		`insert into change_feed (context_code, entity_type, entity_id, revision, body)
		 values ($1,$2,$3,$4,$5)`,
		caller.WriteContext, op.Entity, op.EntityID, revision, body)
	return err
}

// feedTables maps an entity to the table its change-feed body is read from.
//
// A map rather than a switch because the identifier is interpolated, and a
// switch whose arms were all literals could be read as safe. Here the value is
// only ever reached through this map, so an entity with no arm is an error
// before anything is built, not a query against whatever the client named.
var feedTables = map[string]string{
	entityLogBookEntry:           entityLogBookEntry,
	"outing":                     "outing",
	entityDangerousGameEncounter: entityDangerousGameEncounter,
	entityTrailWaypoint:          "trail_waypoint",
	entityPlannedRoute:           "planned_routes",
	entityRouteWaypoint:          "route_waypoints",
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
func derefOr(s *string, fallback string) string {
	if s == nil {
		return fallback
	}
	return *s
}

func derefOrInt(i *int, fallback int) int {
	if i == nil {
		return fallback
	}
	return *i
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

// enumValue reports whether v is a member of typeName.
//
// The check is here because an enum column reached with a value the type does
// not have returns an error, and an error from correct() fails the entire batch
// — every other operation in it, belonging to other people, is lost with it. A
// client that is a version behind and sends a behaviour this database does not
// have yet has made a mistake, not proven this service broken, so it gets a
// refusal scoped to its own operation. typeName is never client input: the only
// two call sites pass literals in this file.
func (t *pgxTx) enumValue(ctx context.Context, typeName, v string) (bool, error) {
	var ok bool
	err := t.tx.QueryRow(ctx,
		`select exists (select 1 from unnest(enum_range(null::`+typeName+`)) e where e::text = $1)`,
		v,
	).Scan(&ok)
	if err != nil {
		return false, err
	}
	return ok, nil
}

// payloadInt handles both numeric kinds a payload can carry.
//
// It used to accept only float64, while its nullable twin accepted float64 and
// int. A payload built with a native Go int therefore got a silent NULL: the
// sighting was recorded with no count, and nothing reported an error — the worst
// shape a bug can have here, because rule 21 is precisely that an absent count
// and a count of zero mean different things and neither may be invented.
func payloadInt(m map[string]any, k string) any {
	if v := payloadIntPtr(m, k); v != nil {
		return *v
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
func payloadBoolPtr(m map[string]any, k string) *bool {
	if v, ok := m[k].(bool); ok {
		return &v
	}
	return nil
}

// isUUID reports whether s is a uuid as PostgreSQL would accept one.
//
// Checked here rather than left to the database so the answer is a refusal with a
// code a client can branch on, instead of a 500 with a type error in it. The
// format is checked by hand rather than parsed, because the only question is
// "would the database accept this", and a parser would be a second, looser idea
// of the same thing.
// recordedAt is when the record was written down, which rule 13 says is a
// different fact from when the thing was seen.
//
// The client's value is preferred because it knows when the trainee actually
// wrote it down, and a device out of coverage records that honestly. A client
// that did not say gets the service's own clock rather than a copy of
// captured_at: binding both from one value makes the two permanently identical,
// which is the thing rule 13 exists to prevent, and it would hide the difference
// between an observation made at dawn and an entry written up that evening.
func recordedAt(op Operation, now time.Time) time.Time {
	if !op.RecordedAt.IsZero() {
		return op.RecordedAt
	}
	return now
}

func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
			if !isHex {
				return false
			}
		}
	}
	return true
}

func payloadUUID(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}

// updatePlannedRoute updates a planned route's outing_id or timestamps.
//
// A planned route is created with an outing_id and can be updated to change
// its outing association or just touch its updated_at timestamp. The base
// revision ensures optimistic concurrency.
func (t *pgxTx) updatePlannedRoute(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "update_without_entity_id"), nil
	}
	if op.BaseRevision == nil {
		return refuse(op, "update_without_base_revision"), nil
	}

	var serverRevision int64
	var serverOutingID string
	err := t.tx.QueryRow(ctx,
		`select revision, outing_id from planned_routes where id = $1 and context_code = $2`,
		op.EntityID, caller.WriteContext).
		Scan(&serverRevision, &serverOutingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "update_unknown_planned_route"), nil
	}
	if err != nil {
		return Result{}, err
	}

	if serverRevision != *op.BaseRevision {
		var existing map[string]any
		_ = json.Unmarshal([]byte("{}"), &existing) // no serverState in query above
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision, ServerState: existing,
			ClientState: op.Payload,
		}, nil
	}

	// Optional: update outing_id if provided
	newOutingID := payloadUUID(op.Payload, "outing_id")
	if isUUID(newOutingID) {
		known, err := t.outingInContext(ctx, newOutingID, caller.WriteContext)
		if err != nil {
			return Result{}, err
		}
		if !known {
			return refuse(op, "unknown_outing"), nil
		}
	}

	const q = `
		update planned_routes set
		  revision    = revision + 1,
		  outing_id   = coalesce($2::uuid, outing_id),
		  updated_at  = now()
		where id = $1 and context_code = $3
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID,
		payloadUUID(op.Payload, "outing_id"),
		caller.WriteContext,
	).Scan(&revision)
	if err != nil {
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

// updateRouteWaypoint updates a waypoint's position or metadata.
//
// Waypoints are append-only by contract (no delete path in the client), but
// the server supports sparse updates to position, label, kind, or note.
// The base revision ensures optimistic concurrency.
func (t *pgxTx) updateRouteWaypoint(ctx context.Context, caller Caller, op Operation) (Result, error) {
	parts := splitEntityID(op.EntityID)
	if len(parts) != 2 {
		return refuse(op, "route_waypoint_invalid_entity_id"), nil
	}
	routeID := parts[0]
	ordinal, err := strconv.Atoi(parts[1])
	if err != nil || ordinal < 0 {
		return refuse(op, "route_waypoint_invalid_ordinal"), nil
	}

	if op.BaseRevision == nil {
		return refuse(op, "update_without_base_revision"), nil
	}

	// Verify route exists and caller has access
	var routeOutingID string
	err = t.tx.QueryRow(ctx,
		`select outing_id from planned_routes where id = $1 and context_code = $2`,
		routeID, caller.WriteContext).
		Scan(&routeOutingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "unknown_route"), nil
	}
	if err != nil {
		return Result{}, err
	}

	known, err := t.outingInContext(ctx, routeOutingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		return refuse(op, "unknown_outing"), nil
	}

	var serverRevision int64
	err = t.tx.QueryRow(ctx,
		`select revision from route_waypoints where route_id = $1 and ordinal = $2`,
		routeID, ordinal).
		Scan(&serverRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "update_unknown_route_waypoint"), nil
	}
	if err != nil {
		return Result{}, err
	}

	if serverRevision != *op.BaseRevision {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision,
			ClientState:    op.Payload,
		}, nil
	}

	// Sparse update of waypoint fields
	const q = `
		update route_waypoints set
		  revision = revision + 1,
		  latitude  = coalesce($3, latitude),
		  longitude = coalesce($4, longitude),
		  label     = coalesce($5, label),
		  kind      = coalesce($6, kind),
		  note      = coalesce($7, note)
		where route_id = $1 and ordinal = $2
		returning revision`

	var revision int64
	err = t.tx.QueryRow(ctx, q,
		routeID, ordinal,
		payloadFloatPtr(op.Payload, "latitude"),
		payloadFloatPtr(op.Payload, "longitude"),
		payloadTextPtr(op.Payload, "label"),
		payloadTextPtr(op.Payload, "kind"),
		payloadTextPtr(op.Payload, "note"),
	).Scan(&revision)
	if err != nil {
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

// deletePlannedRoute tombstones a planned route and its waypoints.
//
// A deletion is a soft delete (is_tombstone = true) so the record remains
// in the change feed and the client can reconcile. The base revision
// ensures optimistic concurrency.
func (t *pgxTx) deletePlannedRoute(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "delete_without_entity_id"), nil
	}
	if op.BaseRevision == nil {
		return refuse(op, "delete_without_base_revision"), nil
	}

	var serverRevision int64
	err := t.tx.QueryRow(ctx,
		`select revision from planned_routes where id = $1 and context_code = $2`,
		op.EntityID, caller.WriteContext).
		Scan(&serverRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "delete_unknown_planned_route"), nil
	}
	if err != nil {
		return Result{}, err
	}

	if serverRevision != *op.BaseRevision {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision,
			ClientState:    op.Payload,
		}, nil
	}

	// Soft delete: tombstone the route and cascade to waypoints
	_, err = t.tx.Exec(ctx,
		`update planned_routes set
		  is_tombstone = true,
		  deleted_at = now(),
		  revision = revision + 1,
		  updated_at = now()
		where id = $1 and context_code = $2`,
		op.EntityID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}

	// Tombstone waypoints
	_, err = t.tx.Exec(ctx,
		`update route_waypoints set
		  is_tombstone = true,
		  deleted_at = now(),
		  revision = revision + 1
		where route_id = $1`,
		op.EntityID)
	if err != nil {
		return Result{}, err
	}

	// Get the new revision for the feed
	var revision int64
	err = t.tx.QueryRow(ctx,
		`select revision from planned_routes where id = $1`, op.EntityID).
		Scan(&revision)
	if err != nil {
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

// deleteRouteWaypoint tombstones a single waypoint.
//
// The base revision ensures optimistic concurrency. The route is verified
// to exist and be in the caller's context.
func (t *pgxTx) deleteRouteWaypoint(ctx context.Context, caller Caller, op Operation) (Result, error) {
	parts := splitEntityID(op.EntityID)
	if len(parts) != 2 {
		return refuse(op, "route_waypoint_invalid_entity_id"), nil
	}
	routeID := parts[0]
	ordinal, err := strconv.Atoi(parts[1])
	if err != nil || ordinal < 0 {
		return refuse(op, "route_waypoint_invalid_ordinal"), nil
	}

	if op.BaseRevision == nil {
		return refuse(op, "delete_without_base_revision"), nil
	}

	// Verify route exists and caller has access
	var routeOutingID string
	err = t.tx.QueryRow(ctx,
		`select outing_id from planned_routes where id = $1 and context_code = $2`,
		routeID, caller.WriteContext).
		Scan(&routeOutingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "unknown_route"), nil
	}
	if err != nil {
		return Result{}, err
	}

	known, err := t.outingInContext(ctx, routeOutingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		return refuse(op, "unknown_outing"), nil
	}

	var serverRevision int64
	err = t.tx.QueryRow(ctx,
		`select revision from route_waypoints where route_id = $1 and ordinal = $2`,
		parts[0], parts[1]).
		Scan(&serverRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "delete_unknown_route_waypoint"), nil
	}
	if err != nil {
		return Result{}, err
	}

	if serverRevision != *op.BaseRevision {
		return Result{
			OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
			Outcome: OutcomeRefused, ErrorCode: "revision_conflict",
			ServerRevision: serverRevision,
			ClientState:    op.Payload,
		}, nil
	}

	// Soft delete the waypoint
	_, err = t.tx.Exec(ctx,
		`update route_waypoints set
		  is_tombstone = true,
		  deleted_at = now(),
		  revision = revision + 1
		where route_id = $1 and ordinal = $2`,
		parts[0], parts[1])
	if err != nil {
		return Result{}, err
	}

	var revision int64
	err = t.tx.QueryRow(ctx,
		`select revision from route_waypoints where route_id = $1 and ordinal = $2`,
		parts[0], parts[1]).
		Scan(&revision)
	if err != nil {
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

// createPlannedRoute creates a new planned route for an outing.
//
// A planned route records where the guide intends to go. It is owned by an
// outing and carries waypoints that define the route geometry. The contract
// is local-first: the device plans the route, and the server may or may not
// ever receive it.
func (t *pgxTx) createPlannedRoute(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "planned_route_without_entity_id"), nil
	}
	if op.BaseRevision != nil {
		return refuse(op, "create_with_base_revision"), nil
	}

	outingID := payloadUUID(op.Payload, "outing_id")
	if !isUUID(outingID) {
		return refuse(op, "planned_route_without_outing"), nil
	}
	known, err := t.outingInContext(ctx, outingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		return refuse(op, "unknown_outing"), nil
	}

	const q = `
		insert into planned_routes
		  (id, context_code, outing_id, created_at, updated_at, revision)
		values ($1,$2,$3,now(),now(),1)
		on conflict (id) do nothing
		returning revision`
	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID, caller.WriteContext, outingID,
	).Scan(&revision)
	if err != nil {
		return Result{}, err
	}
	if revision == 0 {
		// Row already existed (conflict on id)
		return refuse(op, "already_exists"), nil
	}

	if err := t.feed(ctx, caller, op, revision); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: revision,
	}, nil
}

func (t *pgxTx) createRouteWaypoint(ctx context.Context, caller Caller, op Operation) (Result, error) {
	if !isUUID(op.EntityID) {
		return refuse(op, "route_waypoint_without_entity_id"), nil
	}
	if op.BaseRevision != nil {
		return refuse(op, "create_with_base_revision"), nil
	}

	// entityID format: "route_id:ordinal"
	parts := splitEntityID(op.EntityID)
	if len(parts) != 2 {
		return refuse(op, "route_waypoint_invalid_entity_id"), nil
	}
	routeID := parts[0]
	ordinal, err := strconv.Atoi(parts[1])
	if err != nil || ordinal < 0 {
		return refuse(op, "route_waypoint_invalid_ordinal"), nil
	}

	// Verify route exists and caller has access
	var routeOutingID string
	err = t.tx.QueryRow(ctx,
		`select outing_id from planned_routes where id = $1`, routeID,
	).Scan(&routeOutingID)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse(op, "unknown_route"), nil
	}
	if err != nil {
		return Result{}, err
	}

	known, err := t.outingInContext(ctx, routeOutingID, caller.WriteContext)
	if err != nil {
		return Result{}, err
	}
	if !known {
		return refuse(op, "unknown_outing"), nil
	}

	longitude := payloadFloatPtr(op.Payload, "longitude")
	latitude := payloadFloatPtr(op.Payload, "latitude")
	if longitude == nil || latitude == nil {
		return refuse(op, "route_waypoint_without_location"), nil
	}

	ordinalVal, ok := payloadInt(op.Payload, "ordinal").(int)
	if !ok || ordinalVal < 0 {
		return refuse(op, "route_waypoint_without_ordinal"), nil
	}

	const q = `
		insert into route_waypoints
		  (route_id, ordinal, latitude, longitude, label, kind, note, revision)
		values ($1,$2,$3,$4,$5,$6,$7,1)
		on conflict (route_id, ordinal) do nothing
		returning revision`
	var revision int64
	err = t.tx.QueryRow(ctx, q,
		op.EntityID, ordinal, // route_id, ordinal
		payloadFloat(op.Payload, "longitude"), payloadFloat(op.Payload, "latitude"),
		payloadTextPtr(op.Payload, "label"),
		payloadTextPtr(op.Payload, "kind"),
		payloadTextPtr(op.Payload, "note"),
	).Scan(&revision)
	if err != nil {
		return Result{}, err
	}
	if revision == 0 {
		return refuse(op, "already_exists"), nil
	}

	if err := t.feed(ctx, caller, op, revision); err != nil {
		return Result{}, err
	}
	return Result{
		OperationID: op.OperationID, Entity: op.Entity, EntityID: op.EntityID,
		Outcome: OutcomeApplied, NewRevision: revision,
	}, nil
}

// splitEntityID splits "route_id:ordinal" into [route_id, ordinal]
func splitEntityID(id string) []string {
	for i := len(id) - 1; i >= 0; i-- {
		if id[i] == ':' {
			return []string{id[:i], id[i+1:]}
		}
	}
	return []string{id, "0"}
}
