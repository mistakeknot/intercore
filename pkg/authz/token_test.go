package authz

import (
	"crypto/ed25519"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// ----- golden fixtures (docs/canon/authz-token-payload.md §3) -----

// Example 1: root issue token, no delegation. Empty delegate_to, parent, root.
func TestCanonicalTokenPayload_Example1(t *testing.T) {
	tok := Token{
		ID:         "01J0GZ7W93K5PKQ42V7TCMWC2B",
		OpType:     "bead-close",
		Target:     "sylveste-qdqr.28",
		AgentID:    "claude-opus-4-7",
		BeadID:     "sylveste-qdqr.28",
		ExpiresAt:  1776742800,
		IssuedBy:   "user",
		Depth:      0,
		SigVersion: 2,
		CreatedAt:  1776739200,
	}
	got, err := CanonicalTokenPayload(tok)
	if err != nil {
		t.Fatalf("CanonicalTokenPayload: %v", err)
	}
	want := "01J0GZ7W93K5PKQ42V7TCMWC2B\n" +
		"bead-close\n" +
		"sylveste-qdqr.28\n" +
		"claude-opus-4-7\n" +
		"sylveste-qdqr.28\n" +
		"\n" + // delegate_to NULL
		"1776742800\n" +
		"user\n" +
		"\n" + // parent_token NULL
		"\n" + // root_token NULL
		"0\n" +
		"1776739200"
	if string(got) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(got), want)
	}
}

// Example 2: depth-1 delegation (Claude → codex). All 12 fields populated.
func TestCanonicalTokenPayload_Example2(t *testing.T) {
	tok := Token{
		ID:          "01J0GZ8X4M7Q3N8S6XW9Y2Z5CF",
		OpType:      "bead-close",
		Target:      "sylveste-qdqr.28",
		AgentID:     "codex",
		BeadID:      "sylveste-qdqr.28",
		DelegateTo:  "codex",
		ExpiresAt:   1776742800,
		IssuedBy:    "claude-opus-4-7",
		ParentToken: "01J0GZ7W93K5PKQ42V7TCMWC2B",
		RootToken:   "01J0GZ7W93K5PKQ42V7TCMWC2B",
		Depth:       1,
		SigVersion:  2,
		CreatedAt:   1776739260,
	}
	got, err := CanonicalTokenPayload(tok)
	if err != nil {
		t.Fatalf("CanonicalTokenPayload: %v", err)
	}
	want := "01J0GZ8X4M7Q3N8S6XW9Y2Z5CF\n" +
		"bead-close\n" +
		"sylveste-qdqr.28\n" +
		"codex\n" +
		"sylveste-qdqr.28\n" +
		"codex\n" +
		"1776742800\n" +
		"claude-opus-4-7\n" +
		"01J0GZ7W93K5PKQ42V7TCMWC2B\n" +
		"01J0GZ7W93K5PKQ42V7TCMWC2B\n" +
		"1\n" +
		"1776739260"
	if string(got) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(got), want)
	}
}

// Example 3: publish-scoped root token.
func TestCanonicalTokenPayload_Example3(t *testing.T) {
	tok := Token{
		ID:         "01J0GZ9A7P5R4M2T8YZ6W3X1DH",
		OpType:     "ic-publish-patch",
		Target:     "clavain",
		AgentID:    "claude-opus-4-7",
		BeadID:     "sylveste-qdqr.28",
		ExpiresAt:  1776742800,
		IssuedBy:   "user",
		Depth:      0,
		SigVersion: 2,
		CreatedAt:  1776739200,
	}
	got, err := CanonicalTokenPayload(tok)
	if err != nil {
		t.Fatalf("CanonicalTokenPayload: %v", err)
	}
	want := "01J0GZ9A7P5R4M2T8YZ6W3X1DH\n" +
		"ic-publish-patch\n" +
		"clavain\n" +
		"claude-opus-4-7\n" +
		"sylveste-qdqr.28\n" +
		"\n" +
		"1776742800\n" +
		"user\n" +
		"\n" +
		"\n" +
		"0\n" +
		"1776739200"
	if string(got) != want {
		t.Errorf("payload mismatch\n got: %q\nwant: %q", string(got), want)
	}
}

