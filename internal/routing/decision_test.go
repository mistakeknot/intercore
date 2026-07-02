package routing

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func testDecisionStore(t *testing.T) *DecisionStore {
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
	return NewDecisionStore(d.SqlDB())
}

func TestRecord(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	id, err := store.Record(ctx, RecordDecisionOpts{
		ProjectDir:    "/home/user/project",
		Agent:         "interflux:review:fd-safety",
		Category:      "review",
		SelectedModel: "opus",
		RuleMatched:   "override",
		PolicyHash:    "abc123",
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id <= 0 {
		t.Errorf("Record returned id=%d, want > 0", id)
	}
}

func TestRecord_WithAllFields(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	id, err := store.Record(ctx, RecordDecisionOpts{
		DispatchID:    "disp-001",
		RunID:         "run-001",
		SessionID:     "sess-001",
		BeadID:        "iv-abc",
		ProjectDir:    "/home/user/project",
		Phase:         "executing",
		Agent:         "interflux:review:fd-safety",
		Category:      "review",
		SelectedModel: "sonnet",
		RuleMatched:   "phase_category",
		FloorApplied:  true,
		FloorFrom:     "haiku",
		FloorTo:       "sonnet",
		Candidates:    `["haiku","sonnet","opus"]`,
		Excluded:      `[{"model":"haiku","reason":"floor"}]`,
		PolicyHash:    "abc123",
		OverrideID:    "override-001",
		Complexity:    3,
		ContextJSON:   `{"sprint":true}`,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	// Verify via Get
	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Agent != "interflux:review:fd-safety" {
		t.Errorf("Agent = %q, want %q", d.Agent, "interflux:review:fd-safety")
	}
	if d.SelectedModel != "sonnet" {
		t.Errorf("SelectedModel = %q, want %q", d.SelectedModel, "sonnet")
	}
	if d.RuleMatched != "phase_category" {
		t.Errorf("RuleMatched = %q, want %q", d.RuleMatched, "phase_category")
	}
	if !d.FloorApplied {
		t.Error("FloorApplied = false, want true")
	}
	if d.FloorFrom == nil || *d.FloorFrom != "haiku" {
		t.Errorf("FloorFrom = %v, want haiku", d.FloorFrom)
	}
	if d.FloorTo == nil || *d.FloorTo != "sonnet" {
		t.Errorf("FloorTo = %v, want sonnet", d.FloorTo)
	}
	if d.DispatchID == nil || *d.DispatchID != "disp-001" {
		t.Errorf("DispatchID = %v, want disp-001", d.DispatchID)
	}
	if d.Complexity == nil || *d.Complexity != 3 {
		t.Errorf("Complexity = %v, want 3", d.Complexity)
	}
}

func TestGet_NotFound(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	_, err := store.Get(ctx, 999)
	if err == nil {
		t.Fatal("Get(999): expected error, got nil")
	}
}

func TestList_Empty(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	decisions, err := store.List(ctx, ListDecisionOpts{ProjectDir: "/nonexistent"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(decisions) != 0 {
		t.Errorf("List returned %d decisions, want 0", len(decisions))
	}
}

func TestList_ByAgent(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	// Insert two decisions with different agents
	store.Record(ctx, RecordDecisionOpts{
		ProjectDir: "/project", Agent: "fd-safety", SelectedModel: "opus", RuleMatched: "override",
	})
	store.Record(ctx, RecordDecisionOpts{
		ProjectDir: "/project", Agent: "fd-architecture", SelectedModel: "sonnet", RuleMatched: "default_model",
	})

	decisions, err := store.List(ctx, ListDecisionOpts{Agent: "fd-safety"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("List returned %d decisions, want 1", len(decisions))
	}
	if decisions[0].Agent != "fd-safety" {
		t.Errorf("Agent = %q, want fd-safety", decisions[0].Agent)
	}
}

func TestList_ByModel(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordDecisionOpts{
		ProjectDir: "/project", Agent: "a1", SelectedModel: "opus", RuleMatched: "override",
	})
	store.Record(ctx, RecordDecisionOpts{
		ProjectDir: "/project", Agent: "a2", SelectedModel: "haiku", RuleMatched: "default_model",
	})
	store.Record(ctx, RecordDecisionOpts{
		ProjectDir: "/project", Agent: "a3", SelectedModel: "opus", RuleMatched: "fallback",
	})

	decisions, err := store.List(ctx, ListDecisionOpts{Model: "opus"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(decisions) != 2 {
		t.Errorf("List(model=opus) returned %d, want 2", len(decisions))
	}
}

func TestList_Limit(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		store.Record(ctx, RecordDecisionOpts{
			ProjectDir: "/project", Agent: "agent", SelectedModel: "sonnet", RuleMatched: "default_model",
		})
	}

	decisions, err := store.List(ctx, ListDecisionOpts{Limit: 3})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(decisions) != 3 {
		t.Errorf("List(limit=3) returned %d, want 3", len(decisions))
	}
}

func TestList_ByDispatchID(t *testing.T) {
	store := testDecisionStore(t)
	ctx := context.Background()

	store.Record(ctx, RecordDecisionOpts{
		DispatchID: "disp-001", ProjectDir: "/project", Agent: "a1", SelectedModel: "opus", RuleMatched: "override",
	})
	store.Record(ctx, RecordDecisionOpts{
		DispatchID: "disp-002", ProjectDir: "/project", Agent: "a2", SelectedModel: "sonnet", RuleMatched: "default_model",
	})

	decisions, err := store.List(ctx, ListDecisionOpts{DispatchID: "disp-001"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(decisions) != 1 {
		t.Fatalf("List(dispatch=disp-001) returned %d, want 1", len(decisions))
	}
	if decisions[0].DispatchID == nil || *decisions[0].DispatchID != "disp-001" {
		t.Errorf("DispatchID = %v, want disp-001", decisions[0].DispatchID)
	}
}
