package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Queue tests
// ---------------------------------------------------------------------------

func TestJobQueue_EnqueueDequeue_PriorityOrder(t *testing.T) {
	q := NewJobQueue()

	low := newTestJob("low", "s1", PriorityLow)
	high := newTestJob("high", "s1", PriorityHigh)
	urgent := newTestJob("urgent", "s1", PriorityUrgent)
	normal := newTestJob("normal", "s1", PriorityNormal)

	// Enqueue in non-priority order.
	q.Enqueue(low)
	q.Enqueue(high)
	q.Enqueue(urgent)
	q.Enqueue(normal)

	if q.Len() != 4 {
		t.Fatalf("queue length = %d, want 4", q.Len())
	}

	// Dequeue should return in priority order: urgent(0) < high(1) < normal(2) < low(3).
	want := []string{"urgent", "high", "normal", "low"}
	for i, wantID := range want {
		got := q.Dequeue()
		if got == nil {
			t.Fatalf("dequeue %d: got nil, want %s", i, wantID)
		}
		if got.ID != wantID {
			t.Errorf("dequeue %d: got %s, want %s", i, got.ID, wantID)
		}
	}

	if !q.IsEmpty() {
		t.Error("queue should be empty after draining")
	}
}

func TestJobQueue_EnqueueDequeue_FIFO_SamePriority(t *testing.T) {
	q := NewJobQueue()

	// Three jobs with the same priority, enqueued in order.
	for i := 0; i < 3; i++ {
		j := newTestJob(fmt.Sprintf("j%d", i), "s1", PriorityNormal)
		// Ensure distinct creation times for FIFO ordering.
		j.CreatedAt = time.Now().Add(time.Duration(i) * time.Millisecond)
		q.Enqueue(j)
	}

	for i := 0; i < 3; i++ {
		got := q.Dequeue()
		wantID := fmt.Sprintf("j%d", i)
		if got.ID != wantID {
			t.Errorf("dequeue %d: got %s, want %s", i, got.ID, wantID)
		}
	}
}

func TestJobQueue_Peek(t *testing.T) {
	q := NewJobQueue()

	if q.Peek() != nil {
		t.Error("peek on empty queue should return nil")
	}

	q.Enqueue(newTestJob("a", "s1", PriorityNormal))
	q.Enqueue(newTestJob("b", "s1", PriorityHigh))

	peeked := q.Peek()
	if peeked == nil || peeked.ID != "b" {
		t.Errorf("peek = %v, want job 'b' (higher priority)", peeked)
	}

	// Peek should not remove.
	if q.Len() != 2 {
		t.Errorf("length after peek = %d, want 2", q.Len())
	}
}

func TestJobQueue_Get(t *testing.T) {
	q := NewJobQueue()
	q.Enqueue(newTestJob("x", "s1", PriorityNormal))

	if q.Get("x") == nil {
		t.Error("Get('x') should find the job")
	}
	if q.Get("nonexistent") != nil {
		t.Error("Get('nonexistent') should return nil")
	}
}

func TestJobQueue_Remove(t *testing.T) {
	q := NewJobQueue()
	q.Enqueue(newTestJob("a", "s1", PriorityNormal))
	q.Enqueue(newTestJob("b", "s1", PriorityNormal))
	q.Enqueue(newTestJob("c", "s1", PriorityNormal))

	removed := q.Remove("b")
	if removed == nil || removed.ID != "b" {
		t.Errorf("Remove('b') = %v, want job 'b'", removed)
	}
	if q.Len() != 2 {
		t.Errorf("length after remove = %d, want 2", q.Len())
	}

	// Removing again should return nil.
	if q.Remove("b") != nil {
		t.Error("second Remove('b') should return nil")
	}

	// Remaining jobs still dequeue.
	got1 := q.Dequeue()
	got2 := q.Dequeue()
	if got1 == nil || got2 == nil {
		t.Fatal("expected two remaining jobs")
	}
	ids := map[string]bool{got1.ID: true, got2.ID: true}
	if !ids["a"] || !ids["c"] {
		t.Errorf("remaining jobs = {%s, %s}, want {a, c}", got1.ID, got2.ID)
	}
}

func TestJobQueue_EnqueueDuplicate(t *testing.T) {
	q := NewJobQueue()

	j := newTestJob("dup", "s1", PriorityLow)
	q.Enqueue(j)

	// Enqueue the same ID again with a higher priority.
	j2 := newTestJob("dup", "s1", PriorityUrgent)
	q.Enqueue(j2)

	// Should still be one job.
	if q.Len() != 1 {
		t.Fatalf("length = %d, want 1 (duplicate should update)", q.Len())
	}

	// The updated job should reflect the new priority.
	got := q.Dequeue()
	if got.Priority != PriorityUrgent {
		t.Errorf("priority = %d, want %d (updated)", got.Priority, PriorityUrgent)
	}
}

func TestJobQueue_CancelSession(t *testing.T) {
	q := NewJobQueue()

	q.Enqueue(newTestJob("s1-a", "sess1", PriorityNormal))
	q.Enqueue(newTestJob("s1-b", "sess1", PriorityNormal))
	q.Enqueue(newTestJob("s2-a", "sess2", PriorityNormal))

	cancelled := q.CancelSession("sess1")
	if len(cancelled) != 2 {
		t.Errorf("cancelled %d jobs, want 2", len(cancelled))
	}

	if q.Len() != 1 {
		t.Errorf("remaining = %d, want 1", q.Len())
	}

	remaining := q.Dequeue()
	if remaining == nil || remaining.ID != "s2-a" {
		t.Errorf("remaining job = %v, want s2-a", remaining)
	}

	// Verify session counts are cleaned up.
	if q.CountBySession("sess1") != 0 {
		t.Errorf("session count for sess1 = %d, want 0", q.CountBySession("sess1"))
	}
}

