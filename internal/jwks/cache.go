// Package jwks holds the public keys an access token might have been signed
// with, and keeps them fresh enough that a key rotation does not stop the field
// service accepting traffic.
//
// Why this is a cache at all: the contract requires the service to validate
// Chisimba-issued tokens locally against the published JWKS document, and never
// to call back to Chisimba to ask. Fetching that document on every request
// would make Chisimba's availability the field service's availability, which is
// the coupling the rule exists to avoid.
//
// Why it is more than a cache: a JWKS document changes. A key that was
// published an hour ago may have been retired, and a key that was not published
// an hour ago may be the only one that can verify the token in front of us. A
// cache that only ever loads once therefore breaks in both directions — refusing
// tokens signed by a freshly rotated key, and trusting a key that has since been
// withdrawn.
package jwks

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// DefaultRefreshInterval is how often the document is re-read even when every
// key it names is already held.
//
// The interval is a floor on staleness for a *withdrawn* key. A key that is
// removed from the document stays usable in this cache until the next refresh,
// so this is the window in which a compromised signing key remains trusted. It
// is therefore short by design, and short enough that raising it is a decision
// about how long a revoked key stays useful rather than a performance tweak.
const DefaultRefreshInterval = 15 * time.Minute

// MissCooldown is how long the cache refuses to re-fetch after an attempt that
// did not produce the key that was asked for.
//
// This is the difference between a cache and a denial-of-service amplifier. An
// access token carries an attacker-chosen `kid`, so without a cooldown every
// forged token with a random one causes another fetch of the issuer's document.
// Four unrecognised key ids caused four fetches before this existed, which was
// found by a test rather than by reading.
//
// It is short so that a genuine key rotation is not locked out behind it: the
// cost of a rotation arriving mid-cooldown is one rejected token, and the cost
// of no cooldown is an unbounded request rate against the issuer.
const MissCooldown = 30 * time.Second

// DefaultTimeout bounds a single fetch.
//
// A JWKS document is small and the service is offline-first, so a slow fetch is
// a fault rather than a condition to wait out. The caller is an HTTP handler
// with a request behind it.
const DefaultTimeout = 5 * time.Second

// Clock is injected so that refresh timing is testable without sleeping.
type Clock func() time.Time

// Fetcher retrieves the raw JWKS document.
type Fetcher func(ctx context.Context) ([]byte, error)

// Cache resolves key ids from a periodically refreshed JWKS document.
//
// It is safe for concurrent use. Key resolution happens on every request while
// refresh happens rarely, so the two are separated deliberately: resolution
// takes a read lock and never blocks on the network.
type Cache struct {
	fetch     Fetcher
	client    *http.Client
	clock     Clock
	interval  time.Duration
	timeout   time.Duration
	jwksURL   string
	mu        sync.RWMutex
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	// lastMiss is when a completed fetch failed to contain the key id that was
	// asked for. Measured from that rather than from every attempt, because the
	// cooldown's job is to refuse a repeat of a *miss*: a fetch that succeeded and
	// did contain the key must not stop the next key id being tried, or a real
	// rotation arriving within the window would be refused.
	lastMiss time.Time

	// inflight coalesces concurrent fetches. Two requests arriving with two
	// unknown key ids must produce one fetch, not two: the document is the same
	// for both, and a burst of misses is exactly when the upstream is most
	// likely to be struggling.
	inflight *fetchOnce
}

type fetchOnce struct {
	done   chan struct{}
	keys   map[string]*rsa.PublicKey
	at     time.Time
	err    error
	caller bool
}

// Option adjusts a Cache.
type Option func(*Cache)

// WithInterval sets how long a successfully fetched document is trusted.
func WithInterval(d time.Duration) Option {
	return func(c *Cache) {
		if d > 0 {
			c.interval = d
		}
	}
}

// WithTimeout sets the per-fetch timeout.
func WithTimeout(d time.Duration) Option {
	return func(c *Cache) {
		if d > 0 {
			c.timeout = d
		}
	}
}

// WithHTTPClient supplies the client used to build the default fetcher.
//
// Supplied mainly so the default fetcher has somewhere to take its client from;
// tests normally replace the fetcher outright.
func WithHTTPClient(h *http.Client) Option {
	return func(c *Cache) { c.client = h }
}

