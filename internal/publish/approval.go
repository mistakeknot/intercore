package publish

import (
	"database/sql"
	"fmt"
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

// RequiresApproval returns true when the last commit is agent-authored AND
// neither approval signal is present:
//   - a fresh, signature-valid ic-publish-patch authz record for this plugin, OR
//   - a .publish-approved marker file in the plugin root (legacy fallback).
//
// The authz path is preferred. Marker-file fallback is kept through one
// deprecation window and scheduled for removal in v2. When approval is granted
// via the marker while an authz path is reachable, a one-line deprecation
// warning is written to stderr.
func RequiresApproval(pluginRoot string) bool {
	if !isAgentCommit(pluginRoot) {
		return false
	}

	// Authz path first: fresh, valid signature → approved.
	authzOK, authzReason := checkAuthzApproval(pluginRoot)
	if authzOK {
		return false
	}

	approvalFile := filepath.Join(pluginRoot, ".publish-approved")
	if _, err := os.Stat(approvalFile); err == nil {
		// Marker-file approval still wins, but warn when authz was available
		// and either absent or invalid — surfaces the deprecation path.
		if authzReason != "" {
			fmt.Fprintf(os.Stderr,
				"publish: .publish-approved used; authz: %s. "+
					"Marker-file approval is deprecated — see docs/canon/policy-merge.md.\n",
				authzReason)
		}
		return false
	}

	return true
}

// ConsumeApproval removes the .publish-approved marker after successful publish.
// Authz records are audit history and are NOT removed — they persist as a
// permanent record of the approval event.
func ConsumeApproval(pluginRoot string) {
	os.Remove(filepath.Join(pluginRoot, ".publish-approved"))
}

// checkAuthzApproval returns (true, "") when a fresh valid-signature authz
// record approves this publish. Returns (false, reason) otherwise, where
// reason explains why the authz path did not grant approval — used in the
// marker-fallback warning message.
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
