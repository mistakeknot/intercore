package publish

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindPluginRoot(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(`{"name":"test","version":"1.0.0"}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Find from root
	root, err := FindPluginRoot(dir)
	if err != nil {
		t.Fatalf("FindPluginRoot(%q): %v", dir, err)
	}
	if root != dir {
		t.Errorf("got %q, want %q", root, dir)
	}

	// Find from subdirectory
	subdir := filepath.Join(dir, "src", "lib")
	if err := os.MkdirAll(subdir, 0755); err != nil {
		t.Fatal(err)
	}
	root, err = FindPluginRoot(subdir)
	if err != nil {
		t.Fatalf("FindPluginRoot(%q): %v", subdir, err)
	}
	if root != dir {
		t.Errorf("got %q, want %q", root, dir)
	}
}

func TestFindPluginRootNotFound(t *testing.T) {
	dir := t.TempDir()
	_, err := FindPluginRoot(dir)
	if err != ErrNotPlugin {
		t.Errorf("expected ErrNotPlugin, got %v", err)
	}
}

func TestReadPlugin(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(pluginDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginDir, "plugin.json"),
		[]byte(`{"name":"interflux","version":"0.2.29","description":"test"}`), 0644); err != nil {
		t.Fatal(err)
	}

	p, err := ReadPlugin(dir)
	if err != nil {
		t.Fatalf("ReadPlugin: %v", err)
	}
	if p.Name != "interflux" {
		t.Errorf("name = %q, want interflux", p.Name)
	}
	if p.Version != "0.2.29" {
		t.Errorf("version = %q, want 0.2.29", p.Version)
	}
	if p.Root != dir {
		t.Errorf("root = %q, want %q", p.Root, dir)
	}
}

func TestReadPluginMissingFields(t *testing.T) {
	dir := t.TempDir()
	pluginDir := filepath.Join(dir, ".claude-plugin")
	os.MkdirAll(pluginDir, 0755)

	tests := []struct {
		name string
		json string
	}{
		{"missing name", `{"version":"1.0.0"}`},
		{"missing version", `{"name":"test"}`},
		{"empty name", `{"name":"","version":"1.0.0"}`},
		{"empty version", `{"name":"test","version":""}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.WriteFile(filepath.Join(pluginDir, "plugin.json"), []byte(tc.json), 0644)
			_, err := ReadPlugin(dir)
			if err == nil {
				t.Error("expected error")
			}
		})
	}
}

func TestDiscoverVersionFiles(t *testing.T) {
	dir := t.TempDir()

	// Create various version files
	os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"version":"1.0.0"}`), 0644)
	os.MkdirAll(filepath.Join(dir, "server"), 0755)
	os.WriteFile(filepath.Join(dir, "server", "package.json"), []byte(`{"version":"1.0.0"}`), 0644)
	os.WriteFile(filepath.Join(dir, "pyproject.toml"), []byte(`[project]\nversion = "1.0.0"`), 0644)

	files := DiscoverVersionFiles(dir)
	if len(files) != 3 {
		t.Fatalf("expected 3 version files, got %d", len(files))
	}

	types := map[string]bool{}
	for _, f := range files {
		types[filepath.Base(f.Path)+"/"+f.Type] = true
	}
	if !types["package.json/json"] {
		t.Error("missing package.json")
	}
	if !types["pyproject.toml/toml"] {
		t.Error("missing pyproject.toml")
	}
}

func TestDiscoverVersionFilesEmpty(t *testing.T) {
	dir := t.TempDir()
	files := DiscoverVersionFiles(dir)
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestReadVersionFromFile(t *testing.T) {
	dir := t.TempDir()

	// JSON
	jsonPath := filepath.Join(dir, "package.json")
	os.WriteFile(jsonPath, []byte(`{"name":"test","version":"1.2.3","other":"stuff"}`), 0644)
	v, err := ReadVersionFromFile(VersionFile{Path: jsonPath, Type: "json", JSONKey: "version"})
	if err != nil {
		t.Fatalf("json: %v", err)
	}
	if v != "1.2.3" {
		t.Errorf("json: got %q, want 1.2.3", v)
	}

	// TOML
	tomlPath := filepath.Join(dir, "pyproject.toml")
	os.WriteFile(tomlPath, []byte("[project]\nversion = \"2.0.1\"\nname = \"test\"\n"), 0644)
	v, err = ReadVersionFromFile(VersionFile{Path: tomlPath, Type: "toml"})
	if err != nil {
		t.Fatalf("toml: %v", err)
	}
	if v != "2.0.1" {
		t.Errorf("toml: got %q, want 2.0.1", v)
	}

	// Cargo.toml
	cargoPath := filepath.Join(dir, "Cargo.toml")
	os.WriteFile(cargoPath, []byte("[package]\nname = \"test\"\nversion = \"3.1.0\"\n"), 0644)
	v, err = ReadVersionFromFile(VersionFile{Path: cargoPath, Type: "cargo-toml"})
	if err != nil {
		t.Fatalf("cargo: %v", err)
	}
	if v != "3.1.0" {
		t.Errorf("cargo: got %q, want 3.1.0", v)
	}
}
