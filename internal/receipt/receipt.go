// Package receipt implements signed action receipts per
// docs/canon/signed-receipts-v1.md.
//
// A receipt is an HMAC-SHA256-signed JSON artifact emitted when a Sylveste
// agent completes an action. Signing uses a per-agent key from a KeyStore;
// canonicalization is custom (byte-deterministic, strict field order) because
// encoding/json sorts struct fields alphabetically and is not suitable as
// cryptographic input.
//
// See docs/canon/signed-receipts-v1.md for the full schema, canonicalization
// rules, key handling, verification semantics, and trust model. This package
// implements acceptance criterion #1 of bead sylveste-ewy3.5 (canon spec
// landing) and is the substrate for the Dolt table, verify CLI, and routing
// calibration follow-ups (sylveste-ewy3.5.2 / .3 / .4).
package receipt

const (
	// SchemaVersion is the receipt-schema version this package emits.
	SchemaVersion = 1

	// SignatureAlg is the algorithm identifier written into the unsigned
	// envelope. v2 will add "ed25519-v2" alongside this value.
	SignatureAlg = "hmac-sha256-v1"

	// IDPrefix is the receipt_id prefix per canon §Receipt schema.
	IDPrefix = "rcpt_"
)

// ToolCall is one entry of a Receipt's signed tool_calls array.
//
// Field order is fixed by canon §Canonicalization: name, args_hash,
// result_hash, duration_ms. The canonical encoder honors this order
// regardless of struct-field declaration; this struct just mirrors it for
// readability.
type ToolCall struct {
	Name       string
	ArgsHash   string
	ResultHash string
	DurationMs int64
}

// Receipt is a v1 signed action receipt.
//
// The first 8 fields below are the "signed fields" covered by HMAC-SHA256.
// Their order is normative — see canon §Receipt schema. The Canonicalize
// function emits them in this order.
//
// The unsigned envelope (Signature, SignatureAlg, KeyID, SignedAt) is
// populated by Sign and consulted by Verify; it is never included in the
// canonical byte stream.
//
// ParentRunID is a pointer so that a literal nil distinguishes "sprint-root
// receipt" from "child receipt of unknown parent". The canonical encoding
// of nil is the JSON literal null.
type Receipt struct {
	// Signed fields, in canon-declared order.
	ReceiptID     string
	Timestamp     string
	AgentID       string
	Model         string
	ToolCalls     []ToolCall
	ParentRunID   *string
	ContentHash   string
	SchemaVersion int

	// Unsigned envelope.
	Signature    string
	SignatureAlg string
	KeyID        string
	SignedAt     string
}
