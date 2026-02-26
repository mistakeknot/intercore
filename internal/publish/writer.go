package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// WritePluginVersion writes a new version to plugin.json (the source of truth).
func WritePluginVersion(pluginJSON, version string) error {
	return writeJSONVersion(pluginJSON, "version", version)
}

// WriteDerivedVersions patches all derived version files to the given version.
func WriteDerivedVersions(files []VersionFile, version string) error {
	for _, f := range files {
		var err error
		switch f.Type {
		case "json":
			err = writeJSONVersion(f.Path, f.JSONKey, version)
		case "toml":
			err = writeTOMLVersion(f.Path, version)
		case "cargo-toml":
			err = writeTOMLVersion(f.Path, version)
		default:
			err = fmt.Errorf("unknown type %q for %s", f.Type, f.Path)
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", f.Path, err)
		}
	}
	return nil
}

// VerifyVersions checks that all version files contain the expected version.
func VerifyVersions(pluginJSON string, derived []VersionFile, expected string) []string {
	var mismatches []string

	// Check plugin.json
	data, err := os.ReadFile(pluginJSON)
	if err != nil {
		mismatches = append(mismatches, fmt.Sprintf("%s: read error: %v", pluginJSON, err))
	} else {
		v, err := readJSONVersion(data, "version")
		if err != nil || v != expected {
			mismatches = append(mismatches, fmt.Sprintf("%s: got %q, want %q", pluginJSON, v, expected))
		}
	}

	// Check derived files
	for _, f := range derived {
		v, err := ReadVersionFromFile(f)
		if err != nil {
			mismatches = append(mismatches, fmt.Sprintf("%s: read error: %v", f.Path, err))
		} else if v != expected {
			mismatches = append(mismatches, fmt.Sprintf("%s: got %q, want %q", f.Path, v, expected))
		}
	}

	return mismatches
}

// writeJSONVersion updates a version field in a JSON file, preserving formatting.
// Uses a regex-based approach to avoid rewriting the entire JSON structure.
func writeJSONVersion(path, key, version string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// First, validate it's valid JSON and the key exists
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	if _, ok := raw[key]; !ok {
		return fmt.Errorf("key %q not found", key)
	}

	// Use regex replacement to preserve formatting
	pattern := regexp.MustCompile(fmt.Sprintf(`("` + regexp.QuoteMeta(key) + `"\s*:\s*)"[^"]*"`))
	updated := pattern.ReplaceAllString(string(data), fmt.Sprintf(`${1}"%s"`, version))

	if updated == string(data) {
		// Fallback: full JSON rewrite if regex didn't match
		raw[key], _ = json.Marshal(version)
		out, err := json.MarshalIndent(raw, "", "  ")
		if err != nil {
			return err
		}
		return atomicWrite(path, append(out, '\n'))
	}

	return atomicWrite(path, []byte(updated))
}

// writeTOMLVersion updates the version field in a TOML file.
func writeTOMLVersion(path, version string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	// Match: version = "..." (with optional whitespace variations)
	pattern := regexp.MustCompile(`^(version\s*=\s*)"[^"]*"`)
	lines := strings.Split(string(data), "\n")
	found := false
	for i, line := range lines {
		if pattern.MatchString(strings.TrimSpace(line)) {
			// Preserve leading whitespace
			indent := ""
			for _, c := range line {
				if c == ' ' || c == '\t' {
					indent += string(c)
				} else {
					break
				}
			}
			lines[i] = indent + pattern.ReplaceAllString(strings.TrimSpace(line),
				fmt.Sprintf(`${1}"%s"`, version))
			found = true
			break
		}
	}

	if !found {
		return fmt.Errorf("version field not found in TOML")
	}

	return atomicWrite(path, []byte(strings.Join(lines, "\n")))
}
