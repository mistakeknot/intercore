package scheduler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSchedulerStartStop(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	s := New(cfg)

	// Starting without executor should fail.
	if err := s.Start(); err == nil {
		t.Fatal("expected error starting without executor")
	}

	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Double-start should fail.
	if err := s.Start(); err == nil {
		t.Fatal("expected error on double start")
	}

	s.Stop()

	// Double-stop is safe.
	s.Stop()
}

func TestSubmitAndComplete(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	cfg.GlobalRateLimit.Rate = 100 // fast for tests
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var completed atomic.Int32
	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		completed.Add(1)
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	job := NewSpawnJob("test-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait for completion.
	deadline := time.After(5 * time.Second)
	for completed.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for job completion")
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Check stats.
	stats := s.GetStats()
	if stats.TotalSubmitted != 1 {
		t.Errorf("submitted = %d, want 1", stats.TotalSubmitted)
	}
	if stats.TotalCompleted != 1 {
		t.Errorf("completed = %d, want 1", stats.TotalCompleted)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var running atomic.Int32
	var maxRunning atomic.Int32
	var done sync.WaitGroup

	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		cur := running.Add(1)
		// Track max concurrent.
		for {
			old := maxRunning.Load()
			if cur <= old || maxRunning.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		running.Add(-1)
		done.Done()
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	done.Add(5)
	for i := 0; i < 5; i++ {
		job := NewSpawnJob(fmt.Sprintf("job-%d", i), JobTypeDispatch, "session-a")
		if err := s.Submit(job); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	done.Wait()

	if maxRunning.Load() > 2 {
		t.Errorf("max concurrent = %d, want <= 2", maxRunning.Load())
	}
}

func TestCancel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 1
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var started sync.WaitGroup
	started.Add(1)

	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		started.Done()
		<-ctx.Done()
		return ctx.Err()
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	// Submit a blocking job.
	job1 := NewSpawnJob("block-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job1); err != nil {
		t.Fatalf("submit: %v", err)
	}
	started.Wait()

	// Submit a second job that will be queued.
	job2 := NewSpawnJob("queued-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job2); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Cancel the queued job.
	if !s.Cancel("queued-1") {
		t.Error("expected cancel to succeed for queued job")
	}

	// Cancel the running job.
	if !s.Cancel("block-1") {
		t.Error("expected cancel to succeed for running job")
	}

	// Cancel nonexistent job.
	if s.Cancel("nonexistent") {
		t.Error("expected cancel to fail for nonexistent job")
	}
}

func TestPauseResume(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var completed atomic.Int32
	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		completed.Add(1)
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	s.Pause()
	if !s.IsPaused() {
		t.Error("expected scheduler to be paused")
	}

	// Submit while paused.
	job := NewSpawnJob("paused-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Give workers a chance to run — they should skip due to pause.
	time.Sleep(200 * time.Millisecond)
	if completed.Load() != 0 {
		t.Errorf("expected 0 completions while paused, got %d", completed.Load())
	}

	// Resume and wait for completion.
	s.Resume()
	deadline := time.After(5 * time.Second)
	for completed.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for job completion after resume")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestRetryOnFailure(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	cfg.DefaultRetries = 3
	cfg.DefaultRetryDelay = 10 * time.Millisecond
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var attempts atomic.Int32
	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		n := attempts.Add(1)
		if n < 3 {
			return fmt.Errorf("transient error")
		}
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	job := NewSpawnJob("retry-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job); err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for attempts.Load() < 3 {
		select {
		case <-deadline:
			t.Fatalf("timeout waiting for retries, attempts=%d", attempts.Load())
		case <-time.After(10 * time.Millisecond):
		}
	}

	// Wait a bit for stats to update.
	time.Sleep(100 * time.Millisecond)

	stats := s.GetStats()
	if stats.TotalRetried < 2 {
		t.Errorf("retried = %d, want >= 2", stats.TotalRetried)
	}
	if stats.TotalCompleted != 1 {
		t.Errorf("completed = %d, want 1", stats.TotalCompleted)
	}
}

func TestSubmitBatch(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 4
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var completed atomic.Int32
	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		completed.Add(1)
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	jobs := make([]*SpawnJob, 3)
	for i := range jobs {
		jobs[i] = NewSpawnJob(fmt.Sprintf("batch-%d", i), JobTypeDispatch, "session-a")
	}

	batchID, err := s.SubmitBatch(jobs)
	if err != nil {
		t.Fatalf("submit batch: %v", err)
	}
	if batchID == "" {
		t.Error("expected non-empty batch ID")
	}

	deadline := time.After(5 * time.Second)
	for completed.Load() < 3 {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for batch completion")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCancelSession(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 1
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var started sync.WaitGroup
	started.Add(1)

	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		started.Done()
		<-ctx.Done()
		return ctx.Err()
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	// First job blocks the worker.
	job1 := NewSpawnJob("s-1", JobTypeDispatch, "target-session")
	if err := s.Submit(job1); err != nil {
		t.Fatalf("submit: %v", err)
	}
	started.Wait()

	// Queue more jobs for the same session.
	for i := 2; i <= 4; i++ {
		job := NewSpawnJob(fmt.Sprintf("s-%d", i), JobTypeDispatch, "target-session")
		if err := s.Submit(job); err != nil {
			t.Fatalf("submit: %v", err)
		}
	}

	cancelled := s.CancelSession("target-session")
	if cancelled < 3 {
		t.Errorf("cancelled = %d, want >= 3 (queued + running)", cancelled)
	}
}

func TestGetJob(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 1
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	ch := make(chan struct{})
	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		<-ch
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	job := NewSpawnJob("lookup-1", JobTypeDispatch, "session-a")
	if err := s.Submit(job); err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait for it to start running.
	time.Sleep(200 * time.Millisecond)

	found := s.GetJob("lookup-1")
	if found == nil {
		t.Fatal("expected to find running job")
	}
	if found.GetStatus() != StatusRunning {
		t.Errorf("status = %s, want running", found.GetStatus())
	}

	close(ch)

	// Wait for completion.
	time.Sleep(200 * time.Millisecond)

	found = s.GetJob("lookup-1")
	if found == nil {
		t.Fatal("expected to find completed job")
	}
	if found.GetStatus() != StatusCompleted {
		t.Errorf("status = %s, want completed", found.GetStatus())
	}

	// Nonexistent.
	if s.GetJob("nope") != nil {
		t.Error("expected nil for nonexistent job")
	}
}

func TestHooks(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxConcurrent = 2
	cfg.GlobalRateLimit.Rate = 100
	cfg.GlobalRateLimit.MinInterval = 0
	s := New(cfg)

	var enqueued, started, completed atomic.Int32
	s.SetHooks(Hooks{
		OnJobEnqueued:  func(job *SpawnJob) { enqueued.Add(1) },
		OnJobStarted:   func(job *SpawnJob) { started.Add(1) },
		OnJobCompleted: func(job *SpawnJob) { completed.Add(1) },
	})

	s.SetExecutor(func(ctx context.Context, job *SpawnJob) error {
		return nil
	})

	if err := s.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Stop()

	if err := s.Submit(NewSpawnJob("h-1", JobTypeDispatch, "s")); err != nil {
		t.Fatalf("submit: %v", err)
	}

	deadline := time.After(5 * time.Second)
	for completed.Load() < 1 {
		select {
		case <-deadline:
			t.Fatal("timeout")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if enqueued.Load() != 1 {
		t.Errorf("enqueued = %d, want 1", enqueued.Load())
	}
	if started.Load() != 1 {
		t.Errorf("started = %d, want 1", started.Load())
	}
}
