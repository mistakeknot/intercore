package dispatch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// oldCompletedDispatch makes a terminal dispatch older than the prune threshold.
func oldCompletedDispatch(t *testing.T, s *Store) string {
	t.Helper()
	ctx := context.Background()
	id, err := s.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: "/tmp/m"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, id, StatusCompleted, UpdateFields{"exit_code": 0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE dispatches SET created_at = ? WHERE id = ?", time.Now().Unix()-7200, id); err != nil {
		t.Fatal(err)
	}
	return id
}

func insertSchedulerJob(t *testing.T, s *Store, jobID, status string, dispatchID any) {
	t.Helper()
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT INTO scheduler_jobs (id, status, spawn_opts, created_at, dispatch_id) VALUES (?, ?, '{}', 1, ?)`,
		jobID, status, dispatchID); err != nil {
		t.Fatalf("insert job: %v", err)
	}
}

// M1: finalize clears the job's dispatch_id, so only the attempt row refers to
// a finished dispatch. Pruning that dispatch must succeed: the attempt keeps
// linked_at and loses the dispatch_id.
func TestPruneDispatchLinkedOnlyFromAttempt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	did := oldCompletedDispatch(t, s)
	insertSchedulerJob(t, s, "job-m1", "completed", nil)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scheduler_job_attempts (job_id, attempt_no, claim_token, dispatch_id, linked_at, claimed_at)
		 VALUES ('job-m1', 1, 'tok-m1', ?, 42, 40)`, did); err != nil {
		t.Fatal(err)
	}

	n, err := s.Prune(ctx, time.Hour)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if n != 1 {
		t.Fatalf("pruned %d dispatches, want 1", n)
	}
	var linked int64
	var disp *string
	if err := s.db.QueryRowContext(ctx,
		`SELECT dispatch_id, linked_at FROM scheduler_job_attempts WHERE claim_token = 'tok-m1'`).Scan(&disp, &linked); err != nil {
		t.Fatal(err)
	}
	if disp != nil || linked != 42 {
		t.Fatalf("attempt dispatch_id=%v linked_at=%d, want NULL and 42", disp, linked)
	}
}

// M2: the legacy scheduler_jobs.dispatch_id foreign key has no ON DELETE
// action. A job that still points at an old terminal dispatch (an in-flight or
// quarantined row) blocks the prune. This is a documented risk, not a feature:
// the prune fails whole, and nothing is deleted.
func TestPruneBlockedByJobDispatchForeignKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	did := oldCompletedDispatch(t, s)
	insertSchedulerJob(t, s, "job-m2", "quarantined", did)

	_, err := s.Prune(ctx, time.Hour)
	if err == nil || !strings.Contains(strings.ToUpper(err.Error()), "FOREIGN KEY") {
		t.Fatalf("Prune err = %v, want a foreign key failure", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM dispatches WHERE id = ?`, did).Scan(&n); err != nil || n != 1 {
		t.Fatalf("dispatch rows = %d (%v), want 1 after the failed prune", n, err)
	}
}
