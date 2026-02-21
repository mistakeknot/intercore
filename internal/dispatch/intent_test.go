package dispatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func testIntentStore(t *testing.T) (*IntentStore, *Store) {
	t.Helper()
	store := testStore(t)
	return NewIntentStore(store.db), store
}

func TestRecordIntent(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	id, err := is.RecordIntent(ctx, "dispatch-1", "run-1", "abc123", "hash456")
	if err != nil {
		t.Fatalf("RecordIntent: %v", err)
	}
	if id <= 0 {
		t.Errorf("expected positive ID, got %d", id)
	}

	// Verify it's pending
	intent, err := is.GetByDispatch(ctx, "dispatch-1")
	if err != nil {
		t.Fatalf("GetByDispatch: %v", err)
	}
	if intent == nil {
		t.Fatal("expected intent, got nil")
	}
	if intent.Status != IntentStatusPending {
		t.Errorf("Status = %q, want %q", intent.Status, IntentStatusPending)
	}
	if intent.BaseCommit != "abc123" {
		t.Errorf("BaseCommit = %q, want %q", intent.BaseCommit, "abc123")
	}
	if intent.DispatchID != "dispatch-1" {
		t.Errorf("DispatchID = %q, want %q", intent.DispatchID, "dispatch-1")
	}
}

func TestCompleteIntent(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	id, _ := is.RecordIntent(ctx, "dispatch-1", "", "abc123", "")
	if err := is.CompleteIntent(ctx, id, "def789"); err != nil {
		t.Fatalf("CompleteIntent: %v", err)
	}

	intent, _ := is.GetByDispatch(ctx, "dispatch-1")
	if intent.Status != IntentStatusCompleted {
		t.Errorf("Status = %q, want %q", intent.Status, IntentStatusCompleted)
	}
	if intent.ResultCommit == nil || *intent.ResultCommit != "def789" {
		t.Errorf("ResultCommit = %v, want %q", intent.ResultCommit, "def789")
	}
	if intent.CompletedAt == nil {
		t.Error("CompletedAt should be set")
	}
}

func TestCompleteIntent_NotPending(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	id, _ := is.RecordIntent(ctx, "dispatch-1", "", "abc123", "")
	is.CompleteIntent(ctx, id, "def789") // complete it first

	// Try to complete again — should fail
	err := is.CompleteIntent(ctx, id, "ghi012")
	if err == nil {
		t.Error("expected error completing non-pending intent")
	}
}

func TestFailIntent(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	id, _ := is.RecordIntent(ctx, "dispatch-1", "", "abc123", "")
	if err := is.FailIntent(ctx, id, "merge conflict", []string{"file1.go", "file2.go"}); err != nil {
		t.Fatalf("FailIntent: %v", err)
	}

	intent, _ := is.GetByDispatch(ctx, "dispatch-1")
	if intent.Status != IntentStatusFailed {
		t.Errorf("Status = %q, want %q", intent.Status, IntentStatusFailed)
	}
	if intent.ErrorMessage == nil || *intent.ErrorMessage != "merge conflict" {
		t.Errorf("ErrorMessage = %v, want %q", intent.ErrorMessage, "merge conflict")
	}
	if intent.ConflictFiles == nil || *intent.ConflictFiles != "file1.go\nfile2.go" {
		t.Errorf("ConflictFiles = %v, want %q", intent.ConflictFiles, "file1.go\nfile2.go")
	}
}

func TestListPendingIntents(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	// Create 3 intents: 1 pending, 1 completed, 1 failed
	id1, _ := is.RecordIntent(ctx, "d1", "", "abc", "")
	id2, _ := is.RecordIntent(ctx, "d2", "", "def", "")
	id3, _ := is.RecordIntent(ctx, "d3", "", "ghi", "")

	is.CompleteIntent(ctx, id2, "xyz")
	is.FailIntent(ctx, id3, "error", nil)

	pending, err := is.ListPendingIntents(ctx)
	if err != nil {
		t.Fatalf("ListPendingIntents: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("expected 1 pending, got %d", len(pending))
	}
	if pending[0].ID != id1 {
		t.Errorf("pending[0].ID = %d, want %d", pending[0].ID, id1)
	}
}

func TestRecoverPendingIntents_HeadAdvanced(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	dir := initGitRepo(t)
	baseCommit, _ := gitHeadCommit(dir)

	// Record an intent with the base commit
	is.RecordIntent(ctx, "d1", "", baseCommit, "")

	// Advance HEAD
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package new\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmds := [][]string{
		{"git", "-C", dir, "add", "."},
		{"git", "-C", dir, "commit", "-m", "advance"},
	}
	for _, args := range cmds {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
	}

	recovered, failed, err := is.RecoverPendingIntents(ctx, dir)
	if err != nil {
		t.Fatalf("RecoverPendingIntents: %v", err)
	}
	if recovered != 1 {
		t.Errorf("recovered = %d, want 1", recovered)
	}
	if failed != 0 {
		t.Errorf("failed = %d, want 0", failed)
	}
}

func TestRecoverPendingIntents_HeadUnchanged(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	dir := initGitRepo(t)
	baseCommit, _ := gitHeadCommit(dir)

	// Record an intent — HEAD hasn't moved
	is.RecordIntent(ctx, "d1", "", baseCommit, "")

	recovered, failed, err := is.RecoverPendingIntents(ctx, dir)
	if err != nil {
		t.Fatalf("RecoverPendingIntents: %v", err)
	}
	if recovered != 0 {
		t.Errorf("recovered = %d, want 0", recovered)
	}
	if failed != 1 {
		t.Errorf("failed = %d, want 1", failed)
	}
}

func TestGetByDispatch_NotFound(t *testing.T) {
	is, _ := testIntentStore(t)
	ctx := context.Background()

	intent, err := is.GetByDispatch(ctx, "nonexistent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if intent != nil {
		t.Error("expected nil intent for nonexistent dispatch")
	}
}

func TestGitCommitExists(t *testing.T) {
	dir := initGitRepo(t)
	head, _ := gitHeadCommit(dir)

	if !gitCommitExists(dir, head) {
		t.Errorf("expected commit %s to exist", head)
	}
	if gitCommitExists(dir, "0000000000000000000000000000000000000000") {
		t.Error("expected nonexistent commit to return false")
	}
}
