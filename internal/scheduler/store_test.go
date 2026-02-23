package scheduler

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)

	// Create the scheduler_jobs table.
	_, err = db.Exec(`
		CREATE TABLE scheduler_jobs (
			id          TEXT PRIMARY KEY,
			status      TEXT NOT NULL DEFAULT 'pending',
			priority    INTEGER NOT NULL DEFAULT 2,
			agent_type  TEXT NOT NULL DEFAULT 'codex',
			session_name TEXT,
			batch_id    TEXT,
			dispatch_id TEXT,
			spawn_opts  TEXT NOT NULL,
			max_retries INTEGER NOT NULL DEFAULT 3,
			retry_count INTEGER NOT NULL DEFAULT 0,
			error_msg   TEXT,
			created_at  INTEGER NOT NULL,
			started_at  INTEGER,
			completed_at INTEGER
		);
		CREATE INDEX idx_scheduler_jobs_status ON scheduler_jobs(status);
		CREATE INDEX idx_scheduler_jobs_session ON scheduler_jobs(session_name);
	`)
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { db.Close() })
	return db
}

func TestStoreCreateAndGet(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	job := &SpawnJob{
		ID:          "test-1",
		Status:      StatusPending,
		Priority:    PriorityNormal,
		AgentType:   "codex",
		SessionName: "session-a",
		SpawnOpts:   `{"prompt_file":"test.md"}`,
		MaxRetries:  3,
		CreatedAt:   time.Now().Truncate(time.Second),
	}

	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}

	got, err := store.Get(ctx, "test-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ID != "test-1" {
		t.Errorf("id = %q, want test-1", got.ID)
	}
	if got.Status != StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.AgentType != "codex" {
		t.Errorf("agent_type = %q, want codex", got.AgentType)
	}
	if got.SessionName != "session-a" {
		t.Errorf("session_name = %q, want session-a", got.SessionName)
	}
	if got.SpawnOpts != `{"prompt_file":"test.md"}` {
		t.Errorf("spawn_opts = %q, want JSON", got.SpawnOpts)
	}
}

func TestStoreUpdate(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	job := &SpawnJob{
		ID:        "upd-1",
		Status:    StatusPending,
		Priority:  PriorityNormal,
		AgentType: "codex",
		SpawnOpts: "{}",
		MaxRetries: 3,
		CreatedAt: time.Now().Truncate(time.Second),
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("create: %v", err)
	}

	// Simulate job starting.
	job.Status = StatusRunning
	job.StartedAt = time.Now().Truncate(time.Second)
	job.DispatchID = "dispatch-abc"
	if err := store.Update(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := store.Get(ctx, "upd-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.DispatchID != "dispatch-abc" {
		t.Errorf("dispatch_id = %q, want dispatch-abc", got.DispatchID)
	}

	// Simulate completion.
	job.Status = StatusCompleted
	job.CompletedAt = time.Now().Truncate(time.Second)
	if err := store.Update(ctx, job); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err = store.Get(ctx, "upd-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != StatusCompleted {
		t.Errorf("status = %q, want completed", got.Status)
	}
}

func TestStoreList(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	for i, status := range []JobStatus{StatusPending, StatusPending, StatusCompleted} {
		job := &SpawnJob{
			ID:        fmt.Sprintf("list-%d", i),
			Status:    status,
			Priority:  PriorityNormal,
			AgentType: "codex",
			SpawnOpts: "{}",
			MaxRetries: 3,
			CreatedAt: time.Now().Truncate(time.Second),
		}
		if status == StatusCompleted {
			job.CompletedAt = time.Now().Truncate(time.Second)
		}
		if err := store.Create(ctx, job); err != nil {
			t.Fatalf("create: %v", err)
		}
	}

	// List all.
	all, err := store.List(ctx, "", 100)
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("list all = %d, want 3", len(all))
	}

	// List pending only.
	pending, err := store.List(ctx, "pending", 100)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	if len(pending) != 2 {
		t.Errorf("list pending = %d, want 2", len(pending))
	}
}

func TestStoreRecoverPending(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// Create jobs in various states.
	states := []struct {
		id     string
		status JobStatus
	}{
		{"r-1", StatusPending},
		{"r-2", StatusRunning},   // Was interrupted — should be recovered as pending.
		{"r-3", StatusCompleted}, // Done — should not be recovered.
		{"r-4", StatusRetrying},  // Was retrying — should be recovered.
	}

	for _, s := range states {
		job := &SpawnJob{
			ID:        s.id,
			Status:    s.status,
			Priority:  PriorityNormal,
			AgentType: "codex",
			SpawnOpts: "{}",
			MaxRetries: 3,
			CreatedAt: time.Now().Truncate(time.Second),
		}
		if s.status == StatusRunning {
			job.StartedAt = time.Now().Truncate(time.Second)
		}
		if s.status == StatusCompleted {
			job.CompletedAt = time.Now().Truncate(time.Second)
		}
		if err := store.Create(ctx, job); err != nil {
			t.Fatalf("create %s: %v", s.id, err)
		}
	}

	recovered, err := store.RecoverPending(ctx)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}

	if len(recovered) != 3 {
		t.Fatalf("recovered %d jobs, want 3", len(recovered))
	}

	// Running should be reset to pending.
	for _, j := range recovered {
		if j.ID == "r-2" && j.Status != StatusPending {
			t.Errorf("r-2 status = %q, want pending (crash recovery)", j.Status)
		}
	}
}

func TestStorePrune(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	oldTime := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	newTime := time.Now().Truncate(time.Second)

	jobs := []struct {
		id          string
		status      JobStatus
		completedAt time.Time
	}{
		{"old-1", StatusCompleted, oldTime},
		{"old-2", StatusFailed, oldTime},
		{"new-1", StatusCompleted, newTime},
		{"pend-1", StatusPending, time.Time{}},
	}

	for _, j := range jobs {
		job := &SpawnJob{
			ID:          j.id,
			Status:      j.status,
			Priority:    PriorityNormal,
			AgentType:   "codex",
			SpawnOpts:   "{}",
			MaxRetries:  3,
			CreatedAt:   oldTime,
			CompletedAt: j.completedAt,
		}
		if err := store.Create(ctx, job); err != nil {
			t.Fatalf("create %s: %v", j.id, err)
		}
	}

	pruned, err := store.Prune(ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned %d, want 2", pruned)
	}

	// Verify remaining.
	remaining, err := store.List(ctx, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(remaining) != 2 {
		t.Errorf("remaining %d, want 2 (new-1 + pend-1)", len(remaining))
	}
}

