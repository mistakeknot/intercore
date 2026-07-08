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
