package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"field-service/internal/pull"
	"field-service/internal/push"
)

// What these prove is not that push and pull work — their own packages test that
// against the database. It is that a request cannot reach either of them without
// a caller, and that what crosses the boundary is read the way the contract
// says rather than the way that is convenient to parse.

// The guard's own fakes are reused rather than redeclared: goodToken is a
// caller who should be admitted, and a second copy here would be a second thing
// to keep in step with the guard's expectations.

type allowChecker struct{}

func (allowChecker) Revoked(context.Context, string, time.Time, int64) (bool, error) {
	return false, nil
}

type quietLog struct{ reasons []string }

func (q *quietLog) Refused(_ *http.Request, reason string, _ error) {
	q.reasons = append(q.reasons, reason)
}

// aWorkingSession stands in for a session that works.
//
// It has to invoke the callback, which is the point. The guard runs every handler
// inside session.InTx, so a fake that returns an error makes the guard answer 500
// and the handler never runs — and then a test about field names measures the
// fake. That is the fifth time this session a fake has failed more interestingly
// than the code, and the shape is always the same: the fake is louder than the
// thing it stands in for.
type aWorkingSession struct{}

func (aWorkingSession) InTx(ctx context.Context, _ string, _ []string, fn func(context.Context) error) error {
	return fn(ctx)
}
func (aWorkingSession) Querier(ctx context.Context, _ string, _ []string, fn func(Querier) error) error {
	return fn(nil)
}

// aRoutes builds the router with a push service that must never be reached, so
// any test that passes proves the guard stopped the request first.
func aRoutes(pusher PushService, puller func(context.Context, string, string, Querier) (pull.Page, error)) http.Handler {
	v := &fakeVerifier{principal: goodToken()}
	g := NewGuard(v, allowChecker{}, aWorkingSession{}, &quietLog{}, goodScope)
	return NewSyncRoutes(g, &Sync{Push: pusher, Pull: puller, Session: aWorkingSession{}, Log: &quietLog{}})
}

// goodScope is the scope goodToken() carries. Kept as a name so the routes and
// the guard's own tests cannot disagree about which scope a valid caller holds.
const goodScope = "field:read"

// post sends a request as an admitted caller, so a test measuring the body or
// the field names is not measuring the guard instead.
func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer token-value")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// postAnon sends the same request with no credentials at all.
func postAnon(h http.Handler, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// No route is reachable without a token, whatever it claims to carry.
//
// The pusher here has no store, so if a request reached it the batch would fail
// for the wrong reason and a status of 500 could be mistaken for a working
// route. A 401 with no token is the only honest answer at this boundary.
func TestNoSyncRouteIsReachableWithoutAToken(t *testing.T) {
	h := aRoutes(nil, nil)
	for _, path := range []string{"/api/v1/sync/push", "/api/v1/sync/pull"} {
		got := postAnon(h, path, `{"operations":[]}`)
		if got.Code != http.StatusUnauthorized {
			t.Errorf("%s with no token: %d, want 401. A route that answers "+
				"without a token is a route nobody reasoned about.", path, got.Code)
		}
	}
}

// A path that is not registered is not a route. The distinction matters: an
// unrouted path must not be reported as authorised, because a client reading the
// status would conclude the service understood it.
func TestAnUnroutedPathIsNotFoundRatherThanAuthorised(t *testing.T) {
	h := aRoutes(nil, nil)
	got := postAnon(h, "/api/v1/sync/anything-else", `{}`)
	if got.Code != http.StatusNotFound {
		t.Errorf("unrouted path: %d, want 404", got.Code)
	}
}

// An unrouted path must not be reachable even with a valid token. Otherwise a
// client would read the answer as the service understanding the request.
func TestAnUnroutedPathIsNotReachableWithAValidTokenEither(t *testing.T) {
	h := aRoutes(nil, nil)
	if got := post(h, "/api/v1/sync/anything-else", `{}`); got.Code != http.StatusNotFound {
		t.Errorf("unrouted path with a valid token: %d, want 404", got.Code)
	}
}

func decodeBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q is not JSON: %v", w.Body.String(), err)
	}
	return m
}

func TestARefusalIsIdenticalWhateverTheReason(t *testing.T) {
	g := NewGuard(&fakeVerifier{principal: goodToken(), err: errors.New("signature does not verify")},
		allowChecker{}, aWorkingSession{}, &quietLog{}, "sync:write")
	h := NewSyncRoutes(g, &Sync{Session: aWorkingSession{}})

	got := post(h, "/api/v1/sync/push", `{"operations":[]}`)
	if got.Code != http.StatusUnauthorized {
		t.Fatalf("%d", got.Code)
	}
	if strings.Contains(got.Body.String(), "signature") {
		t.Error("the response said which check failed. A caller that can tell a " +
			"bad signature from a missing grant can use that to find out which " +
			"tokens exist.")
	}
	if got.Header().Get("WWW-Authenticate") == "" {
		t.Error("no challenge header on a 401")
	}
}

