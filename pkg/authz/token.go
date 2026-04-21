package authz

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
)

// Token mirrors one authz_tokens row. All nullable DB fields are represented
// as empty strings or zero so CanonicalTokenPayload can treat them uniformly
// (the canonical payload encodes SQL NULL as an empty segment; see
// docs/canon/authz-token-payload.md §Encoding rules).
//
// ConsumedAt and RevokedAt are NOT part of the signed payload — they mutate
// post-sign as the token moves through its lifecycle (see
// docs/canon/authz-token-model.md §Lifecycle).
type Token struct {
	ID          string // ULID (26 chars Crockford base32)
	OpType      string
	Target      string
	AgentID     string
	BeadID      string // empty if NULL in DB
	DelegateTo  string // empty if NULL (root tokens); recipient agent on delegations
	ExpiresAt   int64  // unix seconds
	ConsumedAt  int64  // 0 if NULL; not signed
	RevokedAt   int64  // 0 if NULL; not signed
	IssuedBy    string
	ParentToken string // empty if NULL (root)
	RootToken   string // empty if NULL (root); parent's root or parent.ID on delegate
	Depth       int    // 0 for root; capped at 3
	SigVersion  int    // always 2 for v2 tokens
	Signature   []byte
	CreatedAt   int64 // unix seconds
}

// tokenSignedFields pins the 12-field order for sig_version=2. Do not reorder.
// A schema change that adds or removes fields requires sig_version=3 and a
// parallel signer path; the old path continues to sign the old field set.
// Keep aligned with docs/canon/authz-token-payload.md §1.
var tokenSignedFields = []string{
	"id", "op_type", "target", "agent_id", "bead_id",
	"delegate_to", "expires_at", "issued_by", "parent_token",
	"root_token", "depth", "created_at",
}

// Error sentinels. Each maps to a CLI exit class via ExitCode. The wrapper
// discriminates on class, never on sentinel identity — new sentinels can be
// added without wrapper changes as long as ExitCode() maps them correctly.
var (
	// Token-state class (CLI exit 2 — passive state drift; wrapper falls
	// through to legacy policy check).
	ErrAlreadyConsumed = errors.New("authz-token: already consumed")
	ErrExpired         = errors.New("authz-token: expired")

	// Not-found class (CLI exit 3 — wrapper falls through to legacy check).
	ErrNotFound       = errors.New("authz-token: not found")
	ErrBadTokenString = errors.New("authz-token: malformed token string")

	// Auth-failure class (CLI exit 4 — wrapper HARD-FAILS; no legacy
	// fall-through). Operator intent or cryptographic trust is broken; falling
	// through would silently override the signal.
	ErrSigVerify           = errors.New("authz-token: signature verification failed")
	ErrProofOfPossession   = errors.New("authz-token: caller agent_id does not match parent token agent_id (delegate)")
	ErrCallerAgentMismatch = errors.New("authz-token: caller agent_id does not match token agent_id (consume)")
	ErrCrossProject        = errors.New("authz-token: cross-project consumption not permitted in v2")
	ErrScopeWidening       = errors.New("authz-token: child scope must not widen parent scope")
	ErrDepthExceeded       = errors.New("authz-token: delegation depth cap (3) exceeded")
	ErrExpectMismatch      = errors.New("authz-token: --expect-op/--expect-target did not match token scope")
	ErrRevoked             = errors.New("authz-token: revoked — operator explicitly invalidated this token")
	ErrCascadeOnNonRoot    = errors.New("authz-token: --cascade only allowed on root tokens in v2")
)

// ExitCode maps library errors to the 5-class CLI exit space:
//   - 0 = nil error
//   - 1 = unexpected (unwrapped IO / DB / programming error)
//   - 2 = token-state (passive; falls through to legacy)
//   - 3 = not-found (falls through to legacy)
//   - 4 = auth-failure (hard fail; do not fall through)
//
// Wrappers discriminate on exit code, never on sentinel identity.
//
// r3 change: ErrRevoked is exit 4 (auth-failure), NOT exit 2 (token-state).
// An operator-invoked revoke is a stronger intent signal than natural expiry;
// falling through to legacy policy would let a legacy rule silently override
// the revoke intent.
func ExitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, ErrAlreadyConsumed),
		errors.Is(err, ErrExpired):
		return 2
	case errors.Is(err, ErrNotFound),
		errors.Is(err, ErrBadTokenString):
		return 3
	case errors.Is(err, ErrSigVerify),
		errors.Is(err, ErrProofOfPossession),
		errors.Is(err, ErrCallerAgentMismatch),
		errors.Is(err, ErrCrossProject),
		errors.Is(err, ErrScopeWidening),
		errors.Is(err, ErrDepthExceeded),
		errors.Is(err, ErrExpectMismatch),
		errors.Is(err, ErrRevoked),
		errors.Is(err, ErrCascadeOnNonRoot):
		return 4
	default:
		return 1
	}
}

