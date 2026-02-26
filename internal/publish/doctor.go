package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Finding represents a single health check result.
type Finding struct {
	Severity string `json:"severity"` // "error", "warning", "info"
	Category string `json:"category"` // "drift", "cache", "schema", "hooks"
	Plugin   string `json:"plugin"`
	Message  string `json:"message"`
	Fix      string `json:"fix"` // description of auto-fix action
}

// DoctorOpts configures the doctor run.
type DoctorOpts struct {
	Fix  bool
	JSON bool
}

// DoctorResult holds all findings from a doctor run.
type DoctorResult struct {
	Findings   []Finding `json:"findings"`
	PluginDirs []string  `json:"-"` // discovered plugin directories
	MarketRoot string    `json:"-"`
}

// RunDoctor performs comprehensive health checks on the plugin publishing ecosystem.
func RunDoctor(ctx context.Context, opts DoctorOpts) (*DoctorResult, error) {
	result := &DoctorResult{}

	// Find marketplace
	cwd, _ := os.Getwd()
	marketRoot, err := FindMarketplace(cwd)
	if err != nil {
		return nil, fmt.Errorf("cannot find marketplace: %w", err)
	}
	result.MarketRoot = marketRoot

	// Load marketplace versions
	mktVersions, err := ListMarketplacePlugins(marketRoot)
	if err != nil {
		return nil, fmt.Errorf("read marketplace: %w", err)
	}

	// Discover plugin directories (scan interverse/ and os/clavain)
	result.PluginDirs = discoverPluginDirs(cwd)

	// Check 1: Version drift (plugin.json vs marketplace.json)
	checkPluginMarketplaceDrift(result, mktVersions, opts)

	// Check 2: Installed drift (installed_plugins.json vs marketplace.json)
	checkInstalledDrift(result, mktVersions, opts)

	// Check 3: CC marketplace desync
	checkCCMarketplaceSync(result, marketRoot, mktVersions, opts)

	// Check 4: Orphaned cache dirs
	checkOrphanedCache(result, opts)

	// Check 5: Missing cache entries
	checkMissingCache(result, mktVersions, opts)

	// Check 6: .git in cache
	checkGitInCache(result, opts)

	// Check 7: plugin.json schema validation
	checkPluginSchemas(result, opts)

	// Check 8: Undeclared hooks
	checkUndeclaredHooks(result)

	return result, nil
}

func checkPluginMarketplaceDrift(result *DoctorResult, mktVersions map[string]string, opts DoctorOpts) {
	for _, dir := range result.PluginDirs {
		plugin, err := ReadPlugin(dir)
		if err != nil {
			continue
		}
		mktVer, ok := mktVersions[plugin.Name]
		if !ok {
			result.Findings = append(result.Findings, Finding{
				Severity: "warning",
				Category: "drift",
				Plugin:   plugin.Name,
				Message:  "not registered in marketplace",
				Fix:      "run: ic publish init",
			})
			continue
		}
		if plugin.Version != mktVer {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "drift",
				Plugin:   plugin.Name,
				Message:  fmt.Sprintf("plugin.json=%s marketplace=%s", plugin.Version, mktVer),
				Fix:      fmt.Sprintf("update marketplace to %s", plugin.Version),
			})
			if opts.Fix {
				UpdateMarketplaceVersion(result.MarketRoot, plugin.Name, plugin.Version)
			}
		}
	}
}

func checkInstalledDrift(result *DoctorResult, mktVersions map[string]string, opts DoctorOpts) {
	ip, err := ReadInstalled()
	if err != nil {
		result.Findings = append(result.Findings, Finding{
			Severity: "warning",
			Category: "drift",
			Message:  fmt.Sprintf("cannot read installed_plugins.json: %v", err),
		})
		return
	}

	for name, mktVer := range mktVersions {
		key := name + "@interagency-marketplace"
		entries, ok := ip.Plugins[key]
		if !ok || len(entries) == 0 {
			continue // not installed, skip
		}
		instVer := entries[0].Version
		if instVer != mktVer {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "drift",
				Plugin:   name,
				Message:  fmt.Sprintf("installed=%s marketplace=%s", instVer, mktVer),
				Fix:      fmt.Sprintf("update installed to %s", mktVer),
			})
			if opts.Fix {
				cachePath := filepath.Join(CacheBase(), name, mktVer)
				UpdateInstalled(name, mktVer, cachePath)
			}
		}
	}
}

