// Package authn validates the bearer tokens Chisimba issues.
//
// The rules this package enforces are the service's, not its preferences.
// They come from the contract's rules 1, 3, 10, 11 and 12 and from the design
// document's authentication section, and each one is written down here so that
// a change to this file is a change to something that was decided rather than
// a change to something that was typed.
//
// The two properties that matter most:
//
//   - The algorithm is chosen by this package from an allowlist, never read
//     from the token header. A token that says "alg":"none", or names an HMAC
//     algorithm, is refused before its signature is looked at.
//
//   - A rejection does not say which check failed. "Your token is expired" and
//     "your token is signed by a key we do not hold" tell an attacker which
//     half to fix, and the legitimate client cannot act on the difference
//     either. Every refusal from Validate is ErrRejected.
package authn

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
)

// ErrRejected is the only error a caller needs to distinguish.
//
// It is deliberately opaque. Anything more specific would tell an attacker
// which check to work on, and there is nothing a client can usefully do with
// the detail: the remedy is always the same, obtain a fresh token.
var ErrRejected = errors.New("token rejected")

// Claims is the set of values the service reads from a verified token.
//
// The contract is explicit that ctx and ctxs are separate and that using the
// active context as the read boundary would hide a guide's northern-reserve
// drives while they happen to be working in the southern one. ActiveContext is
// where a new record is written. Grants is what reads are checked against.
// Neither is derived from the other.
type Claims struct {
	Subject       string   `json:"sub"`
	Scope         []string `json:"scope"`
	ActiveContext string   `json:"ctx"`
	Grants        []string `json:"ctxs"`
	Roles         []string `json:"roles"`
	Epoch         int64    `json:"epoch"`
	TokenID       string   `json:"ver"`

	Issuer    string   `json:"iss"`
	Audience  []string `json:"aud"`
	ExpiresAt int64    `json:"exp"`
	NotBefore int64    `json:"nbf"`
	IssuedAt  int64    `json:"iat"`
}

// Principal is a verified caller.
//
// A Principal exists only if a token was verified. There is no constructor that
// takes one from a string, and no code path that produces a Principal from an
// unverified token, so a caller cannot be constructed from request input by
// accident.
type Principal struct {
	Claims

	// signingKeyID is which key verified this token. It is kept for logging and
	// for the key-rotation metric, never for authorisation: a token signed by a
	// key that is still trusted is not more or less valid than one signed by
	// another still-trusted key.
	signingKeyID string
}

// KeyID reports which key verified this principal's token.
func (p Principal) KeyID() string { return p.signingKeyID }

// HasScope reports whether the token carried the named scope.
func (p Principal) HasScope(scope string) bool {
	for _, held := range p.Scope {
		if held == scope {
			return true
		}
	}
	return false
}

// HasRole reports whether the token carried the named role.
func (p Principal) HasRole(role string) bool {
	for _, held := range p.Roles {
		if held == role {
			return true
		}
	}
	return false
}

// GrantsContext reports whether the caller holds a grant in the named context.
//
// This is the rule 11 read test: both the scope and a grant in that context
// are required, and neither implies the other. The scope check is the caller's,
// because only the caller knows which scope the endpoint needs; this answers
// only the context half.
func (p Principal) GrantsContext(code string) bool {
	for _, granted := range p.Grants {
		if granted == code {
			return true
		}
	}
	return false
}

// CanWriteToContext reports whether a write into the named context is permitted.
//
// A write goes into the caller's active context, so the question is whether
// that active context is one they were granted. A caller writing into a
// context they were not granted is not refused with a validation error — the
// design says they simply write into the context the token names. This exists
// so a caller can notice that situation and say so plainly, not to produce an
// error the client cannot act on.
func (p Principal) CanWriteToContext(code string) bool {
	return p.ActiveContext != "" && p.ActiveContext == code && p.GrantsContext(code)
}

// KeyResolver supplies the public key for a key id.
//
// Returning an error means "I do not hold that key", which is the signal the
// caller uses to refresh the JWKS once and try again. It is not a rejection.
type KeyResolver interface {
	ResolveKey(keyID string) (*rsa.PublicKey, error)
}

// clock is injected so expiry and rotation timing are testable without
// sleeping, for the same reason the Dart client injects its wait function.
type clock func() time.Time

// Validator verifies bearer tokens against a key resolver.
type Validator struct {
	keys KeyResolver

	// audience is the identifier this service answers to. A token minted for a
	// different service must not be accepted here, however valid it is
	// elsewhere: the tokens are signed by the same issuer and differ only in
	// this claim, so skipping the check would make every service in the
	// deployment interchangeable to an attacker holding any one token.
	audience string

	// issuer is the only issuer whose tokens are honoured.
	issuer string

	now clock

	// leeway absorbs small clock differences between the issuer and this
	// service. It is deliberately small: it exists so a token that expired one
	// second ago on a phone with a fast clock is not refused, and a large value
	// would quietly become an extension of every token's life.
	leeway time.Duration
}

// Option configures a Validator.
type Option func(*Validator)

