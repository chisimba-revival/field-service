package authn

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A test issuer. Every token in these tests is signed by one of these, so a
// test failure means the validator changed behaviour rather than that a fixture
// drifted.
type issuer struct {
	private  *rsa.PrivateKey
	keyID    string
	audience string
	issuerID string
}

// issuerCounter gives every test issuer its own key id. Two issuers sharing a
// kid is not a scenario this service has to survive — a kid names one key — and
// sharing one made a test fail for the wrong reason: registering the second
// issuer's key overwrote the first, so the "signed by a stranger" case became a
// token correctly signed by the key now published for that kid.
var issuerCounter int

func newIssuer(t *testing.T) *issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	issuerCounter++
	return &issuer{
		private:  key,
		keyID:    fmt.Sprintf("test-key-%d", issuerCounter),
		audience: "field-service",
		issuerID: "https://auth.test",
	}
}

// sign produces a token with arbitrary overrides, so a test can state exactly
// one thing that is wrong about it.
func (i *issuer) sign(t *testing.T, mutate func(map[string]any), headerMutate func(map[string]any)) string {
	t.Helper()

	header := map[string]any{"alg": algRS256, "kid": i.keyID, "typ": "JWT"}
	if headerMutate != nil {
		headerMutate(header)
	}
	claims := map[string]any{
		"sub":   "user-1",
		"iss":   i.issuerID,
		"aud":   i.audience,
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Add(-time.Minute).Unix(),
		"ctx":   "reserve-north",
		"ctxs":  []string{"reserve-north", "reserve-south"},
		"role":  nil,
		"scope": []string{"sightings:write"},
		"roles": []string{"guide"},
		"epoch": 7,
		"ver":   "token-id-1",
	}
	delete(claims, "role")
	if mutate != nil {
		mutate(claims)
	}

	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON)

	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, i.private, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(signature)
}

// staticResolver holds one key and reports which key ids it was asked for, so
// a test can prove the unknown-kid path was taken.
type staticResolver struct {
	keys     map[string]*rsa.PublicKey
	askedFor []string
}

func (r *staticResolver) ResolveKey(keyID string) (*rsa.PublicKey, error) {
	r.askedFor = append(r.askedFor, keyID)
	key, ok := r.keys[keyID]
	if !ok {
		return nil, errors.New("no such key")
	}
	return key, nil
}

func newValidator(t *testing.T, iss *issuer) (*Validator, *staticResolver) {
	t.Helper()
	resolver := &staticResolver{keys: map[string]*rsa.PublicKey{
		iss.keyID: &iss.private.PublicKey,
	}}
	v, err := NewValidator(resolver, "field-service", "https://auth.test", WithLeeway(0))
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	return v, resolver
}

func TestAValidTokenIsAccepted(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	principal, err := v.Validate(iss.sign(t, nil, nil))
	if err != nil {
		t.Fatalf("a valid token was rejected: %v", err)
	}

	if principal.Subject != "user-1" {
		t.Errorf("subject = %q, want user-1", principal.Subject)
	}
	if principal.ActiveContext != "reserve-north" {
		t.Errorf("active context = %q, want reserve-north", principal.ActiveContext)
	}
	if len(principal.Grants) != 2 {
		t.Errorf("grants = %v, want two", principal.Grants)
	}
	if principal.Epoch != 7 {
		t.Errorf("epoch = %d, want 7", principal.Epoch)
	}
	if !principal.HasScope("sightings:write") {
		t.Error("HasScope did not find a scope the token carried")
	}
	if principal.HasScope("sightings:delete") {
		t.Error("HasScope found a scope the token did not carry")
	}
	if !principal.HasRole("guide") {
		t.Error("HasRole did not find a role the token carried")
	}
	if principal.KeyID() != iss.keyID {
		t.Errorf("key id = %q, want %q", principal.KeyID(), iss.keyID)
	}
}