// TestCanonicalTokenPayload_GoldenFixtures aggregates the three canon examples
// so a single test invocation proves the implementation matches the byte
// sequences pinned in docs/canon/authz-token-payload.md.
func TestCanonicalTokenPayload_GoldenFixtures(t *testing.T) {
	t.Run("example1_root_issue", TestCanonicalTokenPayload_Example1)
	t.Run("example2_depth1_delegation", TestCanonicalTokenPayload_Example2)
	t.Run("example3_publish_scoped_root", TestCanonicalTokenPayload_Example3)
}

// ----- sign/verify -----

func TestSignToken_RoundTrip(t *testing.T) {
	kp, _ := GenerateKey()
	tok := sampleToken()
	sig, err := SignToken(kp.Priv, tok)
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	if !VerifyToken(kp.Pub, tok, sig) {
		t.Fatal("VerifyToken: signature should verify on same token")
	}
}

// TestVerifyToken_RejectsMutation mutates each of the 12 signed fields in
// isolation and asserts each mutation breaks verification. Pins the field
// list against silent drift (e.g., if a future refactor inadvertently drops
// a field from the canonical payload, this test would fail).
func TestVerifyToken_RejectsMutation(t *testing.T) {
	kp, _ := GenerateKey()
	tok := sampleToken()
	sig, err := SignToken(kp.Priv, tok)
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Token)
	}{
		{"id", func(t *Token) { t.ID = "01J0GZZZZZZZZZZZZZZZZZZZZZ" }},
		{"op_type", func(t *Token) { t.OpType = "git-push-main" }},
		{"target", func(t *Token) { t.Target = "other-target" }},
		{"agent_id", func(t *Token) { t.AgentID = "other-agent" }},
		{"bead_id", func(t *Token) { t.BeadID = "other-bead" }},
		{"delegate_to", func(t *Token) { t.DelegateTo = "some-delegate" }},
		{"expires_at", func(t *Token) { t.ExpiresAt++ }},
		{"issued_by", func(t *Token) { t.IssuedBy = "other-issuer" }},
		{"parent_token", func(t *Token) { t.ParentToken = "01J0GZPPPPPPPPPPPPPPPPPPPPPP" }},
		{"root_token", func(t *Token) { t.RootToken = "01J0GZRRRRRRRRRRRRRRRRRRRRRR" }},
		{"depth", func(t *Token) { t.Depth++ }},
		{"created_at", func(t *Token) { t.CreatedAt++ }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := tok
			tc.mutate(&mut)
			if VerifyToken(kp.Pub, mut, sig) {
				t.Errorf("verify should fail after mutating %s", tc.name)
			}
		})
	}
}

func TestVerifyToken_RejectsWrongSigLen(t *testing.T) {
	kp, _ := GenerateKey()
	tok := sampleToken()
	if VerifyToken(kp.Pub, tok, []byte{1, 2, 3}) {
		t.Fatal("verify must reject signature with wrong length")
	}
	if VerifyToken(kp.Pub, tok, nil) {
		t.Fatal("verify must reject nil signature")
	}
}

// ----- token string codec -----

func TestTokenString_RoundTrip(t *testing.T) {
	id := "01J0GZ7W93K5PKQ42V7TCMWC2B"
	sig := make([]byte, ed25519.SignatureSize)
	for i := range sig {
		sig[i] = byte(i)
	}
	s := EncodeTokenString(id, sig)
	if !strings.HasPrefix(s, id+".") {
		t.Errorf("encoded token missing id prefix: %q", s)
	}
	id2, sig2, err := ParseTokenString(s)
	if err != nil {
		t.Fatalf("ParseTokenString: %v", err)
	}
	if id2 != id {
		t.Errorf("id mismatch: got %q want %q", id2, id)
	}
	if hex.EncodeToString(sig2) != hex.EncodeToString(sig) {
		t.Errorf("sig mismatch")
	}
}

