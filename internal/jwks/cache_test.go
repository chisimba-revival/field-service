package jwks

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"strings"
	"sync"
	"testing"
	"time"

	"field-service/internal/authn"
)

// A key the tests can sign with, and the JWKS entry that publishes it.
type issuer struct {
	kid string
	key *rsa.PrivateKey
}

func newIssuer(t *testing.T, kid string) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	return &issuer{kid: kid, key: key}
}

// entry renders the issuer's public key as the JWKS document the service reads.
func (i *issuer) entry() map[string]string {
	pub := i.key.PublicKey
	return map[string]string{
		"kty": "RSA",
		"kid": i.kid,
		"alg": "RS256",
		"use": "sig",
		"n":   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func document(t *testing.T, entries ...map[string]string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"keys": entries})
	if err != nil {
		t.Fatalf("rendering document: %v", err)
	}
	return raw
}

// countingFetcher returns a fetcher and a pointer to how many times it ran, so
// the tests can assert on how often the network was touched. That distinction is
// the whole point of a cache: a key that is already held must not cause a fetch.
func countingFetcher(t *testing.T, doc func() []byte) (Fetcher, *int) {
	t.Helper()
	calls := 0
	return func(context.Context) ([]byte, error) {
		calls++
		if d := doc(); d != nil {
			return d, nil
		}
		return nil, errors.New("jwks: upstream is down")
	}, &calls
}

func newTestCache(t *testing.T, f Fetcher, now *time.Time, interval time.Duration) *Cache {
	t.Helper()
	c := New("https://gateway.test/.well-known/jwks.json",
		WithInterval(interval), WithTimeout(time.Second))
	c.fetch = f
	c.clock = func() time.Time { return *now }
	return c
}

func TestAKeyIsResolvedAndOnlyFetchedOnce(t *testing.T) {
	a := newIssuer(t, "kid-a")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	fetch, calls := countingFetcher(t, func() []byte {
		return document(t, a.entry())
	})
	c := newTestCache(t, fetch, &now, time.Hour)

	for i := 0; i < 5; i++ {
		key, err := c.ResolveKey("kid-a")
		if err != nil {
			t.Fatalf("resolve %d: %v", i, err)
		}
		if key.N.Cmp(a.key.PublicKey.N) != 0 {
			t.Fatalf("resolve %d returned a different key than was published", i)
		}
	}
	if *calls != 1 {
		t.Errorf("fetched %d times for one key held five times. A token validated "+
			"per request must not make the issuer's availability this service's.", *calls)
	}
}

// The reason this exists at all: a rotation must not lock the service out.
func TestARotatedKeyIsPickedUpWithoutARestart(t *testing.T) {
	old := newIssuer(t, "kid-old")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	current := document(t, old.entry())

	fetch, calls := countingFetcher(t, func() []byte { return current })
	c := newTestCache(t, fetch, &now, time.Hour)

	if _, err := c.ResolveKey("kid-old"); err != nil {
		t.Fatalf("the original key should resolve: %v", err)
	}

	// The issuer rotates and publishes a new key, retiring the old one.
	fresh := newIssuer(t, "kid-new")
	current = document(t, fresh.entry())

	// A token arrives signed by the new key. Nothing about it is wrong except
	// that this process has never seen the key before.
	key, err := c.ResolveKey("kid-new")
	if err != nil {
		t.Fatalf("a key published after start-up did not resolve: %v", err)
	}
	if key.N.Cmp(fresh.key.PublicKey.N) != 0 {
		t.Error("resolved to the wrong key")
	}
	if *calls != 2 {
		t.Errorf("fetched %d times, want 2: the initial load and one for the new key", *calls)
	}
}

// A withdrawn key must stop being trusted once the document says so. This is
// the reason for the periodic refresh rather than a load-once cache.
func TestAWithdrawnKeyStopsResolvingAfterARefresh(t *testing.T) {
	old := newIssuer(t, "kid-old")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	current := document(t, old.entry())

	fetch, _ := countingFetcher(t, func() []byte { return current })
	c := newTestCache(t, fetch, &now, time.Minute)

	if _, err := c.ResolveKey("kid-old"); err != nil {
		t.Fatalf("the original key should resolve: %v", err)
	}

	// The issuer withdraws it and publishes a replacement under a new id.
	current = document(t, newIssuer(t, "kid-new").entry())

	// The key is still held, so within the interval it resolves — that window is
	// the accepted cost of not calling back on every request, and the interval is
	// what bounds it.
	if _, err := c.ResolveKey("kid-old"); err != nil {
		t.Fatalf("within the refresh interval a held key should still resolve: %v", err)
	}

	// Past the interval the document is re-read and the withdrawn key is gone.
	now = now.Add(2 * time.Minute)
	if _, err := c.ResolveKey("kid-old"); err == nil {
		t.Error("a withdrawn key still resolves after the refresh interval. " +
			"A compromised signing key would stay trusted for as long as the " +
			"process lives.")
	}
}

// An unreachable issuer must not lock out the keys this service already holds.
func TestAKeyIsStillServedWhileTheIssuerIsUnreachable(t *testing.T) {
	a := newIssuer(t, "kid-a")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	up := true
	fetch, _ := countingFetcher(t, func() []byte {
		if !up {
			return nil
		}
		return document(t, a.entry())
	})
	c := newTestCache(t, fetch, &now, time.Minute)

	if _, err := c.ResolveKey("kid-a"); err != nil {
		t.Fatalf("the original key should resolve: %v", err)
	}

	up = false
	now = now.Add(2 * time.Minute)

	if _, err := c.ResolveKey("kid-a"); err != nil {
		t.Errorf("refused a token it holds a current-enough key for, because "+
			"the issuer was briefly unreachable: %v", err)
	}
	if _, err := c.ResolveKey("kid-never-published"); err == nil {
		t.Error("resolved a key that was never published while the issuer was down")
	}
}

