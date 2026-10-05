//go:build e2e

// Package e2e drives the assembled service over real HTTP.
//
// Every other test in this repository calls a handler or a store directly. That
// is deliberate for those tests — a fake session is what lets a field-name test
// measure field names — but it left a gap with a very specific shape: the faults
// that only exist where two real pieces meet. Four of them, all found on the day
// the binary was first run and none of them reachable from a unit test.
//
//   - The Redis adapter swallowed redis.Nil, so an absent denylist entry was
//     indistinguishable from an unreadable one, and every request was refused as
//     revoked. A hundred unit tests passed throughout, because they all use
//     their own fake: the adapter was the fake.
//   - A refusal did not stop the handler. problem() returned writeJSON's error,
//     which is nil on success, so "refused and already answered" and "carried on"
//     were the same value and a wrong content type got two responses.
//   - The fault path panicked, so the fault was never reported.
//   - A create missing a required field failed the whole batch, although the
//     contract says a batch is not all-or-nothing.
//
// None of those was a mistake in a handler. All of them were mistakes in the
// wiring between handlers, and no test that stops one component short of another
// can see them. This package is the part that does not stop short.
//
// What it deliberately does not do is stand up Chisimba. Minting a token here
// keeps the suite runnable by anyone with the two databases, and the claim shape
// is pinned by hand against what Chisimba emits — a stale comment is a far
// smaller risk than a suite that only runs on the machine where it was written.
package e2e

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"field-service/internal/authn"
	"field-service/internal/httpapi"
	"field-service/internal/jwks"
	"field-service/internal/pull"
	"field-service/internal/push"
	"field-service/internal/species"
	"field-service/internal/wiring"
)

// integrationTestLock must match the constant in internal/store, internal/push
// and internal/pull. See where it is used for why that is fragile.
const integrationTestLock int64 = 821004001

const (
	issuer   = "chisimba"
	audience = "chisimba-api"
	scope    = "Site Admin"

	// A context code that is deliberately not the token's, so that a sighting
	// landing in the right place is evidence of something rather than a
	// coincidence of naming.
	foreignContext = "someone-elses-reserve"
)

func adminDSN() string {
	if d := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_ADMIN_DSN")); d != "" {
		return d
	}
	return "postgres://fieldsvc:fieldsvc@127.0.0.1:55433/fieldlog?sslmode=disable"
}

func redisAddr() string {
	if a := strings.TrimSpace(os.Getenv("FIELDSVC_TEST_REDIS_ADDR")); a != "" {
		return a
	}
	return "127.0.0.1:56380"
}

// rig is a running service and everything needed to interrogate it.
type rig struct {
	t      *testing.T
	server *httptest.Server
	keys   *rsa.PrivateKey
	kid    string
	pool   *pgx.Conn
	redis  *redis.Client
}

// start assembles the service the way cmd/field-service does, over real
// Postgres and real Redis, and refuses to go on without them.
func start(t *testing.T) *rig {
	t.Helper()
	ctx := context.Background()

	conn, err := pgx.Connect(ctx, adminDSN())
	if err != nil {
		t.Skipf("no database at %s: %v", adminDSN(), err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr()})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no redis at %s: %v", redisAddr(), err)
	}
	t.Cleanup(func() { _ = rdb.Close() })

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	// The key is served by this process rather than by a separate server on a
	// fixed port. A hand-started server was how an earlier attempt "found" a key
	// mismatch: a stale process from a previous session held the port and served
	// a different keypair, so the diagnosis was right about the symptom and
	// completely wrong about the cause. There is no port to be stale here.
	jwksServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, jwksDocument(t, key))
	}))
	t.Cleanup(jwksServer.Close)

	cache := jwks.New(jwksServer.URL)
	validator, err := authn.NewValidator(cache, audience, issuer)
	if err != nil {
		t.Fatalf("validator: %v", err)
	}

	// A real pool, through the real adapters, because the adapters are exactly
	// what broke. The redis.Nil fault lived in the seam between the socket and
	// the denylist, and no arrangement of fakes reproduces a seam.
	pool, err := pgxpool.New(ctx, adminDSN())
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// The same lock the other integration packages take. This suite writes real
	// sightings, so running it beside a package that asserts on sighting counts
	// would make two suites corrupt each other's fixtures — and the failure would
	// be an assertion about data neither suite wrote.
	//
	// The value is repeated rather than imported because it is declared in three
	// separate test packages, which cannot see each other. That makes it a
	// coupling with nothing enforcing it: if one package changed its key, this
	// one would quietly stop mutual exclusion and two suites would start
	// corrupting each other's fixtures again. Naming where it came from is the
	// only thing standing between a reader and a silent change.
	if _, err := conn.Exec(ctx, "select pg_advisory_lock($1)", integrationTestLock); err != nil {
		t.Fatalf("taking the integration-test lock: %v", err)
	}
	t.Cleanup(func() {
		// The connection is closed immediately after this, and closing a session
		// releases its advisory locks anyway, so a failed unlock here is not a
		// lock left held.
		_, _ = conn.Exec(context.Background(), "select pg_advisory_unlock($1)", integrationTestLock)
	})

	session := wiring.NewSession(wiring.NewPgxPool(pool))
	guard := httpapi.NewGuard(
		wiring.NewVerifier(validator),
		wiring.NewDenylist(rdb),
		session,
		testLogger{t},
		scope,
	)

	sync := &httpapi.Sync{
		Push:    push.New(push.NewPgxStore(pool)),
		Session: session,
		Pull: func(ctx context.Context, caller, cursor string, q httpapi.Querier) (pull.Page, error) {
			return pull.New(pull.NewPgxStore(q)).Pull(ctx, caller, cursor)
		},
		Log: testLogger{t},
	}

	catalogue := &httpapi.Catalogue{
		Catalogue: species.New(species.NewPgxStore(pool)),
		Log:       testLogger{t},
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/sync/", httpapi.NewSyncRoutes(guard, sync))
	// Registered exactly as cmd/field-service does. The rig previously wired
	// only the sync routes, so it was not the assembly it claimed to be: a route
	// added to main and forgotten here would have been tested by neither.
	mux.Handle("/api/v1/species", httpapi.NewCatalogueRoutes(guard, catalogue))
	mux.Handle("/api/v1/species/", httpapi.NewCatalogueRoutes(guard, catalogue))
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &rig{
		t: t, server: server, keys: key, kid: kidOf(key),
		pool: conn, redis: rdb,
	}
}