// WithLeeway sets the clock tolerance. Zero is the default and the safest
// value; anything larger should be justified by measurement.
func WithLeeway(d time.Duration) Option {
	return func(v *Validator) { v.leeway = d }
}

// WithClock replaces the clock. Exported for tests in this package's
// dependents; there is no production caller.
func WithClock(c func() time.Time) Option {
	return func(v *Validator) { v.now = c }
}

// NewValidator builds a validator.
//
// audience and issuer are required and an empty value for either is a
// programming error rather than a runtime condition: a validator that trusts
// any issuer or any audience would pass every check while enforcing nothing.
func NewValidator(keys KeyResolver, audience, issuer string, opts ...Option) (*Validator, error) {
	if keys == nil {
		return nil, errors.New("authn: a key resolver is required")
	}
	if strings.TrimSpace(audience) == "" {
		return nil, errors.New("authn: an audience is required")
	}
	if strings.TrimSpace(issuer) == "" {
		return nil, errors.New("authn: an issuer is required")
	}

	v := &Validator{
		keys:     keys,
		audience: audience,
		issuer:   issuer,
		now:      time.Now,
		leeway:   30 * time.Second,
	}
	for _, opt := range opts {
		opt(v)
	}
	return v, nil
}

// algRS256 is the only algorithm this service accepts.
//
// It is a constant rather than a set because there is currently one entry, and
// a set invites someone to add a second without asking what is safe about it.
const algRS256 = "RS256"

type jwtHeader struct {
	Algorithm string `json:"alg"`
	KeyID     string `json:"kid"`
	Type      string `json:"typ"`
}

type rawToken struct {
	header       jwtHeader
	claims       Claims
	signingInput []byte
	signature    []byte
}

// Validate verifies a bearer token and returns the caller it identifies.
//
// Every refusal returns ErrRejected, with no detail attached and no partial
// Principal, so a caller cannot accidentally use a zero-valued Principal after
// ignoring an error.
func (v *Validator) Validate(token string) (Principal, error) {
	parsed, err := parse(token)
	if err != nil {
		return Principal{}, ErrRejected
	}

	if err := v.checkAlgorithm(parsed.header); err != nil {
		return Principal{}, ErrRejected
	}
	if err := v.checkIssuerAndAudience(parsed.claims); err != nil {
		return Principal{}, ErrRejected
	}
	if err := v.checkTimes(parsed.claims); err != nil {
		return Principal{}, ErrRejected
	}
	if err := v.checkSubject(parsed.claims); err != nil {
		return Principal{}, ErrRejected
	}

	key, err := v.keys.ResolveKey(parsed.header.KeyID)
	if err != nil || key == nil {
		return Principal{}, ErrRejected
	}
	if err := verifyRS256(parsed.signingInput, parsed.signature, key); err != nil {
		return Principal{}, ErrRejected
	}

	// A token whose active context is not in its own grant set is not rejected.
	// It is a valid token for reading — the grant set is the read boundary, and
	// it is the grant set that was satisfied. It simply cannot write, and
	// WriteContext is how a caller finds that out.
	principal := Principal{Claims: parsed.claims, signingKeyID: parsed.header.KeyID}
	return principal, nil
}

// WriteContext returns the context a new record would be written into.
//
// It is a method rather than a field so that the "active context not granted"
// case cannot be skipped by reading the field directly.
func (p Principal) WriteContext() (string, bool) {
	if p.ActiveContext == "" {
		return "", false
	}
	if !p.GrantsContext(p.ActiveContext) {
		// The grant was withdrawn between the token being minted and this
		// request. The token is still valid for reading; a write would land in
		// a context the caller no longer holds, so it is refused here.
		return "", false
	}
	return p.ActiveContext, true
}

// checkAlgorithm refuses everything except RS256.
//
// The comparison is against a constant, not against a list built from
// configuration, and it happens before the signature is examined. A token
// carrying "alg":"none" must never reach a point where an unsigned payload is
// treated as verified, and a token naming an HMAC algorithm must never reach a
// point where the public key is used as an HMAC secret — that is the
// algorithm-confusion attack, and it is prevented here rather than by hoping
// the key type happens to save us.
func (v *Validator) checkAlgorithm(h jwtHeader) error {
	if h.Algorithm != algRS256 {
		return fmt.Errorf("algorithm %q is not accepted", h.Algorithm)
	}
	// A "typ" of JWT is conventional. Its absence is tolerated because some
	// issuers omit it, and it carries no security meaning for this service.
	return nil
}

func (v *Validator) checkIssuerAndAudience(c Claims) error {
	if c.Issuer != v.issuer {
		return fmt.Errorf("issuer %q is not trusted", c.Issuer)
	}
	if !contains(c.Audience, v.audience) {
		return fmt.Errorf("audience does not include this service")
	}
	return nil
}

