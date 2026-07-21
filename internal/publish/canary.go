package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// ReleaseCanary is one post-publish health record (sylveste-ao0q). A publish
// registers a pending canary; the session-start check (Clavain
// release-canary-check.sh) marks it passed when the plugin actually resolved
// in a live session, or failed with a loud alert carrying the ready-to-run
// rollback command.
type ReleaseCanary struct {
	Plugin       string `json:"plugin"`
	Marketplace  string `json:"marketplace"`
	Version      string `json:"version"`
	PriorVersion string `json:"prior_version,omitempty"`
	PublishedAt  int64  `json:"published_at"`
	Status       string `json:"status"` // pending | passed | failed | rolled_back
	CheckedAt    int64  `json:"checked_at,omitempty"`
	Note         string `json:"note,omitempty"`
}

// CanaryPath returns the release-canary state file (~/.clavain/release-canaries.json).
// JSON on purpose: the session-start check is a jq-based hook.
func CanaryPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".clavain", "release-canaries.json")
}

func readCanariesFrom(path string) ([]ReleaseCanary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read release canaries: %w", err)
	}
	var cs []ReleaseCanary
	if err := json.Unmarshal(data, &cs); err != nil {
		return nil, fmt.Errorf("parse release canaries: %w", err)
	}
	return cs, nil
}

func writeCanariesTo(path string, cs []ReleaseCanary) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create canary dir: %w", err)
	}
	data, err := json.MarshalIndent(cs, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal release canaries: %w", err)
	}
	return atomicWrite(path, append(data, '\n'))
}

// RegisterCanary upserts the record for the plugin+marketplace: one live
// canary per plugin — a new publish replaces any older record regardless of
// its status (the newest release is the one under watch).
func RegisterCanary(c ReleaseCanary) error {
	path := CanaryPath()
	if path == "" {
		return fmt.Errorf("cannot determine canary path")
	}
	return registerCanaryIn(path, c)
}

// registerCanaryIn is the testable core of RegisterCanary.
func registerCanaryIn(path string, c ReleaseCanary) error {
	cs, err := readCanariesFrom(path)
	if err != nil {
		return err
	}
	out := cs[:0]
	for _, existing := range cs {
		if existing.Plugin == c.Plugin && existing.Marketplace == c.Marketplace {
			continue
		}
		out = append(out, existing)
	}
	out = append(out, c)
	return writeCanariesTo(path, out)
}

// markCanaryIn updates the status/note of a plugin's canary record.
func markCanaryIn(path, plugin, marketplace, status, note string) error {
	cs, err := readCanariesFrom(path)
	if err != nil {
		return err
	}
	for i := range cs {
		if cs[i].Plugin == plugin && cs[i].Marketplace == marketplace {
			cs[i].Status = status
			cs[i].Note = note
			cs[i].CheckedAt = time.Now().Unix()
		}
	}
	return writeCanariesTo(path, cs)
}

// ProbeIssue is one failed check from the post-release probe.
type ProbeIssue struct {
	Check  string // schema | hooks | cache-entry | pointer
	Detail string
}

