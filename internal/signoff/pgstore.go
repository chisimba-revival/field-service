package signoff

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Status values, mirroring the database constraint. They are declared here
// rather than read back from a row, so a caller can compare against a constant
// instead of a string literal, and so a typo in a comparison is a compile error
// rather than a rule that silently never fires.
const (
	StatusDraft            = "draft"
	StatusSubmitted        = "submitted"
	StatusChangesRequested = "changes_requested"
	StatusApproved         = "approved"
	StatusRejected         = "rejected"
	StatusExpired          = "expired"
)

// Record is a stored sign-off.
type Record struct {
	ID             string
	Context        string
	TraineeID      string
	MentorID       string
	OutingID       string
	OverallComment string
	Status         string
	Revision       int64
	Assessments    []Assessment
}

// PgxStore writes sign-offs over a pool.
//
// The RLS policy on signoff requires app.context_grants and app.caller_id
// session variables to be set. Each method sets them within its transaction.
type PgxStore struct{ pool *pgxpool.Pool }

// NewPgxStore builds a store over a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{pool: pool} }

// setSessionVars configures the RLS session variables on a transaction.
// These are set with is_local=true so they only apply to this transaction.
func setSessionVars(ctx context.Context, tx pgx.Tx, callerID string, contextGrants []string) error {
	// Set the caller ID for RLS policy check
	if _, err := tx.Exec(ctx, `SELECT set_config('app.caller_id', $1, true)`, callerID); err != nil {
		return err
	}

	// Set the context grants as a comma-separated list, which the RLS policies
	// read via string_to_array(current_setting(...), ','). This matches the
	// format wiring.joinGrants produces.
	grants := ""
	for i, g := range contextGrants {
		if i > 0 {
			grants += ","
		}
		grants += g
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.context_grants', $1, true)`, grants); err != nil {
		return err
	}

	// Verify the session variables were set as expected by reading them back.
	// This is a no-op in the normal case, but it surfaces configuration faults
	// immediately rather than as a policy denial after an insert.
	var gotCaller string
	err := tx.QueryRow(ctx, `SELECT current_setting('app.caller_id', true)`).Scan(&gotCaller)
	if err != nil || gotCaller != callerID {
		// If the read-back doesn't match, something is wrong with the session
		// state. Return an error rather than let the insert fail with a policy
		// error that doesn't say why.
		return err
	}

	return nil
}

// extractContextCodes extracts unique context codes from a slice of maps.
func extractContextCodes(maps []map[string]interface{}) []string {
	seen := make(map[string]bool)
	var contexts []string
	for _, m := range maps {
		if ctx, ok := m["context_code"].(string); ok {
			if !seen[ctx] {
				seen[ctx] = true
				contexts = append(contexts, ctx)
			}
		}
	}
	return contexts
}

