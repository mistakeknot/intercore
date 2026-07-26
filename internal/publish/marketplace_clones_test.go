package publish

import (
	"os"
	"path/filepath"
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
