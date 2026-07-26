package publish

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

	// Discover plugin directories (scan interverse/ and os/Clavain)
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

	// Check 7: Stale cache versions
	checkStaleCacheVersions(result, opts)

	// Check 8: plugin.json schema validation
	checkPluginSchemas(result, opts)

	// Check 9: Hook declaration correctness
	checkUndeclaredHooks(result, opts)

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
			localIsNewer := semverLess(mktVer, plugin.Version)
			role := CurrentPublishRole()

			// A local checkout that TRAILS the marketplace is the normal steady
			// state on a machine that does not publish (mk-ldnb). Reporting 21
			// of those as errors trains everyone to ignore doctor output.
			if !localIsNewer && role == RoleVerifier {
				result.Findings = append(result.Findings, Finding{
					Severity: "info",
					Category: "drift",
					Plugin:   plugin.Name,
					Message: fmt.Sprintf("local checkout behind marketplace (plugin.json=%s marketplace=%s); this machine is role=%s",
						plugin.Version, mktVer, role),
					Fix: "no action -- publish from the signer machine",
				})
				continue
			}

			sev := "error"
			fix := fmt.Sprintf("update marketplace to %s", plugin.Version)
			if !localIsNewer {
				// Local is older on a signer machine: still worth flagging, but
				// the fix is to pull, NOT to push a stale version outward.
				sev = "warning"
				fix = fmt.Sprintf("local checkout is behind; pull before publishing (marketplace=%s)", mktVer)
			}

			result.Findings = append(result.Findings, Finding{
				Severity: sev,
				Category: "drift",
				Plugin:   plugin.Name,
				Message:  fmt.Sprintf("plugin.json=%s marketplace=%s", plugin.Version, mktVer),
				Fix:      fix,
			})

			// Only ever move the marketplace FORWARD. The previous version wrote
			// the local value unconditionally, so --fix on a machine whose
			// checkouts trailed would downgrade the marketplace -- silently
			// rolling back every plugin it "repaired".
			if opts.Fix && localIsNewer {
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
				UpdateInstalled(name, mktVer, cachePath, "")

				// Also rebuild cache content from source
				for _, dir := range result.PluginDirs {
					p, err := ReadPlugin(dir)
					if err == nil && p.Name == name {
						if err := ForceRebuildCache(name, mktVer, dir); err == nil {
							result.Findings = append(result.Findings, Finding{
								Severity: "info",
								Category: "cache",
								Plugin:   name,
								Message:  fmt.Sprintf("rebuilt cache from source (v%s)", mktVer),
							})
						}
						break
					}
				}
			}
		}
	}
}

// checkCCMarketplaceSync compares every known marketplace clone against every
// other and reports version disagreements as ERRORS.
//
// The previous implementation had a blind spot that hid a real divergence
// (mk-963o). It began:
//
//	if absMarket == absCCPath { return }   // "same directory"
//
// but marketRoot IS the CC checkout whenever doctor runs from a plugin outside
// the Sylveste tree -- which is precisely the situation that creates the
// divergence. So the check disabled itself in the one directory where it
// mattered. Publishing interbrowse 0.5.2 left core/marketplace at 0.5.1 and
// doctor, run from ~/projects/interbrowse, reported nothing.
//
// It was also severity "warning", so even when it did fire it did not fail.
func checkCCMarketplaceSync(result *DoctorResult, marketRoot string, mktVersions map[string]string, opts DoctorOpts) {
	divergence, clones := MarketplaceCloneDivergence(marketRoot)

	if len(clones) < 2 {
		return // only one clone on this machine; nothing to disagree with
	}

	if len(divergence) == 0 {
		return
	}

	names := make([]string, 0, len(divergence))
	for name := range divergence {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		byClone := divergence[name]
		paths := make([]string, 0, len(byClone))
		for p := range byClone {
			paths = append(paths, p)
		}
		sort.Strings(paths)

		parts := make([]string, 0, len(paths))
		newest := ""
		for _, p := range paths {
			parts = append(parts, fmt.Sprintf("%s=%s", shortenClonePath(p), byClone[p]))
			if v := byClone[p]; newest == "" || semverLess(newest, v) {
				newest = v
			}
		}

		result.Findings = append(result.Findings, Finding{
			Severity: "error",
			Category: "drift",
			Plugin:   name,
			Message:  fmt.Sprintf("marketplace clones disagree: %s", strings.Join(parts, " ")),
			Fix:      fmt.Sprintf("sync all clones to %s", newest),
		})

		if opts.Fix && newest != "" {
			// Write the newest version into whichever clone already holds this
			// plugin; SyncPeerMarketplaces skips clones that lack it.
			for _, p := range paths {
				if byClone[p] == newest {
					SyncPeerMarketplaces(p, name, newest)
					break
				}
			}
		}
	}
}

// shortenClonePath trims the home prefix so findings stay readable.
func shortenClonePath(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+string(filepath.Separator)) {
		return "~" + p[len(home):]
	}
	return p
}

