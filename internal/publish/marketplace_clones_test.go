package publish

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the multi-clone marketplace handling (mk-963o).
//
// The bug these lock down: `ic publish` resolves the marketplace by walking up
// from cwd, so a plugin inside the Sylveste tree updates core/marketplace while
// a plugin outside it updates the Claude Code checkout. Syncing only
// "monorepo -> CC" left the monorepo clone stale, and the doctor check that
// should have caught it disabled itself with `if absMarket == absCCPath` --
// true in exactly the case that produces the divergence.

// isolateHome points HOME at a temp dir so KnownMarketplaceClones cannot pick up
// the developer's real ~/projects/Sylveste/core/marketplace.
func isolateHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	t.Setenv("IC_MARKETPLACE_CLONES", "")
	return home
}

func TestKnownMarketplaceClones_DedupesAndRequiresManifest(t *testing.T) {
	isolateHome(t)
	a := setupMarketplace(t, pluginEntry{Name: "interflux", Version: "0.2.84"})
	empty := t.TempDir() // no .claude-plugin/marketplace.json

	got := KnownMarketplaceClones(a)
	if len(got) != 1 {
		t.Fatalf("want 1 clone, got %d: %v", len(got), got)
	}

	// A directory without a manifest is never reported as a clone.
	t.Setenv("IC_MARKETPLACE_CLONES", empty)
	got = KnownMarketplaceClones(a)
	for _, c := range got {
		if c == empty {
			t.Errorf("directory without a manifest was reported as a clone: %s", c)
		}
	}

	// The same path passed twice appears once.
	t.Setenv("IC_MARKETPLACE_CLONES", a)
	got = KnownMarketplaceClones(a)
	if len(got) != 1 {
		t.Errorf("duplicate path not deduplicated: %v", got)
	}
}

func TestKnownMarketplaceClones_HonoursEnvOverride(t *testing.T) {
	isolateHome(t)
	a := setupMarketplace(t, pluginEntry{Name: "interflux", Version: "0.2.84"})
	b := setupMarketplace(t, pluginEntry{Name: "interflux", Version: "0.2.84"})

	t.Setenv("IC_MARKETPLACE_CLONES", b)
	got := KnownMarketplaceClones(a)
	if len(got) != 2 {
		t.Fatalf("want both clones, got %d: %v", len(got), got)
	}
}

// The regression: publishing through the clone that ISN'T the monorepo must
// still propagate to the monorepo clone.
func TestSyncPeerMarketplaces_PropagatesFromEitherDirection(t *testing.T) {
	isolateHome(t)
	monorepo := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.1"})
	cc := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.2"})

	t.Setenv("IC_MARKETPLACE_CLONES", monorepo)

	// Publish resolved to `cc` (the outside-the-tree case). The monorepo clone
	// must learn about 0.5.2 -- this is what previously did not happen.
	if err := SyncPeerMarketplaces(cc, "interbrowse", "0.5.2"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := ReadMarketplaceVersion(monorepo, "interbrowse")
	if err != nil {
		t.Fatalf("read monorepo: %v", err)
	}
	if got != "0.5.2" {
		t.Errorf("monorepo clone still at %s, want 0.5.2 -- the mk-963o regression", got)
	}
}

func TestSyncPeerMarketplaces_SkipsClonesLackingThePlugin(t *testing.T) {
	isolateHome(t)
	other := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.289"})
	src := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.2"})

	t.Setenv("IC_MARKETPLACE_CLONES", other)
	if err := SyncPeerMarketplaces(src, "interbrowse", "0.5.2"); err != nil {
		t.Fatalf("sync: %v", err)
	}
	// interbrowse must NOT be injected into a clone that never carried it.
	if _, err := ReadMarketplaceVersion(other, "interbrowse"); err == nil {
		t.Error("plugin was added to a clone that did not carry it")
	}
	// and the clone's own entries are untouched
	if v, _ := ReadMarketplaceVersion(other, "clavain"); v != "0.6.289" {
		t.Errorf("unrelated entry changed: %s", v)
	}
}

func TestMarketplaceCloneDivergence_DetectsAndIgnoresAbsent(t *testing.T) {
	isolateHome(t)
	a := setupMarketplace(t,
		pluginEntry{Name: "interbrowse", Version: "0.5.1"},
		pluginEntry{Name: "interflux", Version: "0.2.84"},
		pluginEntry{Name: "onlyhere", Version: "1.0.0"},
	)
	b := setupMarketplace(t,
		pluginEntry{Name: "interbrowse", Version: "0.5.2"},
		pluginEntry{Name: "interflux", Version: "0.2.84"},
	)
	t.Setenv("IC_MARKETPLACE_CLONES", b)

	div, clones := MarketplaceCloneDivergence(a)
	if len(clones) != 2 {
		t.Fatalf("want 2 clones, got %v", clones)
	}
	if _, ok := div["interbrowse"]; !ok {
		t.Error("did not detect the interbrowse version disagreement")
	}
	if _, ok := div["interflux"]; ok {
		t.Error("reported agreement as divergence")
	}
	if _, ok := div["onlyhere"]; ok {
		t.Error("a plugin present in only one clone is not a disagreement")
	}
}

func TestMarketplaceCloneDivergence_SingleCloneIsQuiet(t *testing.T) {
	isolateHome(t)
	a := setupMarketplace(t, pluginEntry{Name: "interflux", Version: "0.2.84"})
	div, clones := MarketplaceCloneDivergence(a)
	if len(clones) > 1 {
		t.Fatalf("expected a single clone, got %v", clones)
	}
	if len(div) != 0 {
		t.Errorf("single clone reported divergence: %v", div)
	}
}

// The doctor must report divergence as an ERROR, and must do so even when the
// resolved marketRoot IS the CC checkout -- the old blind spot.
func TestCheckCCMarketplaceSync_ErrorsFromEitherCwd(t *testing.T) {
	home := isolateHome(t)

	ccParent := filepath.Join(home, ".claude", "plugins", "marketplaces")
	if err := os.MkdirAll(ccParent, 0755); err != nil {
		t.Fatal(err)
	}
	cc := filepath.Join(ccParent, "interagency-marketplace")
	src := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.2"})
	if err := os.Rename(src, cc); err != nil {
		t.Fatal(err)
	}
	monorepo := setupMarketplace(t, pluginEntry{Name: "interbrowse", Version: "0.5.1"})
	t.Setenv("IC_MARKETPLACE_CLONES", monorepo)

	for _, tc := range []struct{ name, root string }{
		{"from the monorepo clone", monorepo},
		{"from the CC clone (the old blind spot)", cc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := &DoctorResult{}
			versions, err := ListMarketplacePlugins(tc.root)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			checkCCMarketplaceSync(result, tc.root, versions, DoctorOpts{})
			if len(result.Findings) == 0 {
				t.Fatal("divergence not reported")
			}
			for _, f := range result.Findings {
				if f.Severity != "error" {
					t.Errorf("severity = %q, want error (a warning does not fail)", f.Severity)
				}
			}
		})
	}
}

