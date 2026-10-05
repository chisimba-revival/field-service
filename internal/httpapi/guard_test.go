package httpapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakePrincipal stands in for a verified token.
//
// The guard's Principal is an interface so this package can be tested without
// minting RSA keys, which is the point: the interesting failures here are the
// ordering of the checks and what reaches the handler, not the signature.
type fakePrincipal struct {
	sub    string
	scopes []string
	active string
	grants []string
	epoch  int64
	issued time.Time
	noIat  bool
}

func (f fakePrincipal) Subject() string       { return f.sub }
func (f fakePrincipal) Scopes() []string      { return f.scopes }
func (f fakePrincipal) ActiveContext() string { return f.active }
func (f fakePrincipal) Grants() []string      { return f.grants }
func (f fakePrincipal) Epoch() int64          { return f.epoch }
func (f fakePrincipal) HasScope(s string) bool {
	for _, held := range f.scopes {
		if held == s {
			return true
		}
	}
	return false
}

func (f fakePrincipal) IssuedAt() (time.Time, bool) {
	if f.noIat {
		return time.Time{}, false
	}
	return f.issued, true
}

// goodToken is a caller who should be admitted. Each test breaks exactly one
// thing about it, so a failure names the check that stopped working.
func goodToken() fakePrincipal {
	return fakePrincipal{
		sub:    "user-1",
		scopes: []string{"field:read"},
		active: "north-reserve",
		grants: []string{"north-reserve", "south-reserve"},
		epoch:  3,
		issued: time.Now().Add(-time.Minute),
	}
}

type fakeVerifier struct {
	principal fakePrincipal
	err       error
	asked     int
}

func (f *fakeVerifier) Validate(string) (any, error) {
	f.asked++
	if f.err != nil {
		return nil, f.err
	}
	return f.principal, nil
}

type fakeChecker struct {
	revoked bool
	err     error
	// asked records what it was told, so the epoch and issue time can be checked
	// rather than assumed to have arrived.
	sawSubject string
	sawIssued  time.Time
	sawEpoch   int64
	calls      int
}

func (f *fakeChecker) Revoked(_ context.Context, subject string, issuedAt time.Time, epoch int64) (bool, error) {
	f.calls++
	f.sawSubject, f.sawIssued, f.sawEpoch = subject, issuedAt, epoch
	return f.revoked, f.err
}

// recordingSession records the grants it was handed and whether the handler ran.
type recordingSession struct {
	mu           sync.Mutex
	callerID     string
	grants       []string
	handlerRan   bool
	returnErr    error
	transactions int
}

// Querier is here so this fake still satisfies Session after the interface
// gained the method. It records the same facts InTx does and then hands fn
// something that cannot query anything — a pull needs the real handle, and
// pretending otherwise here would let a test pass against a fake that answers
// every read.
func (s *recordingSession) Querier(ctx context.Context, callerID string, grants []string,
	fn func(Querier) error) error {
	if err := s.InTx(ctx, callerID, grants, func(context.Context) error { return nil }); err != nil {
		return err
	}
	return fn(nil)
}

func (s *recordingSession) InTx(_ context.Context, callerID string, grants []string,
	fn func(context.Context) error) error {
	s.callerID = callerID
	s.mu.Lock()
	s.transactions++
	s.grants = append([]string(nil), grants...)
	s.mu.Unlock()
	if s.returnErr != nil {
		return s.returnErr
	}
	if err := fn(context.Background()); err != nil {
		return err
	}
	s.mu.Lock()
	s.handlerRan = true
	s.mu.Unlock()
	return nil
}

type recordingLog struct {
	mu      sync.Mutex
	reasons []string
}

func (l *recordingLog) Refused(_ *http.Request, reason string, _ error) {
	l.mu.Lock()
	l.reasons = append(l.reasons, reason)
	l.mu.Unlock()
}

func (l *recordingLog) last() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.reasons) == 0 {
		return ""
	}
	return l.reasons[len(l.reasons)-1]
}

// harness builds a guard over fakes and returns the pieces for inspection.
func harness(t *testing.T, p fakePrincipal, vErr error, c *fakeChecker) (
	*Guard, *recordingSession, *recordingLog) {
	t.Helper()
	v := &fakeVerifier{principal: p, err: vErr}
	s := &recordingSession{}
	log := &recordingLog{}
	return NewGuard(v, c, s, log, "field:read"), s, log
}

// A caller who passes every check reaches the handler, and the database is
// handed that caller's grants.
func TestAnAuthorisedCallerReachesTheHandler(t *testing.T) {
	p := goodToken()
	g, sess, log := harness(t, p, nil, &fakeChecker{})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
	r.Header.Set("Authorization", "Bearer token-value")
	w := httptest.NewRecorder()

	g.Serve(func(_ http.ResponseWriter, _ *http.Request, c Caller) error {
		if c.Principal.Subject() != "user-1" {
			t.Errorf("handler saw subject %q", c.Principal.Subject())
		}
		w.WriteHeader(http.StatusOK)
		return nil
	}).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status %d, want 200 (refusals: %v)", w.Code, log.reasons)
	}
	if !sess.handlerRan {
		t.Error("the handler did not run")
	}
	if len(sess.grants) != 2 {
		t.Errorf("database was handed grants %v, want both contexts", sess.grants)
	}
}

