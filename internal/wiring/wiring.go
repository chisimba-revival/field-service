// Package wiring joins the token validator, the revocation denylist and the
// database together behind the interfaces the HTTP guard is written against.
//
// It exists as its own package so that each of the three was testable on its own
// terms, and so the one place they meet is small enough to read in full. A
// request that gets further than the guard allows has passed a signature check, a
// revocation check, a scope check and a grant check, and is about to run a
// transaction whose read boundary is the token's own grant set.
package wiring

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"time"

	"field-service/internal/authn"
	"field-service/internal/httpapi"
)

// verifier adapts the token validator to the guard's interface.
//
// The adapter exists because Go has no covariant returns: the guard wants a
// Principal it can hold, and the validator returns the concrete authn type. The
// alternative — widening authn.Principal to implement httpapi.Principal by adding
// accessor methods that shadow its embedded claims — was rejected because it
// would put an HTTP-layer concern inside the token package and couple the two in
// the direction that is hardest to undo.
//
// The token's own scopes, contexts and epoch are passed through unchanged. This
// adapter decides nothing about authorisation; it only makes a type fit.
type verifier struct{ v *authn.Validator }

// NewVerifier wraps a validator for the guard.
func NewVerifier(v *authn.Validator) httpapi.Verifier { return verifier{v: v} }

// Validate satisfies httpapi.Verifier.
//
// The result is returned untyped because the interface says `any`. That is a
// loose contract, and the guard's response to a value it does not recognise is to
// refuse the request rather than to treat it as anonymous: a verifier wired to
// the wrong type is a fault in this service, and admitting traffic because of a
// wiring mistake is how that fault becomes a breach.
func (a verifier) Validate(token string) (any, error) {
	p, err := a.v.Validate(token)
	if err != nil {
		return nil, err
	}
	return claims{p}, nil
}

// claims satisfies httpapi.Principal over an authn.Principal.
type claims struct{ p authn.Principal }

var _ httpapi.Principal = claims{}

func (c claims) Subject() string { return c.p.Subject }

// TokenType reports what the token was issued for: "access" for a person,
// "service" for the platform acting for nobody in particular.
//
// The absent case is returned as "" rather than defaulted to "access", because a
// caller deciding what a token is must be able to tell "a person's token" from "a
// token that did not say", and those are not the same claim.
func (c claims) TokenType() string     { return c.p.Type }
func (c claims) Scopes() []string      { return c.p.Scope }
func (c claims) ActiveContext() string { return c.p.ActiveContext }
func (c claims) Grants() []string      { return c.p.Grants }
func (c claims) Epoch() int64          { return c.p.Epoch }

// HasScope is the validator's own implementation rather than a re-implementation
// here, so there is one answer to "does this token carry this scope".
func (c claims) HasScope(scope string) bool { return c.p.HasScope(scope) }

// HasRole reports whether the token carried a named group.
func (c claims) HasRole(role string) bool { return c.p.HasRole(role) }

// IssuedAt reports the token's issue time and whether it carried one.
//
// The second return is not decoration. The revocation rule needs an issue time to
// compare against the time of the decision, and a token without one cannot be
// ordered against it. Reporting a zero time for a missing claim would let the
// comparison run and produce a confident wrong answer — a zero time is before
// every revocation, so every such token would be refused for the wrong reason
// while looking like a correct refusal. The guard refuses a missing issue time
// before asking.
func (c claims) IssuedAt() (time.Time, bool) {
	if c.p.IssuedAt <= 0 {
		return time.Time{}, false
	}
	return time.Unix(c.p.IssuedAt, 0).UTC(), true
}

// ErrNoGrants is returned when a transaction is asked to run for a caller with
// no context grants.
//
// It is refused rather than run with an empty set. An empty grant list sets
// `app.context_grants` to the empty string, which the policies read as "matches
// no context", so the transaction would see nothing rather than everything — which
// is safe, but silently so, and it turns a caller's mistake into an empty result
// that looks like a correct answer to a query that should have returned rows.
var ErrNoGrants = errors.New("wiring: refusing to run a transaction with no context grants")

// ErrNoCaller is returned when a transaction is opened for nobody.
//
// operation_outcome's policy compares its caller column against this
// identifier, so a transaction with no caller cannot read or write that table
// at all. Refusing up front says so plainly rather than letting the first query
// that touches it fail with an obscure error.
var ErrNoCaller = errors.New("wiring: refusing to run a transaction with no caller")

// PgxSession runs a transaction whose read boundary is the caller's grant set.
//
// This is where the isolation becomes real for a request. The guard has already
// established who the caller is; this hands the database the set of contexts that
// caller may read, and the row-level security policies in the schema do the rest.
// A handler cannot widen the boundary because it never writes the setting itself.
type PgxSession struct {
	pool  PgxPool
	clock func() time.Time
}

// PgxPool is the slice of pgx this needs. Narrow deliberately: the transaction
// has to be startable, has to accept a statement, and has to be closable, and
// nothing else. A wider interface here would let the read boundary be set from
// outside this file.
type PgxPool interface {
	Begin(ctx context.Context) (PgxTx, error)
}

