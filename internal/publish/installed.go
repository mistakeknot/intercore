package publish

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
