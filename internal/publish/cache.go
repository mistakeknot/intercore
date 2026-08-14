package publish

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// CacheBase returns the base directory for the plugin cache.
func CacheBase() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "cache", "interagency-marketplace")
}

// RebuildCache copies tracked plugin source to the cache directory.
// Creates: <CacheBase>/<pluginName>/<version>/
func RebuildCache(pluginName, version, srcRoot string) error {
	base := CacheBase()
	if base == "" {
		return fmt.Errorf("cannot determine cache base directory")
	}

	dest := filepath.Join(base, pluginName, version)
	if _, err := os.Stat(dest); err == nil {
		return nil // already exists
	}

	if err := copyTrackedTree(srcRoot, dest); err != nil {
		os.RemoveAll(dest) // clean up partial copy
		return fmt.Errorf("rebuild cache: %w", err)
	}
	return nil
}

// ForceRebuildCache removes existing cache and rebuilds from source.
// Unlike RebuildCache, this replaces stale content even if the dir exists.
func ForceRebuildCache(pluginName, version, srcRoot string) error {
	base := CacheBase()
	if base == "" {
		return fmt.Errorf("cannot determine cache base directory")
	}
	dest := filepath.Join(base, pluginName, version)
	os.RemoveAll(dest)
	return copyTrackedTree(srcRoot, dest)
}

// CleanOrphans removes cache directories with .orphaned_at markers across ALL
// marketplaces. A marker only triggers removal when it sits at the expected
// plugin-version-root depth (cache/<marketplace>/<plugin>/<version>/.orphaned_at).
// Markers found at other depths are stale artifacts from an older layout or
// false positives (e.g. a file literally named .orphaned_at deep inside a
// plugin's source tree) and are ignored.
//
// Directories a running process is executing out of are kept regardless of
// their marker, and reported in the returned PruneReport.
func CleanOrphans() (PruneReport, error) {
	root := CacheRoot()
	if root == "" {
		return PruneReport{}, fmt.Errorf("cannot determine cache root")
	}
	return cleanOrphansIn(root, 0, RunningExecutables())
}

// CleanOrphansOlderThan removes marked orphans whose marker file is older
// than minAge. The publish engine uses this instead of CleanOrphans: a marker
// is a deferred-deletion signal (a live session may still read hooks from the
// dir), so the automatic path grants a grace window that the explicit
// `ic publish clean` does not.
//
// THE GRACE WINDOW IS A PROXY, AND IT IS THE WRONG ONE. It asks how long ago a
// version was superseded, when the question is whether anyone is still running
// it — and those diverge exactly where it matters: a session open for weeks is
// the likeliest holder AND the furthest outside any window. Measured on
// Clavain 2026-08-14: `ic publish clean` (minAge 0, so no window at all)
// unlinked intermux 0.1.11 while two servers were executing it. The marker age
// stays as a backstop; liveness is now the evidence.
func CleanOrphansOlderThan(minAge time.Duration) (PruneReport, error) {
	root := CacheRoot()
	if root == "" {
		return PruneReport{}, fmt.Errorf("cannot determine cache root")
	}
	return cleanOrphansIn(root, minAge, RunningExecutables())
}

// cleanOrphansIn is the testable core of CleanOrphans/CleanOrphansOlderThan.
// Takes an explicit root path and process table so tests can use t.TempDir()
// and a synthetic fleet.
func cleanOrphansIn(root string, minAge time.Duration, procs []ProcExe) (PruneReport, error) {
	var report PruneReport

	// Same rule as the version prune: an empty process table is a failed
	// reading, not an idle machine, and deleting on that basis is the exact
	// mistake this guard exists to prevent.
	if len(procs) == 0 {
		report.Blocked = "process table unreadable; declined to clean any orphaned directory"
		return report, nil
	}
	held := heldVersionDirs(root, procs)
	return cleanOrphansWalk(root, minAge, held, report)
}

