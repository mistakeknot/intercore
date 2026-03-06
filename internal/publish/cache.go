package publish

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// CacheBase returns the base directory for the plugin cache.
func CacheBase() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "cache", "interagency-marketplace")
}

// RebuildCache copies plugin source to the cache directory, excluding .git.
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

	if err := copyDirExcludeGit(srcRoot, dest); err != nil {
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
	return copyDirExcludeGit(srcRoot, dest)
}

// CleanOrphans removes cache directories with .orphaned_at markers.
// Returns the count of removed dirs and bytes freed.
func CleanOrphans() (count int, bytesFreed int64, err error) {
	base := CacheBase()
	if base == "" {
		return 0, 0, fmt.Errorf("cannot determine cache base")
	}

	err = filepath.WalkDir(base, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip inaccessible entries
		}
		if d.Name() == ".orphaned_at" && !d.IsDir() {
			orphanDir := filepath.Dir(path)
			// Don't remove temp_git dirs
			if strings.Contains(orphanDir, "temp_git_") {
				return nil
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
	Version   string
	Path      string
	IsSymlink bool
	Orphaned  bool
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

// copyDirExcludeGit recursively copies src to dst, skipping .git directories.
func copyDirExcludeGit(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip .git directories
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}

		// Handle symlinks
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}

		return copyFile(path, target)
	})
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