// PgxTx is the transaction handle this package uses.
type PgxTx interface {
	Exec(ctx context.Context, sql string, args ...any) (PgxCommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (PgxRows, error)
	QueryRow(ctx context.Context, sql string, args ...any) PgxRow
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

type (
	PgxCommandTag interface{ RowsAffected() int64 }
	PgxRows       interface {
		Next() bool
		Scan(dest ...any) error
		Err() error
		Close()
	}
	PgxRow interface{ Scan(dest ...any) error }
)

// NewSession builds a session over a pool.
func NewSession(pool PgxPool) *PgxSession {
	return &PgxSession{pool: pool, clock: time.Now}
}

var _ httpapi.Session = (*PgxSession)(nil)

// grantsSetting is the PostgreSQL parameter the row-level security policies read.
//
// It is a constant rather than a literal at each use site because a typo in one of
// them would not error: PostgreSQL accepts any custom setting name, so a
// misspelled grants variable would be set and never read, and the policies would
// fall back to refusing everything. Every row would be invisible, which is safe
// and would present as an unexplained empty result in production.
const grantsSetting = "app.context_grants"

// callerSetting is a constant for the same reason grantsSetting is, and it is
// here for the same reason: a typo here does not error, so it would be set and
// never read, and the one table whose policy depends on it would refuse every
// row. That presents as an operation outcome that is mysteriously never found.
const callerSetting = "app.caller_user_id"

// InTx runs fn inside a transaction whose visibility is the given grants.
//
// Three details that are not interchangeable with their alternatives.
//
// The grant set is passed as a bind parameter through set_config rather than
// interpolated into a SET statement. SET takes no bind parameters, and the
// values come from a token, so building the statement with them interpolated would
// put an injection point in the one place that decides what a caller may read.
// set_config's third argument is what makes the setting transaction-local.
//
// Transaction-local matters because the pool reuses connections. A plain SET
// would persist for the life of the connection and hand this caller's grants to
// whoever used it next, which is a guide reading another reserve's drives.
//
// An empty grant set is refused rather than run. Setting it to the empty string
// makes the policies match no context, so the transaction would see nothing
// rather than everything — safe, but silently, and it converts a caller's mistake
// into an empty answer that looks correct.
// InTx runs fn inside a transaction whose visibility is the given grants, and
// whose caller identity is the given user.
func (s *PgxSession) InTx(ctx context.Context, callerID string, grants []string,
	fn func(context.Context) error) error {
	if strings.TrimSpace(callerID) == "" {
		return ErrNoCaller
	}
	if len(grants) == 0 {
		return ErrNoGrants
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	// Rollback is deferred and its error ignored: after a successful Commit it
	// fails harmlessly, and the commit's error is the one worth reporting.
	defer func() { _ = tx.Rollback(ctx) }()

	rendered, err := joinGrants(grants)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`select set_config($1, $2, true)`, grantsSetting, rendered); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx,
		`select set_config($1, $2, true)`, callerSetting, callerID); err != nil {
		return err
	}

	if err := fn(ctx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Querier runs fn inside a transaction carrying the caller's grants and hands it
// something to read through.
//
// Implemented by opening the same transaction InTx opens and sharing the setup,
// so there is exactly one place that sets the read boundary. A second copy of
// that setup would be a second chance to get it wrong, and getting it wrong is
// not a crash: it is a wider read that looks like a successful one.
func (s *PgxSession) Querier(ctx context.Context, callerID string, grants []string,
	fn func(httpapi.Querier) error) error {
	if strings.TrimSpace(callerID) == "" {
		return ErrNoCaller
	}
	if len(grants) == 0 {
		return ErrNoGrants
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rendered, err := joinGrants(grants)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`select set_config($1, $2, true)`, grantsSetting, rendered); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`select set_config($1, $2, true)`, callerSetting, callerID); err != nil {
		return err
	}

	raw, err := pgxOf(tx)
	if err != nil {
		return err
	}
	if err := fn(readable{raw}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// readable adapts the transaction to the surface a handler is allowed to use.
//
// It exposes no Commit and no Exec, deliberately. A handler that could write
// outside a checked statement, or end the transaction it is reading in, would be
// able to do things the read boundary was set up to prevent.
type readable struct{ tx pgx.Tx }

func (r readable) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return r.tx.Query(ctx, sql, args...)
}
func (r readable) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return r.tx.QueryRow(ctx, sql, args...)
}

// pgxOf recovers the real pgx transaction from whatever the pool handed back.
//
// A pool that is not the one in adapters.go — a fake in a test, say — may hand
// back something that is already a pgx.Tx and has no Raw method, so both shapes
// are accepted. Refusing the second would make the transaction in a test a
// different type from the transaction in production, which is the arrangement
// that lets a test pass against a thing that cannot work.
func pgxOf(tx PgxTx) (pgx.Tx, error) {
	if raw, ok := tx.(RawQuerier); ok {
		return raw.Raw(), nil
	}
	// The second branch an earlier version of this had — asserting the
	// transaction is already a pgx.Tx — is impossible, and go vet said so. A type
	// cannot implement both PgxTx and pgx.Tx, because their Exec methods return
	// different types. So the read handle can only be built from a transaction
	// that came through this package's adapter, which is one more reason the
	// adapter lives here rather than in the assembly file.
	return nil, errors.New("wiring: transaction cannot be read as a pgx.Tx")
}

// joinGrants renders the grant set the way the policies parse it.
//
// Commas are rejected inside a context code rather than escaped, because a code
// containing one cannot be represented in this format at all and silently
// dropping it would widen the set to something the token did not grant. Context
// codes come from Chisimba and are not free text, so this is a refusal of
// malformed input rather than a policy decision.
func joinGrants(grants []string) (string, error) {
	for _, g := range grants {
		if g == "" {
			return "", errors.New("wiring: an empty context code is not a grant")
		}
		for _, r := range g {
			if r == ',' {
				return "", errors.New("wiring: a context code containing a comma " +
					"cannot be represented in the grants setting")
			}
		}
	}
	out := grants[0]
	for _, g := range grants[1:] {
		out += "," + g
	}
	return out, nil
}
