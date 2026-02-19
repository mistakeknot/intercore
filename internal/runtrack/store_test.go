package runtrack

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
)

func setupTestStore(t *testing.T) (*Store, *db.DB) {
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

	return New(d.SqlDB()), d
}

// createHelperRun inserts a run row so FK references work.
func createHelperRun(t *testing.T, d *db.DB, id string) {
	t.Helper()
	_, err := d.SqlDB().Exec(`
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity,
			force_full, auto_advance, created_at, updated_at)
		VALUES (?, '/tmp/test', 'test goal', 'active', 'brainstorm', 3, 0, 1, ?, ?)`,
		id, time.Now().Unix(), time.Now().Unix(),
	)
	if err != nil {
		t.Fatalf("createHelperRun(%s): %v", id, err)
	}
}

func TestStore_AddAgent(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	name := "brainstorm-agent"
	agent := &Agent{
		RunID:     "testrun1",
		AgentType: "claude",
		Name:      &name,
	}

	id, err := store.AddAgent(ctx, agent)
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("ID length = %d, want 8", len(id))
	}

	// Verify via GetAgent
	got, err := store.GetAgent(ctx, id)
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.RunID != "testrun1" {
		t.Errorf("RunID = %q, want %q", got.RunID, "testrun1")
	}
	if got.AgentType != "claude" {
		t.Errorf("AgentType = %q, want %q", got.AgentType, "claude")
	}
	if got.Name == nil || *got.Name != "brainstorm-agent" {
		t.Errorf("Name = %v, want %q", got.Name, "brainstorm-agent")
	}
	if got.Status != StatusActive {
		t.Errorf("Status = %q, want %q", got.Status, StatusActive)
	}
	if got.DispatchID != nil {
		t.Errorf("DispatchID = %v, want nil", got.DispatchID)
	}
}

func TestStore_AddAgent_BadRunID(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	// No run with ID "nonexist" — FK should reject
	agent := &Agent{
		RunID:     "nonexist",
		AgentType: "claude",
	}

	_, err := store.AddAgent(ctx, agent)
	if err != ErrRunNotFound {
		t.Errorf("AddAgent(nonexist) error = %v, want ErrRunNotFound", err)
	}
}

func TestStore_UpdateAgent(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	id, _ := store.AddAgent(ctx, &Agent{
		RunID:     "testrun1",
		AgentType: "claude",
	})

	if err := store.UpdateAgent(ctx, id, StatusCompleted); err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}

	got, _ := store.GetAgent(ctx, id)
	if got.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", got.Status, StatusCompleted)
	}
	if got.IsTerminal() != true {
		t.Error("IsTerminal() = false, want true for completed agent")
	}
}

func TestStore_UpdateAgent_NotFound(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	err := store.UpdateAgent(ctx, "nonexist", StatusCompleted)
	if err != ErrAgentNotFound {
		t.Errorf("UpdateAgent(nonexist) error = %v, want ErrAgentNotFound", err)
	}
}

func TestStore_GetAgent_NotFound(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	_, err := store.GetAgent(ctx, "nonexist")
	if err != ErrAgentNotFound {
		t.Errorf("GetAgent(nonexist) error = %v, want ErrAgentNotFound", err)
	}
}

func TestStore_ListAgents_Empty(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	agents, err := store.ListAgents(ctx, "testrun1")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if agents != nil && len(agents) != 0 {
		t.Errorf("ListAgents for new run should be empty, got %d", len(agents))
	}
}

func TestStore_ListAgents_Multiple(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")
	createHelperRun(t, d, "testrun2")

	// Add 2 agents to run1, 1 to run2
	store.AddAgent(ctx, &Agent{RunID: "testrun1", AgentType: "claude"})
	store.AddAgent(ctx, &Agent{RunID: "testrun1", AgentType: "codex"})
	store.AddAgent(ctx, &Agent{RunID: "testrun2", AgentType: "claude"})

	agents, err := store.ListAgents(ctx, "testrun1")
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Errorf("ListAgents(testrun1) count = %d, want 2", len(agents))
	}

	// Verify ordering: first created should come first
	if agents[0].AgentType != "claude" {
		t.Errorf("agents[0].AgentType = %q, want %q", agents[0].AgentType, "claude")
	}
	if agents[1].AgentType != "codex" {
		t.Errorf("agents[1].AgentType = %q, want %q", agents[1].AgentType, "codex")
	}
}

