package publish

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/pkg/authz"
	_ "modernc.org/sqlite"
)

// setupApprovalSandbox builds a project root with:
//   - a git repo inside a plugin subdirectory
//   - a v34-schema .clavain/intercore.db at the project root (authorizations
//     + authz_tokens + migration markers — needed for token path tests)
//   - (optionally) a signing keypair
//
// Returns (projectRoot, pluginRoot).
func setupApprovalSandbox(t *testing.T, initKey bool) (string, string) {
	t.Helper()
	projectRoot := t.TempDir()
	pluginRoot := filepath.Join(projectRoot, "plugins", "demo")
	if err := os.MkdirAll(pluginRoot, 0o755); err != nil {
		t.Fatalf("mkdir plugin: %v", err)
	}

	runGit(t, pluginRoot, "init", "-q")
	runGit(t, pluginRoot, "config", "user.email", "noreply@anthropic.com")
	runGit(t, pluginRoot, "config", "user.name", "Claude")
	runGit(t, pluginRoot, "config", "commit.gpgsign", "false")

	file := filepath.Join(pluginRoot, "README.md")
	if err := os.WriteFile(file, []byte("# demo\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	runGit(t, pluginRoot, "add", ".")
	runGit(t, pluginRoot, "commit", "-q", "-m", "agent commit\n\nCo-Authored-By: Claude <noreply@anthropic.com>")

	clavainDir := filepath.Join(projectRoot, ".clavain")
	if err := os.MkdirAll(clavainDir, 0o755); err != nil {
		t.Fatalf("mkdir .clavain: %v", err)
	}
	dbPath := filepath.Join(clavainDir, "intercore.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE authorizations (
			id TEXT PRIMARY KEY, op_type TEXT NOT NULL, target TEXT NOT NULL,
			agent_id TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
			bead_id TEXT, mode TEXT NOT NULL CHECK(mode IN ('auto','confirmed','blocked','force_auto')),
			policy_match TEXT, policy_hash TEXT, vetted_sha TEXT,
			vetting TEXT CHECK(vetting IS NULL OR json_valid(vetting)),
			cross_project_id TEXT, created_at INTEGER NOT NULL,
			sig_version INTEGER NOT NULL DEFAULT 0,
			signature BLOB, signed_at INTEGER);
		CREATE TABLE authz_tokens (
			id TEXT PRIMARY KEY, op_type TEXT NOT NULL, target TEXT NOT NULL,
			agent_id TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
			bead_id TEXT, delegate_to TEXT,
			expires_at INTEGER NOT NULL,
			consumed_at INTEGER, revoked_at INTEGER,
			issued_by TEXT NOT NULL,
			parent_token TEXT REFERENCES authz_tokens(id) ON DELETE RESTRICT,
			root_token TEXT,
			depth INTEGER NOT NULL DEFAULT 0 CHECK (depth >= 0 AND depth <= 3),
			sig_version INTEGER NOT NULL DEFAULT 2,
			signature BLOB NOT NULL,
			created_at INTEGER NOT NULL);
	`)
	if err != nil {
		t.Fatalf("schema: %v", err)
	}
	db.Close()

	if initKey {
		kp, err := authz.GenerateKey()
		if err != nil {
			t.Fatalf("genkey: %v", err)
		}
		if err := authz.WriteKeyPair(projectRoot, kp); err != nil {
			t.Fatalf("writekey: %v", err)
		}
	}

	return projectRoot, pluginRoot
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Claude",
		"GIT_AUTHOR_EMAIL=noreply@anthropic.com",
		"GIT_COMMITTER_NAME=Claude",
		"GIT_COMMITTER_EMAIL=noreply@anthropic.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, string(out))
	}
}

// insertAndSignPublishRow inserts an ic-publish-patch authz row for target
// with createdAt, signs it with the project key, and returns the row ID.
func insertAndSignPublishRow(t *testing.T, projectRoot, target string, createdAt int64) string {
	t.Helper()
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()

	id := fmt.Sprintf("row-%d", createdAt)
	if _, err := db.Exec(`INSERT INTO authorizations
		(id, op_type, target, agent_id, mode, policy_match, policy_hash,
		 vetted_sha, created_at, sig_version)
		VALUES (?, 'ic-publish-patch', ?, 'claude-test', 'auto',
		        'ic-publish-patch#0', 'abc123', 'deadbeef', ?, 1)`,
		id, target, createdAt); err != nil {
		t.Fatalf("insert: %v", err)
	}

	kp, err := authz.LoadPrivKey(projectRoot)
	if err != nil {
		t.Fatalf("loadkey: %v", err)
	}
	row := authz.SignRow{
		ID:          id,
		OpType:      "ic-publish-patch",
		Target:      target,
		AgentID:     "claude-test",
		Mode:        "auto",
		PolicyMatch: "ic-publish-patch#0",
		PolicyHash:  "abc123",
		VettedSHA:   "deadbeef",
		CreatedAt:   createdAt,
	}
	sig, err := authz.Sign(kp.Priv, row)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if _, err := db.Exec(`UPDATE authorizations SET signature=?, signed_at=? WHERE id=?`,
		sig, time.Now().Unix(), id); err != nil {
		t.Fatalf("update sig: %v", err)
	}
	return id
}

// issueTestToken produces a fresh opaque token via authz.IssueToken, signed
// with the project key. target is absolute path conventions used by the
// approval flow. Returns the opaque string.
func issueTestToken(t *testing.T, projectRoot, target, agentID string, ttl time.Duration) string {
	t.Helper()
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	kp, err := authz.LoadPrivKey(projectRoot)
	if err != nil {
		t.Fatalf("loadkey: %v", err)
	}
	_, opaque, err := authz.IssueToken(db, kp.Priv, authz.IssueSpec{
		OpType:   "ic-publish-patch",
		Target:   target,
		AgentID:  agentID,
		IssuedBy: "test",
		TTL:      ttl,
	}, time.Now().Unix())
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return opaque
}

// openSandboxDB opens the sandbox's intercore.db with SetMaxOpenConns(1) so
// callers share the token-consume serialization invariant. Caller closes.
func openSandboxDB(t *testing.T, projectRoot string) *sql.DB {
	t.Helper()
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	return db
}

// loadSandboxPub loads the project's Ed25519 pub key, or returns nil if the
// sandbox was set up without initKey (mirrors the engine's tolerant behavior
// — no pub means no token-path verification is possible).
func loadSandboxPub(t *testing.T, projectRoot string) ed25519.PublicKey {
	t.Helper()
	pub, err := authz.LoadPubKey(projectRoot)
	if err != nil {
		return nil
	}
	return pub
}

// callRequireApproval runs RequiresApproval against a sandbox with explicit
// deps. tokenStr / callerAgentID may be empty to exercise the v1.5
// authz-record + marker paths. Returns (needs, via). now is time.Now() unless
// overridden via nowFn (useful for expired-token tests).
func callRequireApproval(t *testing.T, projectRoot, pluginRoot, tokenStr, callerAgentID string) (bool, string) {
	t.Helper()
	db := openSandboxDB(t, projectRoot)
	defer db.Close()
	pub := loadSandboxPub(t, projectRoot)
	return RequiresApproval(pluginRoot, tokenStr, callerAgentID, db, pub, time.Now().Unix())
}

// captureStderr runs fn with os.Stderr redirected to a buffer.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w
	done := make(chan []byte)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.Bytes()
	}()
	fn()
	w.Close()
	os.Stderr = old
	return string(<-done)
}

// ─── v1.5 existing tests (updated to new signature) ─────────────────────

func TestRequiresApproval_AuthzFreshValid(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if needs {
		t.Fatal("expected approval NOT required with fresh signed authz row")
	}
	if via != ViaAuthzRecord {
		t.Errorf("via = %q, want %q", via, ViaAuthzRecord)
	}
}

func TestRequiresApproval_AuthzStale(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	// 2 hours old (default freshness window is 60 min).
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Add(-2*time.Hour).Unix())

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if !needs {
		t.Fatal("expected approval required when authz row is stale and no marker")
	}
	if via != ViaNone {
		t.Errorf("via = %q, want %q", via, ViaNone)
	}
}

func TestRequiresApproval_NoAuthzMarkerPresent(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var needs bool
	var via string
	stderr := captureStderr(t, func() {
		needs, via = callRequireApproval(t, projectRoot, pluginRoot, "", "")
	})
	if needs {
		t.Fatal("expected approval NOT required when marker present")
	}
	if via != ViaMarker {
		t.Errorf("via = %q, want %q", via, ViaMarker)
	}
	// Deprecation banner must surface the migration target and the reason
	// authz-record didn't cover.
	if !containsAll(stderr, []string{"DEPRECATION", "no fresh ic-publish-patch record"}) {
		t.Errorf("stderr missing expected deprecation banner:\n%s", stderr)
	}
}

func TestRequiresApproval_AuthzMutatedMarkerFallback(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	id := insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	// Tamper with the signed row's policy_match after signing.
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, _ := sql.Open("sqlite", dbPath)
	if _, err := db.Exec(`UPDATE authorizations SET policy_match='TAMPERED' WHERE id=?`, id); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	db.Close()

	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var needs bool
	var via string
	stderr := captureStderr(t, func() {
		needs, via = callRequireApproval(t, projectRoot, pluginRoot, "", "")
	})
	if needs {
		t.Fatal("expected approval NOT required when marker present even with tampered authz")
	}
	if via != ViaMarker {
		t.Errorf("via = %q, want %q", via, ViaMarker)
	}
	if !containsAll(stderr, []string{"verification failed"}) {
		t.Errorf("stderr should mention verification failure: %s", stderr)
	}
}

func TestRequiresApproval_NeitherSignal(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if !needs {
		t.Fatal("expected approval required when neither authz nor marker present")
	}
	if via != ViaNone {
		t.Errorf("via = %q, want %q", via, ViaNone)
	}
}

func TestRequiresApproval_HumanCommitShortCircuits(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)

	// Layer a fresh human-authored commit on top. isAgentCommit reads HEAD,
	// so this flips the verdict regardless of any existing authz/marker state.
	if err := os.WriteFile(filepath.Join(pluginRoot, "CHANGELOG.md"), []byte("chg\n"), 0o644); err != nil {
		t.Fatalf("write changelog: %v", err)
	}
	cmd := exec.Command("git", "-C", pluginRoot, "add", "CHANGELOG.md")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "-C", pluginRoot, "commit", "-q", "-m", "human commit")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Dev",
		"GIT_AUTHOR_EMAIL=dev@example.com",
		"GIT_COMMITTER_NAME=Dev",
		"GIT_COMMITTER_EMAIL=dev@example.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if needs {
		t.Fatal("expected approval NOT required for human commit regardless of authz/marker state")
	}
	if via != ViaHumanCommit {
		t.Errorf("via = %q, want %q", via, ViaHumanCommit)
	}
}

func TestRequiresApproval_PreSigningVintageRejected(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)

	// Insert a row with sig_version=0 (pre-signing vintage) — even if fresh,
	// the plan requires re-authorization for the publish path.
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, _ := sql.Open("sqlite", dbPath)
	if _, err := db.Exec(`INSERT INTO authorizations
		(id, op_type, target, agent_id, mode, created_at, sig_version)
		VALUES ('pre-sig-1', 'ic-publish-patch', ?, 'claude', 'auto', ?, 0)`,
		target, time.Now().Unix()); err != nil {
		t.Fatalf("insert: %v", err)
	}
	db.Close()

	needs, _ := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if !needs {
		t.Fatal("expected approval required when only pre-signing row available")
	}
}

func TestRequiresApproval_FreshnessOverride(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Add(-30*time.Minute).Unix())

	t.Setenv("PUBLISH_AUTHZ_FRESHNESS_MIN", "15")
	needs, _ := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if !needs {
		t.Fatal("expected approval required with 15-min window and 30-min-old row")
	}

	t.Setenv("PUBLISH_AUTHZ_FRESHNESS_MIN", "120")
	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if needs {
		t.Fatal("expected approval NOT required with 120-min window and 30-min-old row")
	}
	if via != ViaAuthzRecord {
		t.Errorf("via = %q, want %q", via, ViaAuthzRecord)
	}
}

// ─── v2 token-path tests ─────────────────────────────────────────────────

func TestRequiresApproval_TokenValid(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent", 60*time.Minute)

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent")
	if needs {
		t.Fatal("expected approval NOT required with valid fresh token")
	}
	if via != ViaToken {
		t.Errorf("via = %q, want %q", via, ViaToken)
	}
}

func TestRequiresApproval_TokenAlreadyConsumedFallbackAuthzRecord(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent", 60*time.Minute)

	// Also have a fresh signed authz row so the fall-through lands on
	// authz-record (not marker, not "none").
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	// First consume — consumed_at gets set.
	if _, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent"); via != ViaToken {
		t.Fatalf("first consume via = %q, want %q", via, ViaToken)
	}
	// Second: token already-consumed (state-class 2) → falls through to
	// authz-record.
	needs, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent")
	if needs {
		t.Fatal("expected approval NOT required (authz-record fallback)")
	}
	if via != ViaAuthzRecord {
		t.Errorf("via = %q, want %q (token-state drift should fall through)", via, ViaAuthzRecord)
	}
}

func TestRequiresApproval_TokenExpired_MarkerFallback(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	// TTL = 1s; sleep 2s to guarantee expiry. A cleaner path would be
	// db.Exec UPDATE expires_at, but ConsumeToken reads expires_at and
	// time.Now() together so the sleep-based path is equivalent.
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent", 1*time.Second)
	time.Sleep(2 * time.Second)

	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var needs bool
	var via string
	stderr := captureStderr(t, func() {
		needs, via = callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent")
	})
	if needs {
		t.Fatal("expected approval NOT required (marker fallback after token expiry)")
	}
	if via != ViaMarker {
		t.Errorf("via = %q, want %q", via, ViaMarker)
	}
	if !containsAll(stderr, []string{"DEPRECATION"}) {
		t.Errorf("stderr should carry deprecation banner:\n%s", stderr)
	}
}

func TestRequiresApproval_TokenScopeMismatch_HardFail(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)

	// Token scoped to a DIFFERENT plugin path — should hard-fail, NOT fall
	// through to the fresh authz-record or the present marker.
	otherTarget := target + "-other"
	opaque := issueTestToken(t, projectRoot, otherTarget, "publisher-agent", 60*time.Minute)
	// Both the authz-record and the marker exist; if we fell through we'd
	// wrongly approve.
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())
	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent")
	if !needs {
		t.Fatal("expected approval REQUIRED on scope mismatch (no fall-through on auth-failure)")
	}
	if via != ViaNone {
		t.Errorf("via = %q, want %q", via, ViaNone)
	}
}

func TestRequiresApproval_TokenSigInvalid_HardFail(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent", 60*time.Minute)

	// Flip a byte in the signature portion (after the "." delimiter).
	id, sig, err := authz.ParseTokenString(opaque)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	sig[0] ^= 0xFF
	forged := authz.EncodeTokenString(id, sig)

	// Marker present — sig-verify hard-fail must NOT fall through to it.
	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, forged, "publisher-agent")
	if !needs {
		t.Fatal("expected approval REQUIRED on sig-verify failure (no fall-through)")
	}
	if via != ViaNone {
		t.Errorf("via = %q, want %q", via, ViaNone)
	}
}

func TestRequiresApproval_TokenCallerMismatch_HardFail(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent-A", 60*time.Minute)

	// Caller says agent-B; token is for agent-A. Auth-failure → hard fail.
	// Fresh authz-record exists — hard-fail must NOT fall through.
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent-B")
	if !needs {
		t.Fatal("expected approval REQUIRED on caller mismatch (no fall-through)")
	}
	if via != ViaNone {
		t.Errorf("via = %q, want %q", via, ViaNone)
	}
}

func TestRequiresApproval_TokenEmpty_AuthzRecordPath(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, "", "")
	if needs {
		t.Fatal("expected approval NOT required (authz-record path)")
	}
	if via != ViaAuthzRecord {
		t.Errorf("via = %q, want %q", via, ViaAuthzRecord)
	}
}

func TestRequiresApproval_TokenConsumeTelemetry(t *testing.T) {
	// A successful token consume should write an authorizations row with
	// vetting.via = "token" so the adoption-gate query can discriminate.
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	opaque := issueTestToken(t, projectRoot, target, "publisher-agent", 60*time.Minute)

	needs, via := callRequireApproval(t, projectRoot, pluginRoot, opaque, "publisher-agent")
	if needs {
		t.Fatal("expected approval NOT required")
	}
	if via != ViaToken {
		t.Fatalf("via = %q, want %q", via, ViaToken)
	}

	db := openSandboxDB(t, projectRoot)
	defer db.Close()
	var vetting string
	err := db.QueryRow(
		`SELECT IFNULL(vetting,'') FROM authorizations
		 WHERE op_type = 'ic-publish-patch' AND target = ? AND created_at >= ?
		 ORDER BY created_at DESC LIMIT 1`,
		target, time.Now().Add(-1*time.Minute).Unix(),
	).Scan(&vetting)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("select audit: %v", err)
	}
	// ConsumeToken writes vetting = {"via":"token","token_id":...,"depth":0,"root_token":null}
	// at row-insert time. We just need `"via":"token"` to appear somewhere
	// in the JSON — the exact shape is the library's concern.
	if !bytes.Contains([]byte(vetting), []byte(`"via":"token"`)) {
		t.Errorf("vetting missing via=token marker: %q", vetting)
	}
}

// containsAll reports whether s contains every needle.
func containsAll(s string, needles []string) bool {
	for _, n := range needles {
		if !bytes.Contains([]byte(s), []byte(n)) {
			return false
		}
	}
	return true
}