// semverLess reports whether a sorts before b for dotted numeric versions.
// Non-numeric segments compare lexically. Good enough to pick the newest of two
// versions of the same plugin; not a general semver implementation.
func semverLess(a, b string) bool {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		ai, aerr := strconv.Atoi(av)
		bi, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			if ai != bi {
				return ai < bi
			}
			continue
		}
		if av != bv {
			return av < bv
		}
	}
	return false
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

func checkStaleCacheVersions(result *DoctorResult, opts DoctorOpts) {
	entries, err := ListCacheEntries()
	if err != nil {
		return
	}

	staleCount := 0
	for pluginName, versions := range entries {
		installedVer := ReadInstalledVersion(pluginName)
		for _, v := range versions {
			if !v.IsSymlink && !v.Orphaned && v.Version != installedVer {
				staleCount++
			}
		}
	}

	if staleCount > 0 {
		result.Findings = append(result.Findings, Finding{
			Severity: "warning",
			Category: "cache",
			Message:  fmt.Sprintf("%d stale cache version(s) found", staleCount),
			Fix:      "run: ic publish clean",
		})
		if opts.Fix {
			count, bytes, _ := PruneStaleVersions(1)
			if count > 0 {
				result.Findings = append(result.Findings, Finding{
					Severity: "info",
					Category: "cache",
					Message:  fmt.Sprintf("pruned %d stale version(s) (%.1f MB freed)", count, float64(bytes)/1024/1024),
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

// checkUndeclaredHooks validates hook declarations against Claude Code's auto-loading behavior.
//
// Claude Code auto-loads hooks/hooks.json from the standard path. If plugin.json
// ALSO declares "hooks": "./hooks/hooks.json", hooks get loaded twice, causing
// a "hook-load-failed" error that blocks the entire plugin.
//
// Logic:
//   - Standard-path hooks on disk + NOT declared → info (correct, auto-loaded)
//   - Standard-path hooks on disk + declared      → error (duplicate, will break)
//   - Non-standard hooks declared + exists         → fine
//   - Non-standard hooks declared + missing        → error
func checkUndeclaredHooks(result *DoctorResult, opts DoctorOpts) {
	// Standard paths that Claude Code auto-loads (no declaration needed)
	standardPaths := []string{"hooks/hooks.json", ".claude-plugin/hooks/hooks.json"}

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

		p, _ := ReadPlugin(dir)
		name := filepath.Base(dir)
		if p != nil {
			name = p.Name
		}

		// Get declared hooks path (if any)
		var declaredPath string
		if hooksRaw, ok := raw["hooks"]; ok {
			json.Unmarshal(hooksRaw, &declaredPath)
		}
		// Normalize: strip leading ./
		normDeclared := strings.TrimPrefix(declaredPath, "./")

		// Check which standard paths exist on disk
		var standardOnDisk string
		for _, sp := range standardPaths {
			if _, err := os.Stat(filepath.Join(dir, sp)); err == nil {
				standardOnDisk = sp
				break
			}
		}

		if declaredPath != "" {
			// Hooks field is declared in plugin.json
			isStandardPath := false
			for _, sp := range standardPaths {
				if normDeclared == sp {
					isStandardPath = true
					break
				}
			}

			if isStandardPath {
				// ERROR: declaring a standard path causes duplicate loading
				result.Findings = append(result.Findings, Finding{
					Severity: "error",
					Category: "hooks",
					Plugin:   name,
					Message:  fmt.Sprintf("hooks field %q duplicates Claude Code auto-loading — causes hook-load-failed error", declaredPath),
					Fix:      "remove \"hooks\" key from plugin.json (standard path is auto-loaded)",
				})
				if opts.Fix {
					removed, err := removeJSONKey(pluginJSON, "hooks")
					if removed && err == nil {
						result.Findings = append(result.Findings, Finding{
							Severity: "info",
							Category: "hooks",
							Plugin:   name,
							Message:  "removed duplicate hooks declaration from plugin.json",
						})
					}
					// Rebuild cache to propagate fix
					if p != nil && p.Version != "" {
						ForceRebuildCache(name, p.Version, dir)
					}
				}
			} else {
				// Non-standard path declared — check it exists
				resolved := filepath.Join(dir, normDeclared)
				if _, err := os.Stat(resolved); os.IsNotExist(err) {
					result.Findings = append(result.Findings, Finding{
						Severity: "error",
						Category: "hooks",
						Plugin:   name,
						Message:  fmt.Sprintf("declared hooks file %q does not exist", declaredPath),
					})
				}
			}
		} else if standardOnDisk != "" {
			// No declaration, but standard hooks exist on disk — this is correct
			result.Findings = append(result.Findings, Finding{
				Severity: "info",
				Category: "hooks",
				Plugin:   name,
				Message:  fmt.Sprintf("hooks at %s will be auto-loaded (no declaration needed)", standardOnDisk),
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

	// Also check the canonical os/Clavain path, with lowercase compatibility
	// for older standalone layouts.
	for _, clavainDir := range clavainDirCandidates(current) {
		clavainJSON := filepath.Join(clavainDir, ".claude-plugin", "plugin.json")
		if _, err := os.Stat(clavainJSON); err == nil {
			dirs = append(dirs, clavainDir)
			break
		}
	}

	return dirs
}
