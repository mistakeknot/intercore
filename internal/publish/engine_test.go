package publish

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPublishClearsLockOnDirtyWorktree verifies the deferred lock-cleanup:
// a publish that fails at the plugin-repo-dirty validation gate must not leave
// a publish_state row behind. Before sylveste-2uhz the row survived at phase
// 'validation' and blocked the next publish until it aged past the stale
// threshold (or an --auto run cleared it).
func TestPublishClearsLockOnDirtyWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	tmp := t.TempDir()

	// Marketplace scaffold so FindMarketplace (Stage 1, monorepo walk) resolves
	// without falling through to the real $HOME checkout.
	mktDir := filepath.Join(tmp, "core", "marketplace", ".claude-plugin")
	if err := os.MkdirAll(mktDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mktDir, "marketplace.json"),
		[]byte(`{"name":"interagency-marketplace","plugins":[{"name":"demo","version":"1.0.0"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Plugin repo: a real git repo with a committed plugin.json, then dirtied so
	// GitStatus reports the worktree as unclean -> ErrDirtyWorktree at validation.
	pluginRoot := filepath.Join(tmp, "demo")
	pluginMeta := filepath.Join(pluginRoot, ".claude-plugin")
	if err := os.MkdirAll(pluginMeta, 0o755); err != nil {
		t.Fatal(err)
	}
	pluginJSON := filepath.Join(pluginMeta, "plugin.json")
	if err := os.WriteFile(pluginJSON, []byte(`{"name":"demo","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginRoot, "init")
	runGit(t, pluginRoot, "add", "-A")
	runGit(t, pluginRoot, "commit", "-m", "init")
	// Dirty the worktree.
	if err := os.WriteFile(filepath.Join(pluginRoot, "dirty.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginRoot, "add", "dirty.txt")

	db := setupTestDB(t)
	ctx := context.Background()

	eng := NewEngine(db, PublishOpts{Mode: BumpPatch, CWD: pluginRoot})
	eng.SetOutput(func(string, ...interface{}) {}) // silence

	err := eng.Publish(ctx)
	if err == nil {
		t.Fatal("expected publish to fail on dirty worktree")
	}

	// The lock row must be gone -- not merely marked stale.
	store := NewStore(db)
	active, gerr := store.GetActive(ctx, "demo")
	if gerr != nil {
		t.Fatalf("get active: %v", gerr)
	}
	if active != nil {
		t.Errorf("publish_state row leaked after failure: %+v", active)
	}

	all, _ := store.ListActive(ctx)
	if len(all) != 0 {
		t.Errorf("ListActive = %d rows after failed publish, want 0", len(all))
	}
}

// TestPublishNonexistentRelativeCWDErrorsWithoutFallback reproduces the
// sylveste-1zu incident: `ic publish --auto --cwd=interverse/interline`, run
// from inside a directory that IS itself a valid plugin (interpulse), where
// the relative --cwd doesn't exist from there. Before the fix,
// filepath.Abs resolved the missing relative path against the process cwd,
// FindPluginRoot's walk-up then landed on the process cwd's own plugin root,
// and publish silently ran against the wrong (process-cwd) plugin three
// times in the real incident. It must now hard-error and must NOT touch the
// process-cwd plugin at all.
func TestPublishNonexistentRelativeCWDErrorsWithoutFallback(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// processCWDPlugin stands in for "interpulse" in the incident: a real,
	// valid, publishable plugin that happens to be the process's actual cwd.
	processCWDPlugin := scaffoldPluginRepo(t, "1.0.0", "1.0.0")

	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(processCWDPlugin); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	db := setupTestDB(t)
	var out strings.Builder
	// "interverse/interline"-shaped relative path: doesn't exist relative to
	// the process cwd (processCWDPlugin).
	eng := NewEngine(db, PublishOpts{Mode: BumpPatch, Auto: true, CWD: filepath.Join("interverse", "interline")})
	eng.SetOutput(func(format string, args ...interface{}) {
		fmt.Fprintf(&out, format, args...)
	})

	err = eng.Publish(context.Background())
	if err == nil {
		t.Fatalf("expected error for nonexistent relative --cwd, got nil (output: %q)", out.String())
	}
	// Compute the expected resolved path the same way the implementation
	// does (filepath.Abs, which joins against the real process cwd) rather
	// than against processCWDPlugin's pre-chdir string form, since macOS
	// tempdirs round-trip through a /private symlink that only os.Getwd
	// resolves.
	realCWD, err2 := os.Getwd()
	if err2 != nil {
		t.Fatal(err2)
	}
	wantPath := filepath.Join(realCWD, "interverse", "interline")
	wantMsg := "publish: --cwd path does not exist: " + wantPath
	if err.Error() != wantMsg {
		t.Errorf("error = %q, want %q", err.Error(), wantMsg)
	}

	// Must NOT have silently published the process-cwd plugin.
	if out.String() != "" {
		t.Errorf("expected no publish output (no fallback to process cwd), got: %q", out.String())
	}
	pluginJSON := filepath.Join(processCWDPlugin, ".claude-plugin", "plugin.json")
	data, rerr := os.ReadFile(pluginJSON)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), `"1.0.0"`) {
		t.Errorf("process-cwd plugin.json was mutated by the failed --cwd publish: %s", data)
	}
	store := NewStore(db)
	active, gerr := store.GetActive(context.Background(), "demo")
	if gerr != nil {
		t.Fatalf("get active: %v", gerr)
	}
	if active != nil {
		t.Errorf("publish_state row created for a --cwd resolution failure: %+v", active)
	}
}

// TestPublishNonexistentAbsoluteCWDErrors covers the non-relative shape of
// the same bug class: an absolute --cwd that doesn't exist must also
// hard-error rather than be handed to FindPluginRoot's walk-up search.
func TestPublishNonexistentAbsoluteCWDErrors(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "does-not-exist")

	db := setupTestDB(t)
	eng := NewEngine(db, PublishOpts{Mode: BumpPatch, CWD: missing})
	eng.SetOutput(func(string, ...interface{}) {})

	err := eng.Publish(context.Background())
	if err == nil {
		t.Fatal("expected error for nonexistent absolute --cwd, got nil")
	}
	wantMsg := "publish: --cwd path does not exist: " + missing
	if err.Error() != wantMsg {
		t.Errorf("error = %q, want %q", err.Error(), wantMsg)
	}
}

