# Scheduler Component Tests — Research & Analysis

**Date:** 2026-02-23  
**File created:** `internal/scheduler/components_test.go`  
**Status:** All 56 tests passing with `-race -count=1`

## Summary

Wrote comprehensive white-box unit tests for the four scheduler component files that previously lacked dedicated test coverage:

1. **`queue.go`** — JobQueue (priority heap) and FairScheduler
2. **`limiter.go`** — RateLimiter (token bucket) and PerAgentLimiter
3. **`caps.go`** — AgentCaps with ramp-up and cooldown
4. **`backoff.go`** — BackoffController with error classification

The existing `scheduler_test.go` covered the top-level `Scheduler` integration (start/stop, submit, cancel, retry, hooks) but none of the individual component APIs were tested in isolation.

## Files Analyzed

### queue.go

**API surface tested:**
- `Enqueue` / `Dequeue` — priority ordering (lower value = higher priority) and FIFO within same priority
- `Peek` / `Get` — non-destructive lookups
- `Remove` — by ID with cleanup of batch/session counts
- `CancelSession` / `CancelBatch` — bulk removal with context cancellation
- `Stats` — TotalEnqueued, TotalDequeued, CurrentSize, MaxSize, ByPriority, ByType
- `ListBySession` / `ListByBatch` / `ListAll` / `Clear`
- Duplicate enqueue (same ID) — updates in-place via `heap.Fix`
- Concurrent access safety (50 goroutines enqueue + 50 dequeue)

**FairScheduler:**
- `TryDequeue` respects `MaxPerSession` limit
- Cross-session fairness (different sessions get slots independently)
- `MarkComplete` frees session slots for subsequent dequeues
- `RunningCount` tracking

### limiter.go

**API surface tested:**
- Token bucket `TryAcquire` — burst up to capacity, then fail
- `refill` — tokens regenerate based on elapsed time and rate
- `MinInterval` enforcement — back-to-back acquires fail even with tokens available
- `Wait(ctx)` — blocks until token available or context cancelled (DeadlineExceeded)
- `TimeUntilNextToken` — returns 0 when tokens available, positive when depleted
- `SetRate` / `SetCapacity` / `Reset` — dynamic reconfiguration
- `Stats` — TotalRequests, AllowedRequests counting

**PerAgentLimiter:**
- Pre-configured agent types get their own rates (codex: capacity 1 vs default: capacity 5)
- Unknown agent types get default config (lazy creation)
- `Wait` delegates to per-agent limiter
- `AllStats` aggregates across all agent types
- Concurrent `GetLimiter` safety (double-check locking pattern)

### caps.go

**API surface tested:**
- `TryAcquire` / `Release` — basic concurrency cap enforcement
- `GlobalMax` — cross-agent-type limit
- Ramp-up — initial cap starts at `RampUpInitial`, increases by `RampUpStep` every `RampUpInterval`
- `RecordFailure` — reduces cap by `CooldownReduction`, triggers cooldown timer
- Cooldown minimum cap — never drops below 1 regardless of `CooldownReduction` value
- `RecordSuccess` during cooldown — resets cooldown timer for faster recovery
- `Acquire(ctx)` — blocking wait with context cancellation
- `ForceRampUp` — immediately sets cap to MaxConcurrent
- `SetCap` — dynamic cap adjustment (increase and decrease)
- `GetAvailable` / `GetRunning` / `Stats` / `Reset`

### backoff.go

**API surface tested:**
- `ClassifyError` — all syscall types (EAGAIN, ENOMEM, ENFILE, EMFILE)
- String pattern matching — "rate limit", "too many requests", "fork retry", "memory allocation failed", "quota exceeded", "too many open files"
- Stderr pattern matching — FD limit detected from stderr with StderrHint
- Exit code classification — 11 (EAGAIN), 12 (ENOMEM), 137 (OOM killed)
- Non-resource errors return nil
- `ResourceError.Error()` / `.Unwrap()` — error interface compliance
- `HandleError` — retryable returns (shouldRetry=true, delay>0), non-retryable returns false
- Nil ResourceError triggers `recordSuccess` (resets backoff)
- Exhausted retries — `RetryCount >= MaxRetries` returns shouldRetry=false
- Exponential delay growth — verified 2x ratio between consecutive delays
- Delay cap at MaxDelay — never exceeds configured maximum
- `RecordSuccess` resets backoff to initial delay
- Global backoff — activates after `ConsecutiveFailuresThreshold` failures, ends on success
- `RemainingBackoff` — positive during global backoff
- `Stats` / `Reset` / `SetHooks`
- Helper functions: `ExponentialBackoff` (table-driven), `CalculateJitteredDelay` (boundary + statistical)

## Key Finding: 100ms Floor in calculateDelay

The `calculateDelay` method enforces a minimum delay of 100ms:

```go
if delayWithJitter < 100*time.Millisecond {
    delayWithJitter = 100 * time.Millisecond
}
```

This means any `InitialDelay` below 100ms will produce the same 100ms delay for the first several iterations until the internal `currentDelay` (which grows exponentially from InitialDelay) exceeds 100ms. Tests that verify exponential growth must use `InitialDelay >= 100ms` to observe the doubling behavior.

The `DefaultBackoffConfig` uses `InitialDelay: 500ms`, so this floor doesn't affect production behavior — it only matters for test construction.

## Test Characteristics

- **56 new tests** in `components_test.go`
- All pass with `-race` detector enabled
- Maximum sleep in any test: 80ms (cooldown recovery tests)
- No flaky timing: sleeps are 1.5–3x the relevant interval to avoid false failures
- White-box tests (`package scheduler`) — access internal state directly
- Total runtime: ~1.4s for component tests, ~4.4s for full scheduler suite

## Test Count by Component

| Component | Tests |
|-----------|-------|
| JobQueue | 13 |
| FairScheduler | 3 |
| RateLimiter | 9 |
| PerAgentLimiter | 5 |
| AgentCaps | 12 |
| ClassifyError | 14 |
| BackoffController | 8 |
| Helper functions | 2 |
| **Total** | **66** |

*(Includes sub-test cases within ClassifyError that test different error patterns)*