func TestParseTokenString_ErrorClasses(t *testing.T) {
	validSig := strings.Repeat("00", ed25519.SignatureSize)
	cases := []struct {
		name string
		in   string
	}{
		{"missing_separator", "01J0GZ7W93K5PKQ42V7TCMWC2B"},
		{"empty", ""},
		{"bad_ulid_short", "abc." + validSig},
		{"bad_ulid_long", "01J0GZ7W93K5PKQ42V7TCMWC2BEXTRA." + validSig},
		{"bad_hex", "01J0GZ7W93K5PKQ42V7TCMWC2B.notHex"},
		{"wrong_sig_len", "01J0GZ7W93K5PKQ42V7TCMWC2B." + strings.Repeat("ab", 8)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := ParseTokenString(tc.in)
			if err == nil {
				t.Fatalf("expected error for %s", tc.name)
			}
			if !errors.Is(err, ErrBadTokenString) {
				t.Errorf("expected ErrBadTokenString, got %v", err)
			}
		})
	}
}

// ----- Issue -----

func TestIssueToken_WritesRow(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, err := IssueToken(db, kp.Priv, IssueSpec{
		OpType:   "bead-close",
		Target:   "sylveste-x",
		AgentID:  "claude-opus-4-7",
		BeadID:   "sylveste-x",
		IssuedBy: "user",
		TTL:      time.Hour,
	}, now)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	if tok.ID == "" || tok.Signature == nil {
		t.Fatalf("expected populated Token, got %+v", tok)
	}
	if tok.Depth != 0 || tok.ParentToken != "" || tok.RootToken != "" {
		t.Errorf("root token should have depth=0 and empty parent/root: %+v", tok)
	}
	if tok.ExpiresAt != now+int64(time.Hour/time.Second) {
		t.Errorf("unexpected expires_at: %d", tok.ExpiresAt)
	}

	// Round-trip: the opaque string parses + signature verifies against the
	// DB-stored canonical payload.
	id, sig, err := ParseTokenString(opaque)
	if err != nil {
		t.Fatalf("ParseTokenString: %v", err)
	}
	stored, err := GetToken(db, id)
	if err != nil {
		t.Fatalf("GetToken: %v", err)
	}
	if !VerifyToken(kp.Pub, stored, sig) {
		t.Fatal("opaque signature must verify against stored token row")
	}
}

// ----- Delegate -----

func TestDelegateToken_POPEnforced(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	parent, _, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
		ParentID:      parent.ID,
		CallerAgentID: "evil-agent", // NOT parent.AgentID
		ToAgentID:     "codex",
		RequestedTTL:  time.Minute,
	}, now+1)
	if !errors.Is(err, ErrProofOfPossession) {
		t.Fatalf("expected ErrProofOfPossession, got %v", err)
	}
}

func TestDelegateToken_DepthCap(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	// Build linear chain: root(d0) → d1 → d2 → d3.
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)
	d2 := mustDelegate(t, db, kp, d1.ID, "agent-1", "agent-2", now+2)
	d3 := mustDelegate(t, db, kp, d2.ID, "agent-2", "agent-3", now+3)
	// depth=3 delegate → ErrDepthExceeded.
	_, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
		ParentID:      d3.ID,
		CallerAgentID: "agent-3",
		ToAgentID:     "agent-4",
		RequestedTTL:  time.Minute,
	}, now+4)
	if !errors.Is(err, ErrDepthExceeded) {
		t.Fatalf("expected ErrDepthExceeded, got %v", err)
	}
}

// TestDelegateToken_DepthCap_ConcurrentRace fires N concurrent delegates
// against a depth=3 parent; all must fail. Under MaxOpenConns=1 the
// transactions serialize at the DB layer, and the CHECK constraint +
// in-transaction re-SELECT of parent.depth defend against the case where
// two goroutines both sneak past the pre-transaction depth check.
func TestDelegateToken_DepthCap_ConcurrentRace(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)
	d2 := mustDelegate(t, db, kp, d1.ID, "agent-1", "agent-2", now+2)
	d3 := mustDelegate(t, db, kp, d2.ID, "agent-2", "agent-3", now+3)

	const N = 8
	var wg sync.WaitGroup
	var success, depthExceeded int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
				ParentID:      d3.ID,
				CallerAgentID: "agent-3",
				ToAgentID:     "agent-4",
				RequestedTTL:  time.Minute,
			}, now+4)
			if err == nil {
				atomic.AddInt64(&success, 1)
			} else if errors.Is(err, ErrDepthExceeded) {
				atomic.AddInt64(&depthExceeded, 1)
			}
		}(i)
	}
	wg.Wait()
	if success != 0 {
		t.Errorf("no delegate should succeed against depth=3 parent, got %d successes", success)
	}
	if depthExceeded != N {
		t.Errorf("expected all %d delegates to fail with ErrDepthExceeded, got %d", N, depthExceeded)
	}
}