// jwksDocument publishes the public half only.
//
// A verifier that can read the private exponent does not need the signature, and
// a document that carried one would be a private key published under the polite
// name of a public key set.
func jwksDocument(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	pub := key.PublicKey.N
	doc := map[string]any{"keys": []any{map[string]any{
		"kid": kidOf(key), "kty": "RSA", "alg": "RS256", "use": "sig",
		"n": base64.RawURLEncoding.EncodeToString(pub.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.PublicKey.E)).Bytes()),
	}}}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal jwks: %v", err)
	}
	return string(out)
}

// kidOf derives the key identifier from the modulus, exactly as Chisimba does.
//
// If this were a random value, a mismatch between the two implementations would
// read as a signature failure rather than as the naming disagreement it would be.
func kidOf(key *rsa.PrivateKey) string {
	sum := sha256.Sum256(key.PublicKey.N.Bytes())
	return fmt.Sprintf("%x", sum[:8])
}

// token mints a token in the shape Chisimba emits.
//
// The claim set is written out rather than derived from a struct, because the
// two shapes that matter here are exactly the ones a struct would hide: scope is
// an ARRAY, which is what the validator requires and what OAuth does not, and
// ctx is a single string which may legitimately be empty. A harness that built
// claims reflectively would agree with the validator by construction and so could
// not catch the two disagreeing.
func (r *rig) token(epoch int64, ctx string, scopes ...string) string {
	r.t.Helper()
	if len(scopes) == 0 {
		scopes = []string{scope}
	}
	now := time.Now()
	header := map[string]any{"alg": "RS256", "typ": "JWT", "kid": r.kid}
	claims := map[string]any{
		"sub": "42", "scope": scopes, "ctx": ctx, "ctxs": []string{ctx},
		"roles": []string{scope}, "epoch": epoch,
		"ver": "harness-token", "iss": issuer, "aud": audience,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	signing := encodeSegment(header) + "." + encodeSegment(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, r.keys, 5, sum[:])
	if err != nil {
		r.t.Fatalf("sign: %v", err)
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func encodeSegment(v any) string {
	out, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(out)
}

// push sends a batch and returns the status and the raw body.
//
// The raw body matters: one of the faults this suite exists to catch was a
// handler writing a second response after the first, which a client sees as one
// truncated message and a status-code assertion sees as perfectly correct.
func (r *rig) push(tok string, body any) (int, []byte) {
	r.t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		r.t.Fatalf("marshal push: %v", err)
	}
	req, err := http.NewRequest("POST", r.server.URL+"/api/v1/sync/push",
		strings.NewReader(string(encoded)))
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return r.do(req)
}

// pushRaw sends a body with whatever content type it is given.
func (r *rig) pushRaw(tok, contentType, body string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest("POST", r.server.URL+"/api/v1/sync/push", strings.NewReader(body))
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return r.do(req)
}

func (r *rig) pull(tok, cursor string) (int, []byte) {
	r.t.Helper()
	req, err := http.NewRequest("POST", r.server.URL+"/api/v1/sync/pull",
		strings.NewReader(`{"cursor":`+strconvQuote(cursor)+`}`))
	if err != nil {
		r.t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	return r.do(req)
}

func strconvQuote(s string) string {
	out, _ := json.Marshal(s)
	return string(out)
}

func (r *rig) do(req *http.Request) (int, []byte) {
	r.t.Helper()
	resp, err := r.server.Client().Do(req)
	if err != nil {
		r.t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		r.t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// sighting is one operation, with a fresh identity each time so a batch never
// accidentally collides with an earlier one.
// sighting is one operation, with a fresh identity each time so a batch never
// accidentally collides with an earlier one.
//
// The observation fields live under "payload", which is where the contract puts
// them. Flattening them is the mistake this harness made first, and it was
// caught by DisallowUnknownFields — a refusal with a message about field names,
// for a body that looked perfectly reasonable.
// ensureOuting provisions a drive in a context and returns its id.
//
// The sighting's drive is a foreign key, so a batch naming a drive that does not
// exist is refused by PostgreSQL rather than by this service — and that refusal
// arrives as a 500, which is one of the two genuine findings this harness has
// produced. Provisioning here keeps it out of the tests that are about
// something else, and keeps the fixture honest: a real drive in the real context,
// rather than a drive borrowed from another one.
func (r *rig) ensureOuting(ctx string) string {
	r.t.Helper()
	// Derived from the context, so the same context always gets the same outing
	// and repeated runs do not accumulate rows.
	sum := sha256.Sum256([]byte("outing:" + ctx))
	h := hex(sum[:16])
	// The version and variant nibbles are overwritten in place rather than
	// skipped. Slicing around them instead drops a character, and a uuid one
	// character short is rejected by the column with a syntax error that names
	// the value and nothing about the shape.
	h = h[:12] + "4" + h[13:16] + "8" + h[17:]
	id := h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
	_, err := r.pool.Exec(context.Background(),
		`insert into outing (id, context_code, guide_id) values ($1, $2, 'guide-1')
		 on conflict (id) do nothing`, id, ctx)
	if err != nil {
		r.t.Fatalf("provisioning an outing in %s: %v", ctx, err)
	}
	return id
}

func (r *rig) logBookEntry(ctx string) map[string]any {
	return map[string]any{
		"operation_id":  newID(),
		"entity":        "log_book_entry",
		"kind":          "create",
		"entity_id":     newID(),
		"base_revision": nil,
		"captured_at":   "2026-10-05T06:14:00Z",
		"recorded_at":   "2026-10-05T18:02:00Z",
		"payload": map[string]any{
			"outing_id":    r.ensureOuting(ctx),
			"species_code": "LEOP",
			"count":        2,
			"notes":        "two males moving east",
			"location":     map[string]any{"type": "Point", "coordinates": []float64{36.8219, -1.2921}},
		},
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0x0f])
	}
	return string(out)
}

// testLogger keeps a fault visible in the test output.
//
// The service tells a client a sentence and the operator the reason. A harness
// that discarded the second half would let the exact fault this suite exists to
// catch pass by unnoticed, because the client-visible half is identical either
// way.
type testLogger struct{ t *testing.T }

func (l testLogger) Refused(r *http.Request, reason string, err error) {
	l.t.Logf("refused: reason=%s remote=%s error=%v", reason, remote(r), err)
}

func remote(r *http.Request) string {
	if r == nil {
		return "-"
	}
	return r.RemoteAddr
}

// assertRow checks a row in the database directly.
//
// The push response reports success for anything the service was willing to
// apply, so the only place a context can be checked is the column it landed in.
func assertRow(t *testing.T, r *rig, entityID, wantContext, wantCreator string) {
	t.Helper()
	var gotContext, gotCreator string
	err := r.pool.QueryRow(context.Background(),
		`select context_code, created_by from log_book_entry where id = $1`, entityID).
		Scan(&gotContext, &gotCreator)
	if err != nil {
		t.Fatalf("no sighting for %s: %v", entityID, err)
	}
	if gotContext != wantContext {
		t.Errorf("sighting landed in %q, want the token's %q", gotContext, wantContext)
	}
	if gotCreator != wantCreator {
		t.Errorf("created_by = %q, want the raw token subject %q", gotCreator, wantCreator)
	}
}

// timestamps reads both columns, so a conflation cannot pass by being equal to
// whichever one was expected.
func (r *rig) timestamps(t *testing.T, entityID string) (captured, recorded time.Time) {
	t.Helper()
	err := r.pool.QueryRow(context.Background(),
		`select captured_at, recorded_at from log_book_entry where id = $1`, entityID).
		Scan(&captured, &recorded)
	if err != nil {
		t.Fatalf("no sighting for %s: %v", entityID, err)
	}
	return captured, recorded
}

func (r *rig) countRows(t *testing.T, entityID string) int {
	t.Helper()
	var n int
	if err := r.pool.QueryRow(context.Background(),
		`select count(*) from log_book_entry where id = $1`, entityID).Scan(&n); err != nil {
		t.Fatalf("count for %s: %v", entityID, err)
	}
	return n
}
