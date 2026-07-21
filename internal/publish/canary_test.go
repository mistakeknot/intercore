package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRegisterCanary_UpsertsByPluginAndMarketplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-canaries.json")

	if err := registerCanaryIn(path, ReleaseCanary{
		Plugin: "clavain", Marketplace: "interagency-marketplace",
		Version: "0.6.281", PriorVersion: "0.6.280", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	// A newer publish replaces the record; a different plugin coexists.
	if err := registerCanaryIn(path, ReleaseCanary{
		Plugin: "clavain", Marketplace: "interagency-marketplace",
		Version: "0.6.282", PriorVersion: "0.6.281", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := registerCanaryIn(path, ReleaseCanary{
		Plugin: "interline", Marketplace: "interagency-marketplace",
		Version: "0.2.16", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}

	cs, err := readCanariesFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("expected 2 records (clavain upserted + interline), got %d", len(cs))
	}
	for _, c := range cs {
		if c.Plugin == "clavain" && c.Version != "0.6.282" {
			t.Fatalf("clavain canary not upserted: %+v", c)
		}
	}
}

func TestMarkCanary_UpdatesStatusAndNote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release-canaries.json")
	if err := registerCanaryIn(path, ReleaseCanary{
		Plugin: "clavain", Marketplace: "interagency-marketplace",
		Version: "0.6.281", Status: "pending",
	}); err != nil {
		t.Fatal(err)
	}
	if err := markCanaryIn(path, "clavain", "interagency-marketplace", "passed", "loaded"); err != nil {
		t.Fatal(err)
	}
	cs, _ := readCanariesFrom(path)
	if cs[0].Status != "passed" || cs[0].CheckedAt == 0 {
		t.Fatalf("canary not marked: %+v", cs[0])
	}
}

func writeArtifact(t *testing.T, pluginJSON string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"), []byte(pluginJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestProbeArtifact_HealthyArtifactPasses(t *testing.T) {
	dir := writeArtifact(t, `{"name":"testplug","version":"1.0.0","description":"x"}`)
	if issues := ProbeArtifact(dir); len(issues) != 0 {
		t.Fatalf("healthy artifact must probe clean, got %v", issues)
	}
}

func TestProbeArtifact_TripsOnBrokenPublish(t *testing.T) {
	cases := []struct {
		name       string
		pluginJSON string
		wantCheck  string
	}{
		{"invalid json", `{broken`, "schema"},
		{"missing version", `{"name":"x"}`, "schema"},
		{"unrecognized key", `{"name":"x","version":"1.0.0","outputStyles":"y"}`, "schema"},
		{"string author", `{"name":"x","version":"1.0.0","author":"me"}`, "schema"},
		{"declared hooks missing", `{"name":"x","version":"1.0.0","hooks":"./missing/hooks.json"}`, "hooks"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeArtifact(t, tc.pluginJSON)
			issues := ProbeArtifact(dir)
			if len(issues) == 0 {
				t.Fatalf("broken artifact probed clean")
			}
			found := false
			for _, is := range issues {
				if is.Check == tc.wantCheck {
					found = true
				}
			}
			if !found {
				t.Fatalf("expected a %q issue, got %v", tc.wantCheck, issues)
			}
		})
	}
}

func TestProbeArtifact_DoubleHookLoadTrips(t *testing.T) {
	dir := writeArtifact(t, `{"name":"x","version":"1.0.0","hooks":"./hooks/hooks.json"}`)
	if err := os.MkdirAll(filepath.Join(dir, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	issues := ProbeArtifact(dir)
	found := false
	for _, is := range issues {
		if is.Check == "hooks" {
			found = true
		}
	}
	if !found {
		t.Fatalf("standard-path + declared hooks must trip the double-load check, got %v", issues)
	}
}

func TestResolveRollbackTarget(t *testing.T) {
	versions := []CacheEntry{
		{Version: "0.6.280", Path: "/x/0.6.280"},
		{Version: "0.6.281", Path: "/x/0.6.281"},
	}

	// Recorded prior survives in cache → chosen.
	got, err := resolveRollbackTarget(versions, "0.6.281", "0.6.280")
	if err != nil || got != "0.6.280" {
		t.Fatalf("want recorded prior 0.6.280, got %q err %v", got, err)
	}

	// Recorded prior pruned → newest retained older version.
	got, err = resolveRollbackTarget(versions, "0.6.281", "0.6.279")
	if err != nil || got != "0.6.280" {
		t.Fatalf("want fallback 0.6.280, got %q err %v", got, err)
	}

	// Only the current version retained → clean refusal.
	only := []CacheEntry{{Version: "0.6.281", Path: "/x/0.6.281"}}
	if _, err := resolveRollbackTarget(only, "0.6.281", ""); err == nil {
		t.Fatal("expected clean refusal with no retained prior version")
	}
}

func TestRollbackLocalState_RestoresPriorPointers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Retained prior + current cache dirs.
	for _, v := range []string{"1.0.0", "1.1.0"} {
		if err := os.MkdirAll(filepath.Join(home, ".claude", "plugins", "cache", "interagency-marketplace", "testplug", v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// installed_plugins.json at the bad new version.
	ipPath := filepath.Join(home, ".claude", "plugins", "installed_plugins.json")
	badPath := filepath.Join(home, ".claude", "plugins", "cache", "interagency-marketplace", "testplug", "1.1.0")
	ip := `{"version":2,"plugins":{"testplug@interagency-marketplace":[{"scope":"user","installPath":"` + badPath + `","version":"1.1.0","installedAt":"t","lastUpdated":"t"}]}}`
	if err := os.WriteFile(ipPath, []byte(ip), 0o644); err != nil {
		t.Fatal(err)
	}
	// Marketplace root at the bad new version.
	marketRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(marketRoot, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	mkt := `{"plugins":[{"name":"testplug","version":"1.1.0"}]}`
	if err := os.WriteFile(filepath.Join(marketRoot, ".claude-plugin", "marketplace.json"), []byte(mkt), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rollbackLocalState(marketRoot, "testplug", "1.0.0"); err != nil {
		t.Fatalf("rollbackLocalState: %v", err)
	}

	// marketplace.json repointed.
	if v, err := ReadMarketplaceVersion(marketRoot, "testplug"); err != nil || v != "1.0.0" {
		t.Fatalf("marketplace.json not restored: %q err %v", v, err)
	}
	// installed_plugins.json repointed at an existing cache dir.
	data, _ := os.ReadFile(ipPath)
	var out struct {
		Plugins map[string][]struct {
			InstallPath string `json:"installPath"`
			Version     string `json:"version"`
		} `json:"plugins"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	rec := out.Plugins["testplug@interagency-marketplace"][0]
	if rec.Version != "1.0.0" {
		t.Fatalf("installed version not restored: %s", rec.Version)
	}
	if _, err := os.Stat(rec.InstallPath); err != nil {
		t.Fatalf("restored installPath does not exist: %s", rec.InstallPath)
	}

	// Refusal when the target cache dir is gone.
	if err := rollbackLocalState(marketRoot, "testplug", "0.9.0"); err == nil {
		t.Fatal("expected refusal for missing target cache dir")
	}
}