// ErrClass returns a stable stderr classifier string for the CLI
// `ERROR <class>: <reason>` emission pattern. Library callers should not
// branch on this string — branch on ExitCode or errors.Is.
func ErrClass(err error) string {
	switch ExitCode(err) {
	case 0:
		return "ok"
	case 2:
		return "token-state"
	case 3:
		return "not-found"
	case 4:
		return "auth-failure"
	case 1:
		return "unexpected"
	default:
		return "unknown"
	}
}

// CanonicalTokenPayload returns the exact byte sequence that Ed25519 signs for
// a given Token under sig_version=2. See docs/canon/authz-token-payload.md.
//
// Format: 12 fields, NFC-normalized, joined by LF (0x0A), no trailing LF,
// no BOM, UTF-8 only. SQL NULL and empty strings encode identically (zero
// bytes between delimiters). Integers use decimal with no leading zeros or
// plus sign. CR and non-LF control characters are rejected — the signer
// refuses rather than silently transliterating (strip at insertion time,
// not at sign time).
func CanonicalTokenPayload(t Token) ([]byte, error) {
	parts := make([]string, len(tokenSignedFields))
	for i, name := range tokenSignedFields {
		v, err := t.fieldBytes(name)
		if err != nil {
			return nil, fmt.Errorf("canonical token payload: %s: %w", name, err)
		}
		parts[i] = v
	}
	return []byte(strings.Join(parts, "\n")), nil
}

func (t Token) fieldBytes(name string) (string, error) {
	switch name {
	case "id":
		return validateText(t.ID)
	case "op_type":
		return validateText(t.OpType)
	case "target":
		return validateText(t.Target)
	case "agent_id":
		return validateText(t.AgentID)
	case "bead_id":
		return validateText(t.BeadID)
	case "delegate_to":
		return validateText(t.DelegateTo)
	case "expires_at":
		if t.ExpiresAt < 0 {
			return "", fmt.Errorf("expires_at negative: %d", t.ExpiresAt)
		}
		return strconv.FormatInt(t.ExpiresAt, 10), nil
	case "issued_by":
		return validateText(t.IssuedBy)
	case "parent_token":
		return validateText(t.ParentToken)
	case "root_token":
		return validateText(t.RootToken)
	case "depth":
		if t.Depth < 0 || t.Depth > 3 {
			return "", fmt.Errorf("depth out of range [0,3]: %d", t.Depth)
		}
		return strconv.Itoa(t.Depth), nil
	case "created_at":
		if t.CreatedAt < 0 {
			return "", fmt.Errorf("created_at negative: %d", t.CreatedAt)
		}
		return strconv.FormatInt(t.CreatedAt, 10), nil
	default:
		return "", fmt.Errorf("unknown field: %q", name)
	}
}

// SignToken returns the 64-byte Ed25519 signature over CanonicalTokenPayload(t).
// Errors only if the payload itself is invalid (control chars, negative
// timestamp, out-of-range depth); ed25519.Sign itself cannot fail.
func SignToken(priv ed25519.PrivateKey, t Token) ([]byte, error) {
	payload, err := CanonicalTokenPayload(t)
	if err != nil {
		return nil, err
	}
	return ed25519.Sign(priv, payload), nil
}

// VerifyToken returns true iff sig is a valid Ed25519 signature over
// CanonicalTokenPayload(t) under pub. Returns false (not error) on wrong
// signature length or invalid payload — the outcome for an untrustable token
// is always verify-false, never a distinguished error.
func VerifyToken(pub ed25519.PublicKey, t Token, sig []byte) bool {
	if len(sig) != ed25519.SignatureSize {
		return false
	}
	payload, err := CanonicalTokenPayload(t)
	if err != nil {
		return false
	}
	return ed25519.Verify(pub, payload, sig)
}

