package species

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PgxStore reads the catalogue over a pgx pool.
//
// No Querier and no transaction, unlike everything that reads field data. The
// catalogue carries no context_code, so there is no grant to set and no
// transaction to read it inside: opening one and setting grants would be
// ceremony implying an isolation that does not exist here.
type PgxStore struct{ pool *pgxpool.Pool }

// NewPgxStore builds a catalogue store over a pool.
func NewPgxStore(pool *pgxpool.Pool) *PgxStore { return &PgxStore{pool: pool} }

// List returns the catalogue ordered by common name, optionally narrowed.
//
// The search is a case-insensitive substring match over three columns.
//
// Two separate things are worth being careful about here, and the first version
// of this function got the second one wrong while claiming to have got it right.
//
// SQL injection is prevented by binding the term as a parameter. That is not
// enough. Inside a LIKE pattern, % and _ are wildcards, so a bound term of "100%"
// still matches every description containing any hundred-something-percent, and
// "a_b" still matches "axb". Parameterising the pattern does nothing about this
// — the term arrives safely and then behaves as a wildcard. So the wildcards in
// the term are escaped here, and the escape character is named in the statement
// rather than left to the server's default.
//
// The code is searched too, which the first version of this left out while its
// own comment described searching it. A trainee who half remembers "BUFA" and
// gets nothing back has learned that search does not work, rather than that they
// misremembered the code — and the code is the field they are most likely to
// have seen already, because it is printed on the sighting.
//
// $1 is the bare term and $2 the built pattern. The empty check uses the bare
// term because an empty term must mean "everything", and comparing a built
// pattern against '%%' would be true for any term consisting only of wildcards,
// which would turn a search for "%" into the whole catalogue.
func (s *PgxStore) List(ctx context.Context, search string) ([]Species, error) {
	const q = `
		select code, common_name, coalesce(scientific_name, ''), coalesce(description, '')
		from species
		where $1 = ''
		   or code                       ilike $2 escape '\'
		   or common_name              ilike $2 escape '\'
		   or coalesce(scientific_name, '') ilike $2 escape '\'
		   or coalesce(description, '')      ilike $2 escape '\'
		order by common_name, code`

	rows, err := s.pool.Query(ctx, q, search, searchLike(search))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Species, 0, 16)
	for rows.Next() {
		var sp Species
		if err := rows.Scan(&sp.Code, &sp.CommonName, &sp.ScientificName, &sp.Description); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// Get returns one entry, or ErrNotFound.
//
// The lookup is on code alone and a missing row is ErrNotFound rather than an
// error from the driver, because "no such species" is an answer and not a fault.
// Returning the driver's own no-rows error would leave the handler comparing
// against a pgx sentinel, and a caller of this package would have to import pgx
// to do it.
func (s *PgxStore) Get(ctx context.Context, code string) (Species, error) {
	const q = `
		select code, common_name, coalesce(scientific_name, ''), coalesce(description, '')
		from species
		where code = $1`

	var sp Species
	err := s.pool.QueryRow(ctx, q, code).Scan(
		&sp.Code, &sp.CommonName, &sp.ScientificName, &sp.Description)
	switch {
	case err == nil:
		return sp, nil
	case err == pgx.ErrNoRows:
		return Species{}, ErrNotFound
	default:
		return Species{}, err
	}
}

// searchLike builds the pattern for a substring match, escaping the wildcards a
// caller might have typed. It is the one place a pattern is built, so a term
// that needs escaping cannot be missed by a second caller building one its own
// way.
func searchLike(term string) string {
	return "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(term) + "%"
}
