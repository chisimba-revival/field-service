package revocation

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// fakeRedis is a stand-in for the store, so the rule can be tested against every
// pairing of values rather than sampled through a socket.
type fakeRedis struct {
	mu      sync.Mutex
	entries map[string]string
	down    error
	gets    int
	sets    int
}

func newFake() *fakeRedis { return &fakeRedis{entries: map[string]string{}} }

func (f *fakeRedis) Get(_ context.Context, k string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.down != nil {
		return "", f.down
	}
	v, ok := f.entries[k]
	if !ok {
		return "", redis.Nil
	}
	return v, nil
}

func (f *fakeRedis) Set(_ context.Context, k, v string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets++
	if f.down != nil {
		return f.down
	}
	f.entries[k] = v
	return nil
}

func (f *fakeRedis) Del(_ context.Context, k string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down != nil {
		return f.down
	}
	delete(f.entries, k)
	return nil
}

func denylist(t *testing.T) (*Redis, *fakeRedis) {
	t.Helper()
	f := newFake()
	return NewRedis(f), f
}

// A subject with no record is not revoked. This is the common case and if it
// were wrong nothing in the service would work at all.
func TestAnUnknownSubjectIsNotRevoked(t *testing.T) {
	d, _ := denylist(t)
	revoked, err := d.Revoked(context.Background(), "user-1",
		time.Now(), 1)
	if err != nil {
		t.Fatalf("a miss should not be an error: %v", err)
	}
	if revoked {
		t.Error("a subject with no record was reported revoked")
	}
}

// The epoch check: a role reduction takes effect at once, not at token expiry.
func TestATokenFromASupersededEpochIsRefused(t *testing.T) {
	d, _ := denylist(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	if err := d.Revoke(ctx, "mentor", Record{
		RevokedAt:    at,
		EpochInForce: 5,
	}); err != nil {
		t.Fatal(err)
	}

	// Issued an hour ago — after the decision, but under the old epoch. Only the
	// epoch check can refuse this, and it is the case that makes a demotion
	// immediate.
	revoked, err := d.Revoked(ctx, "mentor", at.Add(time.Hour), 4)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("a token carrying a superseded epoch was accepted. A demotion " +
			"would then take effect only when the token expired.")
	}

	// The reissued token is fine.
	revoked, err = d.Revoked(ctx, "mentor", at.Add(2*time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("a token carrying the current epoch was refused")
	}
}

// The timestamp check: deactivation then reactivation, where the epoch never
// moved and so cannot help. This is the failure the epoch check cannot see.
func TestADeactivatedThenReactivatedUserIsStillRefused(t *testing.T) {
	d, _ := denylist(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	// Deactivated. The epoch in force does not move, because reactivation leaves
	// the user's authority exactly as it was.
	if err := d.Revoke(ctx, "trainee", Record{
		RevokedAt:    at,
		EpochInForce: 1,
	}); err != nil {
		t.Fatal(err)
	}

	// A token minted an hour before the deactivation, same epoch. Nothing about
	// this token is superseded; only its age relative to the decision separates
	// it from a legitimate one.
	revoked, err := d.Revoked(ctx, "trainee", at.Add(-time.Hour), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("a token issued before a deactivation was accepted after the " +
			"account was reactivated. The epoch is identical, so only the " +
			"timestamp can catch this, which is why both checks exist.")
	}

	// A token issued after the reactivation is honoured, even though the epoch
	// never moved. This is the case an over-strict implementation breaks on.
	revoked, err = d.Revoked(ctx, "trainee", at.Add(time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("a token issued after the reactivation was refused. Refusing " +
			"this would lock out the very person the reactivation was for.")
	}
}

// Both checks, exercised as a table rather than as the two examples above, so a
// change to either cannot quietly invert a case.
func TestTheRuleAcrossEveryPairing(t *testing.T) {
	revokedAt := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name      string
		epoch     int64
		issued    time.Time
		wantRevok bool
	}{
		{"older epoch, issued after", 4, revokedAt.Add(time.Hour), true},
		{"older epoch, issued before", 4, revokedAt.Add(-time.Hour), true},
		{"same epoch, issued before", 5, revokedAt.Add(-time.Second), true},
		{"same epoch, issued in the same second", 5, revokedAt, true},
		{"same epoch, issued after", 5, revokedAt.Add(time.Second), false},
		{"newer epoch, issued before", 6, revokedAt.Add(-time.Hour), true},
		{"newer epoch, issued after", 6, revokedAt.Add(time.Hour), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// decide is the whole rule, kept separate from the store so every
			// pairing is testable rather than sampled.
			got := decide(Record{RevokedAt: revokedAt, EpochInForce: 5},
				c.issued, c.epoch)
			if got != c.wantRevok {
				t.Errorf("epoch=%d issued=%v -> revoked=%v, want %v",
					c.epoch, c.issued, got, c.wantRevok)
			}
		})
	}
}

// The refusal a token gets must be indistinguishable from the refusal an
// unreachable denylist gives, because from the caller's side both mean "not
// permitted" — but the log must not collapse them.
func TestAnUnreachableDenylistRefusesRatherThanAdmits(t *testing.T) {
	d, f := denylist(t)
	ctx := context.Background()
	f.down = errors.New("dial tcp: connection refused")

	revoked, err := d.Revoked(ctx, "user-1", time.Now(), 1)
	if err == nil {
		t.Fatal("an unreachable denylist produced no error. The caller would " +
			"read that as not revoked and admit the request.")
	}
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("expected ErrUnavailable so the failure is distinguishable from "+
			"a clean bill of health, got: %v", err)
	}
	if !revoked {
		t.Error("an unreachable denylist reported not revoked")
	}
}

