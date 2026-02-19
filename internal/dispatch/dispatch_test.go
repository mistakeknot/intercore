package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(d.SqlDB(), nil)
}

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func TestCreateAndGet(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	d := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test-project",
		PromptFile: strPtr("/tmp/prompt.md"),
		Name:       strPtr("test-agent"),
		Model:      strPtr("o3-pro"),
		Sandbox:    strPtr("workspace-write"),
	}

	id, err := store.Create(ctx, d)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("ID length = %d, want 8", len(id))
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	if got.ID != id {
		t.Errorf("ID = %q, want %q", got.ID, id)
	}
	if got.AgentType != "codex" {
		t.Errorf("AgentType = %q, want %q", got.AgentType, "codex")
	}
	if got.Status != StatusSpawned {
		t.Errorf("Status = %q, want %q", got.Status, StatusSpawned)
	}
	if got.ProjectDir != "/tmp/test-project" {
		t.Errorf("ProjectDir = %q, want %q", got.ProjectDir, "/tmp/test-project")
	}
	if got.Name == nil || *got.Name != "test-agent" {
		t.Errorf("Name = %v, want %q", got.Name, "test-agent")
	}
	if got.Model == nil || *got.Model != "o3-pro" {
		t.Errorf("Model = %v, want %q", got.Model, "o3-pro")
	}
	if got.Turns != 0 {
		t.Errorf("Turns = %d, want 0", got.Turns)
	}
	if got.CreatedAt == 0 {
		t.Error("CreatedAt should be set")
	}
}

func TestGetNotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexist")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestUpdateStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	d := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
	}
	id, err := store.Create(ctx, d)
	if err != nil {
		t.Fatal(err)
	}

	// Transition spawned → running with PID
	now := time.Now().Unix()
	err = store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
		"pid":        12345,
		"started_at": now,
	})
	if err != nil {
		t.Fatalf("UpdateStatus to running: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if got.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", got.Status, StatusRunning)
	}
	if got.PID == nil || *got.PID != 12345 {
		t.Errorf("PID = %v, want 12345", got.PID)
	}

	// Transition running → completed with results
	err = store.UpdateStatus(ctx, id, StatusCompleted, UpdateFields{
		"exit_code":      0,
		"completed_at":   time.Now().Unix(),
		"verdict_status":  "pass",
		"verdict_summary": "All checks passed",
		"turns":           5,
		"commands":        3,
		"messages":        8,
		"input_tokens":    1000,
		"output_tokens":   2000,
	})
	if err != nil {
		t.Fatalf("UpdateStatus to completed: %v", err)
	}

	got, _ = store.Get(ctx, id)
	if got.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", got.Status, StatusCompleted)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", got.ExitCode)
	}
	if got.Turns != 5 {
		t.Errorf("Turns = %d, want 5", got.Turns)
	}
	if got.VerdictStatus == nil || *got.VerdictStatus != "pass" {
		t.Errorf("VerdictStatus = %v, want %q", got.VerdictStatus, "pass")
	}
}

func TestUpdateStatusNotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	err := store.UpdateStatus(ctx, "nonexist", StatusRunning, nil)
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestListActive(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create 3 dispatches: 1 spawned, 1 running, 1 completed
	id1, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/a"})
	id2, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/b"})
	id3, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/c"})

	store.UpdateStatus(ctx, id2, StatusRunning, UpdateFields{"pid": 100})
	store.UpdateStatus(ctx, id3, StatusCompleted, UpdateFields{"exit_code": 0})

	active, err := store.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(active) != 2 {
		t.Fatalf("ListActive count = %d, want 2", len(active))
	}

	// Verify the completed one is not in the list
	for _, d := range active {
		if d.ID == id3 {
			t.Error("completed dispatch should not be in active list")
		}
	}
	_ = id1 // used above
}

