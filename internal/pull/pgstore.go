package pull

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PgxStore reads the change feed.
//
// The store names no context in its queries, and that is deliberate. Row-level
// security on change_feed already filters by app.context_grants, set by the
// session inside the transaction. A context filter written again here would be a
// second answer to a question the database is already answering, and the two
// could disagree — in which case the wider one is the one that leaks.
type PgxStore struct{ q Querier }

// Querier is everything this store needs in order to read the feed.
//
// An interface rather than *pgx.Conn, and not only for testability. The grants
// are transaction-local, so a pull that read through a connection taken from the
// pool would find no grants at all — the store has to be built over whatever
// handle is inside the transaction that set them.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// NewPgxStore builds a store over a handle whose grants are already set.
//
// *pgx.Conn satisfies Querier, so a test can hand one directly.
func NewPgxStore(q Querier) *PgxStore { return &PgxStore{q: q} }

// LowestRetainedSeq reports the oldest row still in the feed.
//
// It queries under the caller's own grants, so a caller who can see nothing sees
// no rows and is told the feed is empty. That is correct for the resync
// question: there is nothing they could have missed in a context they cannot
// read.
func (s *PgxStore) LowestRetainedSeq(ctx context.Context) (int64, error) {
	// 1 when the feed is empty, so that a cursor of 0 is never mistaken for one
	// that has fallen behind a feed that has since been filled.
	var lowest int64 = 1
	err := s.q.QueryRow(ctx, `select coalesce(min(seq), 1) from change_feed`).Scan(&lowest)
	if err != nil {
		return 0, fmt.Errorf("pull: lowest retained seq: %w", err)
	}
	return lowest, nil
}

// Page returns up to limit changes after the cursor, oldest first.
//
// Ordered by seq and nothing else. The ordering is the whole reason the feed
// serialises each row at change time instead of joining the entity tables at
// read time: a live join means a page is not a consistent snapshot, so a client
// can receive revision 4 and revision 5 of one entity in one page and apply them
// in the wrong order.
func (s *PgxStore) Page(ctx context.Context, cursor int64, limit int) ([]Change, bool, error) {
	if limit < 1 {
		limit = 1
	}
	// One more than asked for, so has_more is answered by whether a row exists
	// rather than by a second query. A second query would be a second answer to
	// the same question, taken at a different moment.
	rows, err := s.q.Query(ctx, `
		select seq, entity_type, entity_id, revision, changed_at, body
		from change_feed
		where seq > $1
		order by seq
		limit $2`, cursor, limit+1)
	if err != nil {
		return nil, false, fmt.Errorf("pull: reading a page: %w", err)
	}
	defer rows.Close()

	changes := make([]Change, 0, limit)
	for rows.Next() {
		var c Change
		var raw []byte
		if err := rows.Scan(&c.Seq, &c.Entity, &c.EntityID, &c.Revision, &c.ChangedAt, &raw); err != nil {
			return nil, false, fmt.Errorf("pull: scanning a change: %w", err)
		}
		// A body that will not parse is an error rather than an empty change.
		// Handing back an empty body would look like a change to a row with no
		// fields, which the client would apply and blank.
		if err := json.Unmarshal(raw, &c.Body); err != nil {
			return nil, false, fmt.Errorf("pull: change %d has an unreadable body: %w", c.Seq, err)
		}
		changes = append(changes, c)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("pull: reading a page: %w", err)
	}

	more := len(changes) > limit
	if more {
		changes = changes[:limit]
	}
	return changes, more, nil
}

var _ Store = (*PgxStore)(nil)