func TestJobQueue_CancelBatch(t *testing.T) {
	q := NewJobQueue()

	j1 := newTestJob("b1-a", "s1", PriorityNormal)
	j1.BatchID = "batch-1"
	j2 := newTestJob("b1-b", "s1", PriorityNormal)
	j2.BatchID = "batch-1"
	j3 := newTestJob("b2-a", "s1", PriorityNormal)
	j3.BatchID = "batch-2"

	q.Enqueue(j1)
	q.Enqueue(j2)
	q.Enqueue(j3)

	cancelled := q.CancelBatch("batch-1")
	if len(cancelled) != 2 {
		t.Errorf("cancelled %d jobs, want 2", len(cancelled))
	}

	if q.Len() != 1 {
		t.Errorf("remaining = %d, want 1", q.Len())
	}

	if q.CountByBatch("batch-1") != 0 {
		t.Errorf("batch count = %d, want 0", q.CountByBatch("batch-1"))
	}
}

func TestJobQueue_Stats(t *testing.T) {
	q := NewJobQueue()

	j1 := newTestJob("a", "s1", PriorityHigh)
	j1.Type = JobTypeDispatch
	j2 := newTestJob("b", "s1", PriorityNormal)
	j2.Type = JobTypeBatch

	q.Enqueue(j1)
	q.Enqueue(j2)

	stats := q.Stats()
	if stats.TotalEnqueued != 2 {
		t.Errorf("TotalEnqueued = %d, want 2", stats.TotalEnqueued)
	}
	if stats.CurrentSize != 2 {
		t.Errorf("CurrentSize = %d, want 2", stats.CurrentSize)
	}
	if stats.ByPriority[PriorityHigh] != 1 {
		t.Errorf("ByPriority[High] = %d, want 1", stats.ByPriority[PriorityHigh])
	}
	if stats.ByType[string(JobTypeBatch)] != 1 {
		t.Errorf("ByType[batch] = %d, want 1", stats.ByType[string(JobTypeBatch)])
	}

	q.Dequeue()
	stats = q.Stats()
	if stats.TotalDequeued != 1 {
		t.Errorf("TotalDequeued = %d, want 1", stats.TotalDequeued)
	}
	if stats.CurrentSize != 1 {
		t.Errorf("CurrentSize after dequeue = %d, want 1", stats.CurrentSize)
	}
	if stats.MaxSize != 2 {
		t.Errorf("MaxSize = %d, want 2", stats.MaxSize)
	}
}

func TestJobQueue_ListBySession(t *testing.T) {
	q := NewJobQueue()
	q.Enqueue(newTestJob("a", "sess1", PriorityNormal))
	q.Enqueue(newTestJob("b", "sess2", PriorityNormal))
	q.Enqueue(newTestJob("c", "sess1", PriorityNormal))

	jobs := q.ListBySession("sess1")
	if len(jobs) != 2 {
		t.Errorf("ListBySession('sess1') = %d jobs, want 2", len(jobs))
	}
}

func TestJobQueue_ListByBatch(t *testing.T) {
	q := NewJobQueue()
	j1 := newTestJob("a", "s1", PriorityNormal)
	j1.BatchID = "b1"
	j2 := newTestJob("b", "s1", PriorityNormal)
	j2.BatchID = "b1"
	j3 := newTestJob("c", "s1", PriorityNormal)
	j3.BatchID = "b2"

	q.Enqueue(j1)
	q.Enqueue(j2)
	q.Enqueue(j3)

	jobs := q.ListByBatch("b1")
	if len(jobs) != 2 {
		t.Errorf("ListByBatch('b1') = %d jobs, want 2", len(jobs))
	}
}

func TestJobQueue_Clear(t *testing.T) {
	q := NewJobQueue()
	for i := 0; i < 5; i++ {
		q.Enqueue(newTestJob(fmt.Sprintf("j%d", i), "s1", PriorityNormal))
	}

	removed := q.Clear()
	if len(removed) != 5 {
		t.Errorf("Clear returned %d jobs, want 5", len(removed))
	}
	if !q.IsEmpty() {
		t.Error("queue should be empty after Clear")
	}
}

func TestJobQueue_ConcurrentAccess(t *testing.T) {
	q := NewJobQueue()
	var wg sync.WaitGroup

	// Concurrent enqueue.
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			q.Enqueue(newTestJob(fmt.Sprintf("c%d", n), "s1", PriorityNormal))
		}(i)
	}
	wg.Wait()

	if q.Len() != 50 {
		t.Fatalf("length = %d, want 50", q.Len())
	}

	// Concurrent dequeue.
	var dequeued int32
	var mu sync.Mutex
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if q.Dequeue() != nil {
				mu.Lock()
				dequeued++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if dequeued != 50 {
		t.Errorf("dequeued = %d, want 50", dequeued)
	}
}

// ---------------------------------------------------------------------------
// FairScheduler tests
// ---------------------------------------------------------------------------

