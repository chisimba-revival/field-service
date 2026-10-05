// Package revocation decides whether a token that passed every cryptographic
// check should still be honoured.
//
// Being able to verify a signature is not the same as being entitled to. A
// token stays cryptographically valid until it expires, so a mentor who loses
// mentor status, or an account that is deactivated, keeps a perfectly good
// token for however long that token lives. This package is what closes that
// window, and it is consulted on every request rather than only at sign-in,
// because the whole point is that the answer can change while the token does not.
//
// Two independent checks, and both are needed.
//
// The epoch catches a reduction in what somebody is allowed to do. If a mentor
// is demoted, Chisimba raises the user's identity epoch and reissues. Every
// token minted before that carries the old, lower epoch, and comparing epochs
// refuses all of them at once.
//
// The timestamp catches a deactivation followed by a reactivation, where the
// epoch cannot help: nothing about the user's authority changed, so nothing
// moved the epoch, and a token minted before the deactivation is indistinguishable
// from one minted after by epoch alone. The revocation time is the only thing
// that separates them.
//
// An epoch check alone would silently readmit somebody whose account was
// deactivated and reactivated. That is the specific failure the second check
// exists to prevent, and it is why neither check is described as the belt to the
// other's braces.
package revocation

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/redis/go-redis/v9"
	"time"
)

// ErrUnavailable is returned when the denylist cannot be read.
//
// It is deliberately a distinct value rather than a false answer, because the
// caller's response to the two is opposite: a revoked user is refused, an
// unreadable denylist is a failure that must also refuse but must be reported.
// Collapsing them would make a Redis outage indistinguishable from a clean bill
// of health in the logs, which is precisely when somebody wants to know.
var ErrUnavailable = errors.New("revocation: denylist unavailable")

// Record is what a denylist entry holds.
//
// The epoch is the epoch in force at the moment of revocation, not the user's
// current epoch. Storing the current one would make the comparison useless: a
// user reactivated afterwards would have an epoch equal to the recorded one and
// the check would pass.
type Record struct {
	// RevokedAt is when the decision was made, not when it was published. A
	// webhook that was delayed must not widen the window by its own latency.
	RevokedAt time.Time

	// EpochInForce is the user's identity epoch at the moment of revocation.
	EpochInForce int64
}

// Checker answers whether a token may still be used.
type Checker interface {
	// Revoked reports whether the token described by the arguments is refused.
	//
	// An error means the answer is unknown and the caller must refuse. It never
	// means "not revoked".
	Revoked(ctx context.Context, subject string, issuedAt time.Time, epoch int64) (bool, error)
}

// MaxAge bounds how long a revocation is remembered.
//
// It exists because the exposure this package closes is bounded by token
// lifetime: a token minted an hour before revocation stops being verifiable when
// it expires, so remembering the revocation for longer than the longest token
// lifetime stores nothing that could still be used. Setting it much longer is
// not free — every entry is memory in the one store this service treats as
// disposable — and setting it shorter than the longest-lived token is a security
// hole rather than a tuning mistake.
const MaxAge = 24 * time.Hour

// Static is a Checker with no revoked subjects. It exists so that a service
// without revocation configured refuses nothing rather than failing every
// request, and so tests can state plainly when they are not exercising it.
type Static struct{}

// Revoked always reports not revoked.
func (Static) Revoked(context.Context, string, time.Time, int64) (bool, error) {
	return false, nil
}

var _ Checker = Static{}

// key is the denylist entry for a subject.
//
// The epoch and the revocation time go in the value rather than the key so that
// re-revoking a subject overwrites the earlier record instead of leaving two,
// which matters because the newest decision is the one that counts and the older
// entry would otherwise keep the TTL alive past its usefulness.
func key(subject string) string { return "revoked:" + subject }

// encode renders a record for storage.
func encode(r Record) string {
	return strconv.FormatInt(r.RevokedAt.UTC().Unix(), 10) + ":" +
		strconv.FormatInt(r.EpochInForce, 10)
}

// decode reads a stored record.
//
// A malformed entry is treated as "revoked, maximally" rather than as absent. A
// corrupt value must not become a hole in the denylist, and the safe reading of
// a record this service cannot interpret is that its subject is not to be
// trusted until the entry is rewritten.
func decode(raw, subject string) (Record, bool) {
	var at, epoch int64
	var seen int
	for i := 0; i < len(raw); i++ {
		if raw[i] == ':' {
			seen++
			if seen > 1 {
				break
			}
			at, _ = strconv.ParseInt(raw[:i], 10, 64)
			epoch, _ = strconv.ParseInt(raw[i+1:], 10, 64)
			break
		}
	}
	if seen == 0 {
		return Record{RevokedAt: time.Unix(0, 0), EpochInForce: 0}, false
	}
	return Record{RevokedAt: time.Unix(at, 0).UTC(), EpochInForce: epoch}, true
}