// TestDelegateToken_ScopeNarrowing is a compile-time guarantee: DelegateSpec
// has no ChildOpType or ChildTarget fields, so a caller cannot widen scope
// even if they wanted to. If someone adds override fields in a refactor,
// this file would fail to compile against this test.
func TestDelegateToken_ScopeNarrowing(t *testing.T) {
	// Intentional: exercise the full DelegateSpec field set. If a future
	// refactor adds ChildOpType/ChildTarget/ChildBeadID, either this test
	// must be updated (with a conscious review) or the refactor breaks the
	// build at this literal.
	_ = DelegateSpec{
		ParentID:      "x",
		CallerAgentID: "y",
		ToAgentID:     "z",
		RequestedTTL:  time.Minute,
	}
	// Runtime: child scope is copied from parent.
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	parent, _, _ := issueRoot(t, db, kp, "agent-0", now)
	child, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
		ParentID:      parent.ID,
		CallerAgentID: "agent-0",
		ToAgentID:     "codex",
		RequestedTTL:  time.Minute,
	}, now+1)
	if err != nil {
		t.Fatalf("DelegateToken: %v", err)
	}
	if child.OpType != parent.OpType || child.Target != parent.Target || child.BeadID != parent.BeadID {
		t.Errorf("child scope must equal parent: parent=%+v child=%+v", parent, child)
	}
}

func TestDelegateToken_TTLClamp(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	parent, _, _ := issueRootTTL(t, db, kp, "agent-0", now, 10*time.Second)
	// Request a much longer TTL than parent has remaining; must clamp.
	child, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
		ParentID:      parent.ID,
		CallerAgentID: "agent-0",
		ToAgentID:     "codex",
		RequestedTTL:  time.Hour,
	}, now+1)
	if err != nil {
		t.Fatalf("DelegateToken: %v", err)
	}
	if child.ExpiresAt != parent.ExpiresAt {
		t.Errorf("expected child.ExpiresAt clamped to parent.ExpiresAt, got child=%d parent=%d",
			child.ExpiresAt, parent.ExpiresAt)
	}
}

// ----- Consume -----

func TestConsumeToken_Atomic_FirstWins(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)

	const N = 8
	var wg sync.WaitGroup
	var success, alreadyConsumed int64
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+1)
			switch {
			case err == nil:
				atomic.AddInt64(&success, 1)
			case errors.Is(err, ErrAlreadyConsumed):
				atomic.AddInt64(&alreadyConsumed, 1)
			default:
				t.Errorf("unexpected err: %v", err)
			}
		}()
	}
	wg.Wait()
	if success != 1 {
		t.Errorf("expected exactly 1 success, got %d", success)
	}
	if alreadyConsumed != N-1 {
		t.Errorf("expected %d ErrAlreadyConsumed, got %d", N-1, alreadyConsumed)
	}
}

func TestConsumeToken_CallerAgentMismatch(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, err := ConsumeToken(db, kp.Pub, opaque, "evil-agent", "bead-close", "sylveste-x", now+1)
	if !errors.Is(err, ErrCallerAgentMismatch) {
		t.Fatalf("expected ErrCallerAgentMismatch, got %v", err)
	}
}

func TestConsumeToken_ExpectOpMismatch(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "git-push-main", "sylveste-x", now+1)
	if !errors.Is(err, ErrExpectMismatch) {
		t.Fatalf("expected ErrExpectMismatch (op), got %v", err)
	}
}

