package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The two doors.
//
// A service caller exists because mentors verify from a Chisimba module, and a
// module has no user's context to present. That created a route that cannot work
// like the others, and the risk is not in the new route — it is in the existing
// ones. A token that satisfies a relaxed check is a token that satisfies a strict
// one somewhere else.
//
// So every assertion below is about which token is refused by which door. A test
// that only checked "a service token works on the service route" would pass with
// the two doors wired the wrong way round.

// verifyScope is what the service door is built with in these tests. It is
// deliberately not "field:read", which is what the person door requires: one
// token must not be good for both.
const verifyScope = "field:verify"

func aServiceToken() fakePrincipal {
	return fakePrincipal{
		typ:    "service",
		sub:    "svc:mentor-module",
		scopes: []string{verifyScope},
		issued: time.Now().Add(-time.Minute),
	}
}

func aPersonToken() fakePrincipal {
	p := goodToken()
	p.scopes = []string{"field:read"}
	return p
}

// aServiceDoor builds the service door over the same fakes the person-door tests
// already use.
//
// This started with fakes of its own and had to be rewritten: the duplicate that
// mattered was the session. A fake of my own could behave differently from the one
// the rest of the suite runs on, and then a disagreement would have looked like a
// defect in the guard rather than a defect in the fake.
// authorised builds a request carrying a bearer token.
//
// A helper rather than three lines at each call site, because the first version of
// this file set the header on the person-route tests and forgot it on the
// service-route ones, and every service test then failed on "no bearer token
// presented" — which reads like the door refusing a service for the wrong reason,
// and is exactly the kind of failure that invites a fix to the guard instead of to
// the test.
func authorised(method, path string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Authorization", "Bearer token-value")
	return r
}

func aServiceDoor(t *testing.T, p fakePrincipal, revoked bool) (*Guard, *recordingLog) {
	t.Helper()
	g, _, log := harness(t, p, nil, &fakeChecker{revoked: revoked})
	return g, log
}

func TestAServiceTokenIsRefusedByAPersonRoute(t *testing.T) {
	g, log := aServiceDoor(t, aServiceToken(), false)

	reached := false
	h := g.Serve(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		reached = true
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/sync/push"))

	if reached {
		t.Fatal("the handler ran: a service token was admitted to a route that serves a person")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if got := log.last(); got != reasonServiceOnUser {
		t.Fatalf("refused for %q, want %q", got, reasonServiceOnUser)
	}
}

// A token with no type claim is a person's token. Every existing test in this
// package presents a principal with no type, so if this reading changed, the grant
// and scope rules would quietly be applying to service tokens as well.
func TestATokenWithNoTypeIsAPersonToken(t *testing.T) {
	for _, typ := range []string{"", "access", "Access", "service ", "refresh", "SERVICE"} {
		var p Principal = fakePrincipal{typ: typ, sub: "42", grants: []string{"north"}}
		if (Caller{Principal: p}).IsService() {
			t.Fatalf("type %q was treated as a service", typ)
		}
	}
	var p Principal = fakePrincipal{typ: "service", sub: "42", grants: []string{"north"}}
	if !(Caller{Principal: p}).IsService() {
		t.Fatal(`type "service" was not treated as a service`)
	}
}

func TestAPersonTokenIsRefusedByAServiceRoute(t *testing.T) {
	g, log := aServiceDoor(t, aPersonToken(), false)

	reached := false
	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		reached = true
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if reached {
		t.Fatal("the handler ran: a person's token reached a service route")
	}
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status %d, want 401", rec.Code)
	}
	if got := log.last(); got != reasonNotService {
		t.Fatalf("refused for %q, want %q", got, reasonNotService)
	}
}

// A service token whose scopes are not the route's is refused, even though it
// carries plenty. Otherwise a service credential that could also push field data
// would be good for verification, and the two abilities would share one secret.
func TestAServiceRouteInsistsOnItsOwnScope(t *testing.T) {
	p := aServiceToken()
	p.scopes = []string{"field:read", "field:write", "field:push"}
	g, log := aServiceDoor(t, p, false)

	reached := false
	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		reached = true
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if reached {
		t.Fatal("a service token carrying the wrong scopes reached a service route")
	}
	if got := log.last(); got != reasonScope {
		t.Fatalf("refused for %q, want %q", got, reasonScope)
	}
}

// The service door needs no grant set and must not insist on one: that is the
// reason it exists. Asserted because it is the one check deliberately dropped, and
// a future tidy-up that restored it would make the door useless without failing
// any test that only ever presents a person's token.
func TestAServiceTokenNeedsNoGrantsAndNoActiveContext(t *testing.T) {
	g, log := aServiceDoor(t, aServiceToken(), false)

	var got Caller
	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		got = c
		w.WriteHeader(http.StatusOK)
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, refused for %q", rec.Code, log.last())
	}
	if len(got.Grants()) != 0 || got.ActiveContext() != "" {
		t.Fatalf("the service caller was handed grants %v and context %q; a service has neither",
			got.Grants(), got.ActiveContext())
	}
}

// A service is not exempt from being switched off.
func TestAServiceTokenIsStillCheckedAgainstTheDenylist(t *testing.T) {
	g, log := aServiceDoor(t, aServiceToken(), true)

	reached := false
	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		reached = true
		return nil
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if reached {
		t.Fatal("a revoked service token reached the handler")
	}
	if got := log.last(); got != reasonRevoked {
		t.Fatalf("refused for %q, want %q", got, reasonRevoked)
	}
}

// The service door writes one response. A handler that answers and then returns
// ErrAnswered must not be followed by a second write — the fault that cost a real
// client a truncated body before it was found on the ordinary door.
func TestAServiceRouteAddsNoSecondResponse(t *testing.T) {
	g, _ := aServiceDoor(t, aServiceToken(), false)

	var writes int
	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte(`{"error":"refused"}`))
		writes++
		return ErrAnswered
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if rec.Code != http.StatusTeapot {
		t.Fatalf("status %d, want the status the handler chose", rec.Code)
	}
	if writes != 1 {
		t.Fatalf("the handler wrote %d times", writes)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type %q", ct)
	}
}

// A handler that fails on a service route is an internal fault and is reported as
// one. Reporting it as an authorisation problem puts "unauthorised" in the logs
// about a request that was permitted, which is how a real fault gets read as a
// credential problem for a long time.
func TestAServiceRouteReportsAFaultAsAFault(t *testing.T) {
	g, log := aServiceDoor(t, aServiceToken(), false)

	h := g.ServeService(verifyScope)(func(w http.ResponseWriter, r *http.Request, c Caller) error {
		return errors.New("the database fell over")
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, authorised(http.MethodPost, "/api/v1/logbook/x/verification"))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// The reason goes to the log and not to the client: a database error names
	// tables, columns and constraints, which is a schema handed to whoever reads it.
	if strings.Contains(rec.Body.String(), "database fell over") {
		t.Fatal("the fault was described to the client")
	}
	if got := log.last(); got != reasonHandlerFailed {
		t.Fatalf("logged %q, want %q", got, reasonHandlerFailed)
	}
}
