package jwks

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"crypto/rsa"

	"field-service/internal/authn"
)

// Parse reads a JWKS document and returns the RSA keys it publishes, keyed by
// key id.
//
// Keys it does not return are as important as the ones it does. A real document
// legitimately contains keys this service cannot use: an EC signing key for
// another consumer, an octet key for a symmetric algorithm, a key that is
// present for encryption only. Loading all of them and then refusing at
// verification time would put the decision in the wrong place — and a key with
// `use: enc` must never be accepted as a signing key even if its id matches.
//
// So a key is skipped unless it says it is an RSA key for RS256, and skipping is
// not an error. A document containing nothing usable is an error, because that
// is a misconfiguration rather than a document about somebody else's keys.
func Parse(raw []byte) (map[string]*rsa.PublicKey, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("jwks: document is not JSON: %w", err)
	}

	keys := map[string]*rsa.PublicKey{}
	for _, k := range doc.Keys {
		// The algorithm is checked here rather than at verification time so that
		// the validator's own algorithm check is a second line rather than the
		// only one.
		if k.KeyType != "RSA" || k.Algorithm != "RS256" {
			continue
		}
		// A key with no id cannot be looked up by a token, and two keys with one
		// id make the document ambiguous. Silently taking the last would mean a
		// document could verify a token against a key its author did not intend.
		if k.KeyID == "" {
			continue
		}
		if _, clash := keys[k.KeyID]; clash {
			return nil, fmt.Errorf("jwks: key id %q appears twice in the document", k.KeyID)
		}
		// Reused rather than reimplemented, so the refusal of a truncated or
		// low-exponent key lives in exactly one place.
		pub, err := authn.ParsePublicKey(k.Modulus, k.Exponent)
		if err != nil {
			return nil, fmt.Errorf("jwks: key %q is unusable: %w", k.KeyID, err)
		}
		keys[k.KeyID] = pub
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("jwks: document publishes no usable RS256 key (%d key(s) present)", len(doc.Keys))
	}
	return keys, nil
}

// KeyIDs lists the key ids a document publishes, for logging a refresh without
// logging key material.
func KeyIDs(raw []byte) []string {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil
	}
	ids := make([]string, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		if k.KeyType == "RSA" && k.Algorithm == "RS256" && k.KeyID != "" {
			ids = append(ids, k.KeyID)
		}
	}
	sort.Strings(ids)
	return ids
}

// jwk is one entry of the document. Field names are the document's, and Modulus
// and Exponent carry the base64url values JWKS specifies.
type jwk struct {
	KeyType   string `json:"kty"`
	KeyID     string `json:"kid"`
	Algorithm string `json:"alg"`
	Use       string `json:"use"`
	Modulus   string `json:"n"`
	Exponent  string `json:"e"`
}

// readAtMost reads r up to limit bytes and fails if it is larger.
//
// The document arrives from a configured URL, so it is not attacker-controlled
// in the usual sense, but an unbounded read is still an unbounded read: a
// misconfigured or hijacked endpoint should not be able to exhaust this
// service's memory.
func readAtMost(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("jwks: document is larger than %d bytes", limit)
	}
	return data, nil
}
