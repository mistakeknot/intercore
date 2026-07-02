package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const marketplaceRelPath = "core/marketplace/.claude-plugin/marketplace.json"

// FindMarketplace locates marketplace.json via walk-up algorithm.
// Walks up from 'from' up to 4 levels looking for core/marketplace/.claude-plugin/marketplace.json.
// Falls back to ~/.claude/plugins/marketplaces/interagency-marketplace/.claude-plugin/marketplace.json.
func FindMarketplace(from string) (string, error) {
	abs, err := filepath.Abs(from)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}

	// Stage 1: walk up to 4 levels looking for monorepo marketplace
	current := abs
	for i := 0; i < 5; i++ { // 5 iterations = check 'from' + 4 parents
		candidate := filepath.Join(current, marketplaceRelPath)
		if _, err := os.Stat(candidate); err == nil {
			return filepath.Dir(filepath.Dir(candidate)), nil // return the marketplace root (parent of .claude-plugin/)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}

	// Stage 2: Claude Code marketplace checkout
	home, err := os.UserHomeDir()
	if err != nil {
		return "", ErrNoMarketplace
	}
	ccPath := filepath.Join(home, ".claude", "plugins", "marketplaces", "interagency-marketplace", ".claude-plugin", "marketplace.json")
	if _, err := os.Stat(ccPath); err == nil {
		return filepath.Dir(filepath.Dir(ccPath)), nil
	}

	return "", ErrNoMarketplace
}

// marketplaceJSON represents the marketplace.json structure.
// Uses json.RawMessage to preserve fields we don't modify.
type marketplaceJSON struct {
	Plugins []json.RawMessage `json:"plugins"`
}

type pluginEntry struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ReadMarketplaceVersion reads a plugin's version from marketplace.json.
func ReadMarketplaceVersion(marketRoot, pluginName string) (string, error) {
	data, err := readMarketplaceFile(marketRoot)
	if err != nil {
		return "", err
	}

	var mkt marketplaceJSON
	if err := json.Unmarshal(data, &mkt); err != nil {
		return "", fmt.Errorf("parse marketplace.json: %w", err)
	}

	for _, raw := range mkt.Plugins {
		var entry pluginEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		if entry.Name == pluginName {
			return entry.Version, nil
		}
	}
	return "", ErrNotInMarketplace
}

// UpdateMarketplaceVersion updates a plugin's version in marketplace.json.
// Preserves all other fields via json.RawMessage round-trip.
func UpdateMarketplaceVersion(marketRoot, pluginName, version string) error {
	path := marketplaceFilePath(marketRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read marketplace.json: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse marketplace.json: %w", err)
	}

	pluginsRaw, ok := raw["plugins"]
	if !ok {
		return fmt.Errorf("marketplace.json: missing 'plugins' array")
	}

	var plugins []json.RawMessage
	if err := json.Unmarshal(pluginsRaw, &plugins); err != nil {
		return fmt.Errorf("parse plugins array: %w", err)
	}

	found := false
	for i, p := range plugins {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(p, &entry); err != nil {
			continue
		}
		var name string
		if nameRaw, ok := entry["name"]; ok {
			json.Unmarshal(nameRaw, &name)
		}
		if name == pluginName {
			vBytes, _ := json.Marshal(version)
			entry["version"] = vBytes
			updated, _ := json.Marshal(entry)
			plugins[i] = updated
			found = true
			break
		}
	}

	if !found {
		return ErrNotInMarketplace
	}

	updatedPlugins, _ := json.Marshal(plugins)
	raw["plugins"] = updatedPlugins

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marketplace.json: %w", err)
	}

	return atomicWrite(path, append(out, '\n'))
}

// ListMarketplacePlugins returns all plugin names and versions from marketplace.json.
func ListMarketplacePlugins(marketRoot string) (map[string]string, error) {
	data, err := readMarketplaceFile(marketRoot)
	if err != nil {
		return nil, err
	}

	var mkt marketplaceJSON
	if err := json.Unmarshal(data, &mkt); err != nil {
		return nil, fmt.Errorf("parse marketplace.json: %w", err)
	}

	result := make(map[string]string, len(mkt.Plugins))
	for _, raw := range mkt.Plugins {
		var entry pluginEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			continue
		}
		result[entry.Name] = entry.Version
	}
	return result, nil
}