func TestFairScheduler_EnqueueTryDequeue(t *testing.T) {
	fs := NewFairScheduler(FairSchedulerConfig{
		MaxPerSession: 2,
		MaxPerBatch:   5,
	})

	j1 := newTestJob("j1", "sess-a", PriorityNormal)
	j2 := newTestJob("j2", "sess-a", PriorityNormal)
	j3 := newTestJob("j3", "sess-a", PriorityNormal)

	fs.Enqueue(j1)
	fs.Enqueue(j2)
	fs.Enqueue(j3)

	// First two should dequeue (MaxPerSession = 2).
	got1 := fs.TryDequeue()
	got2 := fs.TryDequeue()
	if got1 == nil || got2 == nil {
		t.Fatal("expected two jobs to dequeue")
	}

	// Third should be blocked by per-session limit.
	got3 := fs.TryDequeue()
	if got3 != nil {
		t.Error("expected nil — session limit reached")
	}

	// Mark one complete, then third should be available.
	fs.MarkComplete(got1)
	got3 = fs.TryDequeue()
	if got3 == nil {
		t.Error("expected job after MarkComplete freed a slot")
	}
}

func TestFairScheduler_CrossSessionFairness(t *testing.T) {
	fs := NewFairScheduler(FairSchedulerConfig{
		MaxPerSession: 1,
		MaxPerBatch:   5,
	})

	// One job per session; both should dequeue.
	fs.Enqueue(newTestJob("a1", "alpha", PriorityNormal))
	fs.Enqueue(newTestJob("b1", "beta", PriorityNormal))

	got1 := fs.TryDequeue()
	got2 := fs.TryDequeue()
	if got1 == nil || got2 == nil {
		t.Fatal("expected both sessions to get a job")
	}

	// Neither session should get another until MarkComplete.
	fs.Enqueue(newTestJob("a2", "alpha", PriorityNormal))
	fs.Enqueue(newTestJob("b2", "beta", PriorityNormal))

	if fs.TryDequeue() != nil {
		t.Error("expected nil — both sessions at limit")
	}

	fs.MarkComplete(got1)
	got3 := fs.TryDequeue()
	if got3 == nil {
		t.Error("expected a job after freeing a session slot")
	}
}

func TestFairScheduler_RunningCount(t *testing.T) {
	fs := NewFairScheduler(FairSchedulerConfig{MaxPerSession: 5, MaxPerBatch: 5})
	fs.Enqueue(newTestJob("j1", "s1", PriorityNormal))

	if fs.RunningCount("s1") != 0 {
		t.Error("running should be 0 before dequeue")
	}

	job := fs.TryDequeue()
	if fs.RunningCount("s1") != 1 {
		t.Error("running should be 1 after dequeue")
	}

	fs.MarkComplete(job)
	if fs.RunningCount("s1") != 0 {
		t.Error("running should be 0 after MarkComplete")
	}
}

// ---------------------------------------------------------------------------
// RateLimiter tests
// ---------------------------------------------------------------------------

func TestRateLimiter_TryAcquire_InitialBurst(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:         10,
		Capacity:     3,
		BurstAllowed: true,
		MinInterval:  0,
	})

	// Should be able to acquire capacity tokens immediately.
	for i := 0; i < 3; i++ {
		if !rl.TryAcquire() {
			t.Errorf("TryAcquire %d failed, expected success", i)
		}
	}

	// Fourth should fail (no tokens left).
	if rl.TryAcquire() {
		t.Error("TryAcquire should fail when tokens exhausted")
	}
}

func TestRateLimiter_Refill(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:         100, // 100 tokens/sec => 1 token every 10ms
		Capacity:     1,
		BurstAllowed: true,
		MinInterval:  0,
	})

	// Drain the single token.
	if !rl.TryAcquire() {
		t.Fatal("initial acquire failed")
	}
	if rl.TryAcquire() {
		t.Fatal("should be empty")
	}

	// Wait for refill.
	time.Sleep(20 * time.Millisecond)

	if !rl.TryAcquire() {
		t.Error("TryAcquire should succeed after refill period")
	}
}

func TestRateLimiter_MinInterval(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:         1000,
		Capacity:     10,
		BurstAllowed: true,
		MinInterval:  50 * time.Millisecond,
	})

	// First acquire succeeds.
	if !rl.TryAcquire() {
		t.Fatal("first TryAcquire failed")
	}

	// Immediate second should fail due to minInterval.
	if rl.TryAcquire() {
		t.Error("TryAcquire should fail due to minInterval")
	}

	time.Sleep(60 * time.Millisecond)

	if !rl.TryAcquire() {
		t.Error("TryAcquire should succeed after minInterval elapsed")
	}
}

func TestRateLimiter_Wait_Context(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:     1,
		Capacity: 1,
	})

	// Drain.
	if !rl.TryAcquire() {
		t.Fatal("initial TryAcquire failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	err := rl.Wait(ctx)
	if err == nil {
		t.Error("Wait should fail with cancelled context")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("expected DeadlineExceeded, got %v", err)
	}
}

func TestRateLimiter_Wait_Success(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:        100, // fast refill
		Capacity:    1,
		MinInterval: 0,
	})

	// Drain.
	rl.TryAcquire()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	err := rl.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait returned error: %v", err)
	}
}

func TestRateLimiter_TimeUntilNextToken(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:        2,
		Capacity:    1,
		MinInterval: 0,
	})

	// Full bucket: should be 0.
	d := rl.TimeUntilNextToken()
	if d != 0 {
		t.Errorf("TimeUntilNextToken = %v, want 0 (tokens available)", d)
	}

	// Drain.
	rl.TryAcquire()

	// Now should be positive.
	d = rl.TimeUntilNextToken()
	if d <= 0 {
		t.Errorf("TimeUntilNextToken = %v, want > 0", d)
	}
}

func TestRateLimiter_SetRate(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:     1,
		Capacity: 5,
	})

	rl.SetRate(100)

	// After SetRate, tokens should still exist from initial capacity.
	if !rl.TryAcquire() {
		t.Error("TryAcquire failed after SetRate")
	}
}