// cleanOrphansWalk performs the marker walk itself.
func cleanOrphansWalk(root string, minAge time.Duration, held map[string][]ProcExe,
	report PruneReport) (PruneReport, error) {
	rootDepth := strings.Count(root, string(os.PathSeparator))
	expectedMarkerDepth := rootDepth + 4 // <root>/<marketplace>/<plugin>/<version>/.orphaned_at

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip inaccessible entries
		}
		if d.Name() == ".orphaned_at" && !d.IsDir() {
			// Depth check: only trust markers at <root>/<mkt>/<plugin>/<ver>/
			if strings.Count(path, string(os.PathSeparator)) != expectedMarkerDepth {
				return nil
			}
			orphanDir := filepath.Dir(path)
			// Don't remove temp_git dirs (handled separately)
			if strings.Contains(orphanDir, "temp_git_") {
				return nil
			}
			// The interlock, ahead of the age test: a marked directory someone
			// is executing is not deletable at any marker age, including the
			// zero-grace path `ic publish clean` takes.
			if holders := held[filepath.Clean(orphanDir)]; len(holders) > 0 {
				report.Held = append(report.Held, HeldVersion{
					Key:     CacheEntry{Path: orphanDir}.Key(),
					Version: filepath.Base(orphanDir),
					Path:    orphanDir,
					Holders: holders,
				})
				return filepath.SkipDir
			}
			if minAge > 0 {
				info, statErr := d.Info()
				if statErr != nil || time.Since(info.ModTime()) < minAge {
					return nil // marker still inside its grace window
				}
			}
			size := dirSize(orphanDir)
			if err := os.RemoveAll(orphanDir); err != nil {
				return nil // best effort
			}
			report.Pruned++
			report.BytesFreed += size
			return filepath.SkipDir
		}
		return nil
	})
	sortHeld(report.Held)
	return report, err
}

// StripGitDirs removes .git/ directories from all cache entries.
func StripGitDirs() (count int, bytesFreed int64, err error) {
	base := CacheBase()
	if base == "" {
		return 0, 0, fmt.Errorf("cannot determine cache base")
	}

	// Walk plugin/version dirs
	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" {
			size := dirSize(path)
			if err := os.RemoveAll(path); err != nil {
				return nil // best effort
			}
			count++
			bytesFreed += size
			return filepath.SkipDir
		}
		return nil
	})
	return count, bytesFreed, err
}

// PruneStaleVersions removes old cached versions that are not the currently installed version.
// For each plugin, it keeps the installed version, the version marketplace.json points at,
// and the `keep-1` most recent other versions, removing the rest. Symlinks and orphaned
// directories are skipped (orphans have their own cleanup). Plugins whose installed version
// cannot be determined and have no marketplace record are skipped wholesale (Sylveste-0lt:
// no ground truth means "touch nothing", never "delete everything").
// Version directories a running process is executing out of are kept
// regardless, and reported in the returned PruneReport — `doctor --fix` reaches
// this path, so leaving it unguarded would make the interlock bypassable
// through the command whose whole job is to leave things healthy.
func PruneStaleVersions(keep int) (PruneReport, error) {
	entries, err := ListCacheEntries()
	if err != nil {
		return PruneReport{}, err
	}

	// ListCacheEntries keys are bare plugin names (interagency-marketplace
	// scope), so re-key both guard maps to match.
	installed := make(map[string]string, len(entries))
	for pluginName := range entries {
		if v := ReadInstalledVersion(pluginName); v != "" {
			installed[pluginName] = v
		}
	}
	protect := map[string]string{}
	for key, ver := range MarketplaceVersions() {
		if name, ok := strings.CutSuffix(key, "@interagency-marketplace"); ok {
			protect[name] = ver
		}
	}

	return pruneStaleVersions(CacheRoot(), entries, installed, keep, protect, RunningExecutables()), nil
}

// sortVersionsDesc sorts cache entries by version, newest first.
func sortVersionsDesc(entries []CacheEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && CompareVersions(entries[j].Version, entries[j-1].Version) > 0; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
}