// The read boundary is the grant set. If the active context were used instead,
// this caller would lose sight of the reserve they are not currently working in.
func TestTheDatabaseIsHandedTheGrantSetNotTheActiveContext(t *testing.T) {
	p := goodToken()
	p.active = "south-reserve"
	g, sess, _ := harness(t, p, nil, &fakeChecker{})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
	r.Header.Set("Authorization", "Bearer token-value")
	g.Serve(func(http.ResponseWriter, *http.Request, Caller) error { return nil }).
		ServeHTTP(httptest.NewRecorder(), r)

	if len(sess.grants) != 2 {
		t.Fatalf("grants %v — the active context was used as the read boundary", sess.grants)
	}
	for _, got := range sess.grants {
		if got == "south-reserve" && len(sess.grants) == 1 {
			t.Error("only the active context reached the database")
		}
	}
}

// Every route to the handler must be closed. Each case breaks one thing and
// asserts the handler did not run, because "the response was 401" alone would
// not catch a handler that ran and then refused.
func TestTheHandlerIsUnreachableUnlessEveryCheckPasses(t *testing.T) {
	// The header is part of the case rather than set by the loop below. Setting
	// it once for every row made the two "no token" rows present a perfectly good
	// token, so they passed for the wrong reason and would have gone on passing.
	good := "Bearer token-value"
	cases := []struct {
		name        string
		header      string
		principal   func() fakePrincipal
		verifierErr error
		checker     func() *fakeChecker
		wantReason  string
	}{
		{
			name:       "no authorization header at all",
			header:     "",
			principal:  goodToken,
			checker:    func() *fakeChecker { return &fakeChecker{} },
			wantReason: reasonNoHeader,
		},
		{
			name:       "a header that is not a bearer token",
			header:     "Basic dXNlcjpwYXNz",
			principal:  goodToken,
			checker:    func() *fakeChecker { return &fakeChecker{} },
			wantReason: reasonNoHeader,
		},
		{
			name:        "a token that does not validate",
			header:      good,
			principal:   goodToken,
			verifierErr: errors.New("signature does not verify"),
			checker:     func() *fakeChecker { return &fakeChecker{} },
			wantReason:  reasonInvalid,
		},
		{
			name:   "a token with no subject",
			header: good,
			principal: func() fakePrincipal {
				p := goodToken()
				p.sub = ""
				return p
			},
			checker:    func() *fakeChecker { return &fakeChecker{} },
			wantReason: reasonNoSubject,
		},
		{
			name:   "a token with no issue time",
			header: good,
			principal: func() fakePrincipal {
				p := goodToken()
				p.noIat = true
				return p
			},
			checker: func() *fakeChecker { return &fakeChecker{} },
			// Without iat the timestamp half of the revocation rule cannot be
			// evaluated, and the epoch alone would readmit a user who was
			// deactivated and reactivated.
			wantReason: reasonRevoked,
		},
		{
			name:       "a token on the denylist",
			header:     good,
			principal:  goodToken,
			checker:    func() *fakeChecker { return &fakeChecker{revoked: true} },
			wantReason: reasonRevoked,
		},
		{
			name:       "a denylist that cannot be read",
			header:     good,
			principal:  goodToken,
			checker:    func() *fakeChecker { return &fakeChecker{err: errors.New("connection refused")} },
			wantReason: reasonDenylistDown,
		},
		{
			name:   "a token without the scope this route needs",
			header: good,
			principal: func() fakePrincipal {
				p := goodToken()
				p.scopes = []string{"field:write"}
				return p
			},
			checker:    func() *fakeChecker { return &fakeChecker{} },
			wantReason: reasonScope,
		},
		{
			name:   "a token carrying no context grants",
			header: good,
			principal: func() fakePrincipal {
				p := goodToken()
				p.grants = nil
				return p
			},
			checker: func() *fakeChecker { return &fakeChecker{} },
			// A caller with no contexts can read nothing, so there is no point
			// starting a transaction for one.
			wantReason: reasonNoGrants,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checker := c.checker()
			g, sess, log := harness(t, c.principal(), c.verifierErr, checker)

			r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
			if c.header != "" {
				r.Header.Set("Authorization", c.header)
			}
			w := httptest.NewRecorder()

			g.Serve(func(_ http.ResponseWriter, _ *http.Request, _ Caller) error {
				t.Error("the handler ran for a request that should have been refused")
				return nil
			}).ServeHTTP(w, r)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("status %d, want 401", w.Code)
			}
			if sess.handlerRan {
				t.Error("the session ran a transaction for a refused request")
			}
			if log.last() != c.wantReason {
				t.Errorf("logged %q, want %q", log.last(), c.wantReason)
			}
		})
	}
}

