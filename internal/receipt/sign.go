package receipt

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// Sentinel errors from Sign and Verify. The verify CLI (sylveste-ewy3.5.3)
// maps these to canon §Verification semantics exit codes via errors.Is.
var (
	// ErrSchemaUnsupported corresponds to canon exit code 3: receipt found,
	// signature valid, but SchemaVersion or SignatureAlg is not understood
	// by this binary.
	ErrSchemaUnsupported = errors.New("receipt schema unsupported")

	// ErrKeyNotFound corresponds to canon exit code 4: receipt found,
	// signature uses a KeyID not present in the local keystore (verification
	// not possible; distinct from invalid).
	ErrKeyNotFound = errors.New("receipt signing key not in keystore")

	// ErrInvalidSignature corresponds to canon exit code 2: canonical bytes
	// do not match the HMAC. Compared in constant time per canon
	// §Canonicalization "A verifier MUST … compare HMAC outputs in constant
	// time."
	ErrInvalidSignature = errors.New("receipt signature invalid")
)

// Sign computes the HMAC over the canonical receipt bytes, populates the
// unsigned envelope on r, and returns the canonical byte slice (so callers
// can persist it as the payload_canonical BLOB per canon §Storage and avoid
// re-canonicalizing at verify time).
//
// The Receipt is mutated in place: Signature, SignatureAlg, KeyID, and
// SignedAt are set.
//
// now is injected for deterministic testing. Production callers pass
// time.Now(). If r.SchemaVersion is zero it is set to the current
// SchemaVersion constant; explicit non-zero values are preserved (used by
// tests that pin alternate schema versions).
func Sign(r *Receipt, store KeyStore, now time.Time) ([]byte, error) {
	if r.SchemaVersion == 0 {
		r.SchemaVersion = SchemaVersion
	}
	keyID, key, err := store.Active(r.AgentID)
	if err != nil {
		return nil, fmt.Errorf("active key for %q: %w", r.AgentID, err)
	}
	canon := Canonicalize(r)
	mac := hmac.New(sha256.New, key)
	mac.Write(canon)
	sig := mac.Sum(nil)

	r.Signature = hex.EncodeToString(sig)
	r.SignatureAlg = SignatureAlg
	r.KeyID = keyID
	r.SignedAt = FormatTimestamp(now)
	return canon, nil
}

// Verify recomputes the HMAC over the canonical receipt bytes and compares
// against the receipt's signature in constant time. Returns nil on a valid
// signature; otherwise one of the sentinel errors above (wrapped with detail).
//
// Critical rule from canon §Verification semantics: "Never silently accept
// an unverifiable receipt as valid." Callers must surface non-nil errors to
// the operator. The default-deny stance from Gridfire-v1 applies symmetrically.
func Verify(r *Receipt, store KeyStore) error {
	if r.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: schema_version got %d, supported %d",
			ErrSchemaUnsupported, r.SchemaVersion, SchemaVersion)
	}
	if r.SignatureAlg != SignatureAlg {
		return fmt.Errorf("%w: signature_alg %q", ErrSchemaUnsupported, r.SignatureAlg)
	}
	key, err := store.Get(r.KeyID)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrKeyNotFound, r.KeyID)
	}
	expected, err := hex.DecodeString(r.Signature)
	if err != nil {
		return fmt.Errorf("%w: signature is not hex", ErrInvalidSignature)
	}
	canon := Canonicalize(r)
	mac := hmac.New(sha256.New, key)
	mac.Write(canon)
	actual := mac.Sum(nil)
	if subtle.ConstantTimeCompare(expected, actual) != 1 {
		return ErrInvalidSignature
	}
	return nil
}