func TestRateLimiter_SetCapacity(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:     10,
		Capacity: 5,
	})

	// Should have 5 tokens. Set capacity to 2 — tokens should be capped.
	rl.SetCapacity(2)

	avail := rl.AvailableTokens()
	if avail > 2 {
		t.Errorf("tokens = %f, want <= 2 after SetCapacity(2)", avail)
	}
}

func TestRateLimiter_Reset(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:     10,
		Capacity: 5,
	})

	// Drain all.
	for rl.TryAcquire() {
	}

	rl.Reset()

	avail := rl.AvailableTokens()
	if avail != 5 {
		t.Errorf("tokens after reset = %f, want 5", avail)
	}
}

func TestRateLimiter_Stats(t *testing.T) {
	rl := NewRateLimiter(LimiterConfig{
		Rate:     10,
		Capacity: 2,
	})

	rl.TryAcquire()
	rl.TryAcquire()
	rl.TryAcquire() // will fail — no tokens

	stats := rl.Stats()
	if stats.TotalRequests != 3 {
		t.Errorf("TotalRequests = %d, want 3", stats.TotalRequests)
	}
	if stats.AllowedRequests != 2 {
		t.Errorf("AllowedRequests = %d, want 2", stats.AllowedRequests)
	}
}

// ---------------------------------------------------------------------------
// PerAgentLimiter tests
// ---------------------------------------------------------------------------

func TestPerAgentLimiter_DifferentRates(t *testing.T) {
	cfg := AgentLimiterConfig{
		Default: LimiterConfig{Rate: 10, Capacity: 5, MinInterval: 0},
		PerAgent: map[string]LimiterConfig{
			"codex": {Rate: 1, Capacity: 1, MinInterval: 0},
		},
	}
	pal := NewPerAgentLimiter(cfg)

	// Codex limiter has capacity 1.
	codex := pal.GetLimiter("codex")
	if !codex.TryAcquire() {
		t.Error("codex TryAcquire 1 failed")
	}
	if codex.TryAcquire() {
		t.Error("codex TryAcquire 2 should fail (capacity 1)")
	}

	// Claude uses default (capacity 5).
	claude := pal.GetLimiter("claude")
	for i := 0; i < 5; i++ {
		if !claude.TryAcquire() {
			t.Errorf("claude TryAcquire %d failed", i)
		}
	}
}

func TestPerAgentLimiter_DefaultForUnknown(t *testing.T) {
	cfg := AgentLimiterConfig{
		Default: LimiterConfig{Rate: 10, Capacity: 3, MinInterval: 0},
	}
	pal := NewPerAgentLimiter(cfg)

	// Unknown agent type should get default config.
	lim := pal.GetLimiter("unknown-agent")
	for i := 0; i < 3; i++ {
		if !lim.TryAcquire() {
			t.Errorf("unknown agent TryAcquire %d failed", i)
		}
	}
	if lim.TryAcquire() {
		t.Error("should fail after default capacity exhausted")
	}
}

func TestPerAgentLimiter_Wait(t *testing.T) {
	cfg := AgentLimiterConfig{
		Default: LimiterConfig{Rate: 100, Capacity: 1, MinInterval: 0},
	}
	pal := NewPerAgentLimiter(cfg)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := pal.Wait(ctx, "test"); err != nil {
		t.Fatalf("Wait failed: %v", err)
	}
}

func TestPerAgentLimiter_AllStats(t *testing.T) {
	cfg := AgentLimiterConfig{
		Default: LimiterConfig{Rate: 10, Capacity: 5, MinInterval: 0},
		PerAgent: map[string]LimiterConfig{
			"codex": {Rate: 1, Capacity: 1, MinInterval: 0},
		},
	}
	pal := NewPerAgentLimiter(cfg)

	pal.GetLimiter("codex").TryAcquire()
	pal.GetLimiter("claude") // lazily created

	stats := pal.AllStats()
	if _, ok := stats["codex"]; !ok {
		t.Error("expected codex in AllStats")
	}
	if _, ok := stats["claude"]; !ok {
		t.Error("expected claude in AllStats (lazily created)")
	}
}

func TestPerAgentLimiter_ConcurrentGetLimiter(t *testing.T) {
	cfg := AgentLimiterConfig{
		Default: LimiterConfig{Rate: 10, Capacity: 5, MinInterval: 0},
	}
	pal := NewPerAgentLimiter(cfg)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = pal.GetLimiter("shared-agent")
		}()
	}
	wg.Wait()

	// All should get the same limiter (no data race, single instance).
	l1 := pal.GetLimiter("shared-agent")
	l2 := pal.GetLimiter("shared-agent")
	if l1 != l2 {
		t.Error("concurrent GetLimiter returned different instances")
	}
}

// ---------------------------------------------------------------------------
// AgentCaps tests
// ---------------------------------------------------------------------------

func TestAgentCaps_TryAcquireRelease(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     2,
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	if !caps.TryAcquire("test") {
		t.Error("first TryAcquire should succeed")
	}
	if !caps.TryAcquire("test") {
		t.Error("second TryAcquire should succeed")
	}
	if caps.TryAcquire("test") {
		t.Error("third TryAcquire should fail (cap=2)")
	}

	if caps.GetRunning("test") != 2 {
		t.Errorf("running = %d, want 2", caps.GetRunning("test"))
	}

	caps.Release("test")
	if caps.GetRunning("test") != 1 {
		t.Errorf("running after release = %d, want 1", caps.GetRunning("test"))
	}

	if !caps.TryAcquire("test") {
		t.Error("TryAcquire should succeed after Release")
	}
}

