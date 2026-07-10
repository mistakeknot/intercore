package publish

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDiscoverPluginDirsIncludesCanonicalClavainCase(t *testing.T) {
	root := t.TempDir()
	pluginDir := filepath.Join(root, "interverse", "example")
	clavainDir := filepath.Join(root, "os", "Clavain")
	for _, dir := range []string{pluginDir, clavainDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := discoverPluginDirs(filepath.Join(root, "core", "intercore"))
	for _, want := range []string{pluginDir, clavainDir} {
		if !slices.Contains(got, want) {
			t.Fatalf("discoverPluginDirs() = %v, missing %s", got, want)
		}
	}
}
