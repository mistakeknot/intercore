package publish

import (
	"crypto/ed25519"
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/pkg/authz"
	_ "modernc.org/sqlite"
)

// agentAuthorPatterns are commit author patterns that indicate agent-generated commits.
var agentAuthorPatterns = []string{
	"noreply@anthropic.com",
	"claude@anthropic.com",
	"codex@openai.com",
	"noreply@openai.com",
	"interlab",
	"autoresearch",
}

// agentMessagePatterns are commit message patterns that indicate agent mutations.
var agentMessagePatterns = []string{
	"[interlab]",
	"[autoresearch]",
	"[agent-mutation]",
	"Co-Authored-By: Claude",
	"Co-Authored-By: Codex",
}

// defaultAuthzFreshnessMin is the default max age (minutes) of an authz record
// that can stand in for a .publish-approved marker. Overridable via
// PUBLISH_AUTHZ_FRESHNESS_MIN.
const defaultAuthzFreshnessMin = 60

// ViaPath values returned from RequiresApproval describing how approval was
// granted (or why it wasn't). Also emitted as the `via` field of the vetting
// JSON column for adoption telemetry:
//   SELECT json_extract(vetting, '$.via') FROM authorizations ...
const (
	ViaToken       = "token"        // v2 publish-scoped token consumed
	ViaAuthzRecord = "authz-record" // v1.5 fresh signed authz row matched
	ViaMarker      = "marker"       // legacy .publish-approved file present
	ViaHumanCommit = "human-commit" // non-agent HEAD; approval not required
	ViaNone        = "none"         // approval required; no signal available
)

// RequiresApproval returns whether human approval is needed to publish this
// plugin, plus a viaPath describing which signal granted approval (or why
// approval is required). Dependencies are passed explicitly — the function
// never reads $CLAVAIN_AUTHZ_TOKEN, never calls sql.Open, never loads keys
// from disk. The caller (engine.Publish or a test) threads them in.
//
// Precedence when HEAD is an agent commit (first hit wins):
//  1. token:        tokenStr non-empty AND valid under pub AND consumed
//                   successfully → approved via "token". Auth-failure class
//                   (sig-verify | caller-mismatch | scope-mismatch |
//                   cross-project | revoked) HARD-FAILS: approval required
//                   with viaPath="none". Token-state drift (expired,
//                   already-consumed, not-found, malformed) FALLS THROUGH to
//                   the legacy paths below.
//  2. authz-record: fresh signed ic-publish-patch row under pub → approved
//                   via "authz-record".
//  3. marker:       .publish-approved file present → approved via "marker".
//                   Emits a DEPRECATION stderr banner. Marker removal is
//                   gated on 14-day rolling adoption telemetry measured via
//                   the authorizations.vetting "via" field.
//  4. none:         no signal → approval required.
//
// Human-authored HEAD short-circuits to (false, "human-commit"); the approval
// gate only applies to agent commits.
func RequiresApproval(
	pluginRoot string,
	tokenStr string,
	callerAgentID string,
	db *sql.DB,
	pub ed25519.PublicKey,
	now int64,
) (bool, string) {
	if !isAgentCommit(pluginRoot) {
		return false, ViaHumanCommit
	}

	target, terr := filepath.Abs(pluginRoot)
	if terr != nil {
		target = pluginRoot
	}

	// v2 token path. Only attempts consume when all deps are present; empty
	// token / nil db / nil pub fall through cleanly to v1.5 behavior.
	if tokenStr != "" && db != nil && pub != nil && callerAgentID != "" {
		_, err := authz.ConsumeToken(db, pub, tokenStr, callerAgentID, "ic-publish-patch", target, now)
		switch {
		case err == nil:
			return false, ViaToken
		case authz.ExitCode(err) == 4:
			// Auth-failure: operator intent or cryptographic trust broken.
			// HARD FAIL — do NOT fall through, since falling back to legacy
			// would let a legacy path silently override the revoke / scope
			// / caller mismatch.
			log.Printf("publish: token auth-failure (%s) — refusing to fall back to legacy: %v",
				authz.ErrClass(err), err)
			return true, ViaNone
		default:
			// Token-state (2), not-found (3), unexpected (1) — passive drift.
			// Fall through to the v1.5 paths below.
			log.Printf("publish: token unusable (%s: %v); trying authz-record + marker paths",
				authz.ErrClass(err), err)
		}
	}

	// v1.5 authz-record path (signature over the full row, freshness window).
	authzOK, authzReason := checkAuthzApproval(pluginRoot)
	if authzOK {
		return false, ViaAuthzRecord
	}

	// Legacy marker fallback with louder deprecation banner.
	approvalFile := filepath.Join(pluginRoot, ".publish-approved")
	if _, err := os.Stat(approvalFile); err == nil {
		slug := filepath.Base(pluginRoot)
		fmt.Fprintf(os.Stderr,
			"DEPRECATION: .publish-approved marker used for %s.\n"+
				"  Migration target: clavain-cli policy token issue --op=ic-publish-patch --target=%s\n"+
				"  Marker removal is gated on 14-day rolling adoption telemetry; see docs/canon/authz-token-model.md §deprecation-gate.\n",
			pluginRoot, slug)
		if authzReason != "" {
			fmt.Fprintf(os.Stderr, "  (authz-record path unavailable: %s)\n", authzReason)
		}
		return false, ViaMarker
	}

	return true, ViaNone
}