// TestPublishCWDExistsButNotDirectoryErrors covers the "exists but is a
// file, not a directory" edge case, which must also hard-error rather than
// be passed through to FindPluginRoot.
func TestPublishCWDExistsButNotDirectoryErrors(t *testing.T) {
	tmp := t.TempDir()
	filePath := filepath.Join(tmp, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	db := setupTestDB(t)
	eng := NewEngine(db, PublishOpts{Mode: BumpPatch, CWD: filePath})
	eng.SetOutput(func(string, ...interface{}) {})

	err := eng.Publish(context.Background())
	if err == nil {
		t.Fatal("expected error for --cwd that is a file, got nil")
	}
	wantMsg := "publish: --cwd path is not a directory: " + filePath
	if err.Error() != wantMsg {
		t.Errorf("error = %q, want %q", err.Error(), wantMsg)
	}
}

// scaffoldPluginRepo builds a marketplace scaffold + committed plugin git repo
// in a tempdir. Returns the plugin root.
func scaffoldPluginRepo(t *testing.T, pluginVersion, marketVersion string) string {
	t.Helper()
	tmp := t.TempDir()

	mktDir := filepath.Join(tmp, "core", "marketplace", ".claude-plugin")
	if err := os.MkdirAll(mktDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mkt := `{"name":"interagency-marketplace","plugins":[{"name":"demo","version":"` + marketVersion + `"}]}`
	if err := os.WriteFile(filepath.Join(mktDir, "marketplace.json"), []byte(mkt), 0o644); err != nil {
		t.Fatal(err)
	}

	pluginRoot := filepath.Join(tmp, "demo")
	pluginMeta := filepath.Join(pluginRoot, ".claude-plugin")
	if err := os.MkdirAll(pluginMeta, 0o755); err != nil {
		t.Fatal(err)
	}
	pj := `{"name":"demo","version":"` + pluginVersion + `"}`
	if err := os.WriteFile(filepath.Join(pluginMeta, "plugin.json"), []byte(pj), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginRoot, "init")
	runGit(t, pluginRoot, "add", "-A")
	runGit(t, pluginRoot, "commit", "-m", "init")
	return pluginRoot
}

// TestPublishExplicitVersionSyncsWhenMarketplaceBehind: `ic publish X` where
// plugin.json is already at X but the marketplace is behind must take the
// sync-only path (previously it hard-failed ErrVersionMatch — the asymmetry
// vs --auto that made developers hand-edit marketplace.json).
func TestPublishExplicitVersionSyncsWhenMarketplaceBehind(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	pluginRoot := scaffoldPluginRepo(t, "1.0.0", "0.9.0")
	db := setupTestDB(t)

	var out strings.Builder
	eng := NewEngine(db, PublishOpts{Mode: BumpExact, Version: "1.0.0", CWD: pluginRoot})
	eng.SetOutput(func(format string, args ...interface{}) {
		fmt.Fprintf(&out, format, args...)
	})

	err := eng.Publish(context.Background())
	if errors.Is(err, ErrVersionMatch) {
		t.Fatalf("pre-bumped plugin with stale marketplace returned ErrVersionMatch; want sync-only path (output: %q)", out.String())
	}
	if !strings.Contains(out.String(), "Syncing demo v1.0.0") {
		t.Errorf("sync-only banner not emitted; output: %q (err: %v)", out.String(), err)
	}
}

// TestPublishExplicitVersionMatchIsNoOp: ErrVersionMatch stays reserved for
// the true no-op — explicit target == plugin.json == marketplace.
func TestPublishExplicitVersionMatchIsNoOp(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	pluginRoot := scaffoldPluginRepo(t, "1.0.0", "1.0.0")
	db := setupTestDB(t)

	eng := NewEngine(db, PublishOpts{Mode: BumpExact, Version: "1.0.0", CWD: pluginRoot})
	eng.SetOutput(func(string, ...interface{}) {})

	err := eng.Publish(context.Background())
	if !errors.Is(err, ErrVersionMatch) {
		t.Fatalf("everything-matches publish returned %v; want ErrVersionMatch", err)
	}
}

// scaffoldReleasePublishRepos creates real plugin and marketplace repositories
// with local bare origins. The plugin starts with a stale tracked release
// artifact plus conventional build and verification scripts.
func scaffoldReleasePublishRepos(t *testing.T, pluginVersion, marketVersion string) (string, string, string) {
	t.Helper()
	tmp := t.TempDir()
	trace := filepath.Join(tmp, "release-trace")
	for key, value := range map[string]string{
		"GIT_AUTHOR_NAME":     "Intercore Test",
		"GIT_AUTHOR_EMAIL":    "intercore-test@example.invalid",
		"GIT_COMMITTER_NAME":  "Intercore Test",
		"GIT_COMMITTER_EMAIL": "intercore-test@example.invalid",
	} {
		t.Setenv(key, value)
	}

	home := filepath.Join(tmp, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("RELEASE_TRACE", trace)

	fakeBin := filepath.Join(tmp, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	marketRoot := filepath.Join(tmp, "core", "marketplace")
	marketMeta := filepath.Join(marketRoot, ".claude-plugin")
	if err := os.MkdirAll(marketMeta, 0o755); err != nil {
		t.Fatal(err)
	}
	marketJSON := `{"name":"interagency-marketplace","plugins":[{"name":"demo","version":"` + marketVersion + `"}]}`
	if err := os.WriteFile(filepath.Join(marketMeta, "marketplace.json"), []byte(marketJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, marketRoot, "init")
	runGit(t, marketRoot, "add", "-A")
	runGit(t, marketRoot, "commit", "-m", "init")
	marketRemote := filepath.Join(tmp, "marketplace.git")
	runGit(t, tmp, "init", "--bare", marketRemote)
	runGit(t, marketRoot, "remote", "add", "origin", marketRemote)
	runGit(t, marketRoot, "push", "-u", "origin", "HEAD")

	pluginRoot := filepath.Join(tmp, "demo")
	for _, dir := range []string{
		filepath.Join(pluginRoot, ".claude-plugin"),
		filepath.Join(pluginRoot, "bin"),
		filepath.Join(pluginRoot, "scripts"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	pluginJSON := `{"name":"demo","version":"` + pluginVersion + `"}`
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"), []byte(pluginJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, "bin", "release.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verifyScript := `#!/usr/bin/env bash
set -euo pipefail
printf 'verify\n' >>"$RELEASE_TRACE"
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
grep -qx fresh "$repo_root/bin/release.txt"
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "scripts", "verify-release-binaries.sh"), []byte(verifyScript), 0o755); err != nil {
		t.Fatal(err)
	}
	buildScript := `#!/usr/bin/env bash
set -euo pipefail
printf 'build\n' >>"$RELEASE_TRACE"
repo_root="$(cd "$(dirname "$0")/.." && pwd)"
printf 'fresh\n' >"$repo_root/bin/release.txt"
`
	if err := os.WriteFile(filepath.Join(pluginRoot, "scripts", "build-release.sh"), []byte(buildScript), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginRoot, "init")
	runGit(t, pluginRoot, "add", "-A")
	runGit(t, pluginRoot, "commit", "-m", "init")
	pluginRemote := filepath.Join(tmp, "plugin.git")
	runGit(t, tmp, "init", "--bare", pluginRemote)
	runGit(t, pluginRoot, "remote", "add", "origin", pluginRemote)
	runGit(t, pluginRoot, "push", "-u", "origin", "HEAD")

	return pluginRoot, marketRoot, trace
}

func TestPublishRebuildsStaleReleaseArtifactsBeforeMutation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	pluginRoot, marketRoot, trace := scaffoldReleasePublishRepos(t, "1.0.0", "1.0.0")
	eng := NewEngine(nil, PublishOpts{Mode: BumpPatch, CWD: pluginRoot})
	eng.SetOutput(func(string, ...interface{}) {})

	if err := eng.Publish(context.Background()); err != nil {
		t.Fatalf("publish: %v", err)
	}

	artifact, err := os.ReadFile(filepath.Join(pluginRoot, "bin", "release.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(artifact) != "fresh\n" {
		t.Fatalf("release artifact = %q, want rebuilt artifact", artifact)
	}
	traceData, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("release scripts were not invoked: %v", err)
	}
	if string(traceData) != "verify\nbuild\nverify\nverify\n" {
		t.Fatalf("release script order = %q, want verify/build/verify/final-verify", traceData)
	}
	clean, err := GitStatus(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !clean {
		t.Fatal("plugin worktree is dirty after publish; rebuilt artifact was not committed")
	}
	cmd := exec.Command("git", "-C", pluginRoot, "show", "HEAD:bin/release.txt")
	committed, err := cmd.Output()
	if err != nil {
		t.Fatalf("read committed artifact: %v", err)
	}
	if string(committed) != "fresh\n" {
		t.Fatalf("committed release artifact = %q, want fresh", committed)
	}
	marketVersion, err := ReadMarketplaceVersion(marketRoot, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if marketVersion != "1.0.1" {
		t.Fatalf("marketplace version = %s, want 1.0.1", marketVersion)
	}
}

func TestPublishSyncOnlyRejectsStaleReleaseArtifactsBeforeMarketplaceMutation(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	pluginRoot, marketRoot, trace := scaffoldReleasePublishRepos(t, "1.0.0", "0.9.0")
	eng := NewEngine(nil, PublishOpts{Mode: BumpExact, Version: "1.0.0", CWD: pluginRoot})
	eng.SetOutput(func(string, ...interface{}) {})

	err := eng.Publish(context.Background())
	if err == nil || !strings.Contains(err.Error(), "release artifacts are stale") {
		t.Fatalf("sync-only publish error = %v, want stale release rejection", err)
	}
	marketVersion, readErr := ReadMarketplaceVersion(marketRoot, "demo")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if marketVersion != "0.9.0" {
		t.Fatalf("marketplace changed to %s before release verification", marketVersion)
	}
	traceData, readErr := os.ReadFile(trace)
	if readErr != nil {
		t.Fatalf("release verifier was not invoked: %v", readErr)
	}
	if string(traceData) != "verify\n" {
		t.Fatalf("sync-only release script order = %q, want verifier only", traceData)
	}
}
