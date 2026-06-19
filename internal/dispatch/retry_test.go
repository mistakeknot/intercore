package dispatch

import (
	"context"
	"testing"
	"time"
)

func TestRetryPolicy_Backoff(t *testing.T) {
	policy := RetryPolicy{
		BaseBackoff:   1 * time.Second,
		MaxBackoff:    30 * time.Second,
		BackoffFactor: 2.0,
	}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Second},
		{1, 2 * time.Second},
		{2, 4 * time.Second},
		{3, 8 * time.Second},
		{4, 16 * time.Second},
		{5, 30 * time.Second}, // capped at MaxBackoff
		{6, 30 * time.Second}, // still capped
	}

	for _, tt := range tests {
		got := policy.Backoff(tt.attempt)
		if got != tt.expected {
			t.Errorf("Backoff(%d) = %v, want %v", tt.attempt, got, tt.expected)
		}
	}
}

func TestRetryPolicy_BackoffDefaults(t *testing.T) {
	policy := RetryPolicy{} // all zero values

	got := policy.Backoff(0)
	if got != 5*time.Second {
		t.Errorf("default Backoff(0) = %v, want 5s", got)
	}

	got = policy.Backoff(1)
	if got != 10*time.Second {
		t.Errorf("default Backoff(1) = %v, want 10s", got)
	}
}

func TestDefaultRetryPolicy(t *testing.T) {
	p := DefaultRetryPolicy()

	if p.MaxRetries != 3 {
		t.Errorf("MaxRetries = %d, want 3", p.MaxRetries)
	}
	if p.BaseBackoff != 5*time.Second {
		t.Errorf("BaseBackoff = %v, want 5s", p.BaseBackoff)
	}
	if p.MaxBackoff != 5*time.Minute {
		t.Errorf("MaxBackoff = %v, want 5m", p.MaxBackoff)
	}
	if p.BackoffFactor != 2.0 {
		t.Errorf("BackoffFactor = %v, want 2.0", p.BackoffFactor)
	}
	if !p.RetryOnTimeout {
		t.Error("RetryOnTimeout should be true")
	}
}