// A bounded body, so a large request is refused rather than read into memory.
func TestABodyLargerThanTheBoundIsRefused(t *testing.T) {
	g := NewGuard(&fakeVerifier{principal: goodToken()}, allowChecker{}, aWorkingSession{}, &quietLog{}, goodScope)
	// The handler is reached by calling the guarded wrapper with a caller, so
	// this measures the body limit rather than the guard.
	h := g.Serve(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var body pushRequest
		return decode(w, r, &body)
	})

	big := `{"operations":[{"payload":{"notes":"` + strings.Repeat("x", MaxBodyBytes+64) + `"}}]}`
	got := post(h, "/api/v1/sync/push", big)
	if got.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("%d, want 413", got.Code)
	}
}

// An unknown field is refused at the boundary.
func TestAnUnknownFieldIsRefusedAtTheBoundary(t *testing.T) {
	g := NewGuard(&fakeVerifier{principal: goodToken()}, allowChecker{}, aWorkingSession{}, &quietLog{}, goodScope)
	h := g.Serve(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		var body pushRequest
		return decode(w, r, &body)
	})
	got := post(h, "/api/v1/sync/push",
		`{"operations":[{"operation_id":"x","entity":"sighting","kind":"create","entity_id":"e","count":2}]}`)
	if got.Code != http.StatusBadRequest {
		t.Fatalf("%d, want 400. count belongs inside payload, and a client that "+
			"puts it at the top level would otherwise have it dropped silently.", got.Code)
	}
	if m := decodeBody(t, got); m["error"] != "unreadable_body" {
		t.Errorf("error %v", m["error"])
	}
}

// recordingPush captures the Caller the real handler built.
type recordingPush struct{ seen push.Caller }

func (r *recordingPush) Push(_ context.Context, caller push.Caller, _ []push.Operation) ([]push.Result, error) {
	r.seen = caller
	return []push.Result{{OperationID: "x", Outcome: push.OutcomeApplied}}, nil
}

// The write context is the token's active context, and nothing in the body can
// redirect a write.
//
// This drives the real route and observes the Caller the real handler built. The
// first version of this test used a probe handler that constructed the Caller
// itself, which proved the probe correct and left the handler unguarded — reading
// the context out of the body in the handler produced zero test failures. The
// service is an interface now so this can be observed at all.
func TestTheWriteContextCannotBeSuppliedByTheBody(t *testing.T) {
	pusher := &recordingPush{}
	h := aRoutes(pusher, nil)

	body := `{"operations":[{"operation_id":"x","entity":"sighting","kind":"create","entity_id":"e",
	"payload":{"context_code":"south-reserve","contextCode":"south-reserve","ctx":"south-reserve"}}]}`
	if got := post(h, "/api/v1/sync/push", body); got.Code != http.StatusOK {
		t.Fatalf("%d, want 200: %s", got.Code, got.Body.String())
	}
	if pusher.seen.WriteContext != "north-reserve" {
		t.Errorf("write context %q. The token says north-reserve and the body "+
			"asked for south-reserve three different ways.", pusher.seen.WriteContext)
	}
	if pusher.seen.ID != "user-1" {
		t.Errorf("caller id %q; rule 2 says the Chisimba id unchanged", pusher.seen.ID)
	}
}

// A fault is reported to the log and described to the client in words.
func TestAFaultIsLoggedAndNotDescribedToTheClient(t *testing.T) {
	log := &quietLog{}
	s := &Sync{Session: aWorkingSession{}, Log: log}
	w := httptest.NewRecorder()
	// A real request, because the logger is handed one. The first version of
	// this test passed nil and would have panicked in the adapter it was
	// exercising — which is exactly how the nil reached production unnoticed.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync/push", nil)
	s.internal(w, req, errors.New(`pq: duplicate key value violates unique constraint "sighting_pkey"`))

	if got := w.Code; got != http.StatusInternalServerError {
		t.Errorf("%d, want 500", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "sighting_pkey") || strings.Contains(body, "pq:") {
		t.Errorf("the response carried %q. A Postgres error names the table and "+
			"the constraint, which is the schema handed to whoever can read it.", body)
	}
	if len(log.reasons) != 1 || log.reasons[0] != "handler_failed" {
		t.Errorf("logged %v, want one handler_failed", log.reasons)
	}
}

// Responses are uncacheable: a device behind a shared proxy must not be served
// another device's page of changes.
func TestResponsesAreNotCacheable(t *testing.T) {
	w := httptest.NewRecorder()
	if err := writeJSON(w, http.StatusOK, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if got := w.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
		t.Errorf("Content-Type %q", ct)
	}
}
