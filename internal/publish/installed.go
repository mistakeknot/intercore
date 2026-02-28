package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// InstalledPlugins represents the ~/.claude/plugins/installed_plugins.json structure.
type InstalledPlugins struct {
	Version int                               `json:"version"`
	Plugins map[string][]InstalledPluginEntry `json:"plugins"`
}

// InstalledPluginEntry represents a single install record for a plugin.
type InstalledPluginEntry struct {
	Scope        string `json:"scope"`
	InstallPath  string `json:"installPath"`
	Version      string `json:"version"`
	InstalledAt  string `json:"installedAt"`
	LastUpdated  string `json:"lastUpdated"`
	GitCommitSha string `json:"gitCommitSha,omitempty"`
}

// InstalledPath returns the path to installed_plugins.json.
func InstalledPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
}

// ReadInstalled reads the installed_plugins.json file.
func ReadInstalled() (*InstalledPlugins, error) {
	path := InstalledPath()
	if path == "" {
		return nil, fmt.Errorf("cannot determine installed_plugins.json path")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &InstalledPlugins{
				Version: 2,
				Plugins: make(map[string][]InstalledPluginEntry),
			}, nil
		}
		return nil, fmt.Errorf("read installed_plugins.json: %w", err)
	}

	var ip InstalledPlugins
	if err := json.Unmarshal(data, &ip); err != nil {
		return nil, fmt.Errorf("parse installed_plugins.json: %w", err)
	}
	if ip.Plugins == nil {
		ip.Plugins = make(map[string][]InstalledPluginEntry)
	}
	return &ip, nil
}

// UpdateInstalled patches the version and installPath for a plugin in installed_plugins.json.
func UpdateInstalled(pluginName, version, installPath string) error {
	path := InstalledPath()
	if path == "" {
		return fmt.Errorf("cannot determine installed_plugins.json path")
	}

	ip, err := ReadInstalled()
	if err != nil {
		return err
	}

	key := pluginName + "@interagency-marketplace"
	now := time.Now().UTC().Format(time.RFC3339)

	entries, ok := ip.Plugins[key]
	if ok && len(entries) > 0 {
		entries[0].Version = version
		entries[0].InstallPath = installPath
		entries[0].LastUpdated = now
		ip.Plugins[key] = entries
	} else {
		ip.Plugins[key] = []InstalledPluginEntry{{
			Scope:       "user",
			InstallPath: installPath,
			Version:     version,
			InstalledAt: now,
			LastUpdated: now,
		}}
	}

	data, err := json.MarshalIndent(ip, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal installed_plugins.json: %w", err)
	}

	return atomicWrite(path, append(data, '\n'))
}

// EnableInSettings adds "pluginName@interagency-marketplace": true to ~/.claude/settings.json.
// This is idempotent — if the key already exists, it is left unchanged.
func EnableInSettings(pluginName string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("cannot determine home directory: %w", err)
	}
	settingsPath := filepath.Join(home, ".claude", "settings.json")

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		return fmt.Errorf("read settings.json: %w", err)
	}

	// Parse as generic JSON to preserve all fields
	var settings map[string]interface{}
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse settings.json: %w", err)
	}

	// Find or create the plugin enablement object.
	// Claude Code uses a top-level key whose value is a map of "name@marketplace": bool.
	// The key name varies — find the one containing existing @interagency-marketplace entries.
	var enableKey string
	for k, v := range settings {
		if m, ok := v.(map[string]interface{}); ok {
			for mk := range m {
				if strings.Contains(mk, "@interagency-marketplace") {
					enableKey = k
					break
				}
			}
		}
		if enableKey != "" {
			break
		}
	}

	key := pluginName + "@interagency-marketplace"

	if enableKey != "" {
		m := settings[enableKey].(map[string]interface{})
		if _, exists := m[key]; exists {
			return nil // already enabled
		}
		m[key] = true
	} else {
		// No existing enablement object — this shouldn't happen in practice
		// but handle it by creating one
		settings["plugins"] = map[string]interface{}{key: true}
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal settings.json: %w", err)
	}
	return atomicWrite(settingsPath, append(out, '\n'))
}

// ReadInstalledVersion returns the installed version for a plugin, or empty string.
func ReadInstalledVersion(pluginName string) string {
	ip, err := ReadInstalled()
	if err != nil {
		return ""
	}
	key := pluginName + "@interagency-marketplace"
	entries, ok := ip.Plugins[key]
	if !ok || len(entries) == 0 {
		return ""
	}
	return entries[0].Version
}