// TestTheAlgorithmIsNeverReadFromTheToken is the algorithm-confusion defence.
// Each of these tokens is signed correctly by the issuer's own key, and each
// would be accepted by a verifier that trusted the header.
func TestTheAlgorithmIsNeverReadFromTheToken(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	for _, algorithm := range []string{"none", "None", "NONE", "HS256", "hs256", "RS512", "ES256", "PS256"} {
		t.Run(algorithm, func(t *testing.T) {
			token := iss.sign(t, nil, func(h map[string]any) { h["alg"] = algorithm })
			if _, err := v.Validate(token); !errors.Is(err, ErrRejected) {
				t.Errorf("a token declaring alg %q was accepted; the algorithm must come "+
					"from this package, not from the token", algorithm)
			}
		})
	}
}

// TestAnUnsignedTokenIsRefused covers the "none" attack properly: a token with
// no signature at all, which some libraries accept by default.
func TestAnUnsignedTokenIsRefused(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	headerJSON, _ := json.Marshal(map[string]any{"alg": "none", "kid": iss.keyID})
	claimsJSON, _ := json.Marshal(map[string]any{
		"sub": "user-1", "iss": iss.issuerID, "aud": iss.audience,
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	unsigned := base64.RawURLEncoding.EncodeToString(headerJSON) + "." +
		base64.RawURLEncoding.EncodeToString(claimsJSON) + "."

	if _, err := v.Validate(unsigned); !errors.Is(err, ErrRejected) {
		t.Error("an unsigned token was accepted")
	}
}

func TestATokenSignedByAnotherKeyIsRefused(t *testing.T) {
	iss := newIssuer(t)
	attacker := newIssuer(t)
	// Same key id, different key: the attacker is trying to be the issuer.
	v, resolver := newValidator(t, iss)
	resolver.keys[attacker.keyID] = &attacker.private.PublicKey

	token := attacker.sign(t, nil, func(h map[string]any) { h["kid"] = iss.keyID })
	if _, err := v.Validate(token); !errors.Is(err, ErrRejected) {
		t.Error("a token signed by a key other than the one published for that kid was accepted")
	}
}

func TestAnUnknownKeyIDIsRefusedAndTheResolverIsAsked(t *testing.T) {
	iss := newIssuer(t)
	v, resolver := newValidator(t, iss)

	token := iss.sign(t, nil, func(h map[string]any) { h["kid"] = "never-published" })
	if _, err := v.Validate(token); !errors.Is(err, ErrRejected) {
		t.Fatal("a token naming an unpublished key id was accepted")
	}

	// The resolver must actually have been consulted for that key id. This is
	// the signal the HTTP layer uses to trigger one JWKS refresh and retry, so
	// a token signed by a newly rotated key is not refused forever.
	if len(resolver.askedFor) == 0 || resolver.askedFor[0] != "never-published" {
		t.Errorf("resolver was asked for %v, want it asked for the unknown kid", resolver.askedFor)
	}
}

func TestATamperedPayloadIsRefused(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	token := iss.sign(t, nil, nil)
	parts := strings.Split(token, ".")

	// Re-encode the claims with a wider grant set, keeping the signature.
	forged := map[string]any{
		"sub": "user-1", "iss": iss.issuerID, "aud": iss.audience,
		"exp": time.Now().Add(time.Hour).Unix(),
		"ctx": "reserve-north", "ctxs": []string{"every-reserve-in-the-country"},
	}
	forgedJSON, _ := json.Marshal(forged)
	parts[1] = base64.RawURLEncoding.EncodeToString(forgedJSON)

	if _, err := v.Validate(strings.Join(parts, ".")); !errors.Is(err, ErrRejected) {
		t.Error("a token whose claims were edited after signing was accepted")
	}
}

// TestEveryRejectionIsOpaque is the rule 8 property: a refusal must not say
// which check failed, because that tells an attacker which half to fix.
func TestEveryRejectionIsOpaque(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)
	other := newIssuer(t)

	cases := []struct {
		name  string
		token string
	}{
		{"expired", iss.sign(t, func(c map[string]any) {
			c["exp"] = time.Now().Add(-time.Hour).Unix()
		}, nil)},
		{"wrong audience", iss.sign(t, func(c map[string]any) {
			c["aud"] = "some-other-service"
		}, nil)},
		{"untrusted issuer", iss.sign(t, func(c map[string]any) {
			c["iss"] = "https://evil.test"
		}, nil)},
		{"no expiry", iss.sign(t, func(c map[string]any) {
			delete(c, "exp")
		}, nil)},
		{"no subject", iss.sign(t, func(c map[string]any) {
			delete(c, "sub")
		}, nil)},
		{"not yet valid", iss.sign(t, func(c map[string]any) {
			c["nbf"] = time.Now().Add(time.Hour).Unix()
		}, nil)},
		{"audience is an array without us", iss.sign(t, func(c map[string]any) {
			c["aud"] = []string{"a", "b"}
		}, nil)},
		{"signed by a stranger", other.sign(t, nil, func(h map[string]any) { h["kid"] = iss.keyID })},
		{"not a jwt at all", "hello"},
		{"too few parts", "a.b"},
		{"empty", ""},
	}

	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Validate(tc.token)
			if !errors.Is(err, ErrRejected) {
				t.Fatalf("err = %v, want ErrRejected", err)
			}
			if err.Error() != ErrRejected.Error() {
				t.Errorf("error text %q differs from ErrRejected; a refusal must not "+
					"reveal which check failed", err.Error())
			}
			seen[err.Error()] = true
		})
	}

	if len(seen) != 1 {
		t.Errorf("saw %d distinct refusal messages across cases, want 1", len(seen))
	}
}