// CreateSymlinks creates version bridge symlinks for hook session continuity.
// When a session loaded hooks from version A and we publish version B,
// create a symlink so the old path still resolves.
func CreateSymlinks(pluginName, oldVersion, newVersion string) error {
	base := CacheBase()
	if base == "" {
		return nil
	}

	pluginCache := filepath.Join(base, pluginName)
	if _, err := os.Stat(pluginCache); os.IsNotExist(err) {
		return nil
	}

	// Find the first real (non-symlink) directory — this is the canonical version
	entries, err := os.ReadDir(pluginCache)
	if err != nil {
		return nil
	}

	var realDir string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		fullPath := filepath.Join(pluginCache, e.Name())
		info, err := os.Lstat(fullPath)
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			realDir = e.Name()
			break
		}
	}

	if realDir == "" {
		return nil // no real directory found
	}

	// Create symlinks for old and new versions pointing to the real dir
	for _, ver := range []string{oldVersion, newVersion} {
		if ver == "" || ver == realDir {
			continue
		}
		link := filepath.Join(pluginCache, ver)
		if _, err := os.Lstat(link); err == nil {
			continue // already exists
		}
		os.Symlink(realDir, link)
	}
	return nil
}

// CacheEntry represents a cached plugin version.
type CacheEntry struct {
	Version     string
	Path        string
	IsSymlink   bool
	Orphaned    bool
	Marketplace string // empty for ListCacheEntries, populated by ListAllCacheEntries
}

// Key is the "<plugin>@<marketplace>" identity used throughout the prune — the
// same shape as installed_plugins.json's keys.
//
// Derived from Path rather than from the struct's fields because Marketplace is
// only populated by ListAllCacheEntries; the layout
// <root>/<marketplace>/<plugin>/<version> holds for both walkers, so the path
// is the one source that is always right.
func (c CacheEntry) Key() string {
	pluginDir := filepath.Dir(c.Path)
	return filepath.Base(pluginDir) + "@" + filepath.Base(filepath.Dir(pluginDir))
}

// CacheRoot returns the parent directory of all marketplace caches.
// Unlike CacheBase (which is interagency-marketplace-specific for publishing),
// CacheRoot is used by cleanup operations that should sweep across all marketplaces.
func CacheRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "cache")
}

// ListAllCacheEntries returns all cached plugin versions across every marketplace.
// Result is keyed by "<plugin>@<marketplace>" so cleanup can correctly disambiguate
// plugins of the same name across marketplaces (e.g. "notion@claude-plugins-official"
// vs a hypothetical "notion@interagency-marketplace").
func ListAllCacheEntries() (map[string][]CacheEntry, error) {
	root := CacheRoot()
	if root == "" {
		return nil, fmt.Errorf("cannot determine cache root")
	}
	return listAllCacheEntriesIn(root)
}

// listAllCacheEntriesIn is the testable core of ListAllCacheEntries.
// Takes an explicit root path so tests can use t.TempDir().
func listAllCacheEntriesIn(root string) (map[string][]CacheEntry, error) {
	result := make(map[string][]CacheEntry)

	marketplaces, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, m := range marketplaces {
		if !m.IsDir() {
			continue
		}
		marketplaceName := m.Name()
		// Skip temp_git_* directories — those are leftover from interrupted
		// git operations, not real marketplaces, and they confuse the walk
		// (their internal .orphaned_at files would each register as a separate
		// fake "orphaned plugin"). CleanOrphans already skips temp_git for
		// removal; we extend the same hygiene to listing.
		if strings.HasPrefix(marketplaceName, "temp_git_") {
			continue
		}
		marketplaceDir := filepath.Join(root, marketplaceName)

		plugins, err := os.ReadDir(marketplaceDir)
		if err != nil {
			continue
		}
		for _, p := range plugins {
			if !p.IsDir() {
				continue
			}
			pluginDir := filepath.Join(marketplaceDir, p.Name())
			versions, err := os.ReadDir(pluginDir)
			if err != nil {
				continue
			}
			for _, v := range versions {
				vPath := filepath.Join(pluginDir, v.Name())
				info, err := os.Lstat(vPath)
				if err != nil {
					continue
				}
				entry := CacheEntry{
					Version:     v.Name(),
					Path:        vPath,
					IsSymlink:   info.Mode()&os.ModeSymlink != 0,
					Marketplace: marketplaceName,
				}
				if _, err := os.Stat(filepath.Join(vPath, ".orphaned_at")); err == nil {
					entry.Orphaned = true
				}
				key := p.Name() + "@" + marketplaceName
				result[key] = append(result[key], entry)
			}
		}
	}
	return result, nil
}

