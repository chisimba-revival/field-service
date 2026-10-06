// Package httpapi is where a request stops being anonymous.
//
// Three things are true of every request that reaches a handler here, and they
// are true structurally rather than by convention:
//
//   - The caller is known. A Handler takes a Caller, so it cannot be invoked
//     without one and cannot forget to check who is calling. Validating a
//     signature and then letting each handler decide what to do produces
//     handlers that each remember to check, and a handler that forgets is a data
//     breach rather than a failing test.
//   - The database is told what the caller may read. Every handler runs inside a
//     transaction carrying the caller's context grants, and the grants are the
//     token's grant set rather than its active context: using the active context
//     as the read boundary would hide a guide's northern-reserve drives while
//     they happen to be working in the southern one.
//   - A refusal says nothing. Every failure writes the same status, the same body
//     and the same challenge. A client able to tell a bad signature from an
//     expired one from a missing grant could use the difference to find out which
//     tokens exist, so the detail goes to the log and never to the wire.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net/http"
	"strings"
	"time"
)

// Principal is what a verified token tells us about the request's origin.
//
// It is an interface rather than authn.Principal so that this package does not
// depend on the token package, which keeps the dependency arrow one way and
// means these tests need no RSA keys to run.
type Principal interface {
	Subject() string
	TokenType() string
	Scopes() []string
	// HasRole reports whether the token carried a named group, which is how
	// Chisimba expresses what a person is.
	//
	// It is on the interface because a read rule needs to ask, and it reads the
	// token rather than a list configured here on purpose: a group membership is
	// already the authority for what a person may reach, so answering "is this
	// person an administrator" from this service's own configuration would be a
	// second source free to disagree with the first — and a person who is an
	// administrator in Chisimba and absent from that list would be two people.
	HasRole(string) bool
	ActiveContext() string
	Grants() []string
	Epoch() int64
	IssuedAt() (time.Time, bool)
	HasScope(string) bool
}

// Caller is the verified identity of a request, handed to every handler.
//
// It is a struct wrapping Principal rather than being one, so a handler reaches
// the raw claims through a named field and cannot shadow a method with its own
// idea of one.
type Caller struct{ Principal }

// IsService reports whether this caller is a platform service rather than a
// person.
//
// Only an exact "service" counts. A token with no type claim, or with a type this
// service has never heard of, is a person's token for the purpose of every check
// here — because a person is what every existing rule is written about, and an
// unrecognised claim is not evidence of anything.
func (c Caller) IsService() bool { return c.Principal.TokenType() == "service" }

// Verifier checks a bearer token's signature and claims.
//
// It returns any rather than Caller because the concrete validator returns its
// own principal type and Go has no covariant returns. The guard therefore
// asserts the result to a Caller below, and refuses if the assertion fails —
// which is the only safe reading of a verifier that returned something this
// guard does not understand.
type Verifier interface {
	Validate(token string) (any, error)
}

// RevocationChecker reports whether a token that verified may still be used.
//
// Its signature is satisfied by revocation.Checker as written, with no adapter
// that could get the issue time's unit wrong.
type RevocationChecker interface {
	Revoked(ctx context.Context, subject string, issuedAt time.Time, epoch int64) (bool, error)
}

// Session runs work inside a transaction that carries the caller's grants.
//
// This is where isolation layer 2 is set up. The implementation sets the grants
// for the transaction and lets them revert at its end, which is why a pooled
// connection cannot hand one caller's grants to the next.
type Session interface {
	InTx(ctx context.Context, callerID string, grants []string,
		fn func(context.Context) error) error

	// Querier runs fn inside a transaction carrying the caller's grants, and
	// hands fn something it can read through.
	//
	// InTx alone is not enough, and the reason is worth stating because the
	// omission looks harmless. The grants are set with set_config(..., true),
	// which is transaction-local, so they exist only inside the transaction that
	// set them. A read attempted outside it — or on a connection from the pool —
	// finds no grants, and a reader that opens its own transaction has to set
	// them itself, which duplicates the one place that decides what a caller may
	// read and gives the duplication a chance to be forgotten.
	Querier(ctx context.Context, callerID string, grants []string,
		fn func(Querier) error) error
}

// Querier is what a handler reads through, and the only thing it needs.
//
// Deliberately narrower than a database handle: a handler cannot begin a
// transaction, cannot commit one, and cannot Exec. So the read boundary cannot
// be widened from inside a handler even by accident.
//
// The row types are pgx's own rather than interfaces declared here. That is not
// a compromise for convenience — it is because a handler hands this straight to
// the pull store, which also wants pgx's row types, and two structurally
// identical interfaces with different names do not satisfy each other. Declaring
// a second set of identical shapes here would only create a place for the two
// to drift apart.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Logger records why a request was refused.
//
// It exists because the wire cannot carry the reason. A refusal that is opaque to
// the client is only safe if it is legible to the operator.
type Logger interface {
	Refused(r *http.Request, reason string, err error)
}

