package publish

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeInstalled plants an installed_plugins.json under a temporary HOME so the
// drift check has something to read. Returns nothing: InstalledPath() resolves
// from HOME, which t.Setenv has already redirected.
func writeInstalled(t *testing.T, home string, versions map[string]string) {
	t.Helper()
	dir := filepath.Join(home, ".claude", "plugins")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	ip := InstalledPlugins{Version: 2, Plugins: map[string][]InstalledPluginEntry{}}
	for name, v := range versions {
		ip.Plugins[name+"@interagency-marketplace"] = []InstalledPluginEntry{
			{Scope: "user", Version: v, InstallPath: filepath.Join(dir, "cache", name)},
		}
	}
	data, err := json.Marshal(ip)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "installed_plugins.json"), data, 0o600); err != nil {
		t.Fatalf("write installed_plugins.json: %v", err)
	}
}

// TestCheckInstalledDrift_SeverityDependsOnDirection pins the mk-fkfr decision.
//
// A trailing install is the normal state between a publish and the next Claude
// Code restart and self-heals, so it must not be an error -- seven such findings
// stood open on zklw and made `doctor` exit non-zero on a healthy machine. An
// install AHEAD of the marketplace cannot arise from any normal path and must
// stay an error.
//
// This test is the encoding of that decision. If someone restores a blanket
// "error" severity, this fails and says why.
func TestCheckInstalledDrift_SeverityDependsOnDirection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeInstalled(t, home, map[string]string{
		"trailing": "0.1.8", // marketplace has 0.1.9 -- normal, self-heals
		"ahead":    "0.9.0", // marketplace has 0.2.0 -- impossible normally
		"matching": "1.0.0", // no finding at all
	})

	mkt := map[string]string{
		"trailing": "0.1.9",
		"ahead":    "0.2.0",
		"matching": "1.0.0",
	}

	result := &DoctorResult{}
	checkInstalledDrift(result, mkt, DoctorOpts{})

	got := map[string]string{}
	for _, f := range result.Findings {
		if f.Category == "drift" && f.Plugin != "" {
			got[f.Plugin] = f.Severity
		}
	}

	if sev, ok := got["trailing"]; !ok {
		t.Errorf("trailing install produced no finding; it should still be reported, just not as an error")
	} else if sev != "info" {
		t.Errorf("trailing install severity = %q, want \"info\" (it self-heals on the next Claude Code restart)", sev)
	}

	if sev, ok := got["ahead"]; !ok {
		t.Errorf("install AHEAD of the marketplace produced no finding; that state cannot self-heal")
	} else if sev != "error" {
		t.Errorf("ahead-of-marketplace severity = %q, want \"error\"", sev)
	}

	if sev, ok := got["matching"]; ok {
		t.Errorf("matching versions produced a %q finding; want none", sev)
	}
}

// TestCheckInstalledDrift_TrailingDoesNotFailDoctor guards the operational
// consequence rather than the label: the reason severity was changed is that a
// machine whose only findings are trailing installs should come back clean.
func TestCheckInstalledDrift_TrailingDoesNotFailDoctor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	writeInstalled(t, home, map[string]string{
		"a": "0.1.1",
		"b": "0.6.284",
	})
	mkt := map[string]string{"a": "0.1.2", "b": "0.6.289"}

	result := &DoctorResult{}
	checkInstalledDrift(result, mkt, DoctorOpts{})

	for _, f := range result.Findings {
		if f.Severity == "error" {
			t.Fatalf("trailing-only installs produced an error finding (%s: %s); doctor would exit non-zero on a healthy machine",
				f.Plugin, f.Message)
		}
	}
}