// Every refusal must look the same from outside, or a client can use the
// difference to discover which tokens exist.
func TestEveryRefusalIsIndistinguishableFromOutside(t *testing.T) {
	var bodies, statuses []string
	var wwwAuth []string

	refuseCases := map[string]func() fakePrincipal{
		"invalid token":      func() fakePrincipal { return goodToken() },
		"revoked":            func() fakePrincipal { return goodToken() },
		"no scope":           func() fakePrincipal { p := goodToken(); p.scopes = nil; return p },
		"no grants":          func() fakePrincipal { p := goodToken(); p.grants = nil; return p },
		"no subject":         func() fakePrincipal { p := goodToken(); p.sub = ""; return p },
		"denylist unreadabl": func() fakePrincipal { return goodToken() },
	}
	checkers := map[string]*fakeChecker{
		"invalid token":      {},
		"revoked":            {revoked: true},
		"no scope":           {},
		"no grants":          {},
		"no subject":         {},
		"denylist unreadabl": {err: errors.New("down")},
	}
	verifierErrs := map[string]error{
		"invalid token":      errors.New("bad signature"),
		"denylist unreadabl": nil,
	}

	for name, p := range refuseCases {
		vErr := verifierErrs[name]
		g, _, _ := harness(t, p(), vErr, checkers[name])
		r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
		r.Header.Set("Authorization", "Bearer token-value")
		w := httptest.NewRecorder()
		g.Serve(func(http.ResponseWriter, *http.Request, Caller) error { return nil }).
			ServeHTTP(w, r)
		statuses = append(statuses, name+":"+strconv.Itoa(w.Code))
		bodies = append(bodies, w.Body.String())
		wwwAuth = append(wwwAuth, w.Header().Get("WWW-Authenticate"))
	}

	first := bodies[0]
	for i, b := range bodies {
		if b != first {
			t.Errorf("%v answered %q where another refusal answered %q",
				statuses[i], strings.TrimSpace(b), strings.TrimSpace(first))
		}
	}
	for i, h := range wwwAuth {
		if h != wwwAuth[0] {
			t.Errorf("%v sent a different WWW-Authenticate", statuses[i])
		}
	}
}

// The checker must be given what it needs to decide, or the two rules — epoch
// and issue time — are being applied to defaults.
func TestTheDenylistIsGivenTheTokensOwnEpochAndIssueTime(t *testing.T) {
	p := goodToken()
	p.epoch = 42
	p.issued = time.Date(2026, 10, 4, 9, 0, 0, 0, time.UTC)
	checker := &fakeChecker{}
	g, _, _ := harness(t, p, nil, checker)

	r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
	r.Header.Set("Authorization", "Bearer token-value")
	g.Serve(func(http.ResponseWriter, *http.Request, Caller) error { return nil }).
		ServeHTTP(httptest.NewRecorder(), r)

	if checker.sawSubject != "user-1" {
		t.Errorf("denylist was asked about %q", checker.sawSubject)
	}
	if checker.sawEpoch != 42 {
		t.Errorf("denylist was given epoch %d, want 42", checker.sawEpoch)
	}
	if !checker.sawIssued.Equal(p.issued) {
		t.Errorf("denylist was given issue time %v, want %v", checker.sawIssued, p.issued)
	}
}

// A handler that fails must not be reported as an authorisation problem, and the
// transaction must not be left looking like it succeeded.
func TestAHandlerFailureIsNotAnAuthorisationProblem(t *testing.T) {
	g, sess, log := harness(t, goodToken(), nil, &fakeChecker{})

	r := httptest.NewRequest(http.MethodGet, "/api/v1/drives", nil)
	r.Header.Set("Authorization", "Bearer token-value")
	w := httptest.NewRecorder()
	g.Serve(func(http.ResponseWriter, *http.Request, Caller) error {
		return errors.New("the database is unreachable")
	}).ServeHTTP(w, r)

	if log.last() != reasonHandlerFailed {
		t.Errorf("logged %q — a handler failure will be read as an authorisation "+
			"problem if it is reported as one", log.last())
	}
	if sess.handlerRan {
		t.Error("the session reported success for a handler that failed")
	}
}

func TestBearerHeaderParsing(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"a well formed header":      {"Bearer abc", "abc", true},
		"lowercase scheme":          {"bearer abc", "abc", true},
		"extra whitespace":          {"Bearer   abc  ", "abc", true},
		"nothing at all":            {"", "", false},
		"scheme only":               {"Bearer", "", false},
		"scheme with nothing after": {"Bearer ", "", false},
		"a different scheme":        {"Basic abc", "", false},
		"bearer as a substring":     {"NotBearer abc", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := bearer(c.header)
			if ok != c.ok || got != c.want {
				t.Errorf("bearer(%q) = %q,%v want %q,%v", c.header, got, ok, c.want, c.ok)
			}
		})
	}
}