// --- mk-pn74 (a): a push that could not land must be reported ---------------

// gitMarketplaceClone returns a marketplace checkout backed by a real git repo
// with a working bare origin, so push behaviour is exercised rather than assumed.
func gitMarketplaceClone(t *testing.T, plugins ...pluginEntry) (clone, origin string) {
	t.Helper()
	clone = setupMarketplace(t, plugins...)
	origin = newBareOrigin(t)
	runGit(t, clone, "init", "-b", "main")
	runGit(t, clone, "remote", "add", "origin", origin)
	runGit(t, clone, "add", ".")
	runGit(t, clone, "commit", "-m", "seed")
	runGit(t, clone, "push", "-u", "origin", "main")
	return clone, origin
}

// The silent failure. SyncPeerMarketplaces used to discard the return values of
// GitAdd, GitCommit and GitPush, so a peer clone that could not publish its sync
// commit kept it locally and reported nothing. That unpushed commit is what made
// the NEXT publish from that clone start diverged -- and the divergence is what
// conflicted on the version line and left a publish half-applied.
//
// The peer here is a real git repo with NO remote, so its push cannot succeed.
// An error is the only correct outcome.
func TestSyncPeerMarketplaces_ReportsAPushItCouldNotLand(t *testing.T) {
	isolateHome(t)
	src := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.300"})

	peer := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.299"})
	runGit(t, peer, "init", "-b", "main")
	runGit(t, peer, "add", ".")
	runGit(t, peer, "commit", "-m", "seed")

	t.Setenv("IC_MARKETPLACE_CLONES", peer)

	err := SyncPeerMarketplaces(src, "clavain", "0.6.300")
	if err == nil {
		t.Fatal("the sync commit could not be pushed, yet SyncPeerMarketplaces " +
			"reported success -- this is the mk-pn74 silent failure")
	}
	if !strings.Contains(err.Error(), peer) {
		t.Errorf("error does not name the clone that failed, so an operator cannot act on it: %v", err)
	}

	// The state the silence used to hide: file written, commit made, nothing pushed.
	if v, _ := ReadMarketplaceVersion(peer, "clavain"); v != "0.6.300" {
		t.Errorf("peer file not updated: %q", v)
	}
	if got := gitOutput(t, peer, "log", "-1", "--pretty=%s"); !strings.Contains(got, "sync clavain to v0.6.300") {
		t.Errorf("no sync commit in the peer clone: %q", got)
	}
}

