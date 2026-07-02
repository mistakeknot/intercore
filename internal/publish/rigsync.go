package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// agentRig represents the structure of agent-rig.json (only the fields we need).
type agentRig struct {
	raw map[string]json.RawMessage // preserve all fields
}

type rigPluginEntry struct {
	Source      string `json:"source"`
	Description string `json:"description"`
}

// RigSyncResult reports what happened during rig sync.
type RigSyncResult struct {
	RigPath  string // absolute path to agent-rig.json
	ClavRoot string // absolute path to Clavain repo root
	Added    bool   // true if a new entry was added
	Plugin   string // plugin name that was added
}

// FindAgentRig locates agent-rig.json by walking up from a plugin directory.
// It looks for the Demarch monorepo pattern: <root>/os/clavain/agent-rig.json
func FindAgentRig(from string) (string, error) {
	abs, _ := filepath.Abs(from)
	current := abs
	for i := 0; i < 6; i++ {
		candidate := filepath.Join(current, "os", "clavain", "agent-rig.json")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("agent-rig.json not found (walked up from %s)", from)
}

// SyncRig ensures a plugin is listed in agent-rig.json's recommended section.
// If the plugin is already listed in any tier (required, recommended, optional),
// this is a no-op. Returns a result describing what happened.
func SyncRig(rigPath string, pluginName, description, marketplace string) (*RigSyncResult, error) {
	result := &RigSyncResult{
		RigPath:  rigPath,
		ClavRoot: filepath.Dir(rigPath), // agent-rig.json is at clavain root
		Plugin:   pluginName,
	}

	data, err := os.ReadFile(rigPath)
	if err != nil {
		return nil, fmt.Errorf("read agent-rig.json: %w", err)
	}

	// Check if plugin is already listed in any tier
	source := pluginName + "@" + marketplace
	if rigContainsPlugin(data, source) {
		result.Added = false
		return result, nil
	}

	// Add to recommended
	updated, err := rigAddRecommended(data, source, description)
	if err != nil {
		return nil, fmt.Errorf("add to agent-rig.json: %w", err)
	}

	if err := atomicWrite(rigPath, updated); err != nil {
		return nil, fmt.Errorf("write agent-rig.json: %w", err)
	}

	result.Added = true
	return result, nil
}

// CommitAndPushRig commits the agent-rig.json change and pushes.
func CommitAndPushRig(clavRoot, pluginName, version string) error {
	relPath := "agent-rig.json"
	if err := GitAdd(clavRoot, relPath); err != nil {
		return fmt.Errorf("git add agent-rig.json: %w", err)
	}

	msg := fmt.Sprintf("feat(rig): add %s to recommended companions", pluginName)
	if version != "" {
		msg = fmt.Sprintf("feat(rig): add %s v%s to recommended companions", pluginName, version)
	}

	if err := GitCommit(clavRoot, msg); err != nil {
		return fmt.Errorf("commit agent-rig.json: %w", err)
	}

	if err := GitPullRebase(clavRoot); err != nil {
		return fmt.Errorf("pull --rebase (clavain): %w", err)
	}

	if err := GitPush(clavRoot); err != nil {
		return fmt.Errorf("push (clavain): %w", err)
	}

	return nil
}

// rigContainsPlugin checks if a plugin source string appears anywhere in agent-rig.json.
func rigContainsPlugin(data []byte, source string) bool {
	// Simple string search — source strings are unique enough
	return strings.Contains(string(data), `"`+source+`"`)
}

// rigAddRecommended adds a new entry to the "recommended" array in agent-rig.json.
// Uses careful JSON manipulation to preserve formatting.
func rigAddRecommended(data []byte, source, description string) ([]byte, error) {
	// Parse the full structure
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse agent-rig.json: %w", err)
	}

	pluginsRaw, ok := raw["plugins"]
	if !ok {
		return nil, fmt.Errorf("no 'plugins' key in agent-rig.json")
	}

	var plugins map[string]json.RawMessage
	if err := json.Unmarshal(pluginsRaw, &plugins); err != nil {
		return nil, fmt.Errorf("parse plugins: %w", err)
	}

	recRaw, ok := plugins["recommended"]
	if !ok {
		return nil, fmt.Errorf("no 'recommended' key in plugins")
	}

	var recommended []json.RawMessage
	if err := json.Unmarshal(recRaw, &recommended); err != nil {
		return nil, fmt.Errorf("parse recommended: %w", err)
	}

	// Create new entry
	newEntry := rigPluginEntry{
		Source:      source,
		Description: description,
	}
	entryBytes, err := json.Marshal(newEntry)
	if err != nil {
		return nil, fmt.Errorf("marshal new entry: %w", err)
	}

	recommended = append(recommended, json.RawMessage(entryBytes))

	// Marshal back through the hierarchy
	recBytes, _ := json.Marshal(recommended)
	plugins["recommended"] = json.RawMessage(recBytes)

	pluginsBytes, _ := json.Marshal(plugins)
	raw["plugins"] = json.RawMessage(pluginsBytes)

	result, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent-rig.json: %w", err)
	}

	// Ensure trailing newline
	if len(result) > 0 && result[len(result)-1] != '\n' {
		result = append(result, '\n')
	}

	return result, nil
}
