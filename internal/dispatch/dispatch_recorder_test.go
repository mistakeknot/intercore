package dispatch

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// The CLI wires the event recorder to the same single-connection pool the store
// uses (cmd/ic/recorders.go). UpdateStatus must hand its connection back before
// the recorder runs, or the recorder waits forever for that connection.
func TestUpdateStatus_RecorderSharingPoolDoesNotDeadlock(t *testing.T) {
	ctx := context.Background()
	handle, err := db.Open(filepath.Join(t.TempDir(), "test.db"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { handle.Close() })
	if err := handle.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool := handle.SqlDB()

	recorded := make(chan string, 1)
	store := New(pool, func(id, runID, from, to string) {
		var status string
		if err := pool.QueryRowContext(ctx, "SELECT status FROM dispatches WHERE id = ?", id).Scan(&status); err != nil {
			recorded <- "read failed: " + err.Error()
			return
		}
		recorded <- status
	})

	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- store.UpdateStatus(ctx, id, StatusRunning, nil) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("UpdateStatus: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("UpdateStatus did not return: the recorder is blocked on the store's connection")
	}
	if got := <-recorded; got != StatusRunning {
		t.Fatalf("recorder observed %q, want %q committed before it ran", got, StatusRunning)
	}
}