// Control. Without it, the test above would still pass if SyncPeerMarketplaces
// had simply been changed to always return an error.
func TestSyncPeerMarketplaces_QuietWhenThePushLands(t *testing.T) {
	isolateHome(t)
	src := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.300"})
	peer, _ := gitMarketplaceClone(t, pluginEntry{Name: "clavain", Version: "0.6.299"})

	t.Setenv("IC_MARKETPLACE_CLONES", peer)

	if err := SyncPeerMarketplaces(src, "clavain", "0.6.300"); err != nil {
		t.Fatalf("the push could land, but sync reported: %v", err)
	}
	if got := gitOutput(t, peer, "log", "origin/main", "-1", "--pretty=%s"); !strings.Contains(got, "sync clavain to v0.6.300") {
		t.Errorf("commit never reached origin: %q", got)
	}
}

// A marketplace that is a plain directory is fully synced by the file write.
// Reporting git failures for it would turn every such clone into a permanent
// warning, so the git steps are skipped rather than attempted-and-excused.
func TestSyncPeerMarketplaces_PlainDirectoryIsNotAGitFailure(t *testing.T) {
	isolateHome(t)
	src := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.300"})
	peer := setupMarketplace(t, pluginEntry{Name: "clavain", Version: "0.6.299"}) // no git init

	t.Setenv("IC_MARKETPLACE_CLONES", peer)

	if err := SyncPeerMarketplaces(src, "clavain", "0.6.300"); err != nil {
		t.Fatalf("non-git marketplace reported as a git failure: %v", err)
	}
	if v, _ := ReadMarketplaceVersion(peer, "clavain"); v != "0.6.300" {
		t.Errorf("plain-directory clone not updated: %q", v)
	}
}

// --- mk-pn74 (c): pull before writing, so the bump cannot conflict ----------

// The reorder. A clone that is BEHIND its remote must be able to publish a
// bump. Under the old write-commit-then-pull order this conflicted on the
// version line every single time the clone was behind, because the replayed
// commit edited the same line the fresh base had just changed.
func TestUpdateMarketplaceAndPublish_FromACloneThatIsBehind(t *testing.T) {
	isolateHome(t)
	clone, origin := gitMarketplaceClone(t, pluginEntry{Name: "clavain", Version: "0.6.298"})

	// Another machine publishes 0.6.299 first, so `clone` is now behind.
	other := cloneOf(t, origin)
	if err := UpdateMarketplaceVersion(other, "clavain", "0.6.299"); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "commit", "-am", "chore: bump clavain to v0.6.299")
	runGit(t, other, "push", "origin", "main")

	if err := UpdateMarketplaceAndPublish(clone, "clavain", "0.6.300"); err != nil {
		t.Fatalf("publishing from a behind clone: %v", err)
	}
	if rebaseInProgress(clone) {
		t.Error("clone left mid-rebase")
	}

	// The bump reached origin ...
	runGit(t, other, "pull", "--ff-only", "origin", "main")
	if v, _ := ReadMarketplaceVersion(other, "clavain"); v != "0.6.300" {
		t.Errorf("origin at %q, want 0.6.300", v)
	}
	// ... and the concurrent 0.6.299 publish was not run over on the way.
	if log := gitOutput(t, other, "log", "--pretty=%s"); !strings.Contains(log, "bump clavain to v0.6.299") {
		t.Errorf("the other machine's publish was lost: %s", log)
	}
}

// Pulling first makes "someone already published this exact version" reachable
// for the first time. That has to be a no-op: the write would change nothing
// and the commit would fail as empty, turning a successful outcome into a hard
// error. The reorder creates this case, so the reorder answers for it.
func TestUpdateMarketplaceAndPublish_AlreadyAtTargetIsANoOp(t *testing.T) {
	isolateHome(t)
	clone, origin := gitMarketplaceClone(t, pluginEntry{Name: "clavain", Version: "0.6.298"})

	other := cloneOf(t, origin)
	if err := UpdateMarketplaceVersion(other, "clavain", "0.6.300"); err != nil {
		t.Fatal(err)
	}
	runGit(t, other, "commit", "-am", "chore: bump clavain to v0.6.300")
	runGit(t, other, "push", "origin", "main")

	if err := UpdateMarketplaceAndPublish(clone, "clavain", "0.6.300"); err != nil {
		t.Fatalf("republishing a version that already landed: %v", err)
	}
	if v, _ := ReadMarketplaceVersion(clone, "clavain"); v != "0.6.300" {
		t.Errorf("clone at %q after the no-op sync", v)
	}
}

func TestSemverLess(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.5.1", "0.5.2", true},
		{"0.5.2", "0.5.1", false},
		{"0.5.2", "0.5.2", false},
		{"0.9.0", "0.10.0", true}, // numeric, not lexical
		{"1.0.0", "0.99.99", false},
		{"0.2.84", "0.2.84", false},
	}
	for _, c := range cases {
		if got := semverLess(c.a, c.b); got != c.want {
			t.Errorf("semverLess(%q,%q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