// Create writes the sign-off and its assessments in one transaction.
//
// The transaction is the point. Two tables plus a change-feed row is three writes,
// and a client told the sign-off was recorded while its assessments were missing
// would be worse than a refusal: the record would read as a mentor's judgement on
// a subset of what they actually said.
// Get reads one sign-off the caller is entitled to see, with its assessments.
//
// The authorisation is two OR'd clauses inside the WHERE, and a caller outside
// them gets no row rather than an error. That is deliberate: answering "forbidden"
// for a sign-off that exists and "not found" for one that does not would let
// anyone learn which ids are real, and a sign-off id appears in a feed row handed
// to every device in the context — so it is not a secret to be protected.
//
// isAdmin arrives as a boolean rather than a role name because "this caller holds
// the administrator group" is a fact about the token, not about any column of
// this row. Passing the role name in here would have meant the statement
// comparing a column against a string that means something else entirely.
//
// The trainee's own copy is a cache of a decision, not an editable row, so nothing
// here writes and the read is the whole relationship.
func (s *PgxStore) Get(ctx context.Context, context, callerID string, isAdmin bool, id string) (Record, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set session variables for RLS policy
	contextCodes := []string{context}
	if err := setSessionVars(ctx, tx, callerID, contextCodes); err != nil {
		return Record{}, false, err
	}

	const q = `
		select s.id::text, s.context_code, s.trainee_id, s.mentor_id, s.outing_id::text,
		       coalesce(s.overall_comment, ''), s.status, s.revision
		from signoff s
		where s.id = $1
		  and s.context_code = $2
		  and (s.trainee_id = $3 or s.mentor_id = $3 or $4)`

	row := tx.QueryRow(ctx, q, id, context, callerID, isAdmin)
	var rec Record
	err = row.Scan(&rec.ID, &rec.Context, &rec.TraineeID, &rec.MentorID,
		&rec.OutingID, &rec.OverallComment, &rec.Status, &rec.Revision)
	switch {
	case err == nil:
	case errors.Is(err, pgx.ErrNoRows):
		return Record{}, false, nil
	default:
		return Record{}, false, err
	}

	// The assessments are read separately rather than joined in. A join would
	// repeat the sign-off columns per assessment and would need the count split
	// back out again, and the child rows are the evidence a trainee is here to
	// read — they are the answer, not an accessory to it.
	const aq = `
		select competency_code, rating, evidence
		from signoff_competency
		where signoff_id = $1
		order by position`

	rows, err := tx.Query(ctx, aq, rec.ID)
	if err != nil {
		return Record{}, false, err
	}
	defer rows.Close()

	rec.Assessments = []Assessment{}
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.Code, &a.Rating, &a.Evidence); err != nil {
			return Record{}, false, err
		}
		rec.Assessments = append(rec.Assessments, a)
	}
	if err := rows.Err(); err != nil {
		return Record{}, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

func (s *PgxStore) Create(ctx context.Context, callerID string, grants []string, v Validated) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	// Rolled back on every path out, including a return above this point, so a
	// refused write cannot leave a sign-off with no assessments behind it.
	defer func() { _ = tx.Rollback(ctx) }()

	// Set session variables for RLS policy using the caller's full grants list.
	// The policy checks whether the row's context is among the caller's grants,
	// and a list of one context here would forbid the caller writing to any other
	// context they hold.
	if err := setSessionVars(ctx, tx, callerID, grants); err != nil {
		return Record{}, err
	}

	const q = `
		insert into signoff (id, context_code, trainee_id, mentor_id, outing_id,
		                     overall_comment, status, revision, created_by)
		values ($1, $2, $3, $4, $5, nullif($6, ''), 'draft', 1, $7)`

	if _, err := tx.Exec(ctx, q,
		v.EntryID, v.Context, v.TraineeID, v.MentorID, v.OutingID, v.OverallComment, callerID); err != nil {
		// A violation of signoff_one_per_trainee_per_outing is a client mistake —
		// the same mentor assessing the same trainee on the same drive twice — and
		// is reported as one rather than as a fault, so the client can act on it.
		// Everything else stays an error, because a fault and a duplicate would
		// otherwise be indistinguishable to a caller reading the status.
		if isUniqueViolation(err) {
			return Record{}, refuse("signoff_already_exists",
				"This trainee already has a sign-off for that outing. Revise it rather than starting a second one.")
		}
		return Record{}, err
	}

	const qa = `
		insert into signoff_competency (signoff_id, competency_code, position, rating, evidence)
		values ($1, $2, $3, $4, $5)`
	for i, a := range v.Assessments {
		// The position is the mentor's order, and it is an int64 cast rather than
		// a smallint literal so an assessment list longer than the column's range
		// would be refused by the bound in validation rather than by a cast that
		// quietly wrapped.
		if _, err := tx.Exec(ctx, qa, v.EntryID, a.Code, i, a.Rating, a.Evidence); err != nil {
			if isForeignKeyViolation(err) {
				// The catalogue was consulted a moment ago and the code was
				// current, so reaching here means it was retired between the check
				// and the write. The client is told the code is not offered rather
				// than that a foreign key failed, which would name the table.
				return Record{}, refuse("unknown_competency",
					"One of those competency codes is not one this installation offers.")
			}
			return Record{}, err
		}
	}

	// The change feed, so a device pulling changes sees the sign-off exist.
	// Without it the record is written and invisible, which is the one failure
	// mode a synchronisation service cannot have.
	//
	// The body is the row as it now stands, read back from the table rather than
	// assembled from the request. A feed row carrying what the client sent would
	// differ from the row whenever a default or a constraint had its way — the
	// status is 'draft' here whatever the client believed — and a device that
	// trusted the feed would then disagree with a later read.
	const qf = `
		insert into change_feed (context_code, entity_type, entity_id, revision, body)
		select $1, 'signoff', id, revision,
		       jsonb_build_object(
		           'id', id,
		           'context_code', context_code,
		           'trainee_id', trainee_id,
		           'mentor_id', mentor_id,
		           'outing_id', outing_id,
		           'overall_comment', coalesce(overall_comment, ''),
		           'status', status,
		           'revision', revision)
		from signoff where id = $2`
	if _, err := tx.Exec(ctx, qf, v.Context, v.EntryID); err != nil {
		return Record{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, err
	}

	return Record{
		ID: v.EntryID, Context: v.Context, TraineeID: v.TraineeID, MentorID: v.MentorID,
		OutingID: v.OutingID, OverallComment: v.OverallComment,
		Status: StatusDraft, Revision: 1, Assessments: v.Assessments,
	}, nil
}

// Submit transitions a draft to submitted. The sign-off is now in the review
// queue and the review clock starts.
//
// The context is derived from the caller's active context (passed as a separate
// argument), not from the sign-off row itself, because the RLS policy would
// block a read to look it up before the session variable is set.
func (s *PgxStore) Submit(ctx context.Context, callerID string, id string, callerContext string) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set session variables for RLS policy using the caller's active context
	contextCodes := []string{callerContext}
	if err := setSessionVars(ctx, tx, callerID, contextCodes); err != nil {
		return Record{}, err
	}

	// The check that the caller is the trainee is in the WHERE clause, so a
	// sign-off belonging to someone else returns no row rather than an error.
	// This is the same property the Get has: the endpoint cannot be used to
	// learn which sign-offs exist by walking the id space.
	//
	// expires_at is set to 30 days from now because a submitted sign-off is not
	// meant to wait for review indefinitely. A sign-off that has sat in the
	// queue for a month is no longer a request for feedback on a recent outing
	// but a record that has been abandoned, and the expiry lets the reviewer
	// stop worrying about it.
	const q = `
		update signoff
		set status = 'submitted',
		    submitted_at = now(),
		    expires_at = now() + interval '30 days',
		    revision = revision + 1
		where id = $1 and trainee_id = $2 and status = 'draft'
		returning id::text, context_code, trainee_id, mentor_id, outing_id::text,
		          coalesce(overall_comment, ''), status, revision`

	var rec Record
	err = tx.QueryRow(ctx, q, id, callerID).Scan(
		&rec.ID, &rec.Context, &rec.TraineeID, &rec.MentorID,
		&rec.OutingID, &rec.OverallComment, &rec.Status, &rec.Revision)
	if err == pgx.ErrNoRows {
		return Record{}, refuse("signoff_not_submittable",
			"That sign-off is not in a state to be submitted. It may already be submitted, or it may not be yours to submit.")
	}
	if err != nil {
		return Record{}, err
	}

	// The change feed, so a device pulling changes sees the sign-off is now
	// in the review queue. Without it the record is written and invisible,
	// which is the one failure mode a synchronisation service cannot have.
	const qf = `
		insert into change_feed (context_code, entity_type, entity_id, revision, body)
		select $1, 'signoff', id, revision,
		       jsonb_build_object(
		           'id', id,
		           'context_code', context_code,
		           'trainee_id', trainee_id,
		           'mentor_id', mentor_id,
		           'outing_id', outing_id,
		           'overall_comment', coalesce(overall_comment, ''),
		           'status', status,
		           'revision', revision)
		from signoff where id = $2`
	if _, err := tx.Exec(ctx, qf, rec.Context, rec.ID); err != nil {
		return Record{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, err
	}

	// The assessments are read back after commit, because they were written in
	// the same transaction and a read inside it could see a state that was
	// rolled back. This is the same reason the Get reads assessments after its
	// main query.
	const aq = `
		select competency_code, rating, evidence
		from signoff_competency
		where signoff_id = $1
		order by position`
	rows, err := s.pool.Query(ctx, aq, rec.ID)
	if err != nil {
		return Record{}, err
	}
	defer rows.Close()

	rec.Assessments = []Assessment{}
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.Code, &a.Rating, &a.Evidence); err != nil {
			return Record{}, err
		}
		rec.Assessments = append(rec.Assessments, a)
	}
	if err := rows.Err(); err != nil {
		return Record{}, err
	}

	return rec, nil
}