// EncodeTokenString renders the opaque `<ulid>.<sighex>` form carried via
// $CLAVAIN_AUTHZ_TOKEN. ULID is 26 chars Crockford base32; sighex is 128
// chars lowercase hex (64 bytes × 2).
func EncodeTokenString(id string, sig []byte) string {
	return id + "." + hex.EncodeToString(sig)
}

// ParseTokenString splits the opaque form into (id, sig). Exactly one "."
// separator is expected; the ULID is validated via ulid.Parse; the hex is
// decoded and the length checked against ed25519.SignatureSize.
//
// All error paths wrap ErrBadTokenString so errors.Is(err, ErrBadTokenString)
// matches — wrappers map exit code 3 without inspecting the underlying
// cause.
func ParseTokenString(s string) (string, []byte, error) {
	idx := strings.IndexByte(s, '.')
	if idx < 0 {
		return "", nil, fmt.Errorf("%w: missing '.' separator", ErrBadTokenString)
	}
	id, sigHex := s[:idx], s[idx+1:]
	if _, err := ulid.Parse(id); err != nil {
		return "", nil, fmt.Errorf("%w: invalid ulid: %v", ErrBadTokenString, err)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return "", nil, fmt.Errorf("%w: invalid hex: %v", ErrBadTokenString, err)
	}
	if len(sig) != ed25519.SignatureSize {
		return "", nil, fmt.Errorf("%w: signature length %d, want %d", ErrBadTokenString, len(sig), ed25519.SignatureSize)
	}
	return id, sig, nil
}

// IssueSpec is the input to IssueToken. All fields are required except BeadID.
// TTL is the requested lifetime; expires_at = now + TTL.
type IssueSpec struct {
	OpType   string
	Target   string
	AgentID  string        // the agent that will present this token at consume time
	BeadID   string        // optional scope to a bead; empty for no bead scope
	IssuedBy string        // agent id of issuer, or "user" for CLI-initiated
	TTL      time.Duration // requested lifetime; must be positive
}

// DelegateSpec is the input to DelegateToken. Scope narrowing is API-level:
// no ChildOpType / ChildTarget fields — a future version that adds them would
// be a conscious design change, not a flag flip.
type DelegateSpec struct {
	ParentID      string        // ULID of parent token
	CallerAgentID string        // the parent-holder's agent id; proves POP
	ToAgentID     string        // recipient (child) agent
	RequestedTTL  time.Duration // clamped against parent remaining
}

// ListFilter narrows ListTokens results. Zero-valued fields are ignored.
//
// Status values: "" (all), "consumable", "consumed", "revoked", "expired".
// "consumable" and "expired" require Now to be set (unix seconds).
type ListFilter struct {
	RootToken string // match id OR root_token (i.e., the full subtree of a root)
	AgentID   string
	OpType    string
	Status    string
	Now       int64 // required for Status in {"consumable","expired"}
}

// IssueToken generates a ULID, signs the canonical payload, inserts the row,
// and returns the Token plus the opaque `<ulid>.<sighex>` string.
//
// Validation: all IssueSpec fields except BeadID are required; TTL and now
// must be positive. The issued token is a root (depth=0, parent_token NULL,
// root_token NULL).
func IssueToken(db *sql.DB, priv ed25519.PrivateKey, spec IssueSpec, now int64) (Token, string, error) {
	if db == nil {
		return Token{}, "", errors.New("authz: nil db")
	}
	if spec.OpType == "" {
		return Token{}, "", errors.New("authz: IssueSpec.OpType required")
	}
	if spec.Target == "" {
		return Token{}, "", errors.New("authz: IssueSpec.Target required")
	}
	if spec.AgentID == "" {
		return Token{}, "", errors.New("authz: IssueSpec.AgentID required")
	}
	if spec.IssuedBy == "" {
		return Token{}, "", errors.New("authz: IssueSpec.IssuedBy required")
	}
	if spec.TTL <= 0 {
		return Token{}, "", errors.New("authz: IssueSpec.TTL must be positive")
	}
	if now <= 0 {
		return Token{}, "", errors.New("authz: now must be positive")
	}

	t := Token{
		ID:         ulid.Make().String(),
		OpType:     spec.OpType,
		Target:     spec.Target,
		AgentID:    spec.AgentID,
		BeadID:     spec.BeadID,
		ExpiresAt:  now + int64(spec.TTL/time.Second),
		IssuedBy:   spec.IssuedBy,
		Depth:      0,
		SigVersion: 2,
		CreatedAt:  now,
	}
	sig, err := SignToken(priv, t)
	if err != nil {
		return Token{}, "", fmt.Errorf("sign token: %w", err)
	}
	t.Signature = sig

	_, err = db.Exec(`
		INSERT INTO authz_tokens (
			id, op_type, target, agent_id, bead_id, delegate_to,
			expires_at, consumed_at, revoked_at, issued_by,
			parent_token, root_token, depth, sig_version,
			signature, created_at
		) VALUES (?, ?, ?, ?, ?, NULL, ?, NULL, NULL, ?, NULL, NULL, 0, 2, ?, ?)
	`, t.ID, t.OpType, t.Target, t.AgentID, nullIfEmpty(t.BeadID),
		t.ExpiresAt, t.IssuedBy, sig, t.CreatedAt)
	if err != nil {
		return Token{}, "", fmt.Errorf("insert token: %w", err)
	}
	return t, EncodeTokenString(t.ID, sig), nil
}