// TestAnAudienceMayBeAnArrayOrAString covers the two shapes issuers use.
func TestAnAudienceMayBeAnArrayOrAString(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	single := iss.sign(t, func(c map[string]any) {
		c["aud"] = "field-service"
	}, nil)
	if _, err := v.Validate(single); err != nil {
		t.Errorf("a single-string audience was refused: %v", err)
	}

	among := iss.sign(t, func(c map[string]any) {
		c["aud"] = []string{"another-service", "field-service"}
	}, nil)
	if _, err := v.Validate(among); err != nil {
		t.Errorf("an audience array containing this service was refused: %v", err)
	}
}

// TestLeewayIsSmallAndDeliberate checks the boundary rather than trusting it.
func TestLeewayIsSmallAndDeliberate(t *testing.T) {
	iss := newIssuer(t)

	// Just expired, inside the leeway.
	justExpired := iss.sign(t, func(c map[string]any) {
		c["exp"] = time.Now().Add(-10 * time.Second).Unix()
	}, nil)
	leewayValidator, err := NewValidator(
		&staticResolver{keys: map[string]*rsa.PublicKey{iss.keyID: &iss.private.PublicKey}},
		"field-service", "https://auth.test", WithLeeway(30*time.Second))
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	if _, err := leewayValidator.Validate(justExpired); err != nil {
		t.Errorf("a token expired 10s ago was refused despite 30s leeway: %v", err)
	}

	// Long expired, well outside any leeway a sane configuration would use.
	longExpired := iss.sign(t, func(c map[string]any) {
		c["exp"] = time.Now().Add(-2 * time.Hour).Unix()
	}, nil)
	if _, err := leewayValidator.Validate(longExpired); !errors.Is(err, ErrRejected) {
		t.Error("a token expired two hours ago was accepted")
	}
}

// TestActiveContextMustBeGrantedBeforeAWrite covers the contract's separation
// of ctx from ctxs, and the design's rule that a write goes to the active
// context.
func TestActiveContextMustBeGrantedBeforeAWrite(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	granted := iss.sign(t, func(c map[string]any) {
		c["ctx"] = "reserve-north"
		c["ctxs"] = []string{"reserve-north", "reserve-south"}
	}, nil)
	principal, err := v.Validate(granted)
	if err != nil {
		t.Fatalf("refused: %v", err)
	}
	if got, ok := principal.WriteContext(); !ok || got != "reserve-north" {
		t.Errorf("WriteContext() = %q, %v; want reserve-north, true", got, ok)
	}

	// The active context is not in the grant set. The token is still valid for
	// reading — the grant set is the read boundary — but it cannot write.
	notGranted := iss.sign(t, func(c map[string]any) {
		c["ctx"] = "reserve-west"
		c["ctxs"] = []string{"reserve-north"}
	}, nil)
	principal, err = v.Validate(notGranted)
	if err != nil {
		t.Fatalf("a token whose active context is ungranted was refused outright: %v", err)
	}
	if _, ok := principal.WriteContext(); ok {
		t.Error("WriteContext allowed a write into a context the token was not granted")
	}
	if !principal.GrantsContext("reserve-north") {
		t.Error("the token's granted context should still be readable")
	}
}