// ConsumeApproval removes the .publish-approved marker after successful publish.
// Authz records + tokens are permanent audit history and are NOT removed —
// token single-use is enforced by consumed_at at consume time.
func ConsumeApproval(pluginRoot string) {
	os.Remove(filepath.Join(pluginRoot, ".publish-approved"))
}

// checkAuthzApproval returns (true, "") when a fresh valid-signature authz
// record approves this publish. Returns (false, reason) otherwise, where
// reason explains why the authz path did not grant approval — used in the
// marker-fallback warning message. Opens its own DB connection because this
// path runs independently of the token path's caller-supplied db.
func checkAuthzApproval(pluginRoot string) (bool, string) {
	dbPath := findIntercoreDB(pluginRoot)
	if dbPath == "" {
		return false, "no .clavain/intercore.db found"
	}
	projectRoot := filepath.Dir(filepath.Dir(dbPath))

	pub, err := authz.LoadPubKey(projectRoot)
	if err != nil {
		return false, "pubkey not loaded (" + err.Error() + ")"
	}

	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=5000")
	if err != nil {
		return false, "db open: " + err.Error()
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	target, terr := filepath.Abs(pluginRoot)
	if terr != nil {
		target = pluginRoot
	}

	floor := time.Now().Add(-freshnessWindow()).Unix()
	row := db.QueryRow(`
		SELECT id, op_type, target, agent_id, IFNULL(bead_id,''), mode,
		       IFNULL(policy_match,''), IFNULL(policy_hash,''),
		       IFNULL(vetted_sha,''), IFNULL(vetting,''),
		       IFNULL(cross_project_id,''), created_at,
		       sig_version, signature
		FROM authorizations
		WHERE op_type='ic-publish-patch' AND target=? AND created_at >= ?
		ORDER BY created_at DESC LIMIT 1`, target, floor)

	var r authz.SignRow
	var sigVersion int
	var sig []byte
	if err := row.Scan(&r.ID, &r.OpType, &r.Target, &r.AgentID, &r.BeadID, &r.Mode,
		&r.PolicyMatch, &r.PolicyHash, &r.VettedSHA, &r.Vetting,
		&r.CrossProjectID, &r.CreatedAt, &sigVersion, &sig); err != nil {
		if err == sql.ErrNoRows {
			return false, "no fresh ic-publish-patch record (target=" + target + ")"
		}
		return false, "db scan: " + err.Error()
	}

	if sigVersion < 1 {
		return false, "pre-signing vintage row (force re-authorization)"
	}
	if len(sig) == 0 {
		return false, "authz row unsigned (signature NULL)"
	}
	if !authz.Verify(pub, r, sig) {
		return false, "signature verification failed (tampering or key rotation)"
	}
	return true, ""
}

// projectRootForPlugin walks up from a plugin root looking for the nearest
// project root (parent of a .clavain/ dir). Returns empty string if none
// is found. Used by engine.Publish to locate the pub key for token verify.
func projectRootForPlugin(pluginRoot string) string {
	db := findIntercoreDB(pluginRoot)
	if db == "" {
		return ""
	}
	return filepath.Dir(filepath.Dir(db))
}

// findIntercoreDB walks up from start (or its absolute form) looking for
// .clavain/intercore.db. Returns the absolute path or empty string.
func findIntercoreDB(start string) string {
	dir, err := filepath.Abs(start)
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, ".clavain", "intercore.db")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// freshnessWindow returns the max age of an authz record that still grants
// approval. Overridable via PUBLISH_AUTHZ_FRESHNESS_MIN (minutes).
func freshnessWindow() time.Duration {
	if v := os.Getenv("PUBLISH_AUTHZ_FRESHNESS_MIN"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return defaultAuthzFreshnessMin * time.Minute
}

// isAgentCommit checks the HEAD commit for agent authorship patterns.
func isAgentCommit(dir string) bool {
	authorCmd := exec.Command("git", "log", "-1", "--format=%ae")
	authorCmd.Dir = dir
	authorOut, err := authorCmd.Output()
	if err != nil {
		return false
	}
	author := strings.TrimSpace(strings.ToLower(string(authorOut)))

	for _, pattern := range agentAuthorPatterns {
		if strings.Contains(author, pattern) {
			return true
		}
	}

	msgCmd := exec.Command("git", "log", "-1", "--format=%B")
	msgCmd.Dir = dir
	msgOut, err := msgCmd.Output()
	if err != nil {
		return false
	}
	msg := string(msgOut)

	for _, pattern := range agentMessagePatterns {
		if strings.Contains(msg, pattern) {
			return true
		}
	}

	return false
}
