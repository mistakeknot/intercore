package publish

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// RequiresApproval checks if a plugin was recently mutated by an agent
// and requires human approval before auto-publish.
//
// Returns true (approval required) when:
// 1. The last commit matches agent author/message patterns, AND
// 2. No .publish-approved file exists in the plugin root
//
// The .publish-approved file acts as the human approval signal.
// It is consumed (deleted) on successful publish.
func RequiresApproval(pluginRoot string) bool {
	// Check for approval marker
	approvalFile := filepath.Join(pluginRoot, ".publish-approved")
	if _, err := os.Stat(approvalFile); err == nil {
		return false // human approved
	}

	// Check if last commit was agent-authored
	return isAgentCommit(pluginRoot)
}

// ConsumeApproval removes the .publish-approved marker after successful publish.
func ConsumeApproval(pluginRoot string) {
	os.Remove(filepath.Join(pluginRoot, ".publish-approved"))
}

// isAgentCommit checks the HEAD commit for agent authorship patterns.
func isAgentCommit(dir string) bool {
	// Get last commit author email
	authorCmd := exec.Command("git", "log", "-1", "--format=%ae")
	authorCmd.Dir = dir
	authorOut, err := authorCmd.Output()
	if err != nil {
		return false // can't determine, assume human
	}
	author := strings.TrimSpace(strings.ToLower(string(authorOut)))

	for _, pattern := range agentAuthorPatterns {
		if strings.Contains(author, pattern) {
			return true
		}
	}

	// Get last commit message
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