// ProbeArtifact runs doctor-grade checks on a single published artifact dir:
// plugin.json parses with required fields and no unrecognized keys, and hook
// declarations are loadable. Scoped variant of the doctor's checks 8+9 so the
// publish flow can probe the one plugin it just shipped.
func ProbeArtifact(dir string) []ProbeIssue {
	var issues []ProbeIssue

	pluginJSON := filepath.Join(dir, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(pluginJSON)
	if err != nil {
		return append(issues, ProbeIssue{"schema", fmt.Sprintf("plugin.json unreadable: %v", err)})
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return append(issues, ProbeIssue{"schema", fmt.Sprintf("plugin.json invalid JSON: %v", err)})
	}
	for _, field := range []string{"name", "version"} {
		if _, ok := raw[field]; !ok {
			issues = append(issues, ProbeIssue{"schema", "missing required field: " + field})
		}
	}
	allowedKeys := map[string]bool{
		"name": true, "version": true, "description": true, "author": true,
		"repository": true, "homepage": true, "license": true, "keywords": true,
		"skills": true, "commands": true, "agents": true, "mcpServers": true,
		"hooks": true, "lspServers": true,
	}
	for key := range raw {
		if !allowedKeys[key] {
			issues = append(issues, ProbeIssue{"schema", fmt.Sprintf("unrecognized key %q (Claude Code rejects unknown keys)", key)})
		}
	}
	if authorRaw, ok := raw["author"]; ok {
		var authorStr string
		if json.Unmarshal(authorRaw, &authorStr) == nil {
			issues = append(issues, ProbeIssue{"schema", "author must be an object, not a string"})
		}
	}

	// Hook declarations: standard-path hooks.json must parse; a plugin.json
	// "hooks" declaration must not duplicate the auto-loaded standard path
	// and must point at an existing file (both break plugin load).
	standardHooks := ""
	for _, rel := range []string{"hooks/hooks.json", filepath.Join(".claude-plugin", "hooks", "hooks.json")} {
		p := filepath.Join(dir, rel)
		if _, err := os.Stat(p); err == nil {
			standardHooks = p
			break
		}
	}
	if standardHooks != "" {
		hd, err := os.ReadFile(standardHooks)
		if err != nil || !json.Valid(hd) {
			issues = append(issues, ProbeIssue{"hooks", "hooks.json unreadable or invalid JSON"})
		}
	}
	if declRaw, ok := raw["hooks"]; ok {
		var declared string
		if json.Unmarshal(declRaw, &declared) == nil && declared != "" {
			if standardHooks != "" {
				issues = append(issues, ProbeIssue{"hooks", "plugin.json declares hooks while the standard path exists — double load breaks the plugin"})
			}
			declPath := filepath.Join(dir, filepath.FromSlash(declared))
			if _, err := os.Stat(declPath); err != nil {
				issues = append(issues, ProbeIssue{"hooks", fmt.Sprintf("declared hooks file missing: %s", declared)})
			}
		}
	}
	return issues
}

// ProbeRelease is the full post-release probe: the artifact itself plus
// pointer agreement — installed_plugins.json and marketplace.json must both
// name the just-published version and the installPath must exist.
func ProbeRelease(pluginName, marketplace, version string) []ProbeIssue {
	var issues []ProbeIssue
	key := pluginName + "@" + marketplace

	cacheDir := filepath.Join(CacheRoot(), marketplace, pluginName, version)
	if _, err := os.Stat(cacheDir); err != nil {
		issues = append(issues, ProbeIssue{"cache-entry", "published cache dir missing: " + cacheDir})
	} else {
		issues = append(issues, ProbeArtifact(cacheDir)...)
	}

	if ip, err := ReadInstalled(); err == nil {
		if rec, ok := ip.Plugins[key]; ok && len(rec) > 0 {
			if rec[0].Version != version {
				issues = append(issues, ProbeIssue{"pointer", fmt.Sprintf("installed_plugins.json at %s, published %s", rec[0].Version, version)})
			}
			if _, err := os.Stat(rec[0].InstallPath); err != nil {
				issues = append(issues, ProbeIssue{"pointer", "installPath missing: " + rec[0].InstallPath})
			}
		} else {
			issues = append(issues, ProbeIssue{"pointer", "no installed_plugins.json record for " + key})
		}
	}

	if mv := MarketplaceVersions()[key]; mv != "" && mv != version {
		issues = append(issues, ProbeIssue{"pointer", fmt.Sprintf("marketplace.json points at %s, published %s", mv, version)})
	}
	return issues
}

// resolveRollbackTarget picks the version to roll back to: the canary's
// recorded prior version when its cache dir survives, else the newest cached
// version older than current. Errors when nothing usable is retained — the
// verb's clean refusal.
func resolveRollbackTarget(versions []CacheEntry, current, recordedPrior string) (string, error) {
	if recordedPrior != "" && recordedPrior != current {
		for _, v := range versions {
			if v.Version == recordedPrior && !v.IsSymlink && !v.Orphaned {
				return recordedPrior, nil
			}
		}
	}
	var candidates []CacheEntry
	for _, v := range versions {
		if v.IsSymlink || v.Orphaned || v.Version == current {
			continue
		}
		if CompareVersions(v.Version, current) < 0 {
			candidates = append(candidates, v)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no retained prior version of the plugin in the cache (current %s) — nothing to roll back to", current)
	}
	sortVersionsDesc(candidates)
	return candidates[0].Version, nil
}

// rollbackLocalState repoints marketplace.json and installed_plugins.json at
// the target version and verifies the target's cache dir exists. The pure
// state half of RollbackPlugin — no git, so a harness can test the restore.
// Note: the installed record keeps its old gitCommitSha (UpdateInstalled only
// overwrites a non-empty sha); installPath is the primary resolution key.
func rollbackLocalState(marketRoot, pluginName, target string) error {
	cachePath := filepath.Join(CacheBase(), pluginName, target)
	if _, err := os.Stat(cachePath); err != nil {
		return fmt.Errorf("target cache dir missing: %s", cachePath)
	}
	if err := UpdateMarketplaceVersion(marketRoot, pluginName, target); err != nil {
		return fmt.Errorf("marketplace.json: %w", err)
	}
	if err := UpdateInstalled(pluginName, target, cachePath, ""); err != nil {
		return fmt.Errorf("installed_plugins.json: %w", err)
	}
	return nil
}

// RollbackPlugin reverts a plugin to its retained prior version: marketplace
// pointer (committed and pushed), installed pointer, CC marketplace sync, and
// the canary record. Refuses cleanly when no prior version is retained.
func RollbackPlugin(from, pluginName string, out func(string, ...interface{})) error {
	const marketplace = "interagency-marketplace"

	marketRoot, err := FindMarketplace(from)
	if err != nil {
		return fmt.Errorf("locate marketplace: %w", err)
	}
	current, err := ReadMarketplaceVersion(marketRoot, pluginName)
	if err != nil {
		return fmt.Errorf("current version: %w", err)
	}

	entries, err := ListCacheEntries()
	if err != nil {
		return fmt.Errorf("cache entries: %w", err)
	}
	var recordedPrior string
	if cs, err := readCanariesFrom(CanaryPath()); err == nil {
		for _, c := range cs {
			if c.Plugin == pluginName && c.Marketplace == marketplace {
				recordedPrior = c.PriorVersion
			}
		}
	}
	target, err := resolveRollbackTarget(entries[pluginName], current, recordedPrior)
	if err != nil {
		return err
	}

	out("  Rolling back %s: %s → %s\n", pluginName, current, target)
	if err := rollbackLocalState(marketRoot, pluginName, target); err != nil {
		return err
	}

	if err := GitAdd(marketRoot, filepath.Join(".claude-plugin", "marketplace.json")); err == nil {
		if err := GitCommit(marketRoot, fmt.Sprintf("revert: roll back %s to v%s (release canary)", pluginName, target)); err != nil {
			out("  warning: marketplace commit: %v\n", err)
		} else if err := GitPullRebase(marketRoot); err != nil {
			out("  warning: marketplace pull: %v\n", err)
		} else if err := GitPush(marketRoot); err != nil {
			out("  warning: marketplace push: %v\n", err)
		}
	}

	if err := SyncCCMarketplace(marketRoot, pluginName, target); err != nil {
		out("  warning: CC marketplace sync: %v\n", err)
	}
	if err := RefreshCCMarketplace(); err != nil {
		out("  warning: CC marketplace refresh: %v\n", err)
	}
	if path := CanaryPath(); path != "" {
		if err := markCanaryIn(path, pluginName, marketplace, "rolled_back", "rolled back to "+target); err != nil {
			out("  warning: canary record: %v\n", err)
		}
	}
	out("  Rolled back %s to v%s — restart sessions to pick it up\n", pluginName, target)
	return nil
}