// pruneCandidates computes which cache entries are safe to delete. Pure —
// callers supply the cache tree, the installed map ("<plugin>@<marketplace>"
// → version), and an explicit protect map of the same shape (the version a
// publish flow JUST wrote, shielded regardless of installed_plugins.json).
//
// Fail-safe (Sylveste-0lt): a plugin with NO installed version and NO protect
// entry is skipped wholesale. installed_plugins.json can be mid-rewrite while
// `claude plugin marketplace update` re-clones (that is how clavain 0.6.278's
// entire cache dir was deleted right after publishing it) — a missing record
// must mean "touch nothing," never "delete everything."
func pruneCandidates(entries map[string][]CacheEntry, installed map[string]string,
	keep int, protect map[string]string) []CacheEntry {
	var out []CacheEntry
	for key, versions := range entries {
		installedVer := installed[key]
		protectedVer := protect[key]
		if installedVer == "" && protectedVer == "" {
			continue // no ground truth for this plugin — never delete blind
		}

		var candidates []CacheEntry
		for _, v := range versions {
			if v.IsSymlink || v.Orphaned || v.Version == installedVer || v.Version == protectedVer {
				continue
			}
			candidates = append(candidates, v)
		}

		sortVersionsDesc(candidates)
		toKeep := keep - 1
		if toKeep < 0 {
			toKeep = 0
		}
		if toKeep < len(candidates) {
			out = append(out, candidates[toKeep:]...)
		}
	}
	return out
}

// HeldVersion is a stale version directory that was NOT deleted because live
// processes are executing out of it.
type HeldVersion struct {
	Key     string // "<plugin>@<marketplace>"
	Version string
	Path    string
	Holders []ProcExe
}

// PruneReport describes what a prune did and, as importantly, what it declined
// to do. Skips are returned rather than merely logged: a prune that silently
// frees less space than expected is indistinguishable from one that found
// nothing, and the difference is the whole point of the liveness interlock.
type PruneReport struct {
	Pruned     int
	BytesFreed int64
	Held       []HeldVersion
	// Blocked is non-empty when the prune declined to delete ANYTHING because
	// it could not establish what is running.
	Blocked string
}

// PruneStaleVersionsAcrossMarketplaces removes stale plugin versions from EVERY
// marketplace cache, not just interagency-marketplace. For each plugin, keeps the
// version listed in installed_plugins.json, anything in protect (the version a
// publish just wrote), and the keep-1 most recent others. Plugins with no
// installed record and no protect entry are left untouched (Sylveste-0lt).
//
// Version directories that a running process is executing out of are kept
// regardless of all of the above, and reported in the returned PruneReport.
func PruneStaleVersionsAcrossMarketplaces(keep int, protect map[string]string) (PruneReport, error) {
	entries, err := ListAllCacheEntries()
	if err != nil {
		return PruneReport{}, err
	}

	ip, err := ReadInstalled()
	if err != nil {
		return PruneReport{}, err
	}
	installed := make(map[string]string, len(ip.Plugins))
	for key, rec := range ip.Plugins {
		if len(rec) > 0 {
			installed[key] = rec[0].Version
		}
	}

	// Marketplace guard (Sylveste-0lt, second layer): the version each
	// marketplace.json points at is what `claude` will (re)install — never a
	// deletion candidate, even when installed_plugins.json is unreadable or
	// mid-rewrite. Explicit protect entries from the publish flow overlay it.
	merged := MarketplaceVersions()
	for k, v := range protect {
		merged[k] = v
	}

	return pruneStaleVersions(CacheRoot(), entries, installed, keep, merged, RunningExecutables()), nil
}