func TestStore_AddArtifact(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	artifact := &Artifact{
		RunID: "testrun1",
		Phase: "brainstorm",
		Path:  "docs/brainstorms/test.md",
		Type:  "file",
	}

	id, err := store.AddArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}
	if len(id) != 8 {
		t.Errorf("ID length = %d, want 8", len(id))
	}
}

func TestStore_AddArtifact_BadRunID(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	artifact := &Artifact{
		RunID: "nonexist",
		Phase: "brainstorm",
		Path:  "some/path.md",
		Type:  "file",
	}

	_, err := store.AddArtifact(ctx, artifact)
	if err != ErrRunNotFound {
		t.Errorf("AddArtifact(nonexist) error = %v, want ErrRunNotFound", err)
	}
}

func TestStore_ListArtifacts_All(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "brainstorm", Path: "a.md", Type: "file"})
	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "planned", Path: "b.md", Type: "file"})
	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "planned", Path: "c.md", Type: "file"})

	artifacts, err := store.ListArtifacts(ctx, "testrun1", nil)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 3 {
		t.Errorf("ListArtifacts count = %d, want 3", len(artifacts))
	}
}

func TestStore_ListArtifacts_ByPhase(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "brainstorm", Path: "a.md", Type: "file"})
	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "planned", Path: "b.md", Type: "file"})
	store.AddArtifact(ctx, &Artifact{RunID: "testrun1", Phase: "planned", Path: "c.md", Type: "file"})

	phase := "planned"
	artifacts, err := store.ListArtifacts(ctx, "testrun1", &phase)
	if err != nil {
		t.Fatalf("ListArtifacts(planned): %v", err)
	}
	if len(artifacts) != 2 {
		t.Errorf("ListArtifacts(planned) count = %d, want 2", len(artifacts))
	}

	// Verify all are planned phase
	for i, a := range artifacts {
		if a.Phase != "planned" {
			t.Errorf("artifacts[%d].Phase = %q, want %q", i, a.Phase, "planned")
		}
	}
}

func TestStore_ListArtifacts_Empty(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	artifacts, err := store.ListArtifacts(ctx, "testrun1", nil)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if artifacts != nil && len(artifacts) != 0 {
		t.Errorf("ListArtifacts for new run should be empty, got %d", len(artifacts))
	}
}

func TestStore_AgentWithDispatchID(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	dispatchID := "dispatch123"
	agent := &Agent{
		RunID:      "testrun1",
		AgentType:  "codex",
		DispatchID: &dispatchID,
	}

	id, err := store.AddAgent(ctx, agent)
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	got, _ := store.GetAgent(ctx, id)
	if got.DispatchID == nil || *got.DispatchID != "dispatch123" {
		t.Errorf("DispatchID = %v, want %q", got.DispatchID, "dispatch123")
	}
}

func TestStore_UpdateAgentDispatch(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	id, err := store.AddAgent(ctx, &Agent{
		RunID:     "testrun1",
		AgentType: "codex",
	})
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// Initially no dispatch ID
	got, _ := store.GetAgent(ctx, id)
	if got.DispatchID != nil {
		t.Errorf("DispatchID = %v, want nil", got.DispatchID)
	}

	// Set dispatch ID
	if err := store.UpdateAgentDispatch(ctx, id, "dispatch-abc"); err != nil {
		t.Fatalf("UpdateAgentDispatch: %v", err)
	}

	got, _ = store.GetAgent(ctx, id)
	if got.DispatchID == nil || *got.DispatchID != "dispatch-abc" {
		t.Errorf("DispatchID = %v, want %q", got.DispatchID, "dispatch-abc")
	}
}

func TestStore_UpdateAgentDispatch_NotFound(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	err := store.UpdateAgentDispatch(ctx, "nonexist", "dispatch-abc")
	if err != ErrAgentNotFound {
		t.Errorf("UpdateAgentDispatch(nonexist) error = %v, want ErrAgentNotFound", err)
	}
}

