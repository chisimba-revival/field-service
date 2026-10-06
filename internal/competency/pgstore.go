package competency

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxStore reads the catalogue over a pool.
//
// No Querier and no transaction: the catalogue has no context_code, so there is
// no grant to set and nothing to isolate.
type PgxStore struct{ pool *pgxpool.Pool }

// NewPgxStore builds a catalogue store over a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{pool: pool} }

// List returns the catalogue.
//
// Ordered by category then level then code, so a client rendering "what may be
// assessed at this stage" gets a sensible order without sorting itself, and the
// order is stable so a cached bundle does not reshuffle between refreshes.
//
// The search escapes the wildcards a caller typed. Binding the term as a
// parameter stops injection and does nothing about LIKE: a bound "%" still
// matches every row. That was learned the hard way on the species catalogue,
// where the first version documented the escaping it did not perform.
//
// The E prefix on the escape literal is not decoration. This statement is a Go
// raw string, so backslashes in it reach PostgreSQL unprocessed, and ESCAPE
// demands a string of exactly one character: a plain '\\' is two characters and
// the database rejects it with "invalid escape string". The species catalogue hit
// this first and carries E'\\'; this one did not until it was run against a real
// database, where every search — and only the search — returned a 500.
func (s *PgxStore) List(ctx context.Context, q Query) ([]Competency, error) {
	const stmt = `
		select code, name, category, level, coalesce(description, ''), retired_at is not null
		from competency
		where ($1 = '' or category = $1)
		  and ($2 = 0 or level = $2)
		  and (not $3 or retired_at is null)
		  and ($4 = ''
		       or code    ilike $5 escape E'\\'
		       or name    ilike $5 escape E'\\'
		       or coalesce(description, '') ilike $5 escape E'\\')
		order by category, level, code`

	rows, err := s.pool.Query(ctx, stmt, q.Category, q.Level, q.CurrentOnly, q.Search, searchLike(q.Search))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Never nil. A client building an offline bundle has to tell "the catalogue
	// is empty" from "that was not a list", and a nil slice marshals to null.
	out := make([]Competency, 0, 32)
	for rows.Next() {
		var c Competency
		if err := rows.Scan(&c.Code, &c.Name, &c.Category, &c.Level, &c.Description, &c.Retired); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Get returns one entry, or ErrNotFound.
//
// Retired rows are found. Hiding them here would undo the catalogue's whole
// reason for retiring rather than deleting, at the one place a client looks.
func (s *PgxStore) Get(ctx context.Context, code string) (Competency, error) {
	const stmt = `
		select code, name, category, level, coalesce(description, ''), retired_at is not null
		from competency
		where code = $1`

	var c Competency
	err := s.pool.QueryRow(ctx, stmt, code).Scan(
		&c.Code, &c.Name, &c.Category, &c.Level, &c.Description, &c.Retired)
	switch {
	case err == nil:
		return c, nil
	case err == pgx.ErrNoRows:
		return Competency{}, ErrNotFound
	default:
		return Competency{}, err
	}
}

// searchLike builds the pattern for a substring match, escaping the wildcards a
// caller might have typed. It is the one place a pattern is built, so a second
// caller building one its own way cannot miss the escaping.
func searchLike(term string) string {
	return "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(term) + "%"
}
