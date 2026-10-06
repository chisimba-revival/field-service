package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxStore applies mentor decisions to the database.
//
// It takes a pool rather than a Querier, deliberately and against the pattern
// everywhere else in this service. Every other read is scoped by grants set
// inside a transaction, because a person token names a context and the rules are
// written around that. This caller is a service token: it carries no context, no
// grants, and no identity to scope by. Opening a transaction to set grants it
// does not have would be ceremony implying an isolation it has not been given.
//
// So the isolation here is explicit and stated in the statement itself: the entry
// is found by id AND context, and a row that is not in the named context is
// indistinguishable from a row that does not exist. That is what stops this
// endpoint from being an oracle for walking the id space to discover which
// entries another reserve holds.
type PgxStore struct {
	pool *pgxpool.Pool
	// now is injectable so the recorded verification time can be asserted rather
	// than compared to a clock.
	now func() time.Time
}

// NewPgxStore builds a store over a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore {
	return &PgxStore{pool: pool, now: time.Now}
}

// Verify applies the decision, or explains why it did not.
func (s *PgxStore) Verify(ctx context.Context, req Request) (Result, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, err
	}
	// Rollback after a successful Commit is a no-op, so this covers both the
	// error paths and the refusals that return without writing.
	defer func() { _ = tx.Rollback(ctx) }()

	// The replay check comes first. A retry after a dropped response must be told
	// the same thing the first attempt said, and re-applying it would give the
	// entry a second revision for a decision the mentor made once.
	if res, replay, err := s.recorded(ctx, tx, req); err != nil {
		return Result{}, err
	} else if replay {
		return res, nil
	}

	var currentRevision int64
	var currentSpecies *string
	var currentCount *int
	var currentStatus string

	// id AND context_code. The context is not a filter applied afterwards; it is
	// part of the lookup, so an entry in another context returns no row at all.
	err = tx.QueryRow(ctx,
		`select revision, species_code, count, status::text
		   from log_book_entry
		  where id = $1 and context_code = $2 and deleted_at is null`,
		req.EntryID, req.Context).
		Scan(&currentRevision, &currentSpecies, &currentCount, &currentStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return refusal(req, refuse("unknown_log_book_entry")), nil
	}
	if err != nil {
		return Result{}, err
	}

	// A stale revision is refused outright rather than merged or applied on top.
	// This is an authority decision about a record the mentor believed they were
	// reading; if the record has moved since, they did not read this one.
	if req.BaseRevision == nil || *req.BaseRevision != currentRevision {
		return refusal(req, refuse("stale_revision")), nil
	}

	// An entry that is already deleted, or already decided, is not re-decided.
	// Re-verifying would overwrite the first mentor's answer with a second and
	// lose the explanation of why the first said what they said.
	if currentStatus != "pending" && currentStatus != "needs_review" {
		return refusal(req, refuse("already_decided")), nil
	}

	newRevision := currentRevision + 1
	verifiedAt := s.now().UTC()

	// A correction keeps what the trainee recorded, permanently, beside what the
	// mentor put instead. Writing the corrected value alone would erase the
	// reasoning along with the mistake, and there would be nothing left to learn
	// from — which is the whole teaching content of the row.
	// The recorded_* columns are written only when there is a correction to record,
	// and they are never cleared on a later plain decision.
	//
	// This is not tidiness. The database constrains the pair: recorded_* is set
	// only if correction_reason is, and writing recorded_* unconditionally would
	// have turned every verification without a correction into a constraint
	// violation — a 500 on the ordinary case of a mentor simply confirming an
	// entry. The constraint was right and the first version of this was wrong; the
	// failure only appeared once a real decision went through.
	var recordedSpecies *string
	var recordedCount *int
	if corrects(req) {
		recordedSpecies, recordedCount = currentSpecies, currentCount
	}

	species := currentSpecies
	if req.SpeciesCode != nil {
		species = req.SpeciesCode
	}
	count := currentCount
	if req.Count != nil {
		count = req.Count
	}

	_, err = tx.Exec(ctx,
		`update log_book_entry
		    set status = $3::sighting_status,
		        verified_by = $4,
		        verified_at = $5,
		        verification_notes = nullif($6, ''),
		        correction_reason = coalesce(nullif($7, ''), correction_reason),
		        species_code = $8,
		        count = $9,
		        recorded_species_code = coalesce($10, recorded_species_code),
		        recorded_count = coalesce($11, recorded_count),
		        revision = $12
		  where id = $1 and context_code = $2`,
		req.EntryID, req.Context, string(req.Outcome), req.Mentor, verifiedAt,
		req.Notes, req.CorrectionReason, species, count,
		recordedSpecies, recordedCount, newRevision)
	if err != nil {
		return Result{}, err
	}

	res := Result{
		OperationID: req.OperationID,
		EntryID:     req.EntryID,
		Outcome:     string(req.Outcome),
		VerifiedBy:  req.Mentor,
		VerifiedAt:  verifiedAt.Format(time.RFC3339),
		NewRevision: &newRevision,
	}
	if err := s.record(ctx, tx, req, res); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, err
	}
	return res, nil
}

// recorded returns the outcome of an earlier attempt at this operation.
func (s *PgxStore) recorded(ctx context.Context, tx pgx.Tx, req Request) (Result, bool, error) {
	var raw []byte
	err := tx.QueryRow(ctx,
		`select result from operation_outcome
		  where caller_user_id = $1 and operation_id = $2`,
		req.Mentor, req.OperationID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	var res Result
	if err := json.Unmarshal(raw, &res); err != nil {
		// An unreadable result is not a missing result. Reporting "not found"
		// would re-apply an operation whose outcome is already recorded, which is
		// the one thing idempotency exists to prevent.
		return Result{}, false, fmt.Errorf("verify: recorded outcome unreadable: %w", err)
	}
	return res, true, nil
}

// record stores the outcome so a retry is answered identically.
//
// The `outcome` column holds the operation's DISPOSITION — applied or refused —
// and not the mentor's answer. That is not a compromise; the column is
// constrained to a vocabulary of dispositions, and verified/rejected/
// needs_review are not dispositions. The answer itself is recorded where a pull
// can read it: on the entry's status column, and in the result document this
// stores, which is what a replay returns.
//
// Writing the answer into that column was the first version, and it failed on a
// check constraint the moment a real decision went through.
func (s *PgxStore) record(ctx context.Context, tx pgx.Tx, req Request, res Result) error {
	encoded, err := json.Marshal(res)
	if err != nil {
		return err
	}
	disposition := "applied"
	if res.Outcome == "refused" {
		disposition = "refused"
	}
	_, err = tx.Exec(ctx,
		`insert into operation_outcome
		   (caller_user_id, operation_id, entity_type, entity_id, kind, outcome, result)
		 values ($1, $2, 'log_book_entry', $3, 'verify', $4, $5)`,
		req.Mentor, req.OperationID, req.EntryID, disposition, encoded)
	return err
}