// New builds a cache that reads the document at jwksURL.
func New(jwksURL string, opts ...Option) *Cache {
	c := &Cache{
		client:   &http.Client{Timeout: DefaultTimeout},
		clock:    time.Now,
		interval: DefaultRefreshInterval,
		timeout:  DefaultTimeout,
		jwksURL:  jwksURL,
		keys:     map[string]*rsa.PublicKey{},
	}
	for _, o := range opts {
		o(c)
	}
	c.fetch = c.httpFetch
	return c
}

// ResolveKey returns the key for a key id, refreshing once if it is not held.
//
// Returning an error means "I do not hold that key", which the validator treats
// as a refresh signal rather than a rejection. The single refresh is deliberate:
// a token whose kid is genuinely unknown must not be able to make this service
// fetch the document once per attempt, or an unrecognised token becomes a way to
// hammer Chisimba.
//
// The second reason to bound it: caching a *miss* is the classic way a key
// rotation gets locked out. If the first fetch after a rotation also fails, the
// second attempt must be allowed to fetch again rather than inheriting the
// failure. That is why the cooldown below is short and why a failed refresh
// does not update fetchedAt.
func (c *Cache) ResolveKey(keyID string) (*rsa.PublicKey, error) {
	if keyID == "" {
		return nil, errors.New("jwks: empty key id")
	}

	now := c.clock()

	c.mu.RLock()
	key, held := c.keys[keyID]
	fresh := !c.fetchedAt.IsZero() && now.Sub(c.fetchedAt) < c.interval
	fetching := c.inflight != nil
	recentMiss := !c.lastMiss.IsZero() && now.Sub(c.lastMiss) < MissCooldown
	c.mu.RUnlock()

	// A key held and fresh needs no network at all. This is the common case and
	// the reason the cache exists.
	if held && fresh {
		return key, nil
	}

	// A miss, where a fetch very recently completed and did not contain this key
	// id. Fetching again would be the amplifier the cooldown exists to prevent,
	// and the token is refused either way.
	//
	// The in-flight check comes first deliberately: concurrent callers arriving
	// on a cold cache must wait for the one fetch rather than each deciding the
	// other is already handling it and refusing a key that is about to arrive.
	if !held && !fetching && recentMiss {
		return nil, fmt.Errorf("jwks: no key for id %q", keyID)
	}

	// A miss, or a held key gone stale. Either way, one coalesced refresh.
	keys, err := c.refresh(now, keyID)
	if err != nil {
		if held {
			// The document is unreachable but we still hold this key. Refusing to
			// verify tokens we have a key for because the issuer is briefly down
			// would turn Chisimba having a bad minute into every device in the
			// field being locked out, which is the opposite of what this is for.
			return key, nil
		}
		return nil, err
	}
	if fresh, ok := keys[keyID]; ok {
		return fresh, nil
	}
	return nil, fmt.Errorf("jwks: no key for id %q", keyID)
}

// refresh fetches the document once, however many callers want it.
// refresh fetches the document, coalesced, and records whether the key the
// caller asked for was in it. That last part is what arms the cooldown, and it is
// why refresh is told which key it is being asked about rather than just
// re-reading the document.
func (c *Cache) refresh(now time.Time, askedFor string) (map[string]*rsa.PublicKey, error) {
	c.mu.Lock()
	if c.inflight != nil {
		waiter := c.inflight
		c.mu.Unlock()
		<-waiter.done
		return waiter.keys, waiter.err
	}
	f := &fetchOnce{done: make(chan struct{}), caller: true}
	c.inflight = f
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.inflight = nil
		c.mu.Unlock()
		close(f.done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()

	raw, err := c.fetch(ctx)
	if err != nil {
		f.err = fmt.Errorf("jwks: fetching %s: %w", c.jwksURL, err)
		return nil, f.err
	}
	keys, err := Parse(raw)
	if err != nil {
		f.err = err
		return nil, err
	}

	c.mu.Lock()
	c.keys = keys
	c.fetchedAt = now
	if _, present := keys[askedFor]; !present {
		c.lastMiss = now
	}
	c.mu.Unlock()

	f.keys, f.at = keys, now
	return keys, nil
}

// httpFetch is the default Fetcher.
func (c *Cache) httpFetch(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.jwksURL, nil)
	if err != nil {
		return nil, err
	}
	// A gateway or proxy that caches by default would serve a stale key set and
	// there would be no way to tell from here.
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks: %s returned %s", c.jwksURL, resp.Status)
	}
	const maxDocument = 1 << 20
	return readAtMost(resp.Body, maxDocument)
}