func TestListWithScope(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	scope := "review-123"
	store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/a", ScopeID: &scope})
	store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/b", ScopeID: &scope})
	store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/c"}) // no scope

	scoped, err := store.List(ctx, &scope)
	if err != nil {
		t.Fatalf("List with scope: %v", err)
	}
	if len(scoped) != 2 {
		t.Errorf("scoped count = %d, want 2", len(scoped))
	}

	all, err := store.List(ctx, nil)
	if err != nil {
		t.Fatalf("List all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("all count = %d, want 3", len(all))
	}
}

func TestPrune(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create an old completed dispatch
	id1, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/a"})
	store.UpdateStatus(ctx, id1, StatusCompleted, UpdateFields{"exit_code": 0})

	// Backdate it to 2 hours ago
	_, err := store.db.ExecContext(ctx, "UPDATE dispatches SET created_at = ? WHERE id = ?",
		time.Now().Unix()-7200, id1)
	if err != nil {
		t.Fatal(err)
	}

	// Create a recent one
	id2, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/b"})
	_ = id2

	// Prune older than 1 hour
	count, err := store.Prune(ctx, 1*time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if count != 1 {
		t.Errorf("Prune count = %d, want 1", count)
	}

	// The recent one should still be there
	all, _ := store.List(ctx, nil)
	if len(all) != 1 {
		t.Errorf("remaining count = %d, want 1", len(all))
	}
}

func TestPruneSkipsActive(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create an old running dispatch
	id1, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/a"})
	store.UpdateStatus(ctx, id1, StatusRunning, UpdateFields{"pid": 100})

	// Backdate it
	store.db.ExecContext(ctx, "UPDATE dispatches SET created_at = ? WHERE id = ?",
		time.Now().Unix()-7200, id1)

	// Prune should NOT delete active dispatches
	count, _ := store.Prune(ctx, 1*time.Hour)
	if count != 0 {
		t.Errorf("Prune should not delete active dispatches, deleted %d", count)
	}
}

func TestIsTerminal(t *testing.T) {
	tests := []struct {
		status   string
		terminal bool
	}{
		{StatusSpawned, false},
		{StatusRunning, false},
		{StatusCompleted, true},
		{StatusFailed, true},
		{StatusTimeout, true},
		{StatusCancelled, true},
	}
	for _, tt := range tests {
		d := &Dispatch{Status: tt.status}
		if got := d.IsTerminal(); got != tt.terminal {
			t.Errorf("IsTerminal(%q) = %v, want %v", tt.status, got, tt.terminal)
		}
	}
}

func TestCacheHitsViaUpdateStatus(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
	})
	if err != nil {
		t.Fatal(err)
	}

	err = store.UpdateStatus(ctx, id, StatusCompleted, UpdateFields{
		"input_tokens":  1000,
		"output_tokens": 500,
		"cache_hits":    3000,
		"exit_code":     0,
	})
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CacheHits == nil || *got.CacheHits != 3000 {
		t.Errorf("CacheHits = %v, want 3000", got.CacheHits)
	}
	if got.InputTokens != 1000 {
		t.Errorf("InputTokens = %d, want 1000", got.InputTokens)
	}
	if got.OutputTokens != 500 {
		t.Errorf("OutputTokens = %d, want 500", got.OutputTokens)
	}
}

func TestUpdateTokens(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.Create(ctx, &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
	})
	if err != nil {
		t.Fatal(err)
	}

	// UpdateTokens doesn't change status
	err = store.UpdateTokens(ctx, id, UpdateFields{
		"input_tokens":  2000,
		"output_tokens": 800,
		"cache_hits":    5000,
	})
	if err != nil {
		t.Fatalf("UpdateTokens: %v", err)
	}

	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != StatusSpawned {
		t.Errorf("Status = %q, want %q (should not change)", got.Status, StatusSpawned)
	}
	if got.InputTokens != 2000 {
		t.Errorf("InputTokens = %d, want 2000", got.InputTokens)
	}
	if got.CacheHits == nil || *got.CacheHits != 5000 {
		t.Errorf("CacheHits = %v, want 5000", got.CacheHits)
	}
}

func TestUpdateTokensNotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	err := store.UpdateTokens(ctx, "nonexist", UpdateFields{"input_tokens": 100})
	if err != ErrNotFound {
		t.Errorf("UpdateTokens(nonexist) error = %v, want ErrNotFound", err)
	}
}

func TestGenerateID(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 100; i++ {
		id, err := generateID()
		if err != nil {
			t.Fatal(err)
		}
		if len(id) != 8 {
			t.Errorf("ID length = %d, want 8", len(id))
		}
		if seen[id] {
			t.Errorf("duplicate ID: %s", id)
		}
		seen[id] = true
	}
}