func TestAgentCaps_GlobalMax(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     5,
			CooldownOnFailure: false,
		},
		GlobalMax: 3,
	}
	caps := NewAgentCaps(cfg)

	caps.TryAcquire("alpha")
	caps.TryAcquire("beta")
	caps.TryAcquire("gamma")

	// Global max reached.
	if caps.TryAcquire("delta") {
		t.Error("TryAcquire should fail — global max reached")
	}

	caps.Release("alpha")
	if !caps.TryAcquire("delta") {
		t.Error("TryAcquire should succeed after release freed global slot")
	}
}

func TestAgentCaps_RampUp(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     4,
			RampUpEnabled:     true,
			RampUpInitial:     1,
			RampUpStep:        1,
			RampUpInterval:    10 * time.Millisecond, // fast for tests
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	// Initially only 1 slot available.
	if caps.GetCurrentCap("test") != 1 {
		t.Errorf("initial cap = %d, want 1", caps.GetCurrentCap("test"))
	}

	if !caps.TryAcquire("test") {
		t.Fatal("first acquire should succeed")
	}
	if caps.TryAcquire("test") {
		t.Error("second acquire should fail (cap=1)")
	}

	caps.Release("test")

	// Wait for ramp-up interval.
	time.Sleep(30 * time.Millisecond)

	newCap := caps.GetCurrentCap("test")
	if newCap <= 1 {
		t.Errorf("cap after ramp-up = %d, want > 1", newCap)
	}
}

func TestAgentCaps_RecordFailure_Cooldown(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     3,
			CooldownOnFailure: true,
			CooldownReduction: 1,
			CooldownRecovery:  50 * time.Millisecond, // fast for tests
		},
	}
	caps := NewAgentCaps(cfg)

	initialCap := caps.GetCurrentCap("test")
	if initialCap != 3 {
		t.Fatalf("initial cap = %d, want 3", initialCap)
	}

	caps.RecordFailure("test")

	reducedCap := caps.GetCurrentCap("test")
	if reducedCap != 2 {
		t.Errorf("cap after failure = %d, want 2", reducedCap)
	}

	// Wait for recovery.
	time.Sleep(80 * time.Millisecond)

	recoveredCap := caps.GetCurrentCap("test")
	if recoveredCap != 3 {
		t.Errorf("cap after recovery = %d, want 3", recoveredCap)
	}
}

func TestAgentCaps_RecordFailure_MinCap(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     2,
			CooldownOnFailure: true,
			CooldownReduction: 5, // bigger than max — should not go below 1
			CooldownRecovery:  time.Hour,
		},
	}
	caps := NewAgentCaps(cfg)

	caps.RecordFailure("test")
	cap := caps.GetCurrentCap("test")
	if cap < 1 {
		t.Errorf("cap = %d, should never go below 1", cap)
	}
}

func TestAgentCaps_RecordSuccess_ResetsCooldown(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     3,
			CooldownOnFailure: true,
			CooldownReduction: 1,
			CooldownRecovery:  50 * time.Millisecond,
		},
	}
	caps := NewAgentCaps(cfg)

	caps.RecordFailure("test")
	caps.RecordSuccess("test")

	// RecordSuccess during cooldown resets the cooldown timer.
	// After waiting for recovery, cap should be restored.
	time.Sleep(80 * time.Millisecond)

	cap := caps.GetCurrentCap("test")
	if cap != 3 {
		t.Errorf("cap after success + recovery = %d, want 3", cap)
	}
}

func TestAgentCaps_Acquire_Blocking(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     1,
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	// Fill the slot.
	if !caps.TryAcquire("test") {
		t.Fatal("first acquire failed")
	}

	// Acquire should block; use a timeout context.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := caps.Acquire(ctx, "test")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Acquire should timeout, got: %v", err)
	}

	// Release and try again.
	caps.Release("test")

	ctx2, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()

	if err := caps.Acquire(ctx2, "test"); err != nil {
		t.Errorf("Acquire should succeed after release: %v", err)
	}
}

func TestAgentCaps_ForceRampUp(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     5,
			RampUpEnabled:     true,
			RampUpInitial:     1,
			RampUpStep:        1,
			RampUpInterval:    time.Hour, // very slow
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	if caps.GetCurrentCap("test") != 1 {
		t.Fatalf("initial cap = %d, want 1", caps.GetCurrentCap("test"))
	}

	caps.ForceRampUp("test")

	if caps.GetCurrentCap("test") != 5 {
		t.Errorf("cap after ForceRampUp = %d, want 5", caps.GetCurrentCap("test"))
	}
}

func TestAgentCaps_SetCap(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     2,
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	caps.SetCap("test", 10)
	if caps.GetCurrentCap("test") != 10 {
		t.Errorf("cap after SetCap(10) = %d, want 10", caps.GetCurrentCap("test"))
	}

	caps.SetCap("test", 1)
	if caps.GetCurrentCap("test") != 1 {
		t.Errorf("cap after SetCap(1) = %d, want 1", caps.GetCurrentCap("test"))
	}
}

func TestAgentCaps_GetAvailable(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     3,
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	if caps.GetAvailable("test") != 3 {
		t.Errorf("available = %d, want 3", caps.GetAvailable("test"))
	}

	caps.TryAcquire("test")
	if caps.GetAvailable("test") != 2 {
		t.Errorf("available = %d, want 2", caps.GetAvailable("test"))
	}
}