func TestShouldRetry(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:     3,
		RetryOnTimeout: true,
	}

	tests := []struct {
		name     string
		dispatch *Dispatch
		want     bool
	}{
		{"nil dispatch", nil, false},
		{"failed, 0 retries", &Dispatch{Status: StatusFailed, RetryCount: 0}, true},
		{"failed, 2 retries", &Dispatch{Status: StatusFailed, RetryCount: 2}, true},
		{"failed, 3 retries (at max)", &Dispatch{Status: StatusFailed, RetryCount: 3}, false},
		{"timeout, retryable", &Dispatch{Status: StatusTimeout, RetryCount: 0}, true},
		{"completed", &Dispatch{Status: StatusCompleted, RetryCount: 0}, false},
		{"running", &Dispatch{Status: StatusRunning, RetryCount: 0}, false},
		{"spawned", &Dispatch{Status: StatusSpawned, RetryCount: 0}, false},
		{"cancelled", &Dispatch{Status: StatusCancelled, RetryCount: 0}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ShouldRetry(tt.dispatch, policy)
			if got != tt.want {
				t.Errorf("ShouldRetry() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldRetry_TimeoutDisabled(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:     3,
		RetryOnTimeout: false,
	}

	d := &Dispatch{Status: StatusTimeout, RetryCount: 0}
	if ShouldRetry(d, policy) {
		t.Error("ShouldRetry should return false when RetryOnTimeout is disabled")
	}
}

func TestShouldRetry_ZeroMaxRetries(t *testing.T) {
	policy := RetryPolicy{MaxRetries: 0}
	d := &Dispatch{Status: StatusFailed, RetryCount: 0}
	if ShouldRetry(d, policy) {
		t.Error("ShouldRetry should return false when MaxRetries is 0")
	}
}

func TestRetry(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{
		MaxRetries:     3,
		BaseBackoff:    100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		BackoffFactor:  2.0,
		RetryOnTimeout: true,
	}

	scope := "test-run"
	name := "test-agent"
	model := "sonnet"
	promptFile := "/tmp/prompt.md"
	promptHash := "abc123"

	// Create original dispatch and mark it failed
	orig := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
		ScopeID:    &scope,
		Name:       &name,
		Model:      &model,
		PromptFile: &promptFile,
		PromptHash: &promptHash,
	}
	origID, err := store.Create(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, origID, StatusRunning, UpdateFields{"pid": 100})
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{
		"exit_code":     1,
		"error_message": "segfault",
	})

	// Retry
	result, err := Retry(ctx, store, origID, policy)
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}

	if result.OriginalID != origID {
		t.Errorf("OriginalID = %q, want %q", result.OriginalID, origID)
	}
	if result.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", result.Attempt)
	}
	if result.BackoffMs != 100 {
		t.Errorf("BackoffMs = %d, want 100", result.BackoffMs)
	}

	// Verify the new dispatch
	newDisp, err := store.Get(ctx, result.NewID)
	if err != nil {
		t.Fatalf("Get retry: %v", err)
	}
	if newDisp.Status != StatusSpawned {
		t.Errorf("Status = %q, want %q", newDisp.Status, StatusSpawned)
	}
	if newDisp.RetryCount != 1 {
		t.Errorf("RetryCount = %d, want 1", newDisp.RetryCount)
	}
	if newDisp.ParentDispatchID != origID {
		t.Errorf("ParentDispatchID = %q, want %q", newDisp.ParentDispatchID, origID)
	}
	if newDisp.AgentType != "codex" {
		t.Errorf("AgentType = %q, want %q", newDisp.AgentType, "codex")
	}
	if newDisp.Name == nil || *newDisp.Name != "test-agent" {
		t.Errorf("Name = %v, want %q", newDisp.Name, "test-agent")
	}
	if newDisp.Model == nil || *newDisp.Model != "sonnet" {
		t.Errorf("Model = %v, want %q", newDisp.Model, "sonnet")
	}
	if newDisp.ScopeID == nil || *newDisp.ScopeID != "test-run" {
		t.Errorf("ScopeID = %v, want %q", newDisp.ScopeID, "test-run")
	}
	if newDisp.PromptFile == nil || *newDisp.PromptFile != "/tmp/prompt.md" {
		t.Errorf("PromptFile = %v, want %q", newDisp.PromptFile, "/tmp/prompt.md")
	}
}

func TestRetry_MaxRetriesExceeded(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{MaxRetries: 2}

	// Create dispatch already at max retries
	orig := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
		RetryCount: 2,
	}
	origID, _ := store.Create(ctx, orig)
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	_, err := Retry(ctx, store, origID, policy)
	if err == nil {
		t.Fatal("expected error for max retries exceeded")
	}
}

func TestRetry_NotRetryable(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{MaxRetries: 3}

	// Completed dispatch is not retryable
	orig := &Dispatch{
		AgentType:  "codex",
		ProjectDir: "/tmp/test",
	}
	origID, _ := store.Create(ctx, orig)
	store.UpdateStatus(ctx, origID, StatusRunning, UpdateFields{"pid": 1})
	store.UpdateStatus(ctx, origID, StatusCompleted, UpdateFields{"exit_code": 0})

	_, err := Retry(ctx, store, origID, policy)
	if err == nil {
		t.Fatal("expected error for completed dispatch")
	}
}

func TestRetry_NotFound(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{MaxRetries: 3}

	_, err := Retry(ctx, store, "nonexist", policy)
	if err == nil {
		t.Fatal("expected error for nonexistent dispatch")
	}
}