// An unrecognised token must not be a way to make this service fetch the
// document once per attempt.
func TestAnUnknownKeyCausesOneFetchNotOnePerCall(t *testing.T) {
	a := newIssuer(t, "kid-a")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	fetch, calls := countingFetcher(t, func() []byte {
		return document(t, a.entry())
	})
	c := newTestCache(t, fetch, &now, time.Hour)

	for i := 0; i < 4; i++ {
		if _, err := c.ResolveKey("kid-not-real"); err == nil {
			t.Fatal("resolved a key that was never published")
		}
	}
	if *calls != 1 {
		t.Errorf("fetched %d times for four unrecognised key ids. A forged token "+
			"would then be a way to hammer the issuer.", *calls)
	}
}

// Two requests arriving together must produce one fetch. A burst of misses is
// exactly when the issuer is most likely to be struggling.
func TestConcurrentMissesAreCoalescedIntoOneFetch(t *testing.T) {
	a := newIssuer(t, "kid-a")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0

	c := newTestCache(t, func(context.Context) ([]byte, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		<-release
		return document(t, a.entry()), nil
	}, &now, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.ResolveKey("kid-a"); err != nil {
				t.Errorf("concurrent resolve: %v", err)
			}
		}()
	}
	// Let the goroutines pile up behind the single in-flight fetch.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("%d concurrent misses caused %d fetches, want 1", 8, calls)
	}
}

func TestAnEmptyKeyIDIsRefusedWithoutFetching(t *testing.T) {
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	fetch, calls := countingFetcher(t, func() []byte { return document(t) })
	c := newTestCache(t, fetch, &now, time.Hour)

	if _, err := c.ResolveKey(""); err == nil {
		t.Fatal("accepted an empty key id")
	}
	if *calls != 0 {
		t.Errorf("fetched %d times for an empty key id", *calls)
	}
}

// Keys the document publishes for other consumers must not be loaded. A key with
// use: enc must never verify a signature even when its id matches.
func TestKeysThatCannotVerifyASignatureAreNotLoaded(t *testing.T) {
	rsaKey := newIssuer(t, "kid-rsa")
	now := time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)

	ec := rsaKey.entry()
	ec["kty"] = "EC"
	ec["kid"] = "kid-ec"

	oct := rsaKey.entry()
	oct["kty"] = "oct"
	oct["kid"] = "kid-oct"

	forSigningOnly := rsaKey.entry()
	forSigningOnly["alg"] = "RS512"
	forSigningOnly["kid"] = "kid-rs512"

	fetch, _ := countingFetcher(t, func() []byte {
		return document(t, ec, oct, forSigningOnly, rsaKey.entry())
	})
	c := newTestCache(t, fetch, &now, time.Hour)

	for _, kid := range []string{"kid-ec", "kid-oct", "kid-rs512"} {
		if _, err := c.ResolveKey(kid); err == nil {
			t.Errorf("loaded %s, which cannot verify an RS256 signature", kid)
		}
	}
	if _, err := c.ResolveKey("kid-rsa"); err != nil {
		t.Errorf("the one usable key was not loaded: %v", err)
	}
}

func TestADocumentWithNoUsableKeyIsAnError(t *testing.T) {
	rsaKey := newIssuer(t, "kid-rsa")
	ec := rsaKey.entry()
	ec["kty"] = "EC"

	if _, err := Parse(document(t, ec)); err == nil {
		t.Fatal("accepted a document publishing nothing this service can use. " +
			"That is a misconfiguration, not a document about someone else's keys.")
	}
}

// Two keys claiming one id make the document ambiguous, and taking one of them
// silently would let an issuer verify a token against a key it did not intend.
func TestADuplicateKeyIDIsRefused(t *testing.T) {
	a := newIssuer(t, "kid-a")
	other := map[string]string{}
	for k, v := range newIssuer(t, "kid-a").entry() {
		other[k] = v
	}

	if _, err := Parse(document(t, a.entry(), other)); err == nil {
		t.Fatal("accepted a document with two keys claiming one id")
	} else if !strings.Contains(err.Error(), "twice") {
		t.Errorf("expected a duplicate-id failure, got: %v", err)
	}
}

func TestMalformedDocumentsAreRefused(t *testing.T) {
	cases := map[string]string{
		"not json at all":        `not json`,
		"an array":               `[{"kty":"RSA"}]`,
		"a key with no modulus":  `{"keys":[{"kty":"RSA","kid":"k","alg":"RS256","e":"AQAB"}]}`,
		"a modulus that is junk": `{"keys":[{"kty":"RSA","kid":"k","alg":"RS256","n":"!!!","e":"AQAB"}]}`,
		"an empty document":      `{}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Error("accepted a document it should have refused")
			}
		})
	}
}

// KeyIDs exists so a refresh can be logged without logging key material.
func TestKeyIDsListsOnlyUsableKeys(t *testing.T) {
	a := newIssuer(t, "kid-b")
	ec := map[string]string{}
	for k, v := range a.entry() {
		ec[k] = v
	}
	ec["kty"] = "EC"

	got := KeyIDs(document(t, a.entry(), ec))
	if len(got) != 1 || got[0] != "kid-b" {
		t.Errorf("KeyIDs = %v, want [kid-b]", got)
	}
}

// The cache has to satisfy the validator's interface, which is the whole reason
// it exists in this shape.
// Asserted against the real interface rather than a copy of it, so a change to
// the validator's contract breaks the build here instead of at the call site.
var _ authn.KeyResolver = (*Cache)(nil)
