package publish

import (
	"os"
	"path/filepath"
	"testing"
)

// makeCacheTree builds a fixture cache layout: <root>/<marketplace>/<plugin>/<version>/
// versions is a list of {marketplace, plugin, version} triples. An empty version
// string creates the plugin dir but no version subdir (rare but possible in cache).
func makeCacheTree(t *testing.T, triples [][3]string) string {
	t.Helper()
	root := t.TempDir()
	for _, tr := range triples {
		marketplace, plugin, version := tr[0], tr[1], tr[2]
		dir := filepath.Join(root, marketplace, plugin, version)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return root
}

func TestListAllCacheEntries_MultipleMarketplaces(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"claude-plugins-official", "vercel", "0.40.0"},
		{"claude-plugins-official", "vercel", "0.40.1"},
		{"claude-plugins-official", "vercel", "0.42.1"},
		{"interagency-marketplace", "tldr-swinton", "0.7.18"},
		{"interagency-marketplace", "tldr-swinton", "0.7.19"},
		{"clonal-plugins", "interops", "0.2.10"},
	})

	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}

	if got := len(entries); got != 3 {
		t.Errorf("expected 3 plugin keys, got %d (keys: %v)", got, keysOf(entries))
	}

	// Vercel should have 3 versions; tldr-swinton 2; interops 1
	cases := []struct {
		key      string
		wantNVer int
	}{
		{"vercel@claude-plugins-official", 3},
		{"tldr-swinton@interagency-marketplace", 2},
		{"interops@clonal-plugins", 1},
	}
	for _, c := range cases {
		got, ok := entries[c.key]
		if !ok {
			t.Errorf("missing key %q in result", c.key)
			continue
		}
		if len(got) != c.wantNVer {
			t.Errorf("%s: expected %d versions, got %d", c.key, c.wantNVer, len(got))
		}
		for _, v := range got {
			if v.Marketplace == "" {
				t.Errorf("%s: entry missing Marketplace field: %+v", c.key, v)
			}
		}
	}
}

func TestListAllCacheEntries_OrphanMarker(t *testing.T) {
	root := makeCacheTree(t, [][3]string{
		{"interagency-marketplace", "intermux", "0.1.7"},
		{"interagency-marketplace", "intermux", "0.1.8"},
	})

	// Mark 0.1.7 as orphaned
	orphan := filepath.Join(root, "interagency-marketplace", "intermux", "0.1.7", ".orphaned_at")
	if err := os.WriteFile(orphan, []byte("2026-05-07"), 0644); err != nil {
		t.Fatalf("write orphan marker: %v", err)
	}

	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}

	got := entries["intermux@interagency-marketplace"]
	if len(got) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(got))
	}
	for _, v := range got {
		if v.Version == "0.1.7" && !v.Orphaned {
			t.Errorf("0.1.7 should be marked orphaned")
		}
		if v.Version == "0.1.8" && v.Orphaned {
			t.Errorf("0.1.8 should NOT be marked orphaned")
		}
	}
}

func TestListAllCacheEntries_EmptyRoot(t *testing.T) {
	root := t.TempDir()
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("listAllCacheEntriesIn: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty result, got %d entries", len(entries))
	}
}

func TestListAllCacheEntries_NonexistentRoot(t *testing.T) {
	// First-run case: no cache dir yet. Returning an empty result without error
	// matches the existing ListCacheEntries behavior, so callers don't have to
	// special-case "fresh install."
	root := filepath.Join(t.TempDir(), "does-not-exist")
	entries, err := listAllCacheEntriesIn(root)
	if err != nil {
		t.Fatalf("expected nil error for missing root, got %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("expected empty result, got %d entries", len(entries))
	}
}

// keysOf is a tiny helper to make test failures readable when the count is wrong.
func keysOf(m map[string][]CacheEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