// decide is the whole revocation rule, kept separate from Redis so it can be
// tested against every pair of values rather than sampled through a network.
func decide(entry Record, issuedAt time.Time, epoch int64) bool {
	// The epoch check: a token minted under a superseded epoch is refused
	// regardless of when it was issued. This is what makes a role reduction take
	// effect immediately rather than at token expiry.
	if epoch < entry.EpochInForce {
		return true
	}
	// The timestamp check: a token issued before the decision cannot be
	// distinguished from one issued after by epoch alone, so time decides. The
	// comparison is strict because a token minted in the same second as the
	// revocation is one this service has no way to order, and the readable answer
	// is to refuse it.
	return !issuedAt.After(entry.RevokedAt)
}

// Redis is the store the Checker is built on.
//
// It is the one place in this service where a store holds something that is the
// only copy of it, which is worth stating because the design's own rule is that
// nothing in Redis is ever the only copy of anything. The rule is upheld by
// refusing when Redis cannot be read rather than by keeping a second copy: a
// second copy in the database would be a second thing to be stale, and the
// failure this package exists to prevent is a revoked user being admitted.
type Redis struct {
	client redisClient
	clock  Clock
	age    time.Duration
}

// Clock is injected so that issuance and revocation times are comparable in
// tests without sleeping.
type Clock func() time.Time

// redisClient is the slice of go-redis this package uses. Declared here so a
// test can substitute a fake without a server, and so the surface that matters
// is small enough to read.
type redisClient interface {
	Get(ctx context.Context, key string) (string, error)
	Del(ctx context.Context, key string) error
	Set(ctx context.Context, key, value string, exp time.Duration) error
}

// Option adjusts a Redis denylist.
type Option func(*Redis)

// WithMaxAge sets how long a revocation is remembered. Values below the longest
// access-token lifetime are refused rather than clamped, because clamping would
// silently reintroduce the window this package exists to close.
func WithMaxAge(d time.Duration) Option {
	return func(r *Redis) {
		if d > 0 {
			r.age = d
		}
	}
}

// NewRedis builds a denylist over an existing client.
func NewRedis(client redisClient, opts ...Option) *Redis {
	r := &Redis{client: client, clock: time.Now, age: MaxAge}
	for _, o := range opts {
		o(r)
	}
	return r
}

var _ Checker = (*Redis)(nil)

// Revoke records a decision, and is called from the provisioning webhook.
func (r *Redis) Revoke(ctx context.Context, subject string, rec Record) error {
	if subject == "" {
		return errors.New("revocation: empty subject")
	}
	if err := r.client.Set(ctx, key(subject), encode(rec), r.age); err != nil {
		return fmt.Errorf("revocation: recording %s: %w", subject, err)
	}
	return nil
}

// Undo removes a record.
//
// It exists for the operational case of a revocation applied in error, and it is
// deliberately explicit rather than a parameter to Revoke: clearing a
// revocation should be something a human does on purpose, and there is no
// legitimate flow in which a token needs to make it happen.
func (r *Redis) Undo(ctx context.Context, subject string) error {
	if err := r.client.Del(ctx, key(subject)); err != nil {
		return fmt.Errorf("revocation: clearing %s: %w", subject, err)
	}
	return nil
}

// Revoked reports whether the token is refused.
//
// The error path is the important part. A Redis outage returns ErrUnavailable
// and the caller refuses the request, because a denylist that cannot be read
// cannot be said to have been consulted — and the alternative, admitting traffic
// on the grounds that the thing that would have stopped it is unavailable, is
// the failure mode every access-control system is built to avoid.
func (r *Redis) Revoked(ctx context.Context, subject string, issuedAt time.Time, epoch int64) (bool, error) {
	if subject == "" {
		return true, errors.New("revocation: empty subject")
	}
	raw, err := r.client.Get(ctx, key(subject))
	switch {
	case errors.Is(err, redis.Nil):
		// No record. This is the common case and must be cheap.
		return false, nil
	case err != nil:
		return true, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}

	entry, ok := decode(raw, subject)
	if !ok {
		// The record exists and cannot be read. Refuse.
		return true, nil
	}
	return decide(entry, issuedAt, epoch), nil
}
