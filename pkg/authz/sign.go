package authz

import (
	"crypto/ed25519"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// SignRow is the canonical projection of one authorizations row for signing.
// Every field is a string or int64 — the row's stored representation. SQL
// NULLs MUST be passed as empty strings; this is the canonical-payload
// encoding rule (see docs/canon/authz-signing-payload.md).
type SignRow struct {
	ID             string
	OpType         string
	Target         string
	AgentID        string
	BeadID         string // empty if NULL
	Mode           string
	PolicyMatch    string // empty if NULL
	PolicyHash     string // empty if NULL
	VettedSHA      string // empty if NULL
	Vetting        string // empty if NULL; stored JSON bytes verbatim
	CrossProjectID string // empty if NULL
	CreatedAt      int64
}

// signedFields is the strict ordered field list for CanonicalPayload.
// Do not reorder. A new field requires a new sig_version and a parallel
// signing path, not a mutation of this list.
var signedFields = []string{
	"id",
	"op_type",
	"target",
	"agent_id",
	"bead_id",
	"mode",
	"policy_match",
	"policy_hash",
	"vetted_sha",
	"vetting",
	"cross_project_id",
	"created_at",
}

// CanonicalPayload returns the exact byte sequence that Ed25519 signs for
// a given SignRow. See docs/canon/authz-signing-payload.md for the spec.
//
// Format: 12 fields, NFC-normalized, joined by LF (0x0A), no trailing LF,
// no BOM, UTF-8 only. NULLs (empty strings) encode as zero bytes at their
// positions. Integers use decimal with no leading zeros / plus sign.
//
// Rejects rows containing \r (0x0D) or control characters in [0x00, 0x1F]
// except LF in any text field. Callers must strip these at insertion time,
// not at sign time — the signer refuses rather than silently transliterating.
func CanonicalPayload(row SignRow) ([]byte, error) {
	parts := make([]string, len(signedFields))
	for i, name := range signedFields {
		v, err := row.fieldBytes(name)
		if err != nil {
			return nil, fmt.Errorf("canonical payload: %s: %w", name, err)
		}
		parts[i] = v
	}
	return []byte(strings.Join(parts, "\n")), nil
}

func (r SignRow) fieldBytes(name string) (string, error) {
	switch name {
	case "id":
		return validateText(r.ID)
	case "op_type":
		return validateText(r.OpType)
	case "target":
		return validateText(r.Target)
	case "agent_id":
		return validateText(r.AgentID)
	case "bead_id":
		return validateText(r.BeadID)
	case "mode":
		return validateText(r.Mode)
	case "policy_match":
		return validateText(r.PolicyMatch)
	case "policy_hash":
		return validateText(r.PolicyHash)
	case "vetted_sha":
		return validateText(r.VettedSHA)
	case "vetting":
		// vetting is stored JSON verbatim — NFC is NOT re-applied at sign
		// time (see canon spec: "use the stored bytes exactly"). Only the
		// control-character rejection applies.
		if err := rejectControlChars(r.Vetting); err != nil {
			return "", err
		}
		return r.Vetting, nil
	case "cross_project_id":
		return validateText(r.CrossProjectID)
	case "created_at":
		if r.CreatedAt < 0 {
			return "", fmt.Errorf("created_at negative: %d", r.CreatedAt)
		}
		return strconv.FormatInt(r.CreatedAt, 10), nil
	default:
		return "", fmt.Errorf("unknown field: %q", name)
	}
}

// validateText NFC-normalizes s, then rejects any CR or non-LF control char.
func validateText(s string) (string, error) {
	normed := norm.NFC.String(s)
	if err := rejectControlChars(normed); err != nil {
		return "", err
	}
	return normed, nil
}

func rejectControlChars(s string) error {
	for i, r := range s {
		if r == '\n' {
			continue // LF is allowed; it is the separator between fields.
		}
		if r < 0x20 || r == 0x7F {
			return fmt.Errorf("control character 0x%02x at byte offset %d", r, i)
		}
	}
	return nil
}

// Sign returns the 64-byte Ed25519 signature over CanonicalPayload(row).
// Returns an error only if the payload itself is invalid (control chars,
// negative created_at, etc.); the ed25519.Sign call cannot otherwise fail.
func Sign(priv ed25519.PrivateKey, row SignRow) ([]byte, error) {
	payload, err := CanonicalPayload(row)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, payload), nil
}

// Verify returns true iff sig is a valid Ed25519 signature over
// CanonicalPayload(row) under pub. Returns false (not an error) when the
// payload is invalid or the signature length is wrong — a verify-false is
// always the outcome for a row that cannot be trusted.
func Verify(pub ed25519.PublicKey, row SignRow, sig []byte) bool {
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	payload, err := CanonicalPayload(row)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, payload, sig)
}
