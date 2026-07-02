package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
