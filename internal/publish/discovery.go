package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// FindPluginRoot walks up from dir looking for .claude-plugin/plugin.json.
// Returns the parent directory of .claude-plugin/ (the plugin root).
func FindPluginRoot(dir string) (string, error) {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", fmt.Errorf("abs: %w", err)
	}

	current := absDir
	for {
		candidate := filepath.Join(current, ".claude-plugin", "plugin.json")
		if _, err := os.Stat(candidate); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", ErrNotPlugin
}

// ReadPlugin reads plugin identity from .claude-plugin/plugin.json.
func ReadPlugin(root string) (*Plugin, error) {
	pluginJSON := filepath.Join(root, ".claude-plugin", "plugin.json")
	data, err := os.ReadFile(pluginJSON)
	if err != nil {
		return nil, fmt.Errorf("read plugin.json: %w", err)
	}

	var raw struct {
		Name        string `json:"name"`
		Version     string `json:"version"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse plugin.json: %w", err)
	}

	if raw.Name == "" {
		return nil, fmt.Errorf("plugin.json: missing 'name' field")
	}
	if raw.Version == "" {
		return nil, fmt.Errorf("plugin.json: missing 'version' field")
	}

	return &Plugin{
		Name:        raw.Name,
		Version:     raw.Version,
		Root:        root,
		PluginJSON:  pluginJSON,
		description: raw.Description,
	}, nil
}

// DiscoverVersionFiles finds all derived version files in a plugin.
// These are files that contain a version string that should be kept in sync
// with plugin.json. plugin.json itself is NOT included (it's the source of truth).
func DiscoverVersionFiles(root string) []VersionFile {
	var files []VersionFile

	candidates := []struct {
		relPath string
		typ     string
		jsonKey string
	}{
		{"package.json", "json", "version"},
		{"server/package.json", "json", "version"},
		{"agent-rig.json", "json", "version"},
		{"Cargo.toml", "cargo-toml", ""},
		{"pyproject.toml", "toml", ""},
	}

	for _, c := range candidates {
		absPath := filepath.Join(root, c.relPath)
		if _, err := os.Stat(absPath); err == nil {
			files = append(files, VersionFile{
				Path:    absPath,
				Type:    c.typ,
				JSONKey: c.jsonKey,
			})
		}
	}

	return files
}

// ReadVersionFromFile extracts the version string from a derived version file.
func ReadVersionFromFile(vf VersionFile) (string, error) {
	data, err := os.ReadFile(vf.Path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", vf.Path, err)
	}

	switch vf.Type {
	case "json":
		return readJSONVersion(data, vf.JSONKey)
	case "toml":
		return readTOMLVersion(data)
	case "cargo-toml":
		return readCargoTOMLVersion(data)
	default:
		return "", fmt.Errorf("unknown version file type: %s", vf.Type)
	}
}

func readJSONVersion(data []byte, key string) (string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", fmt.Errorf("parse JSON: %w", err)
	}
	val, ok := raw[key]
	if !ok {
		return "", fmt.Errorf("key %q not found", key)
	}
	var version string
	if err := json.Unmarshal(val, &version); err != nil {
		return "", fmt.Errorf("parse version value: %w", err)
	}
	return version, nil
}

// readTOMLVersion extracts version from pyproject.toml using simple line scanning.
// Looks for: version = "X.Y.Z" in the [project] section.
func readTOMLVersion(data []byte) (string, error) {
	return extractTOMLValue(string(data), "version")
}

// readCargoTOMLVersion extracts version from Cargo.toml [package] section.
func readCargoTOMLVersion(data []byte) (string, error) {
	return extractTOMLValue(string(data), "version")
}

// extractTOMLValue does simple line-based extraction of key = "value" from TOML.
// This avoids pulling in a TOML parser dependency.
func extractTOMLValue(content, key string) (string, error) {
	lines := splitLines(content)
	prefix := key + " = \""
	for _, line := range lines {
		trimmed := trimSpace(line)
		if len(trimmed) > len(prefix) && trimmed[:len(prefix)] == prefix {
			end := len(trimmed) - 1
			if end > len(prefix) && trimmed[end] == '"' {
				return trimmed[len(prefix):end], nil
			}
		}
	}
	return "", fmt.Errorf("%s not found in TOML", key)
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t' || s[end-1] == '\r') {
		end--
	}
	return s[start:end]
}