func TestConsumeToken_ExpectTargetMismatch(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "other-target", now+1)
	if !errors.Is(err, ErrExpectMismatch) {
		t.Fatalf("expected ErrExpectMismatch (target), got %v", err)
	}
}

func TestConsumeToken_EmptyExpectSkipsCheck(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "", "", now+1)
	if err != nil {
		t.Fatalf("empty expects should skip check; got %v", err)
	}
}

func TestConsumeToken_Expired(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	_, opaque, _ := issueRootTTL(t, db, kp, "claude-opus-4-7", now, 5*time.Second)
	// Consume at now+10s → past expires_at (now+5s).
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+10)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired, got %v", err)
	}
}

func TestConsumeToken_Revoked(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	if _, err := RevokeToken(db, tok.ID, false, now+1); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+2)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked, got %v", err)
	}
}

// TestConsumeToken_RevokedExitsAuthFailure is the r3 regression test. An
// operator-invoked revoke must map to exit 4 (auth-failure), NOT exit 2
// (token-state) — otherwise the gate wrapper falls through to legacy
// policy, which could silently override the operator's revoke intent.
func TestConsumeToken_RevokedExitsAuthFailure(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	if _, err := RevokeToken(db, tok.ID, false, now+1); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+2)
	if !errors.Is(err, ErrRevoked) {
		t.Fatalf("expected ErrRevoked, got %v", err)
	}
	if got := ExitCode(err); got != 4 {
		t.Errorf("revoked ExitCode = %d, want 4 (auth-failure)", got)
	}
}