func TestRetry_ChainedRetries(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{
		MaxRetries:     3,
		BaseBackoff:    100 * time.Millisecond,
		MaxBackoff:     1 * time.Second,
		BackoffFactor:  2.0,
		RetryOnTimeout: true,
	}

	// Create original → fail → retry1 → fail → retry2
	orig := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/test"}
	origID, _ := store.Create(ctx, orig)
	store.UpdateStatus(ctx, origID, StatusRunning, UpdateFields{"pid": 100})
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	r1, err := Retry(ctx, store, origID, policy)
	if err != nil {
		t.Fatalf("Retry 1: %v", err)
	}
	if r1.Attempt != 1 {
		t.Errorf("Retry 1 Attempt = %d, want 1", r1.Attempt)
	}
	if r1.BackoffMs != 100 {
		t.Errorf("Retry 1 BackoffMs = %d, want 100", r1.BackoffMs)
	}

	// Fail the first retry and create second retry
	store.UpdateStatus(ctx, r1.NewID, StatusRunning, UpdateFields{"pid": 200})
	store.UpdateStatus(ctx, r1.NewID, StatusFailed, UpdateFields{"exit_code": 1})

	r2, err := Retry(ctx, store, r1.NewID, policy)
	if err != nil {
		t.Fatalf("Retry 2: %v", err)
	}
	if r2.Attempt != 2 {
		t.Errorf("Retry 2 Attempt = %d, want 2", r2.Attempt)
	}
	if r2.BackoffMs != 200 {
		t.Errorf("Retry 2 BackoffMs = %d, want 200", r2.BackoffMs)
	}

	// Fail the second retry and create third retry
	store.UpdateStatus(ctx, r2.NewID, StatusRunning, UpdateFields{"pid": 300})
	store.UpdateStatus(ctx, r2.NewID, StatusFailed, UpdateFields{"exit_code": 1})

	r3, err := Retry(ctx, store, r2.NewID, policy)
	if err != nil {
		t.Fatalf("Retry 3: %v", err)
	}
	if r3.Attempt != 3 {
		t.Errorf("Retry 3 Attempt = %d, want 3", r3.Attempt)
	}
	if r3.BackoffMs != 400 {
		t.Errorf("Retry 3 BackoffMs = %d, want 400", r3.BackoffMs)
	}

	// Fourth retry should be rejected (at max)
	store.UpdateStatus(ctx, r3.NewID, StatusRunning, UpdateFields{"pid": 400})
	store.UpdateStatus(ctx, r3.NewID, StatusFailed, UpdateFields{"exit_code": 1})

	_, err = Retry(ctx, store, r3.NewID, policy)
	if err == nil {
		t.Fatal("expected error for 4th retry (max=3)")
	}
}

func TestListRetryChain(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	scope := "chain-test"
	policy := RetryPolicy{
		MaxRetries:    3,
		BaseBackoff:   100 * time.Millisecond,
		BackoffFactor: 2.0,
	}

	// Create chain: orig → retry1 → retry2
	orig := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/test", ScopeID: &scope}
	origID, _ := store.Create(ctx, orig)
	store.UpdateStatus(ctx, origID, StatusRunning, UpdateFields{"pid": 100})
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	r1, _ := Retry(ctx, store, origID, policy)
	store.UpdateStatus(ctx, r1.NewID, StatusRunning, UpdateFields{"pid": 200})
	store.UpdateStatus(ctx, r1.NewID, StatusFailed, UpdateFields{"exit_code": 1})

	r2, _ := Retry(ctx, store, r1.NewID, policy)

	// List chain from any point
	chain, err := ListRetryChain(ctx, store, r2.NewID)
	if err != nil {
		t.Fatalf("ListRetryChain: %v", err)
	}

	if len(chain) != 3 {
		t.Fatalf("chain length = %d, want 3", len(chain))
	}
	if chain[0].ID != origID {
		t.Errorf("chain[0].ID = %q, want %q", chain[0].ID, origID)
	}
	if chain[1].ID != r1.NewID {
		t.Errorf("chain[1].ID = %q, want %q", chain[1].ID, r1.NewID)
	}
	if chain[2].ID != r2.NewID {
		t.Errorf("chain[2].ID = %q, want %q", chain[2].ID, r2.NewID)
	}
}

func TestRetry_TimeoutDispatch(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	policy := RetryPolicy{
		MaxRetries:     2,
		RetryOnTimeout: true,
	}

	orig := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/test"}
	origID, _ := store.Create(ctx, orig)
	store.UpdateStatus(ctx, origID, StatusRunning, UpdateFields{"pid": 100})
	store.UpdateStatus(ctx, origID, StatusTimeout, UpdateFields{"error_message": "timed out"})

	result, err := Retry(ctx, store, origID, policy)
	if err != nil {
		t.Fatalf("Retry timeout dispatch: %v", err)
	}
	if result.Attempt != 1 {
		t.Errorf("Attempt = %d, want 1", result.Attempt)
	}
}