func TestAgentCaps_Stats(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     4,
			CooldownOnFailure: false,
		},
		PerAgent: map[string]AgentCapConfig{
			"codex": {MaxConcurrent: 2, CooldownOnFailure: false},
		},
	}
	caps := NewAgentCaps(cfg)

	caps.TryAcquire("codex")
	stats := caps.Stats()

	codexStats, ok := stats.PerAgent["codex"]
	if !ok {
		t.Fatal("expected codex in Stats")
	}
	if codexStats.Running != 1 {
		t.Errorf("codex running = %d, want 1", codexStats.Running)
	}
	if codexStats.MaxCap != 2 {
		t.Errorf("codex max cap = %d, want 2", codexStats.MaxCap)
	}
}

func TestAgentCaps_Reset(t *testing.T) {
	cfg := AgentCapsConfig{
		Default: AgentCapConfig{
			MaxConcurrent:     3,
			CooldownOnFailure: false,
		},
	}
	caps := NewAgentCaps(cfg)

	caps.TryAcquire("test")
	caps.TryAcquire("test")

	caps.Reset()

	if caps.GetRunning("test") != 0 {
		t.Errorf("running after reset = %d, want 0", caps.GetRunning("test"))
	}
}

// ---------------------------------------------------------------------------
// Backoff tests
// ---------------------------------------------------------------------------

func TestClassifyError_SyscallEAGAIN(t *testing.T) {
	err := syscall.EAGAIN
	re := ClassifyError(err, 0, "")
	if re == nil {
		t.Fatal("expected ResourceError for EAGAIN")
	}
	if re.Type != ResourceErrorEAGAIN {
		t.Errorf("type = %s, want EAGAIN", re.Type)
	}
	if !re.Retryable {
		t.Error("EAGAIN should be retryable")
	}
}

func TestClassifyError_SyscallENOMEM(t *testing.T) {
	re := ClassifyError(syscall.ENOMEM, 0, "")
	if re == nil || re.Type != ResourceErrorENOMEM {
		t.Errorf("expected ENOMEM, got %v", re)
	}
}

func TestClassifyError_SyscallENFILE(t *testing.T) {
	re := ClassifyError(syscall.ENFILE, 0, "")
	if re == nil || re.Type != ResourceErrorENFILE {
		t.Errorf("expected ENFILE, got %v", re)
	}
}

func TestClassifyError_SyscallEMFILE(t *testing.T) {
	re := ClassifyError(syscall.EMFILE, 0, "")
	if re == nil || re.Type != ResourceErrorEMFILE {
		t.Errorf("expected EMFILE, got %v", re)
	}
}

func TestClassifyError_RateLimitString(t *testing.T) {
	err := fmt.Errorf("API error: rate limit exceeded")
	re := ClassifyError(err, 0, "")
	if re == nil || re.Type != ResourceErrorRateLimit {
		t.Errorf("expected RATE_LIMIT, got %v", re)
	}
	if !re.Retryable {
		t.Error("rate limit should be retryable")
	}
}

func TestClassifyError_TooManyRequests(t *testing.T) {
	err := fmt.Errorf("too many requests")
	re := ClassifyError(err, 0, "")
	if re == nil || re.Type != ResourceErrorRateLimit {
		t.Errorf("expected RATE_LIMIT, got %v", re)
	}
}

func TestClassifyError_FDLimitStderr(t *testing.T) {
	err := fmt.Errorf("spawn failed")
	re := ClassifyError(err, 1, "too many open files in system")
	if re == nil || re.Type != ResourceErrorEMFILE {
		t.Errorf("expected EMFILE from stderr, got %v", re)
	}
	if re.StderrHint == "" {
		t.Error("expected StderrHint to be set")
	}
}

func TestClassifyError_ForkRetry(t *testing.T) {
	err := fmt.Errorf("fork: retry: resource temporarily unavailable")
	re := ClassifyError(err, 0, "")
	if re == nil || re.Type != ResourceErrorEAGAIN {
		t.Errorf("expected EAGAIN for fork retry, got %v", re)
	}
}

func TestClassifyError_ExitCode137_OOMKilled(t *testing.T) {
	err := fmt.Errorf("process exited")
	re := ClassifyError(err, 137, "")
	if re == nil || re.Type != ResourceErrorENOMEM {
		t.Errorf("expected ENOMEM for exit 137, got %v", re)
	}
}

func TestClassifyError_ExitCode11(t *testing.T) {
	err := fmt.Errorf("process exited")
	re := ClassifyError(err, 11, "")
	if re == nil || re.Type != ResourceErrorEAGAIN {
		t.Errorf("expected EAGAIN for exit 11, got %v", re)
	}
}

func TestClassifyError_ExitCode12(t *testing.T) {
	err := fmt.Errorf("process exited")
	re := ClassifyError(err, 12, "")
	if re == nil || re.Type != ResourceErrorENOMEM {
		t.Errorf("expected ENOMEM for exit 12, got %v", re)
	}
}

func TestClassifyError_NonResource(t *testing.T) {
	err := fmt.Errorf("some unrelated error")
	re := ClassifyError(err, 1, "normal stderr output")
	if re != nil {
		t.Errorf("expected nil for non-resource error, got %v", re)
	}
}

func TestClassifyError_Nil(t *testing.T) {
	re := ClassifyError(nil, 0, "")
	if re != nil {
		t.Errorf("expected nil for nil error, got %v", re)
	}
}

func TestClassifyError_MemoryAllocationFailed(t *testing.T) {
	err := fmt.Errorf("memory allocation failed for buffer")
	re := ClassifyError(err, 0, "")
	if re == nil || re.Type != ResourceErrorENOMEM {
		t.Errorf("expected ENOMEM, got %v", re)
	}
}

func TestClassifyError_QuotaExceeded(t *testing.T) {
	err := fmt.Errorf("API quota exceeded")
	re := ClassifyError(err, 0, "")
	if re == nil || re.Type != ResourceErrorRateLimit {
		t.Errorf("expected RATE_LIMIT for quota exceeded, got %v", re)
	}
}

