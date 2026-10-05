package wiring

// This file exists because Go has no covariance.
//
// Every interface in this service is hand-written and narrow, on purpose: a
// wider one would let the read boundary be set from outside the file that owns
// it. The cost is that *pgxpool.Pool and *redis.Client do not satisfy them —
// pgx returns pgx.Rows where PgxRows is wanted, redis returns a command where an
// error is wanted — and something has to bridge that.
//
// The bridges live here rather than in main, because this package is the one
// whose job is "the place where the parts meet". An adapter in main would mean
// the assembly file was also deciding how two packages talk to each other, and
// there would be two places to change when the interface underneath moved.

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"field-service/internal/revocation"
)

// NewPgxPool adapts a pgx pool to the narrow pool this package opens
// transactions on.
//
// The adapter exists only to change the return type of Begin. pgx.Tx already
// satisfies PgxTx method for method; what it does not do is return PgxRows
// where PgxTx promises PgxRows, and that difference is invisible until Go tries
// to assign one to the other.
func NewPgxPool(p *pgxpool.Pool) PgxPool { return &poolAdapter{p: p} }

type poolAdapter struct{ p *pgxpool.Pool }

func (a *poolAdapter) Begin(ctx context.Context) (PgxTx, error) {
	tx, err := a.p.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &txAdapter{Tx: tx}, nil
}

// txAdapter narrows pgx.Tx. Commit and Rollback are promoted from the embedded
// value because their signatures already match.
type txAdapter struct{ pgx.Tx }

// Raw exposes the underlying pgx transaction.
//
// It exists because pgx.Rows carries more methods than this package's narrow
// PgxRows — CommandTag and FieldDescriptions among them — so a handle narrowed
// to four methods cannot be handed to a caller that wants a real pgx.Rows.
// Narrowing is right for what a handler is allowed to do and wrong for what it
// is allowed to hold, and this is the seam between the two.
func (t *txAdapter) Raw() pgx.Tx { return t.Tx }

// RawQuerier is implemented by a transaction that can hand back the pgx value
// underneath it.
type RawQuerier interface{ Raw() pgx.Tx }

func (t *txAdapter) Exec(ctx context.Context, sql string, args ...any) (PgxCommandTag, error) {
	return t.Tx.Exec(ctx, sql, args...)
}

// Query returns pgx.Rows directly. It satisfies PgxRows already — Next, Scan,
// Err and Close are the same four methods — so wrapping it would be ceremony.
func (t *txAdapter) Query(ctx context.Context, sql string, args ...any) (PgxRows, error) {
	return t.Tx.Query(ctx, sql, args...)
}

func (t *txAdapter) QueryRow(ctx context.Context, sql string, args ...any) PgxRow {
	return t.Tx.QueryRow(ctx, sql, args...)
}

// NewDenylist adapts a Redis client to the denylist's narrow client.
//
// The three method bodies below are where the awkwardness lives: redis returns
// a command object that has to be asked for its result and its error, where the
// denylist wants a string and an error.
//
// redis.Nil is PROPAGATED, and that is not a detail. The denylist tells "no
// entry" from "an entry that cannot be read" by the error, and treats the second
// as revoked. This adapter's first version swallowed redis.Nil and returned an
// empty string, which made "no entry" indistinguishable from "an entry that
// cannot be read" — so every request was refused as revoked, on an empty Redis,
// with nothing in the logs to suggest why. A hundred unit tests did not catch it
// because they use their own fake: the adapter was the fake, and only a real
// client found it.
func NewDenylist(c redis.Cmdable) *revocation.Redis {
	return revocation.NewRedis(&redisAdapter{c: c})
}

type redisAdapter struct{ c redis.Cmdable }

func (a *redisAdapter) Get(ctx context.Context, key string) (string, error) {
	// Returning the error unchanged, including redis.Nil, is the whole contract.
	v, err := a.c.Get(ctx, key).Result()
	if err != nil {
		return "", err
	}
	return v, nil
}

func (a *redisAdapter) Del(ctx context.Context, key string) error {
	return a.c.Del(ctx, key).Err()
}

func (a *redisAdapter) Set(ctx context.Context, key, value string, exp time.Duration) error {
	return a.c.Set(ctx, key, value, exp).Err()
}