func TestStore_UpdateAgentDispatch_Conflict(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	id, err := store.AddAgent(ctx, &Agent{
		RunID:     "testrun1",
		AgentType: "codex",
	})
	if err != nil {
		t.Fatalf("AddAgent: %v", err)
	}

	// First set succeeds
	if err := store.UpdateAgentDispatch(ctx, id, "dispatch-first"); err != nil {
		t.Fatalf("first UpdateAgentDispatch: %v", err)
	}

	// Second set conflicts (CAS: dispatch_id IS NULL check fails)
	err = store.UpdateAgentDispatch(ctx, id, "dispatch-second")
	if err != ErrDispatchIDConflict {
		t.Errorf("second UpdateAgentDispatch error = %v, want ErrDispatchIDConflict", err)
	}

	// Verify original value preserved
	got, _ := store.GetAgent(ctx, id)
	if got.DispatchID == nil || *got.DispatchID != "dispatch-first" {
		t.Errorf("DispatchID = %v, want %q", got.DispatchID, "dispatch-first")
	}
}

func TestStore_AgentFailedStatus(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	id, _ := store.AddAgent(ctx, &Agent{
		RunID:     "testrun1",
		AgentType: "claude",
	})

	if err := store.UpdateAgent(ctx, id, StatusFailed); err != nil {
		t.Fatalf("UpdateAgent(failed): %v", err)
	}

	got, _ := store.GetAgent(ctx, id)
	if got.Status != StatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, StatusFailed)
	}
	if !got.IsTerminal() {
		t.Error("IsTerminal() = false, want true for failed agent")
	}
}

func TestStore_ArtifactWithHash(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	// Create a temp file so the hash gets computed
	tmpFile := filepath.Join(t.TempDir(), "plan.md")
	if err := os.WriteFile(tmpFile, []byte("# My Plan\n"), 0644); err != nil {
		t.Fatal(err)
	}

	artifact := &Artifact{
		RunID: "testrun1",
		Phase: "planned",
		Path:  tmpFile,
		Type:  "file",
	}

	_, err := store.AddArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	// Retrieve and verify hash was computed
	artifacts, err := store.ListArtifacts(ctx, "testrun1", nil)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	if artifacts[0].ContentHash == nil {
		t.Fatal("expected content_hash to be set")
	}
	if !strings.HasPrefix(*artifacts[0].ContentHash, "sha256:") {
		t.Errorf("content_hash = %q, want sha256: prefix", *artifacts[0].ContentHash)
	}
}

func TestStore_ArtifactWithDispatchID(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	dispatch := "dispatch-abc"
	artifact := &Artifact{
		RunID:      "testrun1",
		Phase:      "executing",
		Path:       "nonexistent/path.md",
		Type:       "file",
		DispatchID: &dispatch,
	}

	_, err := store.AddArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	artifacts, err := store.ListArtifacts(ctx, "testrun1", nil)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	if artifacts[0].DispatchID == nil || *artifacts[0].DispatchID != "dispatch-abc" {
		t.Errorf("dispatch_id = %v, want %q", artifacts[0].DispatchID, "dispatch-abc")
	}
	// Path doesn't exist, so hash should be nil
	if artifacts[0].ContentHash != nil {
		t.Errorf("content_hash = %v, want nil (file doesn't exist)", *artifacts[0].ContentHash)
	}
}

func TestStore_ArtifactExplicitHash(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()
	createHelperRun(t, d, "testrun1")

	// Explicit hash overrides auto-computation
	hash := "sha256:deadbeef"
	artifact := &Artifact{
		RunID:       "testrun1",
		Phase:       "brainstorm",
		Path:        "nonexistent/path.md",
		Type:        "file",
		ContentHash: &hash,
	}

	_, err := store.AddArtifact(ctx, artifact)
	if err != nil {
		t.Fatalf("AddArtifact: %v", err)
	}

	artifacts, err := store.ListArtifacts(ctx, "testrun1", nil)
	if err != nil {
		t.Fatalf("ListArtifacts: %v", err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	if artifacts[0].ContentHash == nil || *artifacts[0].ContentHash != "sha256:deadbeef" {
		t.Errorf("content_hash = %v, want %q", artifacts[0].ContentHash, "sha256:deadbeef")
	}
}
