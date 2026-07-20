package goal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func setupTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(filepath.Join(dir, "test.db"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(d.SqlDB())
}

func TestStore_CreateAndGet(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	g := &Goal{ProjectDir: "/tmp/test", Title: "Ship widget", ConditionText: "go test ./... exits 0", Complexity: 3}
	id, err := s.Create(ctx, g)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("ID length = %d, want 8", len(id))
	}
	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" || got.Title != "Ship widget" || got.FenceGen != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestStore_ListOpenByProject(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()
	_, _ = s.Create(ctx, &Goal{ProjectDir: "/a", Title: "one", ConditionText: "x exits 0"})
	_, _ = s.Create(ctx, &Goal{ProjectDir: "/b", Title: "two", ConditionText: "y exits 0"})
	got, err := s.List(ctx, "/a", "open")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 || got[0].Title != "one" {
		t.Errorf("List = %+v", got)
	}
}