// ErrAnswered says a handler has already written its response and the guard
// should add nothing.
//
// It exists because "refused" and "carried on" cannot both be signalled by a nil
// error. See problem() in sync_routes.go for what went wrong without it.
var ErrAnswered = errors.New("httpapi: response already written")

// ServiceHandler is what a service route does once the caller is known.
//
// It is a separate named type from Handler so the two doors are not wired to each
// other by accident, and it takes the same arguments deliberately: the safety does
// not come from the signature, it comes from Serve refusing a service token and
// ServeService refusing a person's, so a mix-up fails closed at runtime even
// though the compiler cannot see it.
type ServiceHandler func(w http.ResponseWriter, r *http.Request, c Caller) error

// Handler is what a route does once the caller is known.
//
// It cannot be called without a Caller, which is the point: the signature is the
// enforcement.
type Handler func(w http.ResponseWriter, r *http.Request, c Caller) error

// Refusal reasons. These go to the log and never to the client.
//
// A token presented in a header that is not a bearer scheme is reported as no
// token at all rather than as its own reason. It is the same outcome for the
// caller, and one fewer thing to distinguish.
const (
	reasonNoHeader     = "no bearer token presented"
	reasonInvalid      = "token did not validate"
	reasonNoSubject    = "token carries no subject"
	reasonRevoked      = "token is on the denylist"
	reasonDenylistDown = "denylist could not be read"
	reasonScope        = "token does not carry the required scope"
	reasonNoGrants     = "token carries no context grants"
	// The two doors are disjoint. A service token carries no person, and a
	// person token is not a platform; admitting either where it does not belong
	// would let a route decide on a caller it does not understand.
	reasonServiceOnUser    = "a service token was presented to a route that serves a person"
	reasonNotService       = "a person's token was presented to a service route"
	reasonNoServiceContext = "service request names no context"
	reasonHandlerFailed    = "handler failed"
)

// realm identifies this service in the challenge, so a client presenting a token
// to two services can tell which one refused it.
const realm = `Bearer realm="field-service"`

// Guard turns a Handler into an http.Handler that only runs for callers who have
// passed every check.
type Guard struct {
	tokens        Verifier
	revoked       RevocationChecker
	session       Session
	log           Logger
	requiredScope string
}

// NewGuard builds a Guard. requiredScope is the scope every route behind it
// needs; a Guard per scope group is simpler than a scope per route and gives one
// place to be wrong.
func NewGuard(tokens Verifier, revoked RevocationChecker, s Session, log Logger, requiredScope string) *Guard {
	return &Guard{tokens: tokens, revoked: revoked, session: s, log: log, requiredScope: requiredScope}
}

// Serve wraps a handler in the checks.
//
// The order is deliberate and is not interchangeable:
//
//  1. Signature. Everything after this trusts the claims, so nothing can be
//     decided until it is known the token is genuine.
//  2. Revocation. A valid-but-revoked token must not reach a scope check,
//     because answering "you lack the scope" tells a revoked caller something.
//  3. Scope. Cheapest of the remaining, and the one most likely to be
//     misconfigured, so it should fail loudly during setup rather than quietly.
//  4. Grants. A caller with no context grants can read nothing at all, so there
//     is no point opening a transaction for them.
func (g *Guard) Serve(h Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			g.refuse(w, r, reasonNoHeader, nil)
			return
		}

		raw, err := g.tokens.Validate(token)
		if err != nil || raw == nil {
			g.refuse(w, r, reasonInvalid, err)
			return
		}
		claims, ok := raw.(Principal)
		if !ok {
			// A verifier that returned something this guard cannot interrogate is
			// a wiring fault, not a client's mistake, and must not be admitted.
			g.refuse(w, r, reasonInvalid, errors.New("verifier returned claims that are not a Principal"))
			return
		}
		caller := Caller{Principal: claims}

		// The doors are disjoint. A service token carries no person and no
		// context, so every remaining check here is written about somebody: the
		// grant set, the active context, the scope. Admitting one would make each
		// of those checks pass or fail for reasons nobody chose.
		if caller.IsService() {
			g.refuse(w, r, reasonServiceOnUser, nil)
			return
		}

		if caller.Subject() == "" {
			g.refuse(w, r, reasonNoSubject, nil)
			return
		}

		// Without an issue time the timestamp half of the revocation rule cannot
		// be evaluated at all. Refusing is the only safe reading: the epoch check
		// alone would readmit a user who was deactivated and reactivated, which is
		// the specific case the timestamp exists to catch.
		issuedAt, haveIssuedAt := caller.IssuedAt()
		if !haveIssuedAt {
			g.refuse(w, r, reasonRevoked, errors.New("token carries no issue time"))
			return
		}

		revoked, err := g.revoked.Revoked(r.Context(), caller.Subject(), issuedAt, caller.Epoch())
		if err != nil {
			// Fail closed. A denylist that cannot be read cannot be said to have
			// been consulted, and admitting traffic on the grounds that the thing
			// which would have stopped it is unavailable is the failure every
			// access-control system exists to avoid.
			g.refuse(w, r, reasonDenylistDown, err)
			return
		}
		if revoked {
			g.refuse(w, r, reasonRevoked, nil)
			return
		}

		if !caller.HasScope(g.requiredScope) {
			g.refuse(w, r, reasonScope, fmt.Errorf("scope %q is required", g.requiredScope))
			return
		}

		grants := caller.Grants()
		if len(grants) == 0 {
			g.refuse(w, r, reasonNoGrants, nil)
			return
		}

		// The grant set, not the active context. A write into the caller's active
		// context happens because that is where the record belongs, but the read
		// boundary is everywhere they were granted.
		if err := g.session.InTx(r.Context(), caller.Subject(), grants,
			func(ctx context.Context) error {
				return h(w, r, caller)
			}); err != nil {
			if errors.Is(err, ErrAnswered) {
				// The handler already wrote a deliberate response. Adding a 500
				// here would overwrite a 415 that was correct, and the client's
				// only clue would be a truncated body.
				return
			}
			// The caller was authorised. A handler that fails is an internal
			// fault and must not be reported as an authorisation problem, or the
			// logs will say "unauthorised" about requests that were permitted.
			g.log.Refused(r, reasonHandlerFailed, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
		}
	})
}

