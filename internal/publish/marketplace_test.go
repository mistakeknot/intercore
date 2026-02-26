package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func setupMarketplace(t *testing.T, plugins ...pluginEntry) string {
	t.Helper()
	dir := t.TempDir()
	mktDir := filepath.Join(dir, ".claude-plugin")
	os.MkdirAll(mktDir, 0755)

	mkt := map[string]interface{}{
		"plugins": plugins,
	}
	data, _ := json.MarshalIndent(mkt, "", "  ")
	os.WriteFile(filepath.Join(mktDir, "marketplace.json"), data, 0644)
	return dir
}

func TestReadMarketplaceVersion(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
		pluginEntry{Name: "clavain", Version: "0.6.93"},
	)

	v, err := ReadMarketplaceVersion(root, "interflux")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.2.29" {
		t.Errorf("got %q, want 0.2.29", v)
	}

	v, err = ReadMarketplaceVersion(root, "clavain")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v != "0.6.93" {
		t.Errorf("got %q, want 0.6.93", v)
	}
}

func TestReadMarketplaceVersionNotFound(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
	)

	_, err := ReadMarketplaceVersion(root, "nonexistent")
	if err != ErrNotInMarketplace {
		t.Errorf("expected ErrNotInMarketplace, got %v", err)
	}
}

func TestUpdateMarketplaceVersion(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
		pluginEntry{Name: "clavain", Version: "0.6.93"},
	)

	if err := UpdateMarketplaceVersion(root, "interflux", "0.3.0"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify
	v, _ := ReadMarketplaceVersion(root, "interflux")
	if v != "0.3.0" {
		t.Errorf("got %q, want 0.3.0", v)
	}

	// Other plugin unchanged
	v, _ = ReadMarketplaceVersion(root, "clavain")
	if v != "0.6.93" {
		t.Errorf("clavain version changed: got %q", v)
	}
}

func TestUpdateMarketplaceVersionNotFound(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
	)

	err := UpdateMarketplaceVersion(root, "nonexistent", "1.0.0")
	if err != ErrNotInMarketplace {
		t.Errorf("expected ErrNotInMarketplace, got %v", err)
	}
}

func TestListMarketplacePlugins(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
		pluginEntry{Name: "clavain", Version: "0.6.93"},
	)

	plugins, err := ListMarketplacePlugins(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(plugins) != 2 {
		t.Fatalf("expected 2 plugins, got %d", len(plugins))
	}
	if plugins["interflux"] != "0.2.29" {
		t.Errorf("interflux = %q, want 0.2.29", plugins["interflux"])
	}
}

func TestRegisterPlugin(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
	)

	plugin := &Plugin{Name: "newplugin", Version: "0.1.0"}
	if err := RegisterPlugin(root, plugin); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify
	v, err := ReadMarketplaceVersion(root, "newplugin")
	if err != nil {
		t.Fatalf("read after register: %v", err)
	}
	if v != "0.1.0" {
		t.Errorf("got %q, want 0.1.0", v)
	}

	// Original still there
	v, _ = ReadMarketplaceVersion(root, "interflux")
	if v != "0.2.29" {
		t.Errorf("interflux changed: got %q", v)
	}
}

func TestRegisterPluginDuplicate(t *testing.T) {
	root := setupMarketplace(t,
		pluginEntry{Name: "interflux", Version: "0.2.29"},
	)

	plugin := &Plugin{Name: "interflux", Version: "0.3.0"}
	err := RegisterPlugin(root, plugin)
	if err == nil {
		t.Error("expected error for duplicate registration")
	}
}

func TestFindMarketplace(t *testing.T) {
	// Set up a monorepo-like structure
	dir := t.TempDir()
	mktDir := filepath.Join(dir, "core", "marketplace", ".claude-plugin")
	os.MkdirAll(mktDir, 0755)
	os.WriteFile(filepath.Join(mktDir, "marketplace.json"), []byte(`{"plugins":[]}`), 0644)

	// Plugin is nested inside the monorepo
	pluginDir := filepath.Join(dir, "interverse", "interflux")
	os.MkdirAll(pluginDir, 0755)

	root, err := FindMarketplace(pluginDir)
	if err != nil {
		t.Fatalf("FindMarketplace: %v", err)
	}
	expected := filepath.Join(dir, "core", "marketplace")
	if root != expected {
		t.Errorf("got %q, want %q", root, expected)
	}
}

func TestFindMarketplaceNotFound(t *testing.T) {
	dir := t.TempDir()
	// Override HOME so the CC marketplace fallback doesn't find the real one
	t.Setenv("HOME", dir)
	_, err := FindMarketplace(dir)
	if err != ErrNoMarketplace {
		t.Errorf("expected ErrNoMarketplace, got %v", err)
	}
}
