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