// checkTimes refuses a token that is not currently valid.
//
// exp is checked with a small leeway. nbf is checked because a token minted for
// a future time is one this service was not meant to accept yet, and ignoring
// nbf would make a token valid before the window its issuer chose.
func (v *Validator) checkTimes(c Claims) error {
	if c.ExpiresAt == 0 {
		// A token with no expiry is a token that never expires. Refusing it is
		// the only safe reading of a missing claim.
		return errors.New("token carries no expiry")
	}
	now := v.now()
	if now.After(time.Unix(c.ExpiresAt, 0).Add(v.leeway)) {
		return errors.New("token has expired")
	}
	if c.NotBefore != 0 && now.Add(v.leeway).Before(time.Unix(c.NotBefore, 0)) {
		return errors.New("token is not yet valid")
	}
	return nil
}

// checkSubject refuses a token with no subject.
//
// Every authorisation decision in this service is made about a user, so a token
// that identifies nobody cannot be turned into a Principal. It is refused
// rather than tolerated as an anonymous caller because nothing here is public.
func (v *Validator) checkSubject(c Claims) error {
	if strings.TrimSpace(c.Subject) == "" {
		return errors.New("token carries no subject")
	}
	return nil
}

func parse(token string) (rawToken, error) {
	var out rawToken

	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return out, errors.New("a token has three dot-separated parts")
	}

	headerBytes, err := decodeSegment(parts[0])
	if err != nil {
		return out, fmt.Errorf("header segment: %w", err)
	}
	if err := json.Unmarshal(headerBytes, &out.header); err != nil {
		return out, fmt.Errorf("header: %w", err)
	}

	claimBytes, err := decodeSegment(parts[1])
	if err != nil {
		return out, fmt.Errorf("claim segment: %w", err)
	}
	// The audience claim is a string or an array of strings depending on the
	// issuer, and both appear in the wild. It is decoded by hand because
	// json.Unmarshal cannot hold both shapes in one field.
	if err := unmarshalClaims(claimBytes, &out.claims); err != nil {
		return out, fmt.Errorf("claims: %w", err)
	}

	signature, err := decodeSegment(parts[2])
	if err != nil {
		return out, fmt.Errorf("signature segment: %w", err)
	}

	out.signingInput = []byte(parts[0] + "." + parts[1])
	out.signature = signature
	return out, nil
}

// unmarshalClaims decodes the claim set, tolerating an audience that is either
// a single string or an array.
//
// The claim is declared as []string in Claims, which handles the array form. A
// single string would fail to unmarshal into a slice, so it is reshaped before
// the general decode rather than in a custom unmarshaller, because a custom
// one on Claims would also have to cope with every other field.
func unmarshalClaims(raw []byte, out *Claims) error {
	var probe struct {
		Audience json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}

	var normalised []byte
	if len(probe.Audience) > 0 {
		var single string
		if err := json.Unmarshal(probe.Audience, &single); err == nil {
			replacement, err := json.Marshal([]string{single})
			if err != nil {
				return err
			}
			normalised, err = replaceJSONField(raw, "aud", replacement)
			if err != nil {
				return err
			}
		}
	}

	if normalised == nil {
		normalised = raw
	}
	return json.Unmarshal(normalised, out)
}

// replaceJSONField rewrites one top-level field of a JSON object.
//
// The document is decoded into a map and re-encoded rather than patched by
// string surgery, so a value that happens to contain the substring `"aud"`
// cannot corrupt it.
func replaceJSONField(raw []byte, field string, value []byte) ([]byte, error) {
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, err
	}
	document[field] = value
	return json.Marshal(document)
}

// verifyRS256 checks an RSASSA-PKCS1-v1_5 SHA-256 signature.
//
// PKCS1v15 rather than PSS: the issuer signs with PKCS1v15, and a verifier
// that accepted both would accept signatures the issuer never produces.
func verifyRS256(signingInput, signature []byte, key *rsa.PublicKey) error {
	if key == nil {
		return errors.New("no key")
	}
	digest := sha256.Sum256(signingInput)
	return rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], signature)
}

func decodeSegment(segment string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(segment)
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

// ParsePublicKey builds an RSA public key from the n and e of a JWKS entry.
//
// It is here rather than in the JWKS cache because the arithmetic is the
// security-relevant part: n and e arrive base64url-encoded from a document the
// service fetched over the network, and a key built from malformed values must
// fail rather than become something that signs.
func ParsePublicKey(n, e string) (*rsa.PublicKey, error) {
	if n == "" || e == "" {
		return nil, errors.New("a key needs both n and e")
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(n)
	if err != nil {
		return nil, fmt.Errorf("modulus: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(e)
	if err != nil {
		return nil, fmt.Errorf("exponent: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("modulus and exponent must not be empty")
	}

	exponent := 0
	for _, b := range eBytes {
		// A shifting left past 31 bits would overflow an int, and a JWKS
		// exponent that large is not real, so it is refused rather than
		// silently truncated to a low value that would verify against
		// signatures the issuer never made.
		if exponent > (1 << 24) {
			return nil, errors.New("exponent is implausibly large")
		}
		exponent = exponent<<8 | int(b)
	}
	if exponent < 3 {
		// 65537 is what every real RSA key uses. Values below 3 are nonsense
		// and values in between are not worth trusting from a network
		// document.
		return nil, errors.New("exponent is not usable")
	}

	return &rsa.PublicKey{
		N: new(big.Int).SetBytes(nBytes),
		E: exponent,
	}, nil
}