func TestATokenWithNoActiveContextCanStillRead(t *testing.T) {
	iss := newIssuer(t)
	v, _ := newValidator(t, iss)

	readOnly := iss.sign(t, func(c map[string]any) { delete(c, "ctx") }, nil)
	principal, err := v.Validate(readOnly)
	if err != nil {
		t.Fatalf("a token with no active context was refused: %v", err)
	}
	if _, ok := principal.WriteContext(); ok {
		t.Error("WriteContext allowed a write from a token with no active context")
	}
	if !principal.GrantsContext("reserve-south") {
		t.Error("grants should still be readable from a token with no active context")
	}
}

func TestNewValidatorRefusesToBeBuiltWithoutItsConfiguration(t *testing.T) {
	keys := &staticResolver{}

	for _, tc := range []struct{ name, audience, issuer string }{
		{"no audience", "", "https://auth.test"},
		{"blank audience", "   ", "https://auth.test"},
		{"no issuer", "field-service", ""},
		{"blank issuer", "field-service", "  "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A validator that trusted any issuer or any audience would pass
			// every check while enforcing nothing, so this is a programming
			// error and not a runtime condition.
			if _, err := NewValidator(keys, tc.audience, tc.issuer); err == nil {
				t.Error("a validator was built with no audience or issuer to check against")
			}
		})
	}

	if _, err := NewValidator(nil, "field-service", "https://auth.test"); err == nil {
		t.Error("a validator was built with no key resolver")
	}
}

func TestParsePublicKey(t *testing.T) {
	iss := newIssuer(t)

	n := base64.RawURLEncoding.EncodeToString(iss.private.PublicKey.N.Bytes())
	e := base64.RawURLEncoding.EncodeToString(bigEndianExponent(iss.private.PublicKey.E))

	key, err := ParsePublicKey(n, e)
	if err != nil {
		t.Fatalf("a real key was refused: %v", err)
	}
	if key.N.Cmp(iss.private.PublicKey.N) != 0 || key.E != iss.private.PublicKey.E {
		t.Error("the parsed key does not match the one that was encoded")
	}

	// A key that cannot verify anything is worse than no key, so each of these
	// must fail rather than become a key that silently accepts nothing or
	// something.
	for _, tc := range []struct{ name, n, e string }{
		{"empty modulus", "", e},
		{"empty exponent", n, ""},
		{"modulus is not base64", "!!!!", e},
		{"exponent is not base64", n, "!!!!"},
		{"exponent too small", n, base64.RawURLEncoding.EncodeToString([]byte{2})},
		{"exponent is zero", n, base64.RawURLEncoding.EncodeToString([]byte{0})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePublicKey(tc.n, tc.e); err == nil {
				t.Error("an unusable key was accepted")
			}
		})
	}
}

// TestReplaceJSONFieldDoesNotCorruptOnASubstring is a regression test for the
// reason that function decodes and re-encodes rather than patching by string
// surgery.
func TestReplaceJSONFieldDoesNotCorruptOnASubstring(t *testing.T) {
	original := []byte(`{"aud":"a","notes":"the value contains \"aud\" in it"}`)

	replaced, err := replaceJSONField(original, "aud", []byte(`["x"]`))
	if err != nil {
		t.Fatalf("replace: %v", err)
	}

	var back map[string]any
	if err := json.Unmarshal(replaced, &back); err != nil {
		t.Fatalf("the replacement is not valid JSON: %v", err)
	}
	if back["notes"] != `the value contains "aud" in it` {
		t.Errorf("notes = %v; a substring must not be mistaken for the field", back["notes"])
	}
}

func bigEndianExponent(e int) []byte {
	if e == 0 {
		return []byte{0}
	}
	var out []byte
	for value := e; value > 0; value >>= 8 {
		out = append([]byte{byte(value & 0xff)}, out...)
	}
	return out
}