// pruneStaleVersions is the injectable core: the caller supplies the cache
// root, the walked entries, the installed map, and the process table, so the
// whole decision — including the liveness interlock — is exercisable in a test
// against the real code rather than a restatement of it.
func pruneStaleVersions(root string, entries map[string][]CacheEntry, installed map[string]string,
	keep int, protect map[string]string, procs []ProcExe) PruneReport {
	var report PruneReport

	// COULD NOT LOOK IS NOT NOTHING THERE. An empty process table is not a
	// system with no processes — it is a system whose process table we failed
	// to read, and treating the two alike is what would license deleting a
	// directory in active use. Decline the whole prune and say so; disk space
	// is recoverable on the next run, an unnameable running artifact is not.
	if len(procs) == 0 {
		report.Blocked = "process table unreadable; declined to prune anything"
		return report
	}

	held := heldVersionDirs(root, procs)

	for _, c := range pruneCandidates(entries, installed, keep, protect) {
		// The liveness check is deliberately the LAST gate, after policy has
		// already decided this version is stale. It is a safety interlock, not
		// a rule about which versions matter: whatever the policy concludes, a
		// directory someone is running is not deletable.
		if holders := held[filepath.Clean(c.Path)]; len(holders) > 0 {
			report.Held = append(report.Held, HeldVersion{
				Key:     c.Key(),
				Version: c.Version,
				Path:    c.Path,
				Holders: holders,
			})
			continue
		}
		size := dirSize(c.Path)
		if rmErr := os.RemoveAll(c.Path); rmErr != nil {
			continue
		}
		report.Pruned++
		report.BytesFreed += size
	}
	sortHeld(report.Held)
	return report
}

// sortHeld gives the report a stable order so callers print the same thing
// twice for the same state.
func sortHeld(held []HeldVersion) {
	sort.Slice(held, func(i, j int) bool {
		if held[i].Key != held[j].Key {
			return held[i].Key < held[j].Key
		}
		return held[i].Version < held[j].Version
	})
}

// Summary renders a held version as "<plugin>@<marketplace> <version> (N
// process(es): pid, pid, ...)", capped so a fleet of two dozen holders does not
// bury the rest of a publish's output.
func (h HeldVersion) Summary() string {
	const maxPIDs = 5
	pids := make([]string, 0, len(h.Holders))
	for i, p := range h.Holders {
		if i == maxPIDs {
			pids = append(pids, fmt.Sprintf("+%d more", len(h.Holders)-maxPIDs))
			break
		}
		pids = append(pids, strconv.Itoa(p.PID))
	}
	noun := "processes"
	if len(h.Holders) == 1 {
		noun = "process"
	}
	return fmt.Sprintf("%s %s (%d %s: %s)", h.Key, h.Version, len(h.Holders), noun, strings.Join(pids, ", "))
}

// PruneDanglingSymlinks removes version symlinks whose targets no longer
// exist, across all marketplaces. Hook-bridge symlinks (see CreateSymlinks)
// are created on every publish but never retired; once the stale-version
// prune removes their targets they dangle forever, and downstream tools that
// enumerate version dirs misread them as installed versions. A dangling link
// cannot serve session continuity, so removal is always safe.
func PruneDanglingSymlinks() (count int, err error) {
	root := CacheRoot()
	if root == "" {
		return 0, fmt.Errorf("cannot determine cache root")
	}
	return pruneDanglingSymlinksIn(root)
}

// CountDanglingSymlinks reports how many version symlinks dangle, without
// removing them. Used by `ic publish clean --dry-run`.
func CountDanglingSymlinks() (int, error) {
	root := CacheRoot()
	if root == "" {
		return 0, fmt.Errorf("cannot determine cache root")
	}
	paths, err := danglingSymlinksIn(root)
	return len(paths), err
}

// pruneDanglingSymlinksIn is the testable core of PruneDanglingSymlinks.
func pruneDanglingSymlinksIn(root string) (count int, err error) {
	paths, err := danglingSymlinksIn(root)
	if err != nil {
		return 0, err
	}
	for _, p := range paths {
		if rmErr := os.Remove(p); rmErr == nil {
			count++
		}
	}
	return count, nil
}

// danglingSymlinksIn collects version-level symlinks whose resolution chain
// terminates in a missing path. os.Stat follows the full chain, so a link
// pointing at another (also dangling) link is detected in the same pass.
func danglingSymlinksIn(root string) ([]string, error) {
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		return nil, err
	}
	var dangling []string
	for _, versions := range entries {
		for _, v := range versions {
			if !v.IsSymlink {
				continue
			}
			if _, statErr := os.Stat(v.Path); os.IsNotExist(statErr) {
				dangling = append(dangling, v.Path)
			}
		}
	}
	return dangling, nil
}