func checkCCMarketplaceSync(result *DoctorResult, marketRoot string, mktVersions map[string]string, opts DoctorOpts) {
	ccPath := CCMarketplacePath()
	if ccPath == "" {
		return
	}
	absMarket, _ := filepath.Abs(marketRoot)
	absCCPath, _ := filepath.Abs(ccPath)
	if absMarket == absCCPath {
		return // same directory
	}

	ccVersions, err := ListMarketplacePlugins(ccPath)
	if err != nil {
		result.Findings = append(result.Findings, Finding{
			Severity: "warning",
			Category: "drift",
			Message:  fmt.Sprintf("cannot read CC marketplace: %v", err),
		})
		return
	}

	for name, mktVer := range mktVersions {
		ccVer, ok := ccVersions[name]
		if !ok {
			continue
		}
		if ccVer != mktVer {
			result.Findings = append(result.Findings, Finding{
				Severity: "warning",
				Category: "drift",
				Plugin:   name,
				Message:  fmt.Sprintf("CC marketplace=%s monorepo=%s", ccVer, mktVer),
				Fix:      "sync CC marketplace",
			})
			if opts.Fix {
				SyncCCMarketplace(marketRoot, name, mktVer)
			}
		}
	}
}

func checkOrphanedCache(result *DoctorResult, opts DoctorOpts) {
	base := CacheBase()
	if base == "" {
		return
	}

	orphanCount := 0
	filepath.WalkDir(base, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.Name() == ".orphaned_at" && !d.IsDir() {
			if !strings.Contains(filepath.Dir(path), "temp_git_") {
				orphanCount++
			}
		}
		return nil
	})

	if orphanCount > 0 {
		result.Findings = append(result.Findings, Finding{
			Severity: "warning",
			Category: "cache",
			Message:  fmt.Sprintf("%d orphaned cache directories", orphanCount),
			Fix:      "clean orphaned dirs",
		})
		if opts.Fix {
			count, bytes, _ := CleanOrphans()
			if count > 0 {
				result.Findings = append(result.Findings, Finding{
					Severity: "info",
					Category: "cache",
					Message:  fmt.Sprintf("cleaned %d orphaned dirs (%.1f MB freed)", count, float64(bytes)/1024/1024),
				})
			}
		}
	}
}

func checkMissingCache(result *DoctorResult, mktVersions map[string]string, opts DoctorOpts) {
	ip, err := ReadInstalled()
	if err != nil {
		return
	}

	for name, ver := range mktVersions {
		key := name + "@interagency-marketplace"
		entries, ok := ip.Plugins[key]
		if !ok || len(entries) == 0 {
			continue
		}
		cachePath := entries[0].InstallPath
		if cachePath == "" {
			continue
		}
		if _, err := os.Stat(cachePath); os.IsNotExist(err) {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "cache",
				Plugin:   name,
				Message:  fmt.Sprintf("cache dir missing: %s", cachePath),
				Fix:      "rebuild cache entry",
			})
			if opts.Fix {
				// Try to find the plugin source to rebuild
				for _, dir := range result.PluginDirs {
					p, err := ReadPlugin(dir)
					if err == nil && p.Name == name {
						RebuildCache(name, ver, dir)
						break
					}
				}
			}
		}
	}
}

func checkGitInCache(result *DoctorResult, opts DoctorOpts) {
	base := CacheBase()
	if base == "" {
		return
	}

	gitCount := 0
	filepath.WalkDir(base, func(path string, d fs.DirEntry, _ error) error {
		if d != nil && d.IsDir() && d.Name() == ".git" {
			gitCount++
			return filepath.SkipDir
		}
		return nil
	})

	if gitCount > 0 {
		result.Findings = append(result.Findings, Finding{
			Severity: "warning",
			Category: "cache",
			Message:  fmt.Sprintf("%d .git directories in cache entries", gitCount),
			Fix:      "strip .git from cache",
		})
		if opts.Fix {
			count, bytes, _ := StripGitDirs()
			if count > 0 {
				result.Findings = append(result.Findings, Finding{
					Severity: "info",
					Category: "cache",
					Message:  fmt.Sprintf("stripped %d .git dirs (%.1f MB freed)", count, float64(bytes)/1024/1024),
				})
			}
		}
	}
}