// RegisterPlugin adds a new plugin entry to marketplace.json.
// If pluginRoot is non-empty, the source URL is derived from the plugin's git remote origin.
// Otherwise falls back to the plugin name as a GitHub repo under the marketplace org.
func RegisterPlugin(marketRoot string, plugin *Plugin, pluginRoot ...string) error {
	path := marketplaceFilePath(marketRoot)
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read marketplace.json: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("parse marketplace.json: %w", err)
	}

	pluginsRaw, ok := raw["plugins"]
	if !ok {
		return fmt.Errorf("marketplace.json: missing 'plugins' array")
	}

	var plugins []json.RawMessage
	if err := json.Unmarshal(pluginsRaw, &plugins); err != nil {
		return fmt.Errorf("parse plugins array: %w", err)
	}

	// Check not already registered
	for _, p := range plugins {
		var entry pluginEntry
		if err := json.Unmarshal(p, &entry); err != nil {
			continue
		}
		if entry.Name == plugin.Name {
			return fmt.Errorf("plugin %q already registered in marketplace", plugin.Name)
		}
	}

	// Derive source URL from git remote origin if available
	sourceURL := ""
	if len(pluginRoot) > 0 && pluginRoot[0] != "" {
		if remoteURL, err := GitRemoteURL(pluginRoot[0]); err == nil {
			sourceURL = remoteURL
		}
	}
	if sourceURL == "" {
		// Fallback: infer from marketplace remote to get the org
		org := inferOrgFromMarketplace(marketRoot)
		sourceURL = fmt.Sprintf("https://github.com/%s/%s.git", org, plugin.Name)
	}

	newEntry := map[string]interface{}{
		"name": plugin.Name,
		"source": map[string]string{
			"source": "url",
			"url":    sourceURL,
		},
		"description": "",
		"version":     plugin.Version,
		"keywords":    []string{},
		"strict":      true,
	}
	entryBytes, _ := json.Marshal(newEntry)
	plugins = append(plugins, entryBytes)

	updatedPlugins, _ := json.Marshal(plugins)
	raw["plugins"] = updatedPlugins

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal marketplace.json: %w", err)
	}

	return atomicWrite(path, append(out, '\n'))
}

// CCMarketplacePath returns the Claude Code marketplace checkout path, or empty if not found.
func CCMarketplacePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(home, ".claude", "plugins", "marketplaces", "interagency-marketplace")
	if _, err := os.Stat(filepath.Join(p, ".claude-plugin", "marketplace.json")); err == nil {
		return p
	}
	return ""
}

// SyncCCMarketplace updates the CC marketplace checkout to match the monorepo.
func SyncCCMarketplace(marketRoot, pluginName, version string) error {
	ccPath := CCMarketplacePath()
	if ccPath == "" {
		return nil // no CC checkout, nothing to sync
	}

	// Check if they're the same directory
	absMarket, _ := filepath.Abs(marketRoot)
	absCCPath, _ := filepath.Abs(ccPath)
	if absMarket == absCCPath {
		return nil // same directory, no sync needed
	}

	ccVersion, err := ReadMarketplaceVersion(ccPath, pluginName)
	if err != nil {
		return nil // plugin not in CC marketplace, skip
	}
	if ccVersion == version {
		return nil // already in sync
	}

	if err := UpdateMarketplaceVersion(ccPath, pluginName, version); err != nil {
		return fmt.Errorf("update CC marketplace: %w", err)
	}

	// Commit and push the CC marketplace checkout (best-effort)
	GitAdd(ccPath, filepath.Join(".claude-plugin", "marketplace.json"))
	msg := fmt.Sprintf("chore: sync %s to v%s", pluginName, version)
	GitCommit(ccPath, msg)
	GitPush(ccPath) // errors are ignored — CC sync is best-effort

	return nil
}

// RefreshCCMarketplace tells the running Claude Code process to re-read the marketplace index.
// Best-effort: errors are returned but callers should treat them as non-fatal.
func RefreshCCMarketplace() error {
	cmd := execCommand("claude", "plugin", "marketplace", "update", "interagency-marketplace")
	cmd.Stdout = os.Stderr // surface output but don't pollute stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// inferOrgFromMarketplace extracts the GitHub org from the marketplace repo's remote.
// e.g., "https://github.com/mistakeknot/interagency-marketplace.git" → "mistakeknot"
func inferOrgFromMarketplace(marketRoot string) string {
	remoteURL, err := GitRemoteURL(marketRoot)
	if err != nil {
		return "mistakeknot" // final fallback
	}
	// Parse org from GitHub URL patterns:
	//   https://github.com/ORG/REPO.git
	//   git@github.com:ORG/REPO.git
	remoteURL = strings.TrimSuffix(remoteURL, ".git")
	if idx := strings.Index(remoteURL, "github.com"); idx >= 0 {
		// Skip "github.com" + separator (/ or :)
		rest := remoteURL[idx+len("github.com"):]
		if len(rest) > 0 && (rest[0] == '/' || rest[0] == ':') {
			rest = rest[1:]
		}
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) >= 1 && parts[0] != "" {
			return parts[0]
		}
	}
	return "mistakeknot" // final fallback
}

func marketplaceFilePath(root string) string {
	return filepath.Join(root, ".claude-plugin", "marketplace.json")
}

func readMarketplaceFile(root string) ([]byte, error) {
	return os.ReadFile(marketplaceFilePath(root))
}

// atomicWrite writes data to a file atomically via temp file + rename.
func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("write temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("close temp: %w", err)
	}

	// Preserve original file permissions
	if info, err := os.Stat(path); err == nil {
		os.Chmod(tmpPath, info.Mode())
	}

	if err := os.Rename(tmpPath, path); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename: %w", err)
	}
	return nil
}