func TestResourceError_ErrorString(t *testing.T) {
	re := &ResourceError{
		Original: fmt.Errorf("underlying"),
		Type:     ResourceErrorEAGAIN,
	}
	if re.Error() != "underlying" {
		t.Errorf("Error() = %q, want 'underlying'", re.Error())
	}

	re2 := &ResourceError{Type: ResourceErrorENOMEM}
	if re2.Error() != "ENOMEM: resource exhausted" {
		t.Errorf("Error() = %q, want 'ENOMEM: resource exhausted'", re2.Error())
	}
}

func TestResourceError_Unwrap(t *testing.T) {
	inner := fmt.Errorf("inner error")
	re := &ResourceError{Original: inner}
	if !errors.Is(re, inner) {
		t.Error("Unwrap should allow errors.Is to find inner error")
	}
}

// ---------------------------------------------------------------------------
// BackoffController tests
// ---------------------------------------------------------------------------

func TestBackoffController_HandleError_Retryable(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     100 * time.Millisecond,
		Multiplier:   2.0,
		JitterFactor: 0,
		MaxRetries:   5,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	shouldRetry, delay := bc.HandleError(job, resErr)
	if !shouldRetry {
		t.Error("expected shouldRetry=true for retryable error")
	}
	if delay <= 0 {
		t.Errorf("delay = %v, want > 0", delay)
	}
}

func TestBackoffController_HandleError_NonRetryable(t *testing.T) {
	bc := NewBackoffController(DefaultBackoffConfig())

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  fmt.Errorf("not retryable"),
		Type:      ResourceErrorNone,
		Retryable: false,
	}

	shouldRetry, delay := bc.HandleError(job, resErr)
	if shouldRetry {
		t.Error("expected shouldRetry=false for non-retryable error")
	}
	if delay != 0 {
		t.Errorf("delay = %v, want 0", delay)
	}
}

func TestBackoffController_HandleError_NilError(t *testing.T) {
	bc := NewBackoffController(DefaultBackoffConfig())
	job := newTestJob("j1", "s1", PriorityNormal)

	shouldRetry, delay := bc.HandleError(job, nil)
	if shouldRetry {
		t.Error("expected shouldRetry=false for nil error")
	}
	if delay != 0 {
		t.Errorf("delay = %v, want 0", delay)
	}
}

func TestBackoffController_HandleError_ExhaustedRetries(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     100 * time.Millisecond,
		Multiplier:   2.0,
		JitterFactor: 0,
		MaxRetries:   3,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	job.RetryCount = 3 // Already at max

	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	shouldRetry, _ := bc.HandleError(job, resErr)
	if shouldRetry {
		t.Error("expected shouldRetry=false when retries exhausted")
	}
}