// ServeService authenticates a caller for a route that acts in a context named by
// its request. It is the second door, and it is deliberately not Serve with the
// grants check relaxed.
//
// What it does NOT do is open a transaction, and that is the whole of the
// difference. Every rule about which context a write belongs to is written around
// a token that carries one, and there is no honest way to keep those rules for a
// caller that cannot: the context has to arrive with the request. So this door
// establishes only that the caller is a service with the right scope, and the
// handler opens the transaction in the context it was given.
//
// Three things it insists on, because each is a way this could otherwise be
// mistaken for the ordinary door:
//
//   - the token must be a service token. A person's token is refused, so a
//     compromised mentor session cannot reach this route even if it somehow
//     carried the scope.
//   - the scope must be the one the route was built with. Not a superset: a
//     service token that could also push field data would make one leaked
//     credential good for both.
//   - revocation is checked exactly as it is for a person. A service is not
//     exempt from being switched off.
func (g *Guard) ServeService(scope string) func(ServiceHandler) http.Handler {
	return func(h ServiceHandler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearer(r.Header.Get("Authorization"))
			if !ok {
				g.refuse(w, r, reasonNoHeader, nil)
				return
			}
			raw, err := g.tokens.Validate(token)
			if err != nil || raw == nil {
				g.refuse(w, r, reasonInvalid, err)
				return
			}
			claims, ok := raw.(Principal)
			if !ok {
				g.refuse(w, r, reasonInvalid, errors.New("verifier returned claims that are not a Principal"))
				return
			}
			caller := Caller{Principal: claims}

			if !caller.IsService() {
				g.refuse(w, r, reasonNotService, nil)
				return
			}
			if caller.Subject() == "" {
				g.refuse(w, r, reasonNoSubject, nil)
				return
			}
			issuedAt, haveIssuedAt := caller.IssuedAt()
			if !haveIssuedAt {
				g.refuse(w, r, reasonRevoked, errors.New("token carries no issue time"))
				return
			}
			revoked, err := g.revoked.Revoked(r.Context(), caller.Subject(), issuedAt, caller.Epoch())
			if err != nil {
				// Fail closed for the same reason as the ordinary door: admitting
				// traffic because the thing which would have stopped it cannot be
				// read is the failure this system exists to avoid.
				g.refuse(w, r, reasonDenylistDown, err)
				return
			}
			if revoked {
				g.refuse(w, r, reasonRevoked, nil)
				return
			}
			if !caller.HasScope(scope) {
				g.refuse(w, r, reasonScope, fmt.Errorf("scope %q is required", scope))
				return
			}
			if err := h(w, r, caller); err != nil {
				if errors.Is(err, ErrAnswered) {
					return
				}
				if g.log != nil {
					g.log.Refused(r, reasonHandlerFailed, err)
				}
				w.Header().Set("Cache-Control", "no-store")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal_error"}`))
			}
		})
	}
}

// refuse writes the same refusal for every reason.
//
// Identical status, body and challenge, so nothing about which check failed is
// observable from outside. The reason goes to the log.
func (g *Guard) refuse(w http.ResponseWriter, r *http.Request, reason string, err error) {
	g.log.Refused(r, reason, err)
	w.Header().Set("WWW-Authenticate", realm)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorised"}`))
}

// bearer extracts the token from an Authorization header.
//
// Hand-written rather than reached for in a library so that the failure cases
// are visible: what is accepted, what is trimmed, and what is treated as no
// token at all.
func bearer(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	scheme, value, found := strings.Cut(header, " ")
	if !found {
		return "", false
	}
	// The scheme is case-insensitive per RFC 7235; the value is not.
	if !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	return value, true
}
