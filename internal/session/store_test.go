package session

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
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
	return NewStore(d.SqlDB())
}

func TestStart(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id, err := store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/home/user/project",
		AgentType:  "claude-code",
		Model:      "opus-4",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if id <= 0 {
		t.Errorf("Start returned id=%d, want > 0", id)
	}

	// Verify the session exists
	sessions, err := store.List(ctx, ListOpts{SessionID: "sess-001"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentType != "claude-code" {
		t.Errorf("AgentType = %q, want %q", sessions[0].AgentType, "claude-code")
	}
	if sessions[0].Model == nil || *sessions[0].Model != "opus-4" {
		t.Errorf("Model = %v, want %q", sessions[0].Model, "opus-4")
	}
}

func TestStart_Idempotent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	id1, err := store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
		AgentType:  "claude-code",
		Model:      "opus-4",
	})
	if err != nil {
		t.Fatalf("Start 1: %v", err)
	}

	// Re-register with updated metadata
	id2, err := store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
		AgentType:  "codex",
		Model:      "o3-pro",
		Metadata:   `{"version":"2"}`,
	})
	if err != nil {
		t.Fatalf("Start 2: %v", err)
	}

	if id1 != id2 {
		t.Errorf("Idempotent start: id1=%d, id2=%d (should match)", id1, id2)
	}

	// Verify metadata was updated
	sessions, err := store.List(ctx, ListOpts{SessionID: "sess-001"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentType != "codex" {
		t.Errorf("AgentType = %q, want %q (should be updated)", sessions[0].AgentType, "codex")
	}
}

func TestStart_DefaultAgentType(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
	})

	sessions, _ := store.List(ctx, ListOpts{SessionID: "sess-001"})
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].AgentType != "claude-code" {
		t.Errorf("AgentType = %q, want %q", sessions[0].AgentType, "claude-code")
	}
}

func TestAttribute(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
	})

	id, err := store.Attribute(ctx, AttributeOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
		BeadID:     "iv-test1",
		Phase:      "brainstorm",
	})
	if err != nil {
		t.Fatalf("Attribute: %v", err)
	}
	if id <= 0 {
		t.Errorf("Attribute returned id=%d, want > 0", id)
	}
}

func TestAttribute_RequiresSessionID(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	_, err := store.Attribute(ctx, AttributeOpts{
		ProjectDir: "/project",
		BeadID:     "iv-test1",
	})
	if err == nil {
		t.Error("expected error for empty session_id")
	}
}

func TestCurrent(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
	})

	// First attribution
	store.Attribute(ctx, AttributeOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
		BeadID:     "iv-test1",
		Phase:      "brainstorm",
	})

	// Second attribution (should be current)
	store.Attribute(ctx, AttributeOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
		BeadID:     "iv-test1",
		RunID:      "run-abc",
		Phase:      "executing",
	})

	cur, err := store.Current(ctx, "sess-001", "/project")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur == nil {
		t.Fatal("Current returned nil")
	}
	if cur.BeadID == nil || *cur.BeadID != "iv-test1" {
		t.Errorf("BeadID = %v, want %q", cur.BeadID, "iv-test1")
	}
	if cur.RunID == nil || *cur.RunID != "run-abc" {
		t.Errorf("RunID = %v, want %q", cur.RunID, "run-abc")
	}
	if cur.Phase == nil || *cur.Phase != "executing" {
		t.Errorf("Phase = %v, want %q", cur.Phase, "executing")
	}
}

func TestCurrent_NoAttributions(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
	})

	cur, err := store.Current(ctx, "sess-001", "/project")
	if err != nil {
		t.Fatalf("Current: %v", err)
	}
	if cur != nil {
		t.Errorf("expected nil, got %+v", cur)
	}
}

func TestEnd(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{
		SessionID:  "sess-001",
		ProjectDir: "/project",
	})

	if err := store.End(ctx, "sess-001"); err != nil {
		t.Fatalf("End: %v", err)
	}

	// Should not appear in active-only list
	sessions, _ := store.List(ctx, ListOpts{ActiveOnly: true})
	if len(sessions) != 0 {
		t.Errorf("expected 0 active sessions, got %d", len(sessions))
	}

	// Should appear in full list
	sessions, _ = store.List(ctx, ListOpts{SessionID: "sess-001"})
	if len(sessions) != 1 {
		t.Fatalf("expected 1 session, got %d", len(sessions))
	}
	if sessions[0].EndedAt == nil {
		t.Error("expected EndedAt to be set")
	}
}

func TestEnd_NotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	err := store.End(ctx, "nonexistent")
	if err == nil {
		t.Error("expected error for non-existent session")
	}
}

func TestList_Filters(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	store.Start(ctx, StartOpts{SessionID: "s1", ProjectDir: "/p1"})
	store.Start(ctx, StartOpts{SessionID: "s2", ProjectDir: "/p1"})
	store.Start(ctx, StartOpts{SessionID: "s3", ProjectDir: "/p2"})

	// Filter by project
	sessions, _ := store.List(ctx, ListOpts{ProjectDir: "/p1"})
	if len(sessions) != 2 {
		t.Errorf("project filter: got %d, want 2", len(sessions))
	}

	// Filter by session_id
	sessions, _ = store.List(ctx, ListOpts{SessionID: "s1"})
	if len(sessions) != 1 {
		t.Errorf("session filter: got %d, want 1", len(sessions))
	}

	// Active only (end one session first)
	store.End(ctx, "s1")
	sessions, _ = store.List(ctx, ListOpts{ActiveOnly: true})
	if len(sessions) != 2 {
		t.Errorf("active-only filter: got %d, want 2", len(sessions))
	}

	// Since filter
	sessions, _ = store.List(ctx, ListOpts{Since: time.Now().Unix() + 3600})
	if len(sessions) != 0 {
		t.Errorf("since filter: got %d, want 0", len(sessions))
	}
}

func TestList_Limit(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		store.Start(ctx, StartOpts{
			SessionID:  fmt.Sprintf("s%d", i),
			ProjectDir: "/project",
		})
	}

	sessions, _ := store.List(ctx, ListOpts{Limit: 3})
	if len(sessions) != 3 {
		t.Errorf("limit: got %d, want 3", len(sessions))
	}
}
