package publish

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCurrentPublishRole_DefaultsToSigner(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IC_PUBLISH_ROLE", "")
	if got := CurrentPublishRole(); got != RoleSigner {
		t.Errorf("unconfigured machine = %q, want signer (must not silently stop reporting drift)", got)
	}
}

func TestCurrentPublishRole_EnvAndFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	t.Setenv("IC_PUBLISH_ROLE", "verifier")
	if got := CurrentPublishRole(); got != RoleVerifier {
		t.Errorf("env override = %q, want verifier", got)
	}

	t.Setenv("IC_PUBLISH_ROLE", "")
	if err := os.MkdirAll(filepath.Join(home, ".config", "intercore"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".config", "intercore", "publish-role"),
		[]byte("verifier\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := CurrentPublishRole(); got != RoleVerifier {
		t.Errorf("role file = %q, want verifier", got)
	}

	// Anything unrecognised falls back to signer rather than guessing.
	if err := os.WriteFile(filepath.Join(home, ".config", "intercore", "publish-role"),
		[]byte("banana\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := CurrentPublishRole(); got != RoleSigner {
		t.Errorf("unrecognised role = %q, want signer", got)
	}
}

// The hazard: --fix wrote the local plugin.json version into the marketplace
// unconditionally, so on a machine whose checkouts trail the marketplace it
// would roll every one of them backwards (mk-ldnb).
func TestCheckPluginMarketplaceDrift_FixNeverDowngrades(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IC_PUBLISH_ROLE", "signer")

	root := setupMarketplace(t, pluginEntry{Name: "interhelm", Version: "0.2.4"})
	pluginDir := t.TempDir()
	writePluginJSON(t, pluginDir, "interhelm", "0.2.2") // local is BEHIND

	result := &DoctorResult{MarketRoot: root, PluginDirs: []string{pluginDir}}
	checkPluginMarketplaceDrift(result, map[string]string{"interhelm": "0.2.4"}, DoctorOpts{Fix: true})

	got, err := ReadMarketplaceVersion(root, "interhelm")
	if err != nil {
		t.Fatal(err)
	}
	if got != "0.2.4" {
		t.Errorf("--fix downgraded the marketplace to %s; it must only move forward", got)
	}
	for _, f := range result.Findings {
		if f.Severity == "error" {
			t.Errorf("a trailing checkout was reported as an error: %s", f.Message)
		}
	}
}

func TestCheckPluginMarketplaceDrift_VerifierDowngradesTrailingToInfo(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("IC_PUBLISH_ROLE", "verifier")

	root := setupMarketplace(t, pluginEntry{Name: "interhelm", Version: "0.2.4"})
	pluginDir := t.TempDir()
	writePluginJSON(t, pluginDir, "interhelm", "0.2.2")

	result := &DoctorResult{MarketRoot: root, PluginDirs: []string{pluginDir}}
	checkPluginMarketplaceDrift(result, map[string]string{"interhelm": "0.2.4"}, DoctorOpts{})

	if len(result.Findings) != 1 {
		t.Fatalf("want 1 finding, got %d", len(result.Findings))
	}
	if result.Findings[0].Severity != "info" {
		t.Errorf("severity = %q, want info on a verifier machine", result.Findings[0].Severity)
	}
}

// Unpublished local work must still be an error, on either role.
func TestCheckPluginMarketplaceDrift_LocalAheadIsStillAnError(t *testing.T) {
	for _, role := range []string{"signer", "verifier"} {
		t.Run(role, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("IC_PUBLISH_ROLE", role)

			root := setupMarketplace(t, pluginEntry{Name: "interhelm", Version: "0.2.2"})
			pluginDir := t.TempDir()
			writePluginJSON(t, pluginDir, "interhelm", "0.2.4") // local is AHEAD

			result := &DoctorResult{MarketRoot: root, PluginDirs: []string{pluginDir}}
			checkPluginMarketplaceDrift(result, map[string]string{"interhelm": "0.2.2"}, DoctorOpts{})

			if len(result.Findings) != 1 || result.Findings[0].Severity != "error" {
				t.Errorf("unpublished local work must be an error, got %+v", result.Findings)
			}
		})
	}
}

func writePluginJSON(t *testing.T, dir, name, version string) {
	t.Helper()
	cp := filepath.Join(dir, ".claude-plugin")
	if err := os.MkdirAll(cp, 0755); err != nil {
		t.Fatal(err)
	}
	body := `{"name":"` + name + `","version":"` + version + `"}`
	if err := os.WriteFile(filepath.Join(cp, "plugin.json"), []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
}
