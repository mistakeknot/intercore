package portfolio

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func tempDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	dsn := "file:" + path + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D100"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	// Create runs table (minimal) and project_deps
	_, err = db.Exec(`
		CREATE TABLE runs (id TEXT PRIMARY KEY, project_dir TEXT NOT NULL DEFAULT '', goal TEXT NOT NULL DEFAULT '');
		INSERT INTO runs (id, project_dir, goal) VALUES ('portfolio1', '', 'Test portfolio');
		CREATE TABLE project_deps (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			portfolio_run_id TEXT NOT NULL REFERENCES runs(id),
			upstream_project TEXT NOT NULL,
			downstream_project TEXT NOT NULL,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			UNIQUE(portfolio_run_id, upstream_project, downstream_project)
		);
	`)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func TestAddDep(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	if err := s.Add(ctx, "portfolio1", "/proj/a", "/proj/b"); err != nil {
		t.Fatalf("Add: %v", err)
	}

	deps, err := s.List(ctx, "portfolio1")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(deps) != 1 {
		t.Fatalf("expected 1 dep, got %d", len(deps))
	}
	if deps[0].UpstreamProject != "/proj/a" || deps[0].DownstreamProject != "/proj/b" {
		t.Errorf("dep = %q → %q, want /proj/a → /proj/b", deps[0].UpstreamProject, deps[0].DownstreamProject)
	}
}

func TestAddDep_SelfLoop(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	err := s.Add(ctx, "portfolio1", "/proj/a", "/proj/a")
	if err == nil {
		t.Fatal("expected error for self-loop dep")
	}
}

func TestDuplicateRejected(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	if err := s.Add(ctx, "portfolio1", "/proj/a", "/proj/b"); err != nil {
		t.Fatalf("Add: %v", err)
	}
	err := s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")
	if err == nil {
		t.Fatal("expected error for duplicate dep")
	}
}

func TestRemoveDep(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")

	if err := s.Remove(ctx, "portfolio1", "/proj/a", "/proj/b"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	deps, _ := s.List(ctx, "portfolio1")
	if len(deps) != 0 {
		t.Errorf("expected 0 deps after remove, got %d", len(deps))
	}
}

func TestRemoveDep_NotFound(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	err := s.Remove(ctx, "portfolio1", "/proj/x", "/proj/y")
	if err == nil {
		t.Fatal("expected error for non-existent dep")
	}
}

func TestGetDownstream(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")
	s.Add(ctx, "portfolio1", "/proj/a", "/proj/c")

	downstream, err := s.GetDownstream(ctx, "portfolio1", "/proj/a")
	if err != nil {
		t.Fatalf("GetDownstream: %v", err)
	}
	if len(downstream) != 2 {
		t.Fatalf("expected 2 downstream, got %d", len(downstream))
	}
	if downstream[0] != "/proj/b" || downstream[1] != "/proj/c" {
		t.Errorf("downstream = %v, want [/proj/b, /proj/c]", downstream)
	}
}

func TestGetUpstream(t *testing.T) {
	db := tempDB(t)
	s := NewDepStore(db)
	ctx := context.Background()

	s.Add(ctx, "portfolio1", "/proj/a", "/proj/c")
	s.Add(ctx, "portfolio1", "/proj/b", "/proj/c")

	upstream, err := s.GetUpstream(ctx, "portfolio1", "/proj/c")
	if err != nil {
		t.Fatalf("GetUpstream: %v", err)
	}
	if len(upstream) != 2 {
		t.Fatalf("expected 2 upstream, got %d", len(upstream))
	}
}

// Suppress unused import warning — time is used by the Dep struct in deps.go
var _ = time.Now
