package publish

import (
	"bytes"
	"database/sql"
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
//   - a v33-schema .clavain/intercore.db at the project root
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
			signature BLOB, signed_at INTEGER)`)
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

// ─── Tests ───────────────────────────────────────────────────────────

func TestRequiresApproval_AuthzFreshValid(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	if RequiresApproval(pluginRoot) {
		t.Fatal("expected approval NOT required with fresh signed authz row")
	}
}

func TestRequiresApproval_AuthzStale(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	// 2 hours old (default freshness window is 60 min).
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Add(-2*time.Hour).Unix())

	if !RequiresApproval(pluginRoot) {
		t.Fatal("expected approval required when authz row is stale and no marker")
	}
}

func TestRequiresApproval_NoAuthzMarkerPresent(t *testing.T) {
	_, pluginRoot := setupApprovalSandbox(t, true)
	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var result bool
	stderr := captureStderr(t, func() {
		result = RequiresApproval(pluginRoot)
	})
	if result {
		t.Fatal("expected approval NOT required when marker present")
	}
	// Deprecation warning must surface the reason authz didn't cover.
	if stderr == "" || !containsAll(stderr, []string{"deprecated", "no fresh ic-publish-patch record"}) {
		t.Errorf("stderr missing expected deprecation warning:\n%s", stderr)
	}
}

func TestRequiresApproval_AuthzMutatedMarkerFallback(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	id := insertAndSignPublishRow(t, projectRoot, target, time.Now().Unix())

	// Tamper with the signed row's op_type after signing.
	dbPath := filepath.Join(projectRoot, ".clavain", "intercore.db")
	db, _ := sql.Open("sqlite", dbPath)
	if _, err := db.Exec(`UPDATE authorizations SET policy_match='TAMPERED' WHERE id=?`, id); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	db.Close()

	// Marker still wins, but warning must mention the verify failure.
	if err := os.WriteFile(filepath.Join(pluginRoot, ".publish-approved"), []byte{}, 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	var result bool
	stderr := captureStderr(t, func() {
		result = RequiresApproval(pluginRoot)
	})
	if result {
		t.Fatal("expected approval NOT required when marker present even with tampered authz")
	}
	if !containsAll(stderr, []string{"verification failed"}) {
		t.Errorf("stderr should mention verification failure: %s", stderr)
	}
}

func TestRequiresApproval_NeitherSignal(t *testing.T) {
	_, pluginRoot := setupApprovalSandbox(t, true)
	if !RequiresApproval(pluginRoot) {
		t.Fatal("expected approval required when neither authz nor marker present")
	}
}

func TestRequiresApproval_HumanCommitShortCircuits(t *testing.T) {
	_, pluginRoot := setupApprovalSandbox(t, true)

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

	if RequiresApproval(pluginRoot) {
		t.Fatal("expected approval NOT required for human commit regardless of authz/marker state")
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

	if !RequiresApproval(pluginRoot) {
		t.Fatal("expected approval required when only pre-signing row available")
	}
}

func TestRequiresApproval_FreshnessOverride(t *testing.T) {
	projectRoot, pluginRoot := setupApprovalSandbox(t, true)
	target, _ := filepath.Abs(pluginRoot)
	insertAndSignPublishRow(t, projectRoot, target, time.Now().Add(-30*time.Minute).Unix())

	t.Setenv("PUBLISH_AUTHZ_FRESHNESS_MIN", "15")
	if !RequiresApproval(pluginRoot) {
		t.Fatal("expected approval required with 15-min window and 30-min-old row")
	}

	t.Setenv("PUBLISH_AUTHZ_FRESHNESS_MIN", "120")
	if RequiresApproval(pluginRoot) {
		t.Fatal("expected approval NOT required with 120-min window and 30-min-old row")
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
