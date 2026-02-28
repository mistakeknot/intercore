# Task 3: Integration Tests -- Collector With Real SQLite

## Summary

Added integration tests to `internal/observation/observation_test.go` that test the Collector against a real SQLite database with seed data. All 5 tests pass.

## What Was Added

### testCollector Helper

Creates a real SQLite test DB in `t.TempDir()`, migrates it via `db.Migrate()`, instantiates all four real stores (phase, dispatch, event, scheduler), and returns a `Collector` wired to all of them. Follows the exact same pattern used in `dispatch_test.go`.

```go
func testCollector(t *testing.T) (*Collector, *db.DB)
```

### TestCollectIntegration

Seeds a run via `phase.Store.Create()` with a real `phase.Run` struct (`ProjectDir`, `Goal`, `Complexity`). Then calls `Collect()` with no scope and verifies:
- Exactly 1 run in snapshot
- Run ID matches the created run
- Phase is `brainstorm` (the default initial phase)
- Goal matches "Implement feature X"
- Status is `active`
- ProjectDir matches
- CreatedAt is non-zero
- Budget is nil (no token budget set, no RunID scope)

### TestCollectWithRunScope

Creates two runs, then calls `Collect()` with `RunID` set to the first run. Verifies:
- Only 1 run returned (the scoped one)
- Goal matches run 1's goal
- Unscoped `Collect()` returns both runs

### TestCollectIntegrationWithBudget

Creates a run with `TokenBudget = 100000`, creates a dispatch scoped to that run with token usage (5000 input, 3000 output), then collects with `RunID` scope. Verifies:
- Budget is non-nil
- Budget.Budget = 100000
- Budget.Used = 8000 (5000 + 3000)
- Budget.Remaining = 92000

### TestCollectEmptyStores

Collects against real but empty stores (no seed data). Verifies all slices are empty and queue counts are zero. Complements the existing `TestCollectReturnsSnapshot` which uses nil stores.

## Key Findings

### phase.Store.Create Signature

```go
func (s *Store) Create(ctx context.Context, r *Run) (string, error)
```

Takes a `*phase.Run` struct (not individual params) and returns the generated 8-char alphanumeric ID. Initial phase defaults to `PhaseBrainstorm` unless `r.Phases` is set.

### Interface Satisfaction

The concrete store types satisfy the Collector's interfaces without any adapter:
- `*phase.Store` implements `PhaseQuerier` (Get, ListActive)
- `*dispatch.Store` implements `DispatchQuerier` (ListActive, AggregateTokens)
- `*event.Store` implements `EventQuerier` (ListAllEvents, ListEvents)
- `*scheduler.Store` implements `SchedulerQuerier` (CountByStatus)

### Type Assertion in Tests

The integration tests that need to seed data via `phase.Store.Create()` use a type assertion `c.phases.(*phase.Store)` since the Collector's `phases` field is typed as the `PhaseQuerier` interface. This is acceptable in test code.

## Test Results

```
=== RUN   TestCollectReturnsSnapshot
--- PASS: TestCollectReturnsSnapshot (0.00s)
=== RUN   TestCollectIntegration
--- PASS: TestCollectIntegration (0.03s)
=== RUN   TestCollectWithRunScope
--- PASS: TestCollectWithRunScope (0.02s)
=== RUN   TestCollectIntegrationWithBudget
--- PASS: TestCollectIntegrationWithBudget (0.03s)
=== RUN   TestCollectEmptyStores
--- PASS: TestCollectEmptyStores (0.02s)
PASS
ok  	github.com/mistakeknot/intercore/internal/observation	0.104s
```

## Files Modified

- `/home/mk/projects/Demarch/core/intercore/internal/observation/observation_test.go` -- added testCollector helper and 4 new integration tests