// CountStaleAcrossMarketplaces reports the number of stale + orphaned cache entries
// across every marketplace. Used by `ic publish clean --dry-run`.
// A preview that counts a directory the real command will refuse to touch is
// worse than no preview: it is the exact question ("is it safe to clean?")
// answered wrongly. So the dry-run consults the process table too, and reports
// held directories separately rather than folding them into the delete counts.
func CountStaleAcrossMarketplaces() (orphaned int, stale int, held []HeldVersion, blocked string, err error) {
	entries, err := ListAllCacheEntries()
	if err != nil {
		return 0, 0, nil, "", err
	}
	ip, err := ReadInstalled()
	if err != nil {
		return 0, 0, nil, "", err
	}

	installed := make(map[string]string, len(ip.Plugins))
	for key, rec := range ip.Plugins {
		if len(rec) > 0 {
			installed[key] = rec[0].Version
		}
	}

	orphaned, stale, held, blocked = countStaleIn(CacheRoot(), entries, installed, RunningExecutables())
	return orphaned, stale, held, blocked, nil
}

// countStaleIn is the testable core of CountStaleAcrossMarketplaces. Takes an
// explicit root and process list so a test can drive both without a real cache
// or a real process table.
func countStaleIn(root string, entries map[string][]CacheEntry, installed map[string]string,
	procs []ProcExe) (orphaned int, stale int, held []HeldVersion, blocked string) {

	if len(procs) == 0 {
		// Mirrors what the real clean does: an unreadable process table is a
		// failed reading, not an idle machine, so nothing would be deleted.
		blocked = "process table unreadable; a real clean would decline to prune anything"
	}
	heldDirs := heldVersionDirs(root, procs)

	for key, versions := range entries {
		installedVer := installed[key]
		for _, v := range versions {
			isOrphan := v.Orphaned
			isStale := !v.IsSymlink && v.Version != installedVer
			if !isOrphan && !isStale {
				continue
			}
			if holders := heldDirs[filepath.Clean(v.Path)]; len(holders) > 0 {
				held = append(held, HeldVersion{
					Key:     v.Key(),
					Version: v.Version,
					Path:    v.Path,
					Holders: holders,
				})
				continue
			}
			if isOrphan {
				orphaned++
			} else {
				stale++
			}
		}
	}
	sortHeld(held)
	return orphaned, stale, held, blocked
}

// ListCacheEntries returns all cached plugin versions grouped by plugin name.
func ListCacheEntries() (map[string][]CacheEntry, error) {
	base := CacheBase()
	if base == "" {
		return nil, fmt.Errorf("cannot determine cache base")
	}

	result := make(map[string][]CacheEntry)

	plugins, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return nil, err
	}

	for _, p := range plugins {
		if !p.IsDir() {
			continue
		}
		pluginDir := filepath.Join(base, p.Name())
		versions, err := os.ReadDir(pluginDir)
		if err != nil {
			continue
		}

		for _, v := range versions {
			vPath := filepath.Join(pluginDir, v.Name())
			info, err := os.Lstat(vPath)
			if err != nil {
				continue
			}

			entry := CacheEntry{
				Version:   v.Name(),
				Path:      vPath,
				IsSymlink: info.Mode()&os.ModeSymlink != 0,
			}

			// Check for orphan marker
			if _, err := os.Stat(filepath.Join(vPath, ".orphaned_at")); err == nil {
				entry.Orphaned = true
			}

			result[p.Name()] = append(result[p.Name()], entry)
		}
	}

	return result, nil
}