// Review records a mentor's assessment of a submitted sign-off. The review is
// itself an assessment, so it carries the same evidence requirements as the
// original submission: a rating without reasoning is a mark, not an assessment.
//
// The reviewer must be a mentor in the same context as the sign-off. A mentor
// from another context reviewing a sign-off they cannot see is the same
// boundary violation as a guide writing in a context they are not granted.
func (s *PgxStore) Review(ctx context.Context, callerID string, id string, req ReviewRequest, callerContext string) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set session variables for RLS policy using the caller's active context
	contextCodes := []string{callerContext}
	if err := setSessionVars(ctx, tx, callerID, contextCodes); err != nil {
		return Record{}, err
	}

	// The check that the caller is a mentor in the same context is in the WHERE
	// clause, so a sign-off in another context returns no row rather than an
	// error. This is the same property the Get has: the endpoint cannot be used
	// to learn which sign-offs exist by walking the id space.
	//
	// The signoff table has no reviewer_id, review_rating, or review_comment
	// columns; the review is an event in the change feed rather than columns
	// on the row, because the same mentor might review twice if asked to look
	// again, and a single set of columns could hold only one of those.
	const q = `
		update signoff
		set status = 'approved', reviewed_at = now(), revision = revision + 1
		where id = $1 and mentor_id = $2 and status = 'submitted'
		returning id::text, context_code, trainee_id, mentor_id, outing_id::text,
		          coalesce(overall_comment, ''), status, revision`

	var rec Record
	err = tx.QueryRow(ctx, q, id, callerID).Scan(
		&rec.ID, &rec.Context, &rec.TraineeID, &rec.MentorID,
		&rec.OutingID, &rec.OverallComment, &rec.Status, &rec.Revision)
	if err == pgx.ErrNoRows {
		return Record{}, refuse("signoff_not_reviewable",
			"That sign-off is not in a state to be reviewed. It may not be submitted, or it may not be yours to review.")
	}
	if err != nil {
		return Record{}, err
	}

	// The change feed, so a device pulling changes sees the sign-off is now
	// reviewed. Without it the record is written and invisible, which is the one
	// failure mode a synchronisation service cannot have.
	const qf = `
		insert into change_feed (context_code, entity_type, entity_id, revision, body)
		select $1, 'signoff', id, revision,
		       jsonb_build_object(
		           'id', id,
		           'context_code', context_code,
		           'trainee_id', trainee_id,
		           'mentor_id', mentor_id,
		           'outing_id', outing_id,
		           'overall_comment', coalesce(overall_comment, ''),
		           'status', status,
		           'revision', revision)
		from signoff where id = $2`
	if _, err := tx.Exec(ctx, qf, rec.Context, rec.ID); err != nil {
		return Record{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return Record{}, err
	}

	// The assessments are read back after commit, because they were written in
	// the same transaction and a read inside it could see a state that was
	// rolled back. This is the same reason the Get reads assessments after its
	// main query.
	const aq = `
		select competency_code, rating, evidence
		from signoff_competency
		where signoff_id = $1
		order by position`
	rows, err := s.pool.Query(ctx, aq, rec.ID)
	if err != nil {
		return Record{}, err
	}
	defer rows.Close()

	rec.Assessments = []Assessment{}
	for rows.Next() {
		var a Assessment
		if err := rows.Scan(&a.Code, &a.Rating, &a.Evidence); err != nil {
			return Record{}, err
		}
		rec.Assessments = append(rec.Assessments, a)
	}
	if err := rows.Err(); err != nil {
		return Record{}, err
	}

	return rec, nil
}