func checkPluginSchemas(result *DoctorResult, opts DoctorOpts) {
	allowedKeys := map[string]bool{
		"name": true, "version": true, "description": true, "author": true,
		"repository": true, "homepage": true, "license": true, "keywords": true,
		"skills": true, "commands": true, "agents": true, "mcpServers": true,
		"hooks": true, "lspServers": true,
	}

	for _, dir := range result.PluginDirs {
		pluginJSON := filepath.Join(dir, ".claude-plugin", "plugin.json")
		data, err := os.ReadFile(pluginJSON)
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "schema",
				Plugin:   filepath.Base(dir),
				Message:  fmt.Sprintf("invalid JSON: %v", err),
			})
			continue
		}

		// Check for required fields
		if _, ok := raw["name"]; !ok {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "schema",
				Plugin:   filepath.Base(dir),
				Message:  "missing required field: name",
			})
		}
		if _, ok := raw["version"]; !ok {
			result.Findings = append(result.Findings, Finding{
				Severity: "error",
				Category: "schema",
				Plugin:   filepath.Base(dir),
				Message:  "missing required field: version",
			})
		}

		// Check for unrecognized keys
		for key := range raw {
			if !allowedKeys[key] {
				result.Findings = append(result.Findings, Finding{
					Severity: "error",
					Category: "schema",
					Plugin:   filepath.Base(dir),
					Message:  fmt.Sprintf("unrecognized key %q (Claude Code rejects unknown keys)", key),
				})
			}
		}

		// Check author format
		if authorRaw, ok := raw["author"]; ok {
			var authorStr string
			if json.Unmarshal(authorRaw, &authorStr) == nil {
				result.Findings = append(result.Findings, Finding{
					Severity: "error",
					Category: "schema",
					Plugin:   filepath.Base(dir),
					Message:  "author must be an object {\"name\": \"...\"}, not a string",
				})
			}
		}
	}
}

func checkUndeclaredHooks(result *DoctorResult) {
	for _, dir := range result.PluginDirs {
		pluginJSON := filepath.Join(dir, ".claude-plugin", "plugin.json")
		data, err := os.ReadFile(pluginJSON)
		if err != nil {
			continue
		}

		var raw map[string]json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			continue
		}

		_, hooksInJSON := raw["hooks"]

		// Check if hooks exist on disk
		hookPaths := []string{
			filepath.Join(dir, "hooks", "hooks.json"),
			filepath.Join(dir, ".claude-plugin", "hooks", "hooks.json"),
		}

		hooksOnDisk := false
		for _, hp := range hookPaths {
			if _, err := os.Stat(hp); err == nil {
				hooksOnDisk = true
				break
			}
		}

		if hooksOnDisk && !hooksInJSON {
			p, _ := ReadPlugin(dir)
			name := filepath.Base(dir)
			if p != nil {
				name = p.Name
			}
			result.Findings = append(result.Findings, Finding{
				Severity: "warning",
				Category: "hooks",
				Plugin:   name,
				Message:  "hooks found on disk but not declared in plugin.json",
				Fix:      "add \"hooks\": \"./hooks/hooks.json\" to plugin.json",
			})
		}
	}
}

// discoverPluginDirs finds all plugin directories in the monorepo.
func discoverPluginDirs(from string) []string {
	// Walk up to find the monorepo root (look for interverse/)
	abs, _ := filepath.Abs(from)
	current := abs
	for i := 0; i < 5; i++ {
		if _, err := os.Stat(filepath.Join(current, "interverse")); err == nil {
			break
		}
		parent := filepath.Dir(current)
		if parent == current {
			return nil
		}
		current = parent
	}

	var dirs []string

	// Scan interverse/*/
	interverse := filepath.Join(current, "interverse")
	entries, err := os.ReadDir(interverse)
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			pluginJSON := filepath.Join(interverse, e.Name(), ".claude-plugin", "plugin.json")
			if _, err := os.Stat(pluginJSON); err == nil {
				dirs = append(dirs, filepath.Join(interverse, e.Name()))
			}
		}
	}

	// Also check os/clavain
	clavainJSON := filepath.Join(current, "os", "clavain", ".claude-plugin", "plugin.json")
	if _, err := os.Stat(clavainJSON); err == nil {
		dirs = append(dirs, filepath.Join(current, "os", "clavain"))
	}

	return dirs
}
