package wiring_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"field-service/internal/revocation"
	"field-service/internal/wiring"
)

// The adapter's first version swallowed redis.Nil and returned an empty string.
// The denylist tells "no entry" from "an entry that cannot be read" by the error
// it gets, and treats the second as revoked — so on an empty Redis every request
// was refused as revoked, and a hundred unit tests passed because they all use
// their own fake. Only a real client found it.
//
// This is the test that should have existed: it uses the real client, the real
// container, and the real absence of a key.

func redisAddr() string {
	if a := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_REDIS_ADDR")); a != "" {
		return a
	}
	return "127.0.0.1:56380"
}

func TestAnAbsentDenylistEntryIsNotRevoked(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: redisAddr()})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("no redis at %s: %v", redisAddr(), err)
	}

	// A subject that cannot be on any denylist, and cleared anyway so the test
	// does not depend on a previous run having failed to clean up.
	const subject = "wiring-test-never-revoked"
	_ = c.Del(ctx, "revoked:"+subject).Err()

	got, err := wiring.NewDenylist(c).Revoked(ctx, subject, time.Now(), 1)
	if err != nil {
		t.Fatalf("Revoked: %v", err)
	}
	if got {
		t.Error("a subject with no denylist entry came back revoked. The adapter " +
			"is turning absence into an unreadable record, and the denylist " +
			"refuses an unreadable record — so every request is refused on an " +
			"empty Redis.")
	}
}

func TestARealRevocationIsStillSeen(t *testing.T) {
	c := redis.NewClient(&redis.Options{Addr: redisAddr()})
	defer func() { _ = c.Close() }()
	ctx := context.Background()
	if err := c.Ping(ctx).Err(); err != nil {
		t.Skipf("no redis at %s: %v", redisAddr(), err)
	}

	const subject = "wiring-test-revoked"
	d := wiring.NewDenylist(c)
	if err := d.Revoke(ctx, subject, revocation.Record{
		RevokedAt:    time.Now().UTC(),
		EpochInForce: 5,
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	t.Cleanup(func() { _ = d.Undo(context.Background(), subject) })

	got, err := d.Revoked(ctx, subject, time.Now(), 1)
	if err != nil {
		t.Fatalf("Revoked: %v", err)
	}
	if !got {
		t.Error("a real revocation was not seen. An adapter that refused " +
			"everything would pass the absence test above, so both halves are needed.")
	}
}
