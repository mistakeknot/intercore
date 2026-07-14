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
// Returns the count of removed dirs and bytes freed.
func CleanOrphans() (count int, bytesFreed int64, err error) {
	root := CacheRoot()
	if root == "" {
		return 0, 0, fmt.Errorf("cannot determine cache root")
	}
	return cleanOrphansIn(root, 0)
}

// CleanOrphansOlderThan removes marked orphans whose marker file is older
// than minAge. The publish engine uses this instead of CleanOrphans: a marker
// is a deferred-deletion signal (a live session may still read hooks from the
// dir), so the automatic path grants a grace window that the explicit
// `ic publish clean` does not.
func CleanOrphansOlderThan(minAge time.Duration) (count int, bytesFreed int64, err error) {
	root := CacheRoot()
	if root == "" {
		return 0, 0, fmt.Errorf("cannot determine cache root")
	}
	return cleanOrphansIn(root, minAge)
}

// cleanOrphansIn is the testable core of CleanOrphans/CleanOrphansOlderThan.
// Takes an explicit root path so tests can use t.TempDir().
func cleanOrphansIn(root string, minAge time.Duration) (count int, bytesFreed int64, err error) {
	rootDepth := strings.Count(root, string(os.PathSeparator))
	expectedMarkerDepth := rootDepth + 4 // <root>/<marketplace>/<plugin>/<version>/.orphaned_at

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
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
			count++
			bytesFreed += size
			return filepath.SkipDir
		}
		return nil
	})
	return count, bytesFreed, err
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
// For each plugin, it keeps the installed version plus the `keep-1` most recent other versions,
// removing the rest. Symlinks and orphaned directories are skipped (orphans have their own cleanup).
// Returns the number of directories removed and total bytes freed.
func PruneStaleVersions(keep int) (count int, bytesFreed int64, err error) {
	entries, err := ListCacheEntries()
	if err != nil {
		return 0, 0, err
	}

	for pluginName, versions := range entries {
		installedVer := ReadInstalledVersion(pluginName)

		// Separate: installed version stays, symlinks stay, orphans stay (handled by CleanOrphans)
		var candidates []CacheEntry
		for _, v := range versions {
			if v.IsSymlink || v.Orphaned || v.Version == installedVer {
				continue
			}
			candidates = append(candidates, v)
		}

		// Sort candidates by version descending (newest first)
		sortVersionsDesc(candidates)

		// Keep the top `keep-1` (since the installed version is already kept separately)
		toKeep := keep - 1
		if toKeep < 0 {
			toKeep = 0
		}

		for i, c := range candidates {
			if i < toKeep {
				continue // keep this one
			}
			size := dirSize(c.Path)
			if rmErr := os.RemoveAll(c.Path); rmErr != nil {
				continue // best effort
			}
			count++
			bytesFreed += size
		}
	}

	return count, bytesFreed, nil
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

// PruneStaleVersionsAcrossMarketplaces removes stale plugin versions from EVERY
// marketplace cache, not just interagency-marketplace. For each plugin, keeps the
// version listed in installed_plugins.json (and the keep-1 most recent others).
// This is the multi-marketplace counterpart to PruneStaleVersions.
func PruneStaleVersionsAcrossMarketplaces(keep int) (count int, bytesFreed int64, err error) {
	entries, err := ListAllCacheEntries()
	if err != nil {
		return 0, 0, err
	}

	ip, err := ReadInstalled()
	if err != nil {
		return 0, 0, err
	}

	for key, versions := range entries {
		// key is "<plugin>@<marketplace>"; installed_plugins.json uses the same shape
		var installedVer string
		if rec, ok := ip.Plugins[key]; ok && len(rec) > 0 {
			installedVer = rec[0].Version
		}

		var candidates []CacheEntry
		for _, v := range versions {
			if v.IsSymlink || v.Orphaned || v.Version == installedVer {
				continue
			}
			candidates = append(candidates, v)
		}

		sortVersionsDesc(candidates)
		toKeep := keep - 1
		if toKeep < 0 {
			toKeep = 0
		}
		for i, c := range candidates {
			if i < toKeep {
				continue
			}
			size := dirSize(c.Path)
			if rmErr := os.RemoveAll(c.Path); rmErr != nil {
				continue
			}
			count++
			bytesFreed += size
		}
	}
	return count, bytesFreed, nil
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
func CountStaleAcrossMarketplaces() (orphaned int, stale int, err error) {
	entries, err := ListAllCacheEntries()
	if err != nil {
		return 0, 0, err
	}
	ip, err := ReadInstalled()
	if err != nil {
		return 0, 0, err
	}
	for key, versions := range entries {
		var installedVer string
		if rec, ok := ip.Plugins[key]; ok && len(rec) > 0 {
			installedVer = rec[0].Version
		}
		for _, v := range versions {
			if v.Orphaned {
				orphaned++
			} else if !v.IsSymlink && v.Version != installedVer {
				stale++
			}
		}
	}
	return orphaned, stale, nil
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
// Best-effort: returns nil on skip or failure (logged as warning by caller).
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
		return nil // can't determine what to build
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

// parseLauncherScript extracts the go build target and binary name from a launch-mcp.sh.
// Looks for patterns like: go build -o "$SCRIPT_DIR/server" ./cmd/server
func parseLauncherScript(path string) (buildTarget, binaryName string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()

	// Match: go build -o <output> <target>
	goBuildRe := regexp.MustCompile(`go\s+build\s+-o\s+\S+/(\S+)\s+(\S+)`)

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if m := goBuildRe.FindStringSubmatch(line); m != nil {
			return m[2], m[1] // target, binary name
		}
	}
	return "", ""
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