// A record this service cannot read must not become a hole in the denylist.
func TestACorruptRecordRefusesRatherThanAdmits(t *testing.T) {
	d, f := denylist(t)
	ctx := context.Background()
	f.entries[key("user-1")] = "not-a-record"

	revoked, err := d.Revoked(ctx, "user-1", time.Now().Add(time.Hour), 99)
	if err != nil {
		t.Fatalf("a corrupt record should refuse rather than error: %v", err)
	}
	if !revoked {
		t.Error("a record that could not be interpreted was treated as no " +
			"revocation. The safe reading of an unreadable entry is that its " +
			"subject is not to be trusted.")
	}
}

func TestReRevokingReplacesTheEarlierRecord(t *testing.T) {
	d, f := denylist(t)
	ctx := context.Background()
	first := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	if err := d.Revoke(ctx, "user-1", Record{RevokedAt: first, EpochInForce: 1}); err != nil {
		t.Fatal(err)
	}
	if err := d.Revoke(ctx, "user-1", Record{RevokedAt: first.Add(time.Hour), EpochInForce: 2}); err != nil {
		t.Fatal(err)
	}

	if len(f.entries) != 1 {
		t.Errorf("%d entries for one subject; the older record would keep its "+
			"own TTL alive past its usefulness", len(f.entries))
	}
	// The token that distinguishes the two records: issued between the decisions
	// but carrying the epoch in force at the first of them.
	//
	// Under the earlier record alone this token would be admitted — its epoch
	// matches and it postdates that revocation. It is refused only because the
	// later record replaced it. That is what "replaces" has to mean, and a
	// check that merely confirmed one entry per subject would not have caught a
	// version that kept both.
	revoked, err := d.Revoked(ctx, "user-1", first.Add(30*time.Minute), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !revoked {
		t.Error("the earlier record is still being applied after a later one " +
			"replaced it")
	}

	// And a token issued after the later decision is admitted, so replacement
	// did not become permanent refusal.
	revoked, err = d.Revoked(ctx, "user-1", first.Add(90*time.Minute), 2)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("a token issued after the later decision was refused")
	}
}

// The operational case of a revocation applied in error.
func TestUndoClearsARecord(t *testing.T) {
	d, _ := denylist(t)
	ctx := context.Background()
	at := time.Now()

	if err := d.Revoke(ctx, "user-1", Record{RevokedAt: at, EpochInForce: 3}); err != nil {
		t.Fatal(err)
	}
	if err := d.Undo(ctx, "user-1"); err != nil {
		t.Fatal(err)
	}
	revoked, err := d.Revoked(ctx, "user-1", at, 3)
	if err != nil {
		t.Fatal(err)
	}
	if revoked {
		t.Error("a cleared revocation is still being applied")
	}
}

func TestAnEmptySubjectIsRefusedRatherThanLookedUp(t *testing.T) {
	d, f := denylist(t)
	revoked, err := d.Revoked(context.Background(), "", time.Now(), 1)
	if err == nil {
		t.Error("an empty subject was accepted")
	}
	if !revoked {
		t.Error("an empty subject was reported not revoked")
	}
	if f.gets != 0 {
		t.Errorf("the store was read %d times for an empty subject", f.gets)
	}
}

func TestTheRuleIsExercisedThroughTheStoreAsWellAsDirectly(t *testing.T) {
	d, _ := denylist(t)
	ctx := context.Background()
	at := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	if err := d.Revoke(ctx, "user-1", Record{RevokedAt: at, EpochInForce: 7}); err != nil {
		t.Fatal(err)
	}

	// Round-tripped through encode and decode, so a change to the stored format
	// that made the epoch unreadable would show up here rather than in production.
	if _, err := d.Revoked(ctx, "user-1", at.Add(-time.Minute), 7); err != nil {
		t.Fatal(err)
	} else if revoked, _ := d.Revoked(ctx, "user-1", at.Add(-time.Minute), 7); !revoked {
		t.Error("a token issued before the decision was accepted after a " +
			"round trip through the stored format")
	}
	if revoked, _ := d.Revoked(ctx, "user-1", at.Add(time.Minute), 7); revoked {
		t.Error("a token issued after the decision was refused")
	}
	if revoked, _ := d.Revoked(ctx, "user-1", at.Add(time.Minute), 6); !revoked {
		t.Error("a superseded epoch was accepted")
	}
}

func TestErrUnavailableIsDistinguishable(t *testing.T) {
	// The whole reason this is a named error: a caller has to be able to tell an
	// infrastructure failure from a decision, and a string comparison on the
	// message would be how that gets broken.
	wrapped := errors.Join(ErrUnavailable, errors.New("dial tcp"))
	if !errors.Is(wrapped, ErrUnavailable) {
		t.Error("ErrUnavailable does not survive wrapping")
	}
	if strings.Contains(ErrUnavailable.Error(), "user") {
		t.Error("the error names a subject, which would leak one into a log")
	}
}

// The checker a service uses when revocation is not configured must refuse
// nothing, and must say so.
func TestStaticRefusesNobody(t *testing.T) {
	revoked, err := Static{}.Revoked(context.Background(), "user-1",
		time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC), 0)
	if err != nil || revoked {
		t.Errorf("Static revoked=%v err=%v, want false/nil", revoked, err)
	}
}