func TestBackoffController_ExponentialDelay(t *testing.T) {
	// InitialDelay must be >= 100ms (calculateDelay floors at 100ms).
	bc := NewBackoffController(BackoffConfig{
		InitialDelay: 200 * time.Millisecond,
		MaxDelay:     5 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0, // No jitter for predictable testing
		MaxRetries:   10,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	var delays []time.Duration
	for i := 0; i < 4; i++ {
		_, delay := bc.HandleError(job, resErr)
		delays = append(delays, delay)
	}

	// Delays should grow: 200ms, 400ms, 800ms, 1600ms (all above 100ms floor).
	for i := 1; i < len(delays); i++ {
		ratio := float64(delays[i]) / float64(delays[i-1])
		if ratio < 1.5 || ratio > 2.5 {
			t.Errorf("delay[%d]/delay[%d] = %.2f, want ~2.0 (delays: %v)", i, i-1, ratio, delays)
		}
	}

	// Verify monotonic increase.
	for i := 1; i < len(delays); i++ {
		if delays[i] <= delays[i-1] {
			t.Errorf("delays not increasing: %v", delays)
		}
	}
}

func TestBackoffController_DelayCapAtMax(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay: 100 * time.Millisecond,
		MaxDelay:     200 * time.Millisecond,
		Multiplier:   10.0,
		JitterFactor: 0,
		MaxRetries:   20,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	// After a few iterations, delay should never exceed MaxDelay + jitter tolerance.
	for i := 0; i < 10; i++ {
		_, delay := bc.HandleError(job, resErr)
		// With 0 jitter, delay should be capped at MaxDelay.
		// The first delay uses InitialDelay, subsequent are capped.
		if delay > 300*time.Millisecond {
			t.Errorf("delay %d = %v, exceeds reasonable cap", i, delay)
		}
	}
}

func TestBackoffController_RecordSuccess_Resets(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay: 10 * time.Millisecond,
		MaxDelay:     1 * time.Second,
		Multiplier:   2.0,
		JitterFactor: 0,
		MaxRetries:   10,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	// Build up some backoff.
	for i := 0; i < 5; i++ {
		bc.HandleError(job, resErr)
	}

	// Record success — should reset.
	bc.RecordSuccess()

	// Next error should start from initial delay again.
	_, delay := bc.HandleError(job, resErr)
	// With InitialDelay=10ms and no jitter, first delay should be around 10ms (min 100ms floor).
	if delay > 200*time.Millisecond {
		t.Errorf("delay after reset = %v, expected close to initial", delay)
	}
}

func TestBackoffController_GlobalBackoff(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay:                 10 * time.Millisecond,
		MaxDelay:                     100 * time.Millisecond,
		Multiplier:                   2.0,
		JitterFactor:                 0,
		MaxRetries:                   10,
		PauseQueueOnBackoff:          true,
		ConsecutiveFailuresThreshold: 2,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	// First failure should NOT trigger global backoff.
	bc.HandleError(job, resErr)
	if bc.IsInGlobalBackoff() {
		t.Error("global backoff should not activate after 1 failure")
	}

	// Second failure should trigger it (threshold=2).
	bc.HandleError(job, resErr)
	if !bc.IsInGlobalBackoff() {
		t.Error("global backoff should activate after 2 consecutive failures")
	}

	remaining := bc.RemainingBackoff()
	if remaining <= 0 {
		t.Errorf("remaining backoff = %v, want > 0", remaining)
	}

	// RecordSuccess should end global backoff.
	bc.RecordSuccess()
	if bc.IsInGlobalBackoff() {
		t.Error("global backoff should end after RecordSuccess")
	}
}

func TestBackoffController_Stats(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay:                 10 * time.Millisecond,
		MaxDelay:                     100 * time.Millisecond,
		Multiplier:                   2.0,
		JitterFactor:                 0,
		MaxRetries:                   10,
		ConsecutiveFailuresThreshold: 100,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	bc.HandleError(job, resErr)
	bc.HandleError(job, resErr)

	stats := bc.Stats()
	if stats.TotalRetries != 2 {
		t.Errorf("TotalRetries = %d, want 2", stats.TotalRetries)
	}
	if stats.TotalBackoffs != 2 {
		t.Errorf("TotalBackoffs = %d, want 2", stats.TotalBackoffs)
	}
	if stats.MaxConsecutive != 2 {
		t.Errorf("MaxConsecutive = %d, want 2", stats.MaxConsecutive)
	}
	if stats.LastBackoffReason != ResourceErrorEAGAIN {
		t.Errorf("LastBackoffReason = %s, want EAGAIN", stats.LastBackoffReason)
	}
}

func TestBackoffController_Reset(t *testing.T) {
	bc := NewBackoffController(BackoffConfig{
		InitialDelay:                 10 * time.Millisecond,
		MaxDelay:                     100 * time.Millisecond,
		Multiplier:                   2.0,
		JitterFactor:                 0,
		MaxRetries:                   10,
		PauseQueueOnBackoff:          true,
		ConsecutiveFailuresThreshold: 1,
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	bc.HandleError(job, resErr)
	bc.Reset()

	if bc.IsInGlobalBackoff() {
		t.Error("global backoff should be cleared after Reset")
	}
}

func TestBackoffController_Hooks(t *testing.T) {
	var exhaustedCalled bool
	bc := NewBackoffController(BackoffConfig{
		InitialDelay:                 10 * time.Millisecond,
		MaxDelay:                     100 * time.Millisecond,
		Multiplier:                   2.0,
		JitterFactor:                 0,
		MaxRetries:                   0, // Exhaust immediately
		ConsecutiveFailuresThreshold: 100,
	})

	bc.SetHooks(nil, nil, func(job *SpawnJob, attempts int) {
		exhaustedCalled = true
	})

	job := newTestJob("j1", "s1", PriorityNormal)
	resErr := &ResourceError{
		Original:  syscall.EAGAIN,
		Type:      ResourceErrorEAGAIN,
		Retryable: true,
	}

	bc.HandleError(job, resErr)
	if !exhaustedCalled {
		t.Error("onRetryExhausted hook should be called when MaxRetries=0")
	}
}

func TestExponentialBackoff_Helper(t *testing.T) {
	tests := []struct {
		attempt int
		initial time.Duration
		max     time.Duration
		mult    float64
		want    time.Duration
	}{
		{0, 100 * time.Millisecond, 10 * time.Second, 2.0, 100 * time.Millisecond},
		{1, 100 * time.Millisecond, 10 * time.Second, 2.0, 200 * time.Millisecond},
		{2, 100 * time.Millisecond, 10 * time.Second, 2.0, 400 * time.Millisecond},
		{20, 100 * time.Millisecond, 10 * time.Second, 2.0, 10 * time.Second}, // capped
	}

	for _, tt := range tests {
		got := ExponentialBackoff(tt.attempt, tt.initial, tt.max, tt.mult)
		if got != tt.want {
			t.Errorf("ExponentialBackoff(%d, %v, %v, %v) = %v, want %v",
				tt.attempt, tt.initial, tt.max, tt.mult, got, tt.want)
		}
	}
}

func TestCalculateJitteredDelay(t *testing.T) {
	// With jitter=0, should return exact base.
	got := CalculateJitteredDelay(100*time.Millisecond, 0)
	if got != 100*time.Millisecond {
		t.Errorf("jitter=0: got %v, want 100ms", got)
	}

	// With jitter > 0, should be within [base*(1-jitter), base*(1+jitter)].
	base := 100 * time.Millisecond
	jitter := 0.5
	for i := 0; i < 100; i++ {
		d := CalculateJitteredDelay(base, jitter)
		lo := time.Duration(float64(base) * (1 - jitter))
		hi := time.Duration(float64(base) * (1 + jitter))
		if d < lo || d > hi {
			t.Errorf("jitter=0.5: got %v, want in [%v, %v]", d, lo, hi)
		}
	}

	// Jitter > 1 should be clamped to 1.
	d := CalculateJitteredDelay(100*time.Millisecond, 2.0)
	lo := time.Duration(0)
	hi := 200 * time.Millisecond
	if d < lo || d > hi {
		t.Errorf("jitter=2.0 (clamped): got %v, want in [%v, %v]", d, lo, hi)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func newTestJob(id, session string, priority JobPriority) *SpawnJob {
	j := NewSpawnJob(id, JobTypeDispatch, session)
	j.Priority = priority
	return j
}
