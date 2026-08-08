package publish

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The three defects covered here were found by publishing interrank 0.3.5 on
// 2026-08-08 and then reading the plugin repo instead of the exit code:
//
//  1. the version bump was never committed, yet the marketplace advertised it;
//  2. the sync-only path skips the whole validation block, so nothing noticed;
//  3. the release canary recorded the new version as its own prior, and the real
//     prior was then pruned as an orphan.
//
// Each test below fails if its fix is reverted. They are the mutation, not the
// assertion.

// --- committed vs worktree version -----------------------------------------

func TestReadPluginVersionAtHEADReturnsCommittedNotWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	pluginRoot, _, _ := scaffoldReleasePublishRepos(t, "1.0.0", "1.0.0")

	// Bump the worktree WITHOUT committing — the exact state a rejected version
	// commit leaves behind.
	manifest := filepath.Join(pluginRoot, ".claude-plugin", "plugin.json")
	if err := WritePluginVersion(manifest, "1.0.1"); err != nil {
		t.Fatal(err)
	}

	worktree, err := ReadPlugin(pluginRoot)
	if err != nil {
		t.Fatal(err)
	}
	if worktree.Version != "1.0.1" {
		t.Fatalf("worktree version = %s, want 1.0.1 (fixture broken)", worktree.Version)
	}

	committed, err := ReadPluginVersionAtHEAD(pluginRoot)
	if err != nil {
		t.Fatalf("ReadPluginVersionAtHEAD: %v", err)
	}
	if committed != "1.0.0" {
		t.Fatalf("committed version = %s, want 1.0.0 — it read the worktree, not HEAD", committed)
	}
}

// --- sync-only must not publish an uncommitted bump ------------------------

func TestPublishSyncOnlyRefusesUncommittedBump(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	// Committed at 0.9.0, marketplace at 0.9.0.
	pluginRoot, marketRoot, _ := scaffoldReleasePublishRepos(t, "0.9.0", "0.9.0")

	// Bump the worktree to 1.0.0 and leave it uncommitted. plugin.json now reads
	// 1.0.0 while HEAD still says 0.9.0, so `ic publish 1.0.0` sees the target
	// already met locally and takes the sync-only path.
	manifest := filepath.Join(pluginRoot, ".claude-plugin", "plugin.json")
	if err := WritePluginVersion(manifest, "1.0.0"); err != nil {
		t.Fatal(err)
	}

	eng := NewEngine(nil, PublishOpts{Mode: BumpExact, Version: "1.0.0", CWD: pluginRoot})
	eng.SetOutput(func(string, ...interface{}) {})

	err := eng.Publish(context.Background())
	if err == nil {
		t.Fatal("publish succeeded with an uncommitted version bump; " +
			"the marketplace would advertise a version no commit contains")
	}
	if !strings.Contains(err.Error(), "not committed") {
		t.Fatalf("error = %v, want a refusal naming the uncommitted bump", err)
	}

	// And it must refuse BEFORE touching the marketplace.
	marketVersion, readErr := ReadMarketplaceVersion(marketRoot, "demo")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if marketVersion != "0.9.0" {
		t.Fatalf("marketplace moved to %s before the bump was verified", marketVersion)
	}
}

// --- rollback target ------------------------------------------------------

func TestRollbackPriorVersionPrefersMarketplace(t *testing.T) {
	cases := []struct {
		name                             string
		marketplace, local, target, want string
	}{
		{
			// The sync-only shape: plugin.json already holds the target, so the
			// local value is useless as a prior. Recording it is what left
			// interrank with nothing to roll back to.
			name: "local already at target", marketplace: "0.3.4",
			local: "0.3.5", target: "0.3.5", want: "0.3.4",
		},
		{
			name: "normal bump", marketplace: "0.3.4",
			local: "0.3.4", target: "0.3.5", want: "0.3.4",
		},
		{
			// Marketplace unreadable: the local value is all there is.
			name: "marketplace unknown", marketplace: "",
			local: "0.3.4", target: "0.3.5", want: "0.3.4",
		},
		{
			// A prior equal to the target is the same as no record, so prefer
			// whatever the manifest says over a value known to be useless.
			name: "marketplace already at target", marketplace: "0.3.5",
			local: "0.3.4", target: "0.3.5", want: "0.3.4",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rollbackPriorVersion(tc.marketplace, tc.local, tc.target)
			if got != tc.want {
				t.Fatalf("rollbackPriorVersion(%q,%q,%q) = %q, want %q",
					tc.marketplace, tc.local, tc.target, got, tc.want)
			}
			if got == tc.target {
				t.Fatalf("prior == target (%q): resolveRollbackTarget ignores such a "+
					"record and the real prior gets pruned", got)
			}
		})
	}
}

// --- generated-manifest regeneration --------------------------------------

func TestRegenerateGeneratedManifestsNoGeneratorIsNotAnError(t *testing.T) {
	dir := t.TempDir()
	files, err := RegenerateGeneratedManifests(dir, "demo")
	if err != nil {
		t.Fatalf("no generator in tree should be a no-op, got %v", err)
	}
	if len(files) != 0 {
		t.Fatalf("files = %v, want none", files)
	}
}

// The generator resolves --root from its working directory. Run it from the
// plugin directory and it finds no plugins, changes nothing, and exits 0 — a
// silent no-op indistinguishable from success. This pins the working directory
// to the monorepo root so that cannot regress.
func TestRegenerateGeneratedManifestsRunsFromMonorepoRoot(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	monorepo := t.TempDir()
	scripts := filepath.Join(monorepo, "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	cwdTrace := filepath.Join(monorepo, "cwd.txt")
	// Records the directory it was invoked from, and the args it received.
	stub := "import os, sys\n" +
		"open(" + quote(cwdTrace) + ", 'w').write(os.getcwd() + '\\n' + ' '.join(sys.argv[1:]))\n"
	if err := os.WriteFile(filepath.Join(scripts, "gen-kimi-manifests.py"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	pluginRoot := filepath.Join(monorepo, "interverse", "demo")
	if err := os.MkdirAll(filepath.Join(pluginRoot, ".claude-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginRoot, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"demo","version":"1.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, pluginRoot, "init")
	runGit(t, pluginRoot, "add", "-A")
	runGit(t, pluginRoot, "commit", "-m", "init")

	if _, err := RegenerateGeneratedManifests(pluginRoot, "demo"); err != nil {
		t.Fatalf("RegenerateGeneratedManifests: %v", err)
	}

	trace, err := os.ReadFile(cwdTrace)
	if err != nil {
		t.Fatalf("generator was never invoked: %v", err)
	}
	lines := strings.SplitN(strings.TrimSpace(string(trace)), "\n", 2)
	gotCWD, gotArgs := lines[0], ""
	if len(lines) > 1 {
		gotArgs = lines[1]
	}

	wantCWD := monorepo
	if resolved, rErr := filepath.EvalSymlinks(monorepo); rErr == nil {
		wantCWD = resolved
	}
	if gotCWD != wantCWD {
		t.Fatalf("generator cwd = %s, want the monorepo root %s — from a plugin "+
			"directory it is a silent no-op", gotCWD, wantCWD)
	}
	if gotArgs != "--plugin demo" {
		t.Fatalf("generator args = %q, want \"--plugin demo\"", gotArgs)
	}
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "\\'") + "'" }