// DelegateToken creates a child token narrower than or equal to its parent.
//
// Enforced invariants:
//   - POP: spec.CallerAgentID == parent.AgentID (else ErrProofOfPossession).
//   - Depth cap: parent.Depth + 1 ≤ 3 (else ErrDepthExceeded); re-checked
//     inside the insert transaction against TOCTOU races.
//   - Scope: child op_type/target/bead_id are copied from parent; scope
//     narrowing at API level (DelegateSpec has no override fields).
//   - TTL clamp: child.ExpiresAt = min(now + RequestedTTL, parent.ExpiresAt).
//
// root_token is denormalized: parent.RootToken if set, else parent.ID (parent
// IS the root). This makes cascade revoke on a root a single UPDATE scan.
func DelegateToken(db *sql.DB, priv ed25519.PrivateKey, spec DelegateSpec, now int64) (Token, string, error) {
	if db == nil {
		return Token{}, "", errors.New("authz: nil db")
	}
	if spec.ParentID == "" {
		return Token{}, "", errors.New("authz: DelegateSpec.ParentID required")
	}
	if spec.CallerAgentID == "" {
		return Token{}, "", errors.New("authz: DelegateSpec.CallerAgentID required")
	}
	if spec.ToAgentID == "" {
		return Token{}, "", errors.New("authz: DelegateSpec.ToAgentID required")
	}
	if spec.RequestedTTL <= 0 {
		return Token{}, "", errors.New("authz: DelegateSpec.RequestedTTL must be positive")
	}
	if now <= 0 {
		return Token{}, "", errors.New("authz: now must be positive")
	}

	parent, err := GetToken(db, spec.ParentID)
	if err != nil {
		return Token{}, "", err // ErrNotFound when parent missing
	}
	if spec.CallerAgentID != parent.AgentID {
		return Token{}, "", ErrProofOfPossession
	}
	if parent.Depth+1 > 3 {
		return Token{}, "", ErrDepthExceeded
	}

	expiresAt := now + int64(spec.RequestedTTL/time.Second)
	if expiresAt > parent.ExpiresAt {
		expiresAt = parent.ExpiresAt
	}

	rootToken := parent.RootToken
	if rootToken == "" {
		rootToken = parent.ID // parent IS the root
	}

	child := Token{
		ID:          ulid.Make().String(),
		OpType:      parent.OpType,
		Target:      parent.Target,
		AgentID:     spec.ToAgentID,
		BeadID:      parent.BeadID,
		DelegateTo:  spec.ToAgentID,
		ExpiresAt:   expiresAt,
		IssuedBy:    spec.CallerAgentID,
		ParentToken: parent.ID,
		RootToken:   rootToken,
		Depth:       parent.Depth + 1,
		SigVersion:  2,
		CreatedAt:   now,
	}
	sig, err := SignToken(priv, child)
	if err != nil {
		return Token{}, "", fmt.Errorf("sign token: %w", err)
	}
	child.Signature = sig

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return Token{}, "", fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var parentDepth int
	if err := tx.QueryRow(
		`SELECT depth FROM authz_tokens WHERE id = ?`, parent.ID,
	).Scan(&parentDepth); err != nil {
		return Token{}, "", fmt.Errorf("re-select parent depth: %w", err)
	}
	if parentDepth+1 > 3 {
		return Token{}, "", ErrDepthExceeded
	}

	_, err = tx.Exec(`
		INSERT INTO authz_tokens (
			id, op_type, target, agent_id, bead_id, delegate_to,
			expires_at, consumed_at, revoked_at, issued_by,
			parent_token, root_token, depth, sig_version,
			signature, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?, ?, 2, ?, ?)
	`, child.ID, child.OpType, child.Target, child.AgentID, nullIfEmpty(child.BeadID),
		child.DelegateTo, child.ExpiresAt, child.IssuedBy,
		child.ParentToken, child.RootToken, child.Depth, sig, child.CreatedAt)
	if err != nil {
		if strings.Contains(err.Error(), "CHECK constraint") {
			return Token{}, "", ErrDepthExceeded
		}
		return Token{}, "", fmt.Errorf("insert child token: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Token{}, "", fmt.Errorf("commit: %w", err)
	}
	return child, EncodeTokenString(child.ID, sig), nil
}

// ConsumeToken verifies, atomically claims, and audits a single-use token.
//
// Pre-transaction (no writes, no DB lock): parse opaque string, load row,
// verify signature, check caller-agent match, check expect-op/target match.
// Each failure maps to a distinct error class via ExitCode.
//
// Transactional: UPDATE with 5-AND WHERE (id, consumed_at IS NULL,
// revoked_at IS NULL, expires_at > now, agent_id match); if RowsAffected ≠ 1
// re-SELECT to classify (priority: revoked > consumed > expired); then
// INSERT the v1.5-shaped audit row (sig_version=1, unsigned — a separate
// background signer fills signature/signed_at later). COMMIT atomically.
//
// Under MaxOpenConns=1, N concurrent ConsumeToken calls against the same
// token serialize naturally; exactly one UPDATE sets consumed_at, the rest
// see consumed_at IS NOT NULL and re-SELECT classifies as already-consumed.
func ConsumeToken(db *sql.DB, pub ed25519.PublicKey, tokenStr, callerAgentID, expectOp, expectTarget string, now int64) (Token, error) {
	if db == nil {
		return Token{}, errors.New("authz: nil db")
	}
	if callerAgentID == "" {
		return Token{}, errors.New("authz: callerAgentID required")
	}
	if now <= 0 {
		return Token{}, errors.New("authz: now must be positive")
	}

	id, sig, err := ParseTokenString(tokenStr)
	if err != nil {
		return Token{}, err
	}

	t, err := GetToken(db, id)
	if err != nil {
		return Token{}, err // ErrNotFound when absent
	}

	if !VerifyToken(pub, t, sig) {
		return Token{}, ErrSigVerify
	}

	if callerAgentID != t.AgentID {
		return Token{}, ErrCallerAgentMismatch
	}

	if expectOp != "" && expectOp != t.OpType {
		return Token{}, ErrExpectMismatch
	}
	if expectTarget != "" && expectTarget != t.Target {
		return Token{}, ErrExpectMismatch
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return Token{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`
		UPDATE authz_tokens
		   SET consumed_at = ?
		 WHERE id = ?
		   AND consumed_at IS NULL
		   AND revoked_at IS NULL
		   AND expires_at > ?
		   AND agent_id = ?
	`, now, id, now, callerAgentID)
	if err != nil {
		return Token{}, fmt.Errorf("update token: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return Token{}, fmt.Errorf("rows affected: %w", err)
	}
	if n != 1 {
		return Token{}, classifyConsumeFailure(tx, id, now)
	}

	// Fault injection hook — nil in production builds; the testfault build
	// tag wires a function that consults CONSUME_FAULT_INJECT_AFTER_UPDATE
	// so TestConsumeToken_PartialFailure_Atomic can force the audit INSERT
	// to fail and verify the UPDATE rolls back (partial-failure invariant).
	if consumeFaultHook != nil {
		if err := consumeFaultHook(); err != nil {
			return Token{}, fmt.Errorf("consume fault: %w", err)
		}
	}

	vetting := map[string]any{
		"via":      "token",
		"token_id": t.ID,
		"depth":    t.Depth,
	}
	if t.RootToken != "" {
		vetting["root_token"] = t.RootToken
	} else {
		vetting["root_token"] = nil
	}
	vettingBytes, err := json.Marshal(vetting)
	if err != nil {
		return Token{}, fmt.Errorf("marshal vetting: %w", err)
	}

	auditID := ulid.Make().String()
	_, err = tx.Exec(`
		INSERT INTO authorizations (
			id, op_type, target, agent_id, bead_id, mode,
			policy_match, policy_hash, vetted_sha, vetting,
			cross_project_id, created_at, sig_version
		) VALUES (?, ?, ?, ?, ?, 'auto', NULL, NULL, NULL, ?, NULL, ?, 1)
	`, auditID, t.OpType, t.Target, callerAgentID, nullIfEmpty(t.BeadID),
		string(vettingBytes), now)
	if err != nil {
		return Token{}, fmt.Errorf("insert audit row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Token{}, fmt.Errorf("commit: %w", err)
	}
	t.ConsumedAt = now
	return t, nil
}

// consumeFaultHook is nil in production. The testfault build tag wires an
// init() that sets this to a function reading CONSUME_FAULT_INJECT_AFTER_UPDATE.
var consumeFaultHook func() error

func classifyConsumeFailure(tx *sql.Tx, id string, now int64) error {
	var consumedAt, revokedAt sql.NullInt64
	var expiresAt int64
	err := tx.QueryRow(
		`SELECT consumed_at, revoked_at, expires_at FROM authz_tokens WHERE id = ?`, id,
	).Scan(&consumedAt, &revokedAt, &expiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("classify consume failure: %w", err)
	}
	// Priority: revoked > consumed > expired. A row can be in multiple
	// terminal states (consumed-then-revoked for audit intent); report the
	// stronger operator-intent signal first.
	if revokedAt.Valid {
		return ErrRevoked
	}
	if consumedAt.Valid {
		return ErrAlreadyConsumed
	}
	if expiresAt <= now {
		return ErrExpired
	}
	// Row exists, is unconsumed, not revoked, not expired — but UPDATE
	// affected 0 rows. Agent-mismatch was caught pre-tx. Conservative
	// classification: another actor won the concurrent consume race.
	return ErrAlreadyConsumed
}

// RevokeToken flags a token (and optionally its descendants) as revoked.
//
// Non-cascade: UPDATE WHERE id=? AND revoked_at IS NULL. Works for any node
// (root or non-root); only the target row is flagged. Idempotent via the
// NULL predicate.
//
// Cascade: first verifies the target is a root (parent_token IS NULL AND
// root_token IS NULL); else returns ErrCascadeOnNonRoot without writing.
// Then UPDATE WHERE (id=? OR root_token=?) AND revoked_at IS NULL, binding
// target.id to both positions. Descendants denormalize root_token to the
// chain root, so the predicate correctly flags the root plus every
// descendant. This avoids the SQL-NULL-never-equals-NULL pitfall that a
// naive root_token = target.root_token predicate falls into.
//
// Mid-chain cascade is refused in v2 because denormalized root_token points
// at the chain root, not at immediate ancestors; correct mid-chain cascade
// requires either a recursive CTE or a schema change (ancestors TEXT[]).
// Both are v2.x concerns. Returning ErrCascadeOnNonRoot is safer than
// silently half-revoking.
//
// Returns the number of rows whose revoked_at transitioned from NULL to now.
// Idempotent: a second cascade revoke of the same root returns 0.
func RevokeToken(db *sql.DB, tokenID string, cascade bool, now int64) (int, error) {
	if db == nil {
		return 0, errors.New("authz: nil db")
	}
	if tokenID == "" {
		return 0, errors.New("authz: tokenID required")
	}
	if now <= 0 {
		return 0, errors.New("authz: now must be positive")
	}

	if !cascade {
		res, err := db.Exec(
			`UPDATE authz_tokens SET revoked_at = ? WHERE id = ? AND revoked_at IS NULL`,
			now, tokenID,
		)
		if err != nil {
			return 0, fmt.Errorf("revoke: %w", err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			var count int
			if err := db.QueryRow(
				`SELECT COUNT(*) FROM authz_tokens WHERE id = ?`, tokenID,
			).Scan(&count); err != nil {
				return 0, fmt.Errorf("existence check: %w", err)
			}
			if count == 0 {
				return 0, ErrNotFound
			}
			// Row exists and n==0 → already revoked; idempotent.
		}
		return int(n), nil
	}

	target, err := GetToken(db, tokenID)
	if err != nil {
		return 0, err
	}
	if target.ParentToken != "" || target.RootToken != "" {
		return 0, ErrCascadeOnNonRoot
	}

	res, err := db.Exec(`
		UPDATE authz_tokens SET revoked_at = ?
		 WHERE (id = ? OR root_token = ?) AND revoked_at IS NULL
	`, now, tokenID, tokenID)
	if err != nil {
		return 0, fmt.Errorf("cascade revoke: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetToken loads one token row by id. Returns ErrNotFound when the row is
// absent. Nullable DB columns are scanned via COALESCE so the returned
// Token's empty-string/zero representation matches the canonical-payload
// encoding rules.
func GetToken(db *sql.DB, tokenID string) (Token, error) {
	if db == nil {
		return Token{}, errors.New("authz: nil db")
	}
	if tokenID == "" {
		return Token{}, errors.New("authz: tokenID required")
	}
	var t Token
	err := db.QueryRow(tokenSelectSQL+` WHERE id = ?`, tokenID).Scan(tokenScanDest(&t)...)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Token{}, ErrNotFound
		}
		return Token{}, fmt.Errorf("get token: %w", err)
	}
	return t, nil
}

// ListTokens returns matching rows ordered by created_at ascending. An empty
// filter lists all tokens. Status="consumable" and Status="expired" require
// Now to be set; unset Now in those modes returns an error rather than
// silently treating 0 as the reference time.
func ListTokens(db *sql.DB, filter ListFilter) ([]Token, error) {
	if db == nil {
		return nil, errors.New("authz: nil db")
	}
	var conds []string
	var args []any

	if filter.RootToken != "" {
		conds = append(conds, "(id = ? OR root_token = ?)")
		args = append(args, filter.RootToken, filter.RootToken)
	}
	if filter.AgentID != "" {
		conds = append(conds, "agent_id = ?")
		args = append(args, filter.AgentID)
	}
	if filter.OpType != "" {
		conds = append(conds, "op_type = ?")
		args = append(args, filter.OpType)
	}
	switch filter.Status {
	case "":
		// no status filter
	case "consumable":
		if filter.Now <= 0 {
			return nil, errors.New("authz: ListFilter.Now required for status=consumable")
		}
		conds = append(conds, "consumed_at IS NULL AND revoked_at IS NULL AND expires_at > ?")
		args = append(args, filter.Now)
	case "consumed":
		conds = append(conds, "consumed_at IS NOT NULL")
	case "revoked":
		conds = append(conds, "revoked_at IS NOT NULL")
	case "expired":
		if filter.Now <= 0 {
			return nil, errors.New("authz: ListFilter.Now required for status=expired")
		}
		conds = append(conds, "consumed_at IS NULL AND revoked_at IS NULL AND expires_at <= ?")
		args = append(args, filter.Now)
	default:
		return nil, fmt.Errorf("authz: invalid status filter: %q", filter.Status)
	}

	q := tokenSelectSQL
	if len(conds) > 0 {
		q += " WHERE " + strings.Join(conds, " AND ")
	}
	q += " ORDER BY created_at ASC"

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(tokenScanDest(&t)...); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// tokenSelectSQL is the projection used by GetToken and ListTokens.
// Nullable columns are wrapped in COALESCE so the scan target is plain
// types (string / int64) rather than sql.NullString / sql.NullInt64.
const tokenSelectSQL = `
	SELECT id, op_type, target, agent_id,
	       COALESCE(bead_id, ''),
	       COALESCE(delegate_to, ''),
	       expires_at,
	       COALESCE(consumed_at, 0),
	       COALESCE(revoked_at, 0),
	       issued_by,
	       COALESCE(parent_token, ''),
	       COALESCE(root_token, ''),
	       depth, sig_version, signature, created_at
	FROM authz_tokens`

func tokenScanDest(t *Token) []any {
	return []any{
		&t.ID, &t.OpType, &t.Target, &t.AgentID,
		&t.BeadID, &t.DelegateTo, &t.ExpiresAt,
		&t.ConsumedAt, &t.RevokedAt,
		&t.IssuedBy, &t.ParentToken, &t.RootToken,
		&t.Depth, &t.SigVersion, &t.Signature, &t.CreatedAt,
	}
}