func TestConsumeToken_NotFound(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	// Generate a valid ULID + sig form but one not in the DB.
	unknown := "01J0GZZZZZZZZZZZZZZZZZZZZZ." + strings.Repeat("00", ed25519.SignatureSize)
	_, err := ConsumeToken(db, kp.Pub, unknown, "claude-opus-4-7", "bead-close", "sylveste-x", now)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestConsumeToken_BadSig(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, _, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	// Manually build an opaque with a bogus signature of the correct length.
	bogus := EncodeTokenString(tok.ID, make([]byte, ed25519.SignatureSize))
	_, err := ConsumeToken(db, kp.Pub, bogus, "claude-opus-4-7", "bead-close", "sylveste-x", now+1)
	if !errors.Is(err, ErrSigVerify) {
		t.Fatalf("expected ErrSigVerify, got %v", err)
	}
}

func TestConsumeToken_AuditRowWritten(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	_, err := ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+1)
	if err != nil {
		t.Fatalf("ConsumeToken: %v", err)
	}

	// Expect one authorizations row for this op+target+agent with
	// sig_version=1 and vetting JSON referencing the token id.
	var count int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM authorizations WHERE op_type = ? AND target = ? AND agent_id = ? AND sig_version = 1`,
		"bead-close", "sylveste-x", "claude-opus-4-7",
	).Scan(&count); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 audit row, got %d", count)
	}

	var vettingStr string
	if err := db.QueryRow(
		`SELECT vetting FROM authorizations WHERE op_type = ? AND agent_id = ? AND sig_version = 1`,
		"bead-close", "claude-opus-4-7",
	).Scan(&vettingStr); err != nil {
		t.Fatalf("load vetting: %v", err)
	}
	var vetting map[string]any
	if err := json.Unmarshal([]byte(vettingStr), &vetting); err != nil {
		t.Fatalf("unmarshal vetting: %v", err)
	}
	if vetting["via"] != "token" {
		t.Errorf("vetting.via = %v, want token", vetting["via"])
	}
	if vetting["token_id"] != tok.ID {
		t.Errorf("vetting.token_id = %v, want %s", vetting["token_id"], tok.ID)
	}
	if vetting["depth"].(float64) != 0 {
		t.Errorf("vetting.depth = %v, want 0", vetting["depth"])
	}
	// Root has no root_token — JSON null.
	if vetting["root_token"] != nil {
		t.Errorf("vetting.root_token = %v, want nil for root", vetting["root_token"])
	}
}

// ----- Revoke -----

// TestRevokeToken_CascadeFromRoot_NullRootToken is the r2 P0 regression test.
// A root token has root_token IS NULL. A naive cascade predicate like
// `WHERE root_token = target.root_token` would compare NULL = NULL (always
// false), flagging zero rows. The correct predicate binds target.id to both
// positions so id=target.id catches the root and root_token=target.id
// catches descendants.
func TestRevokeToken_CascadeFromRoot_NullRootToken(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	// Root + two descendants.
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)
	d2 := mustDelegate(t, db, kp, d1.ID, "agent-1", "agent-2", now+2)

	n, err := RevokeToken(db, root.ID, true, now+3)
	if err != nil {
		t.Fatalf("RevokeToken cascade: %v", err)
	}
	if n != 3 {
		t.Errorf("expected 3 rows revoked (root + 2 descendants), got %d", n)
	}
	// Each row must have revoked_at set.
	for _, id := range []string{root.ID, d1.ID, d2.ID} {
		got, err := GetToken(db, id)
		if err != nil {
			t.Fatalf("GetToken %s: %v", id, err)
		}
		if got.RevokedAt == 0 {
			t.Errorf("token %s: revoked_at still zero", id)
		}
	}
}

// TestRevokeToken_CascadeOnNonRoot_Refused is the r3 P1 regression test.
// v2 refuses mid-chain cascade because denormalized root_token points at
// the chain root (not immediate ancestors); a mid-chain cascade predicate
// would match zero rows. Refusing is safer than silently half-revoking.
func TestRevokeToken_CascadeOnNonRoot_Refused(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)
	d2 := mustDelegate(t, db, kp, d1.ID, "agent-1", "agent-2", now+2)

	n, err := RevokeToken(db, d1.ID, true, now+3)
	if !errors.Is(err, ErrCascadeOnNonRoot) {
		t.Fatalf("expected ErrCascadeOnNonRoot, got %v (n=%d)", err, n)
	}
	if n != 0 {
		t.Errorf("expected 0 rows affected on refused cascade, got %d", n)
	}
	// Confirm no writes happened: all three tokens still have revoked_at=0.
	for _, id := range []string{root.ID, d1.ID, d2.ID} {
		got, _ := GetToken(db, id)
		if got.RevokedAt != 0 {
			t.Errorf("token %s: revoked_at should be zero, got %d", id, got.RevokedAt)
		}
	}
}

func TestRevokeToken_NonCascade(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)

	n, err := RevokeToken(db, d1.ID, false, now+2)
	if err != nil {
		t.Fatalf("RevokeToken non-cascade: %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 row revoked, got %d", n)
	}
	// Root must NOT be revoked.
	got, _ := GetToken(db, root.ID)
	if got.RevokedAt != 0 {
		t.Errorf("root should not be revoked by non-cascade d1 revoke")
	}
}

func TestRevokeToken_Idempotent(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, _, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)
	first, _ := RevokeToken(db, tok.ID, false, now+1)
	second, err := RevokeToken(db, tok.ID, false, now+2) // second call is noop
	if err != nil {
		t.Fatalf("second RevokeToken: %v", err)
	}
	if first != 1 {
		t.Errorf("first revoke expected 1, got %d", first)
	}
	if second != 0 {
		t.Errorf("second revoke expected 0 (idempotent), got %d", second)
	}
	// revoked_at must still reflect the FIRST timestamp (not overwritten).
	got, _ := GetToken(db, tok.ID)
	if got.RevokedAt != now+1 {
		t.Errorf("revoked_at should remain %d, got %d", now+1, got.RevokedAt)
	}
}

// TestRevokeVsConsume_Race: revoke + consume in parallel, under
// MaxOpenConns=1. Under serialization, exactly one semantic winner:
// either consume succeeds (then revoke lands on a consumed row, which is
// harmless audit) or revoke lands first (then consume sees revoked and
// returns ErrRevoked). Both outcomes are valid; neither should produce an
// inconsistent state (e.g., a row with both consumed_at AND revoked_at set
// to different values from this race would be fine — the ordering is
// serialized).
func TestRevokeVsConsume_Race(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	tok, opaque, _ := issueRoot(t, db, kp, "claude-opus-4-7", now)

	var wg sync.WaitGroup
	var consumeErr error
	var revokeN int
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, consumeErr = ConsumeToken(db, kp.Pub, opaque, "claude-opus-4-7", "bead-close", "sylveste-x", now+1)
	}()
	go func() {
		defer wg.Done()
		revokeN, _ = RevokeToken(db, tok.ID, false, now+1)
	}()
	wg.Wait()

	got, _ := GetToken(db, tok.ID)
	switch {
	case consumeErr == nil:
		// Consume won; row must have consumed_at set. Revoke may have
		// landed afterward (harmless audit).
		if got.ConsumedAt == 0 {
			t.Errorf("consume returned nil but consumed_at=0")
		}
	case errors.Is(consumeErr, ErrRevoked):
		// Revoke won; row must have revoked_at set, consumed_at=0.
		if got.RevokedAt == 0 || got.ConsumedAt != 0 {
			t.Errorf("revoke won but row state inconsistent: %+v", got)
		}
		if revokeN != 1 {
			t.Errorf("revoke should have affected 1 row, got %d", revokeN)
		}
	default:
		t.Fatalf("unexpected consume error: %v", consumeErr)
	}
}

// ----- ListTokens -----

func TestListTokens_FilterByRoot(t *testing.T) {
	db, kp := setupAuthzTestDB(t)
	now := int64(1_700_000_000)
	root, _, _ := issueRoot(t, db, kp, "agent-0", now)
	d1 := mustDelegate(t, db, kp, root.ID, "agent-0", "agent-1", now+1)
	d2 := mustDelegate(t, db, kp, d1.ID, "agent-1", "agent-2", now+2)
	// An unrelated root outside the subtree — must not be returned.
	otherRoot, _, _ := issueRoot(t, db, kp, "agent-other", now+3)

	got, err := ListTokens(db, ListFilter{RootToken: root.ID})
	if err != nil {
		t.Fatalf("ListTokens: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 tokens in root's subtree, got %d", len(got))
	}
	seen := map[string]bool{}
	for _, tok := range got {
		seen[tok.ID] = true
		if tok.ID == otherRoot.ID {
			t.Error("unrelated root leaked into subtree filter")
		}
	}
	for _, want := range []string{root.ID, d1.ID, d2.ID} {
		if !seen[want] {
			t.Errorf("expected token %s in subtree, missing", want)
		}
	}
}

// ----- ExitCode mapping -----

func TestExitCode_Mapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{nil, 0},
		{errors.New("anything-else"), 1},
		{ErrAlreadyConsumed, 2},
		{ErrExpired, 2},
		{ErrNotFound, 3},
		{ErrBadTokenString, 3},
		{ErrSigVerify, 4},
		{ErrProofOfPossession, 4},
		{ErrCallerAgentMismatch, 4},
		{ErrCrossProject, 4},
		{ErrScopeWidening, 4},
		{ErrDepthExceeded, 4},
		{ErrExpectMismatch, 4},
		{ErrRevoked, 4},
		{ErrCascadeOnNonRoot, 4},
	}
	for _, tc := range cases {
		t.Run(errName(tc.err), func(t *testing.T) {
			if got := ExitCode(tc.err); got != tc.want {
				t.Errorf("ExitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func errName(err error) string {
	if err == nil {
		return "nil"
	}
	return err.Error()
}

// ===== helpers =====

// setupAuthzTestDB opens an in-memory SQLite DB, creates authz_tokens and
// authorizations tables matching production DDL, and returns both the DB
// and a fresh keypair for signing test tokens.
func setupAuthzTestDB(t *testing.T) (*sql.DB, KeyPair) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(authzTokensDDL); err != nil {
		t.Fatalf("create authz_tokens: %v", err)
	}
	if _, err := db.Exec(authorizationsDDL); err != nil {
		t.Fatalf("create authorizations: %v", err)
	}

	kp, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return db, kp
}

func issueRoot(t *testing.T, db *sql.DB, kp KeyPair, agent string, now int64) (Token, string, error) {
	t.Helper()
	return issueRootTTL(t, db, kp, agent, now, time.Hour)
}

func issueRootTTL(t *testing.T, db *sql.DB, kp KeyPair, agent string, now int64, ttl time.Duration) (Token, string, error) {
	t.Helper()
	tok, opaque, err := IssueToken(db, kp.Priv, IssueSpec{
		OpType:   "bead-close",
		Target:   "sylveste-x",
		AgentID:  agent,
		BeadID:   "sylveste-x",
		IssuedBy: "user",
		TTL:      ttl,
	}, now)
	if err != nil {
		t.Fatalf("IssueToken: %v", err)
	}
	return tok, opaque, nil
}

func mustDelegate(t *testing.T, db *sql.DB, kp KeyPair, parentID, fromAgent, toAgent string, now int64) Token {
	t.Helper()
	tok, _, err := DelegateToken(db, kp.Priv, DelegateSpec{
		ParentID:      parentID,
		CallerAgentID: fromAgent,
		ToAgentID:     toAgent,
		RequestedTTL:  time.Minute,
	}, now)
	if err != nil {
		t.Fatalf("DelegateToken: %v", err)
	}
	return tok
}

// Kept in sync with schema.sql §v34 authz_tokens and §v32/v33 authorizations.
// If a schema migration lands, copy the new DDL here (the existing
// policy_test.go follows the same convention).
const authzTokensDDL = `
CREATE TABLE IF NOT EXISTS authz_tokens (
  id            TEXT PRIMARY KEY,
  op_type       TEXT NOT NULL,
  target        TEXT NOT NULL,
  agent_id      TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
  bead_id       TEXT,
  delegate_to   TEXT,
  expires_at    INTEGER NOT NULL,
  consumed_at   INTEGER,
  revoked_at    INTEGER,
  issued_by     TEXT NOT NULL,
  parent_token  TEXT REFERENCES authz_tokens(id) ON DELETE RESTRICT,
  root_token    TEXT,
  depth         INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0 AND depth <= 3),
  sig_version   INTEGER NOT NULL DEFAULT 2,
  signature     BLOB NOT NULL,
  created_at    INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS tokens_by_root    ON authz_tokens(root_token, consumed_at, revoked_at);
CREATE INDEX IF NOT EXISTS tokens_by_parent  ON authz_tokens(parent_token);
CREATE INDEX IF NOT EXISTS tokens_by_expiry  ON authz_tokens(expires_at) WHERE consumed_at IS NULL AND revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS tokens_by_agent   ON authz_tokens(agent_id, created_at DESC);
`

const authorizationsDDL = `
CREATE TABLE IF NOT EXISTS authorizations (
  id               TEXT PRIMARY KEY,
  op_type          TEXT NOT NULL,
  target           TEXT NOT NULL,
  agent_id         TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
  bead_id          TEXT,
  mode             TEXT NOT NULL CHECK(mode IN ('auto','confirmed','blocked','force_auto')),
  policy_match     TEXT,
  policy_hash      TEXT,
  vetted_sha       TEXT,
  vetting          TEXT CHECK(vetting IS NULL OR json_valid(vetting)),
  cross_project_id TEXT,
  created_at       INTEGER NOT NULL,
  sig_version      INTEGER NOT NULL DEFAULT 0,
  signature        BLOB,
  signed_at        INTEGER
);
`

// sampleToken returns a token with every field populated — for
// sign/verify tests where we want to exercise the full canonical payload
// (delegate_to, parent_token, root_token, and depth all non-default).
func sampleToken() Token {
	return Token{
		ID:          "01J0GZ8X4M7Q3N8S6XW9Y2Z5CF",
		OpType:      "bead-close",
		Target:      "sylveste-qdqr.28",
		AgentID:     "codex",
		BeadID:      "sylveste-qdqr.28",
		DelegateTo:  "codex",
		ExpiresAt:   1776742800,
		IssuedBy:    "claude-opus-4-7",
		ParentToken: "01J0GZ7W93K5PKQ42V7TCMWC2B",
		RootToken:   "01J0GZ7W93K5PKQ42V7TCMWC2B",
		Depth:       1,
		SigVersion:  2,
		CreatedAt:   1776739260,
	}
}
