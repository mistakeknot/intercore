package publish

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestScopedPublishPreservesUnrelatedState(t *testing.T) {
	testPublishIsolation(t, true)
}

func TestPublishIsolationNegativeControl(t *testing.T) {
	if len(RunningExecutables()) == 0 {
		t.Skip("negative control requires process visibility for real cache pruning")
	}
	testPublishIsolation(t, false)
}

func testPublishIsolation(t *testing.T, scoped bool) {
	t.Helper()
	pluginRoot, marketRoot, trace := scaffoldReleasePublishRepos(t, "1.0.0", "1.0.0")
	write := func(path, body string, mode os.FileMode) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	read := func(path string) string {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	// The real pipeline still commits/pushes the selected plugin and canonical
	// marketplace. A separate clone has an ahead user commit that must survive.
	ccRoot := filepath.Join(os.Getenv("HOME"), ".claude", "plugins", "marketplaces", "interagency-marketplace")
	if err := os.MkdirAll(filepath.Dir(ccRoot), 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, filepath.Dir(ccRoot), "clone", marketRoot, ccRoot)
	write(filepath.Join(ccRoot, "user.txt"), "user work\n", 0o644)
	runGit(t, ccRoot, "add", "user.txt")
	runGit(t, ccRoot, "commit", "-m", "user commit")
	ccHead, err := GitHeadCommit(ccRoot)
	if err != nil {
		t.Fatal(err)
	}
	ccJSON := filepath.Join(ccRoot, ".claude-plugin", "marketplace.json")
	ccBefore := read(ccJSON)

	// Cover every global maintenance operation, including the unconditional
	// dangling-link sweep, plus cross-repo rig and diagram regeneration.
	orphan := filepath.Join(CacheRoot(), "other-market", "orphan", "1.0.0", ".orphaned_at")
	write(orphan, "preserve", 0o644)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(orphan, old, old); err != nil {
		t.Fatal(err)
	}
	otherOld := filepath.Join(CacheBase(), "other", "1.0.0", "sentinel")
	otherNew := filepath.Join(CacheBase(), "other", "2.0.0", "sentinel")
	write(otherOld, "old", 0o644)
	write(otherNew, "new", 0o644)
	dangling := filepath.Join(CacheBase(), "other", "0.1.0")
	if err := os.Symlink("missing", dangling); err != nil {
		t.Fatal(err)
	}
	if err := UpdateInstalled("other", "2.0.0", filepath.Dir(otherNew), "other-sha"); err != nil {
		t.Fatal(err)
	}
	beforeInstalled, err := ReadInstalled()
	if err != nil {
		t.Fatal(err)
	}

	tmp := filepath.Dir(pluginRoot)
	rigPath := filepath.Join(tmp, "os", "Clavain", "agent-rig.json")
	rigBody := `{"plugins":{"recommended":[]}}`
	write(rigPath, rigBody, 0o644)
	write(filepath.Join(tmp, "interchart", "scripts", "generate.sh"), "#!/bin/sh\nprintf 'diagram\\n' >>\"$RELEASE_TRACE\"\n", 0o755)
	write(filepath.Join(tmp, "fake-bin", "claude"), "#!/bin/sh\nprintf 'refresh\\n' >>\"$RELEASE_TRACE\"\n", 0o755)
	write(filepath.Join(pluginRoot, "hooks", "hooks.json"), `{"hooks":{}}`, 0o644)
	runGit(t, pluginRoot, "add", "hooks/hooks.json")
	runGit(t, pluginRoot, "commit", "-m", "add hooks")

	var output strings.Builder
	engine := NewEngine(nil, PublishOpts{Mode: BumpPatch, CWD: pluginRoot, Scoped: scoped})
	engine.SetOutput(func(format string, args ...interface{}) { fmt.Fprintf(&output, format, args...) })
	if err := engine.Publish(context.Background()); err != nil {
		t.Fatalf("publish: %v\n%s", err, &output)
	}

	if scoped {
		if after, err := GitHeadCommit(ccRoot); err != nil || after != ccHead {
			t.Errorf("peer clone HEAD changed: %s, %v", after, err)
		}
		if read(ccJSON) != ccBefore {
			t.Error("peer marketplace changed")
		}
		if clean, err := GitStatus(ccRoot); err != nil || !clean {
			t.Errorf("peer clone dirtied: %v", err)
		}
		if read(orphan) != "preserve" || read(otherOld) != "old" || read(otherNew) != "new" {
			t.Error("unrelated cache changed")
		}
		if target, err := os.Readlink(dangling); err != nil || target != "missing" {
			t.Errorf("unrelated dangling link changed: %s, %v", target, err)
		}
		if read(rigPath) != rigBody {
			t.Error("unrelated rig changed")
		}
		if got := read(trace); got != "verify\nbuild\nverify\nverify\n" {
			t.Errorf("unexpected global command: %q", got)
		}
	} else {
		if after, err := GitHeadCommit(ccRoot); err != nil || after == ccHead {
			t.Errorf("negative control: peer HEAD did not change: %v", err)
		}
		if read(ccJSON) == ccBefore {
			t.Error("negative control: peer marketplace did not change")
		}
		for _, path := range []string{orphan, otherOld, dangling} {
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Errorf("negative control: %s not removed: %v", path, err)
			}
		}
		if read(rigPath) == rigBody {
			t.Error("negative control: rig did not change")
		}
		if got := read(trace); !strings.Contains(got, "refresh\n") || !strings.Contains(got, "diagram\n") {
			t.Errorf("negative control: global commands did not run: %q", got)
		}
	}
	installed, err := ReadInstalled()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(installed.Plugins["other@interagency-marketplace"], beforeInstalled.Plugins["other@interagency-marketplace"]) {
		t.Error("unrelated installed record changed")
	}
	if got := installed.Plugins["demo@interagency-marketplace"]; len(got) != 1 || got[0].Version != "1.0.1" {
		t.Errorf("selected installed record: %+v", got)
	}
	if got, err := ReadMarketplaceVersion(marketRoot, "demo"); err != nil || got != "1.0.1" {
		t.Errorf("canonical marketplace: %s, %v", got, err)
	}
	for _, root := range []string{pluginRoot, marketRoot} {
		if err := GitRemoteReachable(root); err != nil {
			t.Fatal(err)
		}
		// Compare the pushed branch, not merely the local version file.
		head, err := GitHeadCommit(root)
		if err != nil {
			t.Fatal(err)
		}
		cmd := execCommand("git", "-C", root, "rev-parse", "@{upstream}")
		remote, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(remote)) != head {
			t.Errorf("not pushed: %s, %v", root, err)
		}
	}
	if got := read(filepath.Join(CacheBase(), "demo", "1.0.1", "bin", "release.txt")); got != "fresh\n" {
		t.Errorf("cache artifact: %q", got)
	}
	if got, err := os.Readlink(filepath.Join(CacheBase(), "demo", "1.0.0")); err != nil || got != "1.0.1" {
		t.Errorf("hook bridge: %q, %v", got, err)
	}
	canaries, err := readCanariesFrom(CanaryPath())
	if err != nil || len(canaries) != 1 || canaries[0].Status != "pending" {
		t.Errorf("release canary missing: %+v, %v", canaries, err)
	}
	if strings.Contains(output.String(), "ERROR:") || !strings.Contains(output.String(), "Post-release probe passed") {
		t.Errorf("scoped probe must verify canonical marketplace: %s", &output)
	}
	if scoped {
		canonicalRoot, err := filepath.EvalSymlinks(marketRoot)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), canonicalRoot) {
			t.Error("non-dry-run did not disclose canonical marketplace path")
		}
		if len(canaries) != 1 || !canaries[0].Scoped || canaries[0].MarketRoot != canonicalRoot {
			t.Errorf("rollback scope not bound to canonical checkout: %+v", canaries)
		}
		if !strings.Contains(read(CanaryPath()), `"scoped": true`) {
			t.Error("scope not persisted for rollback")
		}
		// A real retained older artifact, separate from the hook bridge. Invoke
		// rollback FROM the protected peer: it must use the recorded canonical
		// root, not rediscover that peer as the new publication authority.
		write(filepath.Join(CacheBase(), "demo", "0.9.0", ".claude-plugin", "plugin.json"), `{"name":"demo","version":"0.9.0"}`, 0o644)
		if err := RollbackPlugin(ccRoot, "demo", func(string, ...interface{}) {}); err != nil {
			t.Fatal(err)
		}
		if after, err := GitHeadCommit(ccRoot); err != nil || after != ccHead {
			t.Errorf("rollback changed peer HEAD: %s, %v", after, err)
		}
		if read(ccJSON) != ccBefore {
			t.Error("rollback changed peer marketplace")
		}
		if got := read(trace); got != "verify\nbuild\nverify\nverify\n" {
			t.Errorf("rollback refreshed peers: %q", got)
		}
		if got, err := ReadMarketplaceVersion(marketRoot, "demo"); err != nil || got != "0.9.0" {
			t.Errorf("rollback did not restore canonical marketplace: %s, %v", got, err)
		}
		if got, err := ReadInstalled(); err != nil || got.Plugins["demo@interagency-marketplace"][0].Version != "0.9.0" {
			t.Errorf("rollback did not restore installed record: %+v, %v", got, err)
		}
	}
}