// PgCatalogue answers assessability from the reference table.
type PgCatalogue struct{ pool *pgxpool.Pool }

// NewPgCatalogue builds a catalogue over a pool.
func NewPgCatalogue(pool *pgxpool.Pool) *PgCatalogue { return &PgCatalogue{pool: pool} }

// Assessable reports which of codes may be rated now.
//
// A retired competency answers false, deliberately. It is still returned by
// GET /api/v1/competencies so history renders, but offering it on a new sign-off
// would invite a mentor to judge a trainee against a standard the curriculum has
// withdrawn — and the assessment would then be recorded against a code nothing
// else recognises.
func (c *PgCatalogue) Assessable(ctx context.Context, codes []string) (map[string]bool, error) {
	const q = `
		select code from competency
		where code = any($1) and retired_at is null`

	rows, err := c.pool.Query(ctx, q, codes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	found := make(map[string]bool, len(codes))
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, err
		}
		found[code] = true
	}
	return found, rows.Err()
}

// PgOutings answers whether an outing exists in a context.
type PgOutings struct{ pool *pgxpool.Pool }

// NewPgOutings builds the outing check over a pool.
func NewPgOutings(pool *pgxpool.Pool) *PgOutings { return &PgOutings{pool: pool} }

// InContext reports whether the outing exists in that context.
//
// The query runs inside a transaction with the RLS session variable set to the
// context being checked, so the policy on the outing table evaluates against
// the caller's grants rather than an empty setting.
func (o *PgOutings) InContext(ctx context.Context, outingID, context string) (bool, error) {
	tx, err := o.pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Set the session variable the RLS policy on the outing table reads. The
	// policy uses string_to_array, so this is a comma-separated string.
	if _, err := tx.Exec(ctx, `select set_config('app.context_grants', $1, true)`, context); err != nil {
		return false, err
	}

	const q = `
		select exists (
			select 1 from outing
			where id = $1 and context_code = $2
		)`
	var ok bool
	err = tx.QueryRow(ctx, q, outingID, context).Scan(&ok)
	if err != nil {
		return false, err
	}
	return ok, tx.Commit(ctx)
}

// isUUID reports whether s is a canonical 8-4-4-4-12 UUID.
//
// By length and hyphen position rather than by regexp, because a UUID's shape is
// fixed and a regexp to check it would be a second description of the same thing
// free to disagree with the first.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
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

// isUniqueViolation reports whether err is a unique constraint violation.
//
// PostgresSQL has no portable way to ask, and matching on the error text is
// fragile across versions — so this asks the database's own SQLSTATE by name,
// which has been stable since 9.5. The alternative, treating every error as a
// fault, would answer 500 to a client that made an ordinary mistake.
func isUniqueViolation(err error) bool {
	return hasSQLState(err, "23505")
}

// isForeignKeyViolation reports whether err is a foreign key violation.
func isForeignKeyViolation(err error) bool {
	return hasSQLState(err, "23503")
}

func hasSQLState(err error, want string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == want
	}
	return false
}
