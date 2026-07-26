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

// MarketplacesRoot returns the Claude Code marketplace checkout root
// (~/.claude/plugins/marketplaces), or "" if HOME cannot be determined.
func MarketplacesRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "marketplaces")
}

// MarketplaceVersions returns the version each marketplace's marketplace.json
// currently points at, keyed "<plugin>@<marketplace>". These are the versions
// `claude` will (re)install, so the prune paths treat them as protected
// regardless of installed_plugins.json state (Sylveste-0lt).
func MarketplaceVersions() map[string]string {
	return marketplaceVersionsIn(MarketplacesRoot())
}

// marketplaceVersionsIn is the testable core of MarketplaceVersions. Any
// unreadable or malformed marketplace.json is skipped: a missing guard entry
// only means less protection, never a failed prune.
func marketplaceVersionsIn(root string) map[string]string {
	out := map[string]string{}
	if root == "" {
		return out
	}
	dirs, err := os.ReadDir(root)
	if err != nil {
		return out
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, d.Name(), ".claude-plugin", "marketplace.json"))
		if err != nil {
			continue
		}
		var mkt marketplaceJSON
		if err := json.Unmarshal(data, &mkt); err != nil {
			continue
		}
		for _, raw := range mkt.Plugins {
			var entry pluginEntry
			if err := json.Unmarshal(raw, &entry); err != nil {
				continue
			}
			if entry.Name != "" && entry.Version != "" {
				out[entry.Name+"@"+d.Name()] = entry.Version
			}
		}
	}
	return out
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

// KnownMarketplaceClones returns every marketplace checkout this machine knows
// about, as absolute paths, deduplicated, each verified to contain a readable
// marketplace.json. marketRoot is always included when it qualifies.
//
// Why this exists (mk-963o): `ic publish` resolves the marketplace by walking up
// from cwd, so a plugin inside the Sylveste tree updates core/marketplace while a
// plugin outside it (interbrowse lives at ~/projects/interbrowse) updates the
// Claude Code checkout instead. Syncing only "monorepo -> CC" left the monorepo
// clone stale whenever a publish came in through the other door: publishing
// interbrowse 0.5.2 left core/marketplace reading 0.5.1.
//
// Extra clones can be declared with IC_MARKETPLACE_CLONES (os.PathListSeparator
// delimited) for layouts this does not guess.
func KnownMarketplaceClones(marketRoot string) []string {
	var cands []string
	if marketRoot != "" {
		cands = append(cands, marketRoot)
	}
	if cc := CCMarketplacePath(); cc != "" {
		cands = append(cands, cc)
	}
	if env := os.Getenv("IC_MARKETPLACE_CLONES"); env != "" {
		for _, p := range strings.Split(env, string(os.PathListSeparator)) {
			if p = strings.TrimSpace(p); p != "" {
				cands = append(cands, p)
			}
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		cands = append(cands, filepath.Join(home, "projects", "Sylveste", "core", "marketplace"))
	}

	seen := map[string]bool{}
	var out []string
	for _, c := range cands {
		abs, err := filepath.Abs(c)
		if err != nil || seen[abs] {
			continue
		}
		if _, err := os.Stat(filepath.Join(abs, ".claude-plugin", "marketplace.json")); err != nil {
			continue
		}
		seen[abs] = true
		out = append(out, abs)
	}
	return out
}

// MarketplaceCloneDivergence reports plugins whose version disagrees across the
// known clones. The returned map is keyed by plugin name; each value maps an
// absolute clone path to the version it records. Only disagreements appear.
//
// A plugin missing from a clone is not a disagreement -- clones legitimately
// carry different plugin sets -- only differing versions are.
func MarketplaceCloneDivergence(marketRoot string) (map[string]map[string]string, []string) {
	clones := KnownMarketplaceClones(marketRoot)
	if len(clones) < 2 {
		return nil, clones
	}
	perClone := make(map[string]map[string]string, len(clones))
	for _, c := range clones {
		if v, err := ListMarketplacePlugins(c); err == nil {
			perClone[c] = v
		}
	}
	out := map[string]map[string]string{}
	for clone, versions := range perClone {
		for name, ver := range versions {
			for other, otherVersions := range perClone {
				if other == clone {
					continue
				}
				otherVer, ok := otherVersions[name]
				if !ok || otherVer == ver {
					continue
				}
				if out[name] == nil {
					out[name] = map[string]string{}
				}
				out[name][clone] = ver
				out[name][other] = otherVer
			}
		}
	}
	return out, clones
}

// SyncPeerMarketplaces propagates a published version to every known clone other
// than the one just written. This is the symmetric replacement for
// SyncCCMarketplace: it works regardless of which clone the publish resolved to,
// which is the whole point (mk-963o).
func SyncPeerMarketplaces(marketRoot, pluginName, version string) error {
	absMarket, err := filepath.Abs(marketRoot)
	if err != nil {
		return fmt.Errorf("abs marketRoot: %w", err)
	}
	var firstErr error
	for _, clone := range KnownMarketplaceClones(marketRoot) {
		if clone == absMarket {
			continue
		}
		have, err := ReadMarketplaceVersion(clone, pluginName)
		if err != nil || have == version {
			continue // plugin absent from this clone, or already in sync
		}
		if err := UpdateMarketplaceVersion(clone, pluginName, version); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("update %s: %w", clone, err)
			}
			continue
		}
		// Best-effort publish of the sync commit, same posture as before.
		GitAdd(clone, filepath.Join(".claude-plugin", "marketplace.json"))
		GitCommit(clone, fmt.Sprintf("chore: sync %s to v%s", pluginName, version))
		GitPush(clone)
	}
	return firstErr
}

// SyncCCMarketplace updates the CC marketplace checkout to match the monorepo.
//
// Deprecated: use SyncPeerMarketplaces. This is one-directional and returns
// early when marketRoot IS the CC checkout, which is exactly the case that
// leaves the monorepo clone stale (mk-963o). Retained so existing call sites
// keep compiling; it now delegates.
func SyncCCMarketplace(marketRoot, pluginName, version string) error {
	return SyncPeerMarketplaces(marketRoot, pluginName, version)
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