func TestScopedRollbackRefusesUnrecoverableScope(t *testing.T) {
	for _, kind := range []string{"malformed", "missing-root", "unavailable-root"} {
		t.Run(kind, func(t *testing.T) {
			pluginRoot, marketRoot, _ := scaffoldReleasePublishRepos(t, "1.0.0", "1.0.0")
			prior := filepath.Join(CacheBase(), "demo", "0.9.0")
			if err := os.MkdirAll(prior, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := UpdateInstalled("demo", "1.0.0", filepath.Join(CacheBase(), "demo", "1.0.0"), ""); err != nil {
				t.Fatal(err)
			}
			before, err := GitHeadCommit(marketRoot)
			if err != nil {
				t.Fatal(err)
			}
			c := ReleaseCanary{Plugin: "demo", Marketplace: "interagency-marketplace", Version: "1.0.0", PriorVersion: "0.9.0", Scoped: true}
			if kind == "unavailable-root" {
				c.MarketRoot = filepath.Join(t.TempDir(), "gone")
			}
			if err := RegisterCanary(c); err != nil {
				t.Fatal(err)
			}
			if kind == "malformed" {
				if err := os.WriteFile(CanaryPath(), []byte("{invalid"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := RollbackPlugin(pluginRoot, "demo", func(string, ...interface{}) {}); err == nil {
				t.Fatal("unsafe rollback accepted")
			}
			if after, err := GitHeadCommit(marketRoot); err != nil || after != before {
				t.Errorf("marketplace HEAD changed: %s, %v", after, err)
			}
			if got, err := ReadMarketplaceVersion(marketRoot, "demo"); err != nil || got != "1.0.0" {
				t.Errorf("marketplace changed: %s, %v", got, err)
			}
			if got, err := ReadInstalled(); err != nil || got.Plugins["demo@interagency-marketplace"][0].Version != "1.0.0" {
				t.Errorf("installed record changed: %+v, %v", got, err)
			}
		})
	}
}

func TestScopedPublishDryRunDeclaresScope(t *testing.T) {
	pluginRoot := scaffoldPluginRepo(t, "1.0.0", "1.0.0")
	var output strings.Builder
	engine := NewEngine(nil, PublishOpts{Mode: BumpPatch, CWD: pluginRoot, Scoped: true, DryRun: true})
	engine.SetOutput(func(format string, args ...interface{}) { fmt.Fprintf(&output, format, args...) })
	if err := engine.Publish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Scoped publish") || !strings.Contains(output.String(), "peer marketplace") {
		t.Fatalf("scope not disclosed: %s", &output)
	}
	if got, err := ReadPlugin(pluginRoot); err != nil || got.Version != "1.0.0" {
		t.Fatalf("dry-run mutated plugin: %+v, %v", got, err)
	}
}
