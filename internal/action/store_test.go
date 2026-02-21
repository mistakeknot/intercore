package action

import (
	"context"
	"database/sql"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			project_dir TEXT NOT NULL DEFAULT '.',
			goal TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active',
			phase TEXT NOT NULL DEFAULT 'brainstorm',
			complexity INTEGER NOT NULL DEFAULT 3,
			force_full INTEGER NOT NULL DEFAULT 0,
			auto_advance INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE run_artifacts (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(id),
			phase TEXT NOT NULL,
			path TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'file',
			status TEXT NOT NULL DEFAULT 'active',
			created_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE phase_actions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES runs(id),
			phase TEXT NOT NULL,
			action_type TEXT NOT NULL DEFAULT 'command',
			command TEXT NOT NULL,
			args TEXT,
			mode TEXT NOT NULL DEFAULT 'interactive',
			priority INTEGER NOT NULL DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT 0,
			updated_at INTEGER NOT NULL DEFAULT 0,
			UNIQUE(run_id, phase, command)
		);
		INSERT INTO runs (id) VALUES ('test-run-1');
	`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestAddAndList(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	id1, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/interflux:flux-drive", Args: strPtr(`["${artifact:plan}"]`), Mode: "interactive"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 == 0 {
		t.Fatal("expected non-zero ID")
	}

	_, err = s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/clavain:interpeer", Mode: "both", Priority: 1})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhase(ctx, "test-run-1", "planned")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
	if actions[0].Command != "/interflux:flux-drive" {
		t.Errorf("expected first action to be flux-drive, got %s", actions[0].Command)
	}
}

func TestAddDuplicate(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	_, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/clavain:work"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/clavain:work"})
	if err == nil {
		t.Fatal("expected duplicate error")
	}
	if err != ErrDuplicate {
		t.Fatalf("expected ErrDuplicate, got %v", err)
	}
}

func TestUpdate(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	_, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/interflux:flux-drive", Args: strPtr(`["old.md"]`)})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Update(ctx, "test-run-1", "planned", "/interflux:flux-drive", &ActionUpdate{Args: strPtr(`["new.md"]`)})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhase(ctx, "test-run-1", "planned")
	if err != nil {
		t.Fatal(err)
	}
	if actions[0].Args == nil || *actions[0].Args != `["new.md"]` {
		t.Errorf("expected updated args, got %v", actions[0].Args)
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	_, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/clavain:work"})
	if err != nil {
		t.Fatal(err)
	}

	err = s.Delete(ctx, "test-run-1", "planned", "/clavain:work")
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhase(ctx, "test-run-1", "planned")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions after delete, got %d", len(actions))
	}
}

func TestDeleteNotFound(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	err := s.Delete(ctx, "test-run-1", "planned", "/nonexistent")
	if err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListAll(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/clavain:work"})
	s.Add(ctx, &Action{RunID: "test-run-1", Phase: "executing", Command: "/clavain:quality-gates"})

	actions, err := s.ListAll(ctx, "test-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
}

func TestAddBatch(t *testing.T) {
	db := setupTestDB(t)
	s := New(db)
	ctx := context.Background()

	batch := map[string]*Action{
		"planned":   {Command: "/interflux:flux-drive", Args: strPtr(`["${artifact:plan}"]`), Mode: "interactive"},
		"executing": {Command: "/clavain:quality-gates", Mode: "interactive"},
	}

	err := s.AddBatch(ctx, "test-run-1", batch)
	if err != nil {
		t.Fatal(err)
	}

	all, err := s.ListAll(ctx, "test-run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2 actions from batch, got %d", len(all))
	}
}

func TestResolveTemplateVars(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO run_artifacts (id, run_id, phase, path, type, status) VALUES ('art1', 'test-run-1', 'planned', 'docs/plans/my-plan.md', 'plan', 'active')`)
	if err != nil {
		t.Fatal(err)
	}

	s := New(db)
	_, err = s.Add(ctx, &Action{RunID: "test-run-1", Phase: "plan-reviewed", Command: "/clavain:work", Args: strPtr(`["${artifact:plan}","${run_id}"]`)})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhaseResolved(ctx, "test-run-1", "plan-reviewed", "/test/project")
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Args == nil || *actions[0].Args != `["docs/plans/my-plan.md","test-run-1"]` {
		t.Errorf("expected resolved args, got %v", *actions[0].Args)
	}
}

func TestResolveUnresolvable(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	s := New(db)
	_, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/test", Args: strPtr(`["${artifact:missing}"]`)})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhaseResolved(ctx, "test-run-1", "planned", ".")
	if err != nil {
		t.Fatal(err)
	}
	// Unresolvable placeholder should be left as-is
	if actions[0].Args == nil || *actions[0].Args != `["${artifact:missing}"]` {
		t.Errorf("expected unresolved placeholder, got %v", *actions[0].Args)
	}
}

func TestResolveProjectDir(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	s := New(db)
	_, err := s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/test", Args: strPtr(`["${project_dir}/file.md"]`)})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhaseResolved(ctx, "test-run-1", "planned", "/my/project")
	if err != nil {
		t.Fatal(err)
	}
	if actions[0].Args == nil || *actions[0].Args != `["/my/project/file.md"]` {
		t.Errorf("expected resolved project_dir, got %v", *actions[0].Args)
	}
}

func TestResolveJSONEscaping(t *testing.T) {
	db := setupTestDB(t)
	ctx := context.Background()

	// Insert an artifact with a path containing a double-quote
	_, err := db.Exec(`INSERT INTO run_artifacts (id, run_id, phase, path, type, status) VALUES ('art-esc', 'test-run-1', 'planned', 'docs/plan "v2".md', 'plan', 'active')`)
	if err != nil {
		t.Fatal(err)
	}

	s := New(db)
	_, err = s.Add(ctx, &Action{RunID: "test-run-1", Phase: "plan-reviewed", Command: "/test", Args: strPtr(`["${artifact:plan}"]`)})
	if err != nil {
		t.Fatal(err)
	}

	actions, err := s.ListForPhaseResolved(ctx, "test-run-1", "plan-reviewed", ".")
	if err != nil {
		t.Fatal(err)
	}
	// The path contains a double-quote; it must be JSON-escaped
	want := `["docs/plan \"v2\".md"]`
	if actions[0].Args == nil || *actions[0].Args != want {
		t.Errorf("expected JSON-escaped args %s, got %v", want, *actions[0].Args)
	}
}

func strPtr(s string) *string { return &s }
