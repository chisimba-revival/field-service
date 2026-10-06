package signoff

import (
	"context"
	"errors"

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
// No Querier and no transaction supplied by the caller: a sign-off is written
// through the ordinary person door, which does set grants, so the statements here
// name their context explicitly and rely on the isolation policy for the rest.
type PgxStore struct{ pool *pgxpool.Pool }

// NewPgxStore builds a store over a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{pool: pool} }

// Create writes the sign-off and its assessments in one transaction.
//
// The transaction is the point. Two tables plus a change-feed row is three writes,
// and a client told the sign-off was recorded while its assessments were missing
// would be worse than a refusal: the record would read as a mentor's judgement on
// a subset of what they actually said.
func (s *PgxStore) Create(ctx context.Context, callerID string, v Validated) (Record, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Record{}, err
	}
	// Rolled back on every path out, including a return above this point, so a
	// refused write cannot leave a sign-off with no assessments behind it.
	defer func() { _ = tx.Rollback(ctx) }()

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
// One statement rather than a select followed by a compare, and it names both in
// the WHERE clause, so an outing in another context is indistinguishable from one
// that does not exist — the same property the drive check has, and for the same
// reason: a message that distinguished them would be an oracle for walking the
// id space to discover other reserves' outings.
// The cancelled filter is absent deliberately: an outing has no deleted_at, and
// cancelling is a status and a reason rather than a deletion. The filter was
// written from the habit of every other table here carrying one, and it made
// every sign-off naming an outing fail with "column deleted_at does not exist",
// which is the whole request returning 500 rather than one row being found.
//
// The unit tests could not see it because they fake InContext, so this query was
// only ever executed by the end-to-end suite.
func (o *PgOutings) InContext(ctx context.Context, outingID, context string) (bool, error) {
	const q = `
		select exists (
			select 1 from outing
			where id = $1 and context_code = $2
		)`
	var ok bool
	err := o.pool.QueryRow(ctx, q, outingID, context).Scan(&ok)
	return ok, err
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