// BuildGoMCPBinary pre-builds a Go binary from source into the cache directory.
// This handles plugins with go.mod replace directives that point to monorepo-relative
// paths — those resolve from the source dir but not from the cache.
//
// Best-effort: the caller logs a warning rather than failing the publish. But
// "best-effort" is not the same as silent. Two skips are genuine non-events —
// no go.mod, no launcher — and stay quiet. A plugin that has BOTH and still
// yields no build target is a defect and says so, because the quiet version of
// that branch hid a 100%-miss parser across all five Go MCP plugins until a
// survey went looking for it (mk-cg3z).
func BuildGoMCPBinary(pluginName, srcRoot, cacheDest string) error {
	// Skip if no go.mod
	if _, err := os.Stat(filepath.Join(srcRoot, "go.mod")); os.IsNotExist(err) {
		return nil
	}

	// Skip if no launcher script (no MCP binary to build)
	launcherPath := filepath.Join(srcRoot, "bin", "launch-mcp.sh")
	if _, err := os.Stat(launcherPath); os.IsNotExist(err) {
		return nil
	}

	// Parse launcher to find the build target and output binary name
	buildTarget, binaryName := parseLauncherScript(launcherPath)
	if buildTarget == "" {
		return fmt.Errorf("%s: cannot find a 'go build -o' line in bin/launch-mcp.sh; "+
			"the MCP binary will not be pre-built and every fresh install pays a "+
			"build on first launch", pluginName)
	}

	// Build from source dir (where replace directives resolve), output to cache
	outputPath := filepath.Join(cacheDest, "bin", binaryName)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0755); err != nil {
		return fmt.Errorf("mkdir for binary: %w", err)
	}

	cmd := execCommand("go", "build", "-o", outputPath, buildTarget)
	cmd.Dir = srcRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go build %s: %s: %w", buildTarget, strings.TrimSpace(stderr.String()), err)
	}

	return nil
}

// parseLauncherScript extracts the go build target and binary name from a
// launch-mcp.sh. Handles both forms in use:
//
//	go build -o "$SCRIPT_DIR/server" ./cmd/server    # output path is literal-ish
//	go build -o "$BINARY" ./cmd/server/              # output path is a variable
//
// The old pattern required a "/" inside the -o argument, so the second form
// matched nothing — and the caller treated "no match" as "nothing to do". Every
// launcher in the marketplace uses the second form, so the pre-build had never
// run for any plugin. Deriving the name from the build target instead of the
// output argument is both more robust and what `go build` itself does when -o
// is omitted.
func parseLauncherScript(path string) (buildTarget, binaryName string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	// Match: go build [flags] -o <output> <target>
	goBuildRe := regexp.MustCompile(`go\s+build\s+.*?-o\s+(\S+)\s+(\S+)`)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") {
			continue
		}
		m := goBuildRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		target := strings.TrimRight(strings.Trim(m[2], `"'`), "/")
		if target == "" {
			continue
		}
		return target, launcherBinaryName(m[1], target)
	}
	return "", ""
}

// launcherBinaryName prefers an explicit literal output name and falls back to
// Go's own default — the last element of the build target — when the -o
// argument is a shell variable we cannot expand.
func launcherBinaryName(outArg, target string) string {
	out := strings.Trim(outArg, `"'`)
	if !strings.Contains(out, "$") {
		if base := filepath.Base(out); base != "." && base != "/" {
			return base
		}
	}
	return filepath.Base(target)
}

// copyTrackedTree copies only paths present in the source repository index.
// Publishing from a developer checkout must never package ignored binaries,
// worktrees, databases, or other host-local state.
func copyTrackedTree(src, dst string) error {
	cmd := exec.Command("git", "-C", src, "ls-files", "-z")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("list tracked plugin files: %w", err)
	}
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, raw := range bytes.Split(out, []byte{0}) {
		if len(raw) == 0 {
			continue
		}
		rel := filepath.Clean(filepath.FromSlash(string(raw)))
		if filepath.IsAbs(rel) || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return fmt.Errorf("unsafe tracked plugin path %q", raw)
		}
		source := filepath.Join(src, rel)
		target := filepath.Join(dst, rel)
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		info, err := os.Lstat(source)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(source)
			if err != nil {
				return err
			}
			if err := os.Symlink(link, target); err != nil {
				return err
			}
			continue
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("tracked plugin path %q is not a regular file or symlink", rel)
		}
		if err := copyFile(source, target); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	info, err := in.Stat()
	if err != nil {
		return err
	}

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

func dirSize(path string) int64 {
	var size int64
	filepath.WalkDir(path, func(_ string, d fs.DirEntry, _ error) error {
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				size += info.Size()
			}
		}
		return nil
	})
	return size
}
