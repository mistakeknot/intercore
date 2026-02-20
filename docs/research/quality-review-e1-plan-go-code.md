# Quality Review: Intercore E1 Kernel Primitives — Go Code Quality

**Plan file:** `/root/projects/Interverse/docs/plans/2026-02-19-intercore-e1-kernel-primitives.md`
**Codebase files read:** `internal/phase/phase.go`, `internal/phase/store.go`, `internal/phase/machine.go`, `internal/phase/machine_test.go`, `internal/phase/errors.go`, `internal/dispatch/dispatch.go`, `internal/runtrack/runtrack.go`, `internal/event/event.go`, `internal/state/state.go`, `cmd/ic/run.go`, `CLAUDE.md`
**Date:** 2026-02-19

---

## Findings Index

| SEVERITY | ID | Section | Title |
|----------|-----|---------|-------|
| MEDIUM | Q1 | Task 2 | `slicesEqual` helper should use stdlib `slices.Equal` (Go 1.22) |
| MEDIUM | Q2 | Task 2 | `Chain*` prefix inverts existing naming convention (`IsValid*`, `IsTerminal*`, `Next*`) |
| MEDIUM | Q3 | Task 4 | Advance skip-walk loop logic is both redundant and O(n²); cleaner suffix-flag approach available |
| MEDIUM | Q4 | Task 6 | `CacheHits *int` vs. `*int64`: token counts should match `nullInt64`/`*int64` for 32-bit safety |
| MEDIUM | Q5 | Task 8 | `BudgetChecker` uses concrete store pointers; should use narrow interfaces per project convention |
| LOW | Q6 | Task 1 | Migration test insert order may violate FK constraint for `run_artifacts.dispatch_id` |
| LOW | Q7 | Task 2 | `ParsePhaseChain("")` emits opaque JSON error; empty-string input not in test matrix |
| LOW | Q8 | Task 4 | `SkipPhase` allows duplicate skip events for same phase; audit trail is corrupted |
| LOW | Q9 | Task 5 | `hashFile` errors silently swallowed; permission errors should not be treated like "not found" |
| LOW | Q10 | Task 2 | Three Scan sites mirror `runCols` — all must be updated atomically; plan only mentions `runCols` |
| INFO | Q11 | Task 4 | Plan text says `strPtrOrNil` is "in `machine.go`" — both files are `package phase`, no import needed; misleading |
| INFO | Q12 | Task 7 | `TokenAggregation.TotalCache` should be `*int64` to distinguish "no data" from zero cache hits |
| INFO | Q13 | Task 9 | `cmdRun` usage string (listing subcommands) not updated to include `skip` and `tokens` |
| INFO | Q14 | Task 9 | Integration test's skip subcommand positional ordering inconsistency; missing `--reason` validation noted |

**Verdict: needs-changes**

---

## Summary

The plan is well-structured, TDD-disciplined, and closely follows existing error-wrapping patterns. Five medium-severity issues need to be resolved before implementation begins. The most impactful are: (1) the `Chain*` naming prefix is inconsistent with the established `IsValid*`/`IsTerminal*`/`Next*` convention in `phase.go`; (2) the advance skip-walk loop has redundant logic that could mask correctness gaps on non-trivial chains; (3) `BudgetChecker` should use narrow interfaces rather than concrete store pointers to match the pattern established by `RuntrackQuerier`/`VerdictQuerier`; (4) `CacheHits *int` should be `*int64` for semantic correctness with token counts; and (5) the migration test insert ordering may produce a FK violation. All low and info items are straightforward to resolve during implementation.

---

## Issues Found

### Q1. MEDIUM: `slicesEqual` — use `slices.Equal` from stdlib (Go 1.22)

The plan defines a hand-rolled helper in the test file (Task 2, Step 1, ~line 242):
```go
func slicesEqual(a, b []string) bool {
    if len(a) != len(b) { return false }
    for i := range a {
        if a[i] != b[i] { return false }
    }
    return true
}
```

Go 1.22 (the stated stack in CLAUDE.md) ships `slices.Equal` in the standard library (`slices` package). The plan should use `slices.Equal(got, tt.want)` in the test and similarly replace the hand-rolled `ChainContains` in production with `slices.Contains`:

```go
import "slices"

// In TestParsePhaseChain:
if !slices.Equal(got, tt.want) { ... }

// In phase.go ChainContains:
func ChainContains(chain []string, p string) bool {
    return slices.Contains(chain, p)
}
```

The hand-rolled versions are not wrong, but adding unnecessary helpers when stdlib covers the case goes against the project's dependency discipline.

---

### Q2. MEDIUM: `Chain*` prefix inverts the existing naming convention

The plan introduces five new exported functions with a `Chain*` prefix (Task 2, Step 3):
- `ParsePhaseChain`
- `ChainNextPhase`
- `ChainIsValidTransition`
- `ChainIsTerminal`
- `ChainContains`

The existing `phase.go` (lines 127-193) uses a consistent subject-first convention:
```go
func NextPhase(current string) (string, error)       // existing
func ShouldSkip(p string, complexity int) bool        // existing
func IsValidTransition(from, to string) bool          // existing
func IsTerminalPhase(p string) bool                   // existing
func IsTerminalStatus(s string) bool                  // existing
```

The `Chain*` prefix inverts the pattern. In IDE completions, `ChainIsValidTransition` will appear alongside `IsValidTransition`, and a reader scanning the package cannot predict function names by convention.

Suggested renames:
| Plan name | Consistent rename |
|-----------|------------------|
| `ChainNextPhase(chain, p)` | `NextPhaseInChain(chain, p)` |
| `ChainIsValidTransition(chain, from, to)` | `IsValidChainTransition(chain, from, to)` |
| `ChainIsTerminal(chain, p)` | `IsChainTerminal(chain, p)` |
| `ChainContains(chain, p)` | `PhaseInChain(chain, p)` or inline |

`ParsePhaseChain` is fine — it's a constructor-style parser and the noun-last form is idiomatic for parsing functions.

Evidence: `phase.go` lines 127-193 show all public functions follow `Verb + Subject` or `Is/Should + Adjective + Subject`.

---

### Q3. MEDIUM: Advance skip-walk loop is redundant and subtly O(n²)

Task 4, Step 6 proposes this loop in `machine.go`:
```go
for _, next := range chain {
    // Find phases after current
    if next == fromPhase {
        continue
    }
    // Only consider phases after current
    if !ChainIsValidTransition(chain, fromPhase, next) {
        continue
    }
    if !skipped[next] {
        toPhase = next
        break
    }
}
```

Two problems:

1. **Redundancy / confusion**: The guard `if next == fromPhase { continue }` is logically subsumed by `!ChainIsValidTransition(chain, fromPhase, next)` because `fromPhase` is not a valid forward transition from itself. Having both creates doubt about whether the author believes the guards cover different cases.

2. **O(n²) scan**: `ChainIsValidTransition` itself is O(n) (it scans the chain to find indices). Called inside the O(n) loop, the total is O(n²). For typical chains (≤10 phases) this is negligible, but it is unnecessary.

Cleaner, correct implementation using a suffix flag:
```go
inSuffix := false
for _, next := range chain {
    if next == fromPhase {
        inSuffix = true
        continue
    }
    if !inSuffix {
        continue
    }
    if !skipped[next] {
        toPhase = next
        break
    }
}
if toPhase == "" {
    // All remaining phases are skipped — go to terminal
    toPhase = chain[len(chain)-1]
}
```

This is O(n) and unambiguous. The `ChainIsValidTransition` call inside a hot loop should be removed entirely here.

Evidence: Task 4, Step 6, plan lines ~739-766.

---

### Q4. MEDIUM: `CacheHits *int` — token counts should use `*int64`

Task 6, Step 3 adds `CacheHits *int` to the `Dispatch` struct. The existing nullable integer pattern in `dispatch.go` uses `nullInt` (returns `*int`, via int64→int truncation) for `PID`, `ExitCode`, `TimeoutSec` — fields that are bounded integers (PIDs max ~4M, timeouts max hours in seconds). Token counts (cache hits) can reach tens of millions and on a future 32-bit build would silently truncate.

The semantic grouping is:
- `InputTokens int` / `OutputTokens int` — non-nullable, DEFAULT 0, existing (these also have the 32-bit issue, but they're established)
- `CacheHits *int64` — new nullable column, analogous to the non-nullable token counts

Using `*int64` with a new `nullCacheHits` scan path (or reusing `nullInt64`) makes the intent explicit and avoids future silent truncation:
```go
CacheHits *int64
```

In `queryDispatches` and `Get`, scan as:
```go
var cacheHits sql.NullInt64
// ...
d.CacheHits = nullInt64(cacheHits)
```

The `nullInt64` helper already exists in `dispatch.go` (line 399-403).

Evidence: `dispatch.go` lines 44-49, 391-404. Task 6, Step 3 plan line ~992.

---

### Q5. MEDIUM: `BudgetChecker` should use interfaces, not concrete store pointers

Task 8, Step 2 introduces `internal/budget/budget.go`:
```go
type Checker struct {
    phaseStore    *phase.Store
    dispatchStore *dispatch.Store
    stateStore    *state.Store
}
```

The project consistently avoids cross-package concrete dependencies by defining narrow interfaces at the call site. `machine.go` already demonstrates this:
```go
// gate.go (inferred from machine_test.go)
type RuntrackQuerier interface { ... }
type VerdictQuerier interface { ... }
```

`BudgetChecker` with concrete imports creates:
1. A potential circular import if `phase` ever imports `budget`
2. Inability to unit test `Checker.CheckBudget` without real SQLite databases
3. A new coupling hub that the rest of the codebase avoids

The correct pattern:
```go
// internal/budget/budget.go
type RunReader interface {
    Get(ctx context.Context, id string) (*RunBudget, error)
}

type RunBudget struct {
    TokenBudget   *int64
    BudgetWarnPct int
}

type TokenReader interface {
    AggregateTokens(ctx context.Context, scopeID string) (in, out, cache int64, err error)
}

type StateExistsChecker interface {
    Get(ctx context.Context, key, scopeID string) (json.RawMessage, error)
}

type Checker struct {
    runs   RunReader
    tokens TokenReader
    state  StateExistsChecker
}
```

Evidence: `machine.go` defines `GateConfig` and uses `RuntrackQuerier`/`VerdictQuerier` interfaces. Task 8, Step 2, plan lines ~1205-1244.

---

### Q6. LOW: Migration test insert order may violate FK constraint

Task 1, Step 1 — the test inserts a `dispatches` row with `id='d1'` and then an `run_artifacts` row with `dispatch_id='d1'`. If `run_artifacts.dispatch_id` has a FK to `dispatches.id` (which the plan implies by adding the column as a linkage field), the insert order is correct. However the plan also inserts `runs ('r2', ...)` *after* the `dispatches` insert, and the `run_artifacts` insert uses `run_id='r2'` which depends on `runs.id='r2'` existing. The order in the plan is:

```
1. INSERT INTO runs ('test1') — for column verification
2. INSERT INTO dispatches ('d1') — for column verification
3. INSERT INTO runs ('r2') — parent for artifact
4. INSERT INTO run_artifacts ('a1', run_id='r2', dispatch_id='d1')
```

This order is correct for the artifact insert. But the first `runs` insert at step 1 uses columns `phases, token_budget, budget_warn_pct` without including all NOT NULL columns (like `goal`, `status`, `phase`) — this will fail if the schema has NOT NULL constraints. The existing `runs` schema requires `goal TEXT NOT NULL`. The test should mirror the production INSERT column list.

Evidence: Task 1, Step 1, plan lines ~53-75. `store.go` lines 48-57 show the required NOT NULL columns.

---

### Q7. LOW: `ParsePhaseChain("")` produces an opaque error; missing test case

Task 2, Step 3 — `ParsePhaseChain` calls `json.Unmarshal([]byte(jsonStr), &chain)`. With `jsonStr = ""`:
```
json.Unmarshal([]byte(""), &chain) → "unexpected end of JSON input"
```
Wrapped: `parse phase chain: unexpected end of JSON input`.

The test matrix (plan lines ~178-183) covers `"invalid json"` with `not json` as input, but not the empty string `""`. This matters because a DB row with `phases = ""` (empty string, as opposed to NULL) would hit this code path. The `ParsePhaseChain` function should add a guard:
```go
if jsonStr == "" {
    return nil, fmt.Errorf("parse phase chain: empty string (use NULL for default chain)")
}
```

And the test should add: `{"empty string", "", nil, true}`.

Evidence: Task 2, Step 1 test table, plan lines ~178-183. Step 3 implementation.

---

### Q8. LOW: `SkipPhase` permits duplicate skip events for the same target phase

Task 4, Step 3 — `SkipPhase` records a `PhaseEvent` with `EventType = EventSkip` and `ToPhase = targetPhase`. It validates that the target is ahead in the chain, but does not check if a skip event already exists for that phase. Calling `SkipPhase` twice for phase `"b"` produces two `EventSkip` entries in `phase_events`.

The advance walk in `machine.go` handles duplicates correctly (the map assignment is idempotent), so correctness is not affected. But the `ic run events` output and audit trail will show confusing duplicate skip records.

Add a dedup check in `SkipPhase`:
```go
events, err := s.Events(ctx, runID)
if err != nil {
    return fmt.Errorf("skip: load events: %w", err)
}
for _, e := range events {
    if e.EventType == EventSkip && e.ToPhase == targetPhase {
        return fmt.Errorf("skip: phase %q is already marked for skip", targetPhase)
    }
}
```

The `TestSkipPhaseErrors` test (plan lines ~558-583) does not cover this case — add a test case for double-skip.

Evidence: Task 4, Step 3 implementation, plan lines ~594-633. `TestSkipPhaseErrors`, plan lines ~558-583.

---

### Q9. LOW: `hashFile` errors silently swallowed — distinguish not-found from read errors

Task 5, Step 4 shows:
```go
if h, err := hashFile(a.Path); err == nil {
    contentHash = &h
}
// If file doesn't exist or can't be read, contentHash stays nil
```

The comment treats all errors as equivalent. This violates the project's convention where errors are either propagated or explicitly documented as intentionally dropped. The project wraps every error:
```go
// store.go line 59
return "", fmt.Errorf("run create: %w", err)
```

A permission error on a file that should exist is a real failure. The correct approach:
```go
if h, err := hashFile(a.Path); err != nil {
    if !errors.Is(err, os.ErrNotExist) {
        return "", fmt.Errorf("artifact add: hash %q: %w", a.Path, err)
    }
    // File not yet written — hash will be nil; acceptable for pre-registration
} else {
    contentHash = &h
}
```

This requires `"errors"` and `"os"` in imports (both already present for `hashFile`).

Evidence: Task 5, Step 4, plan lines ~861-895. Compare with `store.go` error-handling pattern throughout.

---

### Q10. LOW: Three Scan sites in store.go mirror `runCols` — all must be updated atomically

`store.go` has a `runCols` constant used in three query functions: `Get` (line 76-84), `Current` (line 270-278), and `queryRuns` (line 300-332). Each has its own `Scan(...)` argument list that must match `runCols` column-for-column. The plan (Task 2, Step 5) says "Update `runCols` to include `phases, token_budget, budget_warn_pct`" and "Update `Get`, `Current`, `queryRuns` to read `phases` column," but it does not explicitly list all three scan sites or specify the scan variable declarations needed.

Concretely: adding three columns to `runCols` requires:
1. Three new `sql.NullXxx` variable declarations in each of `Get`, `Current`, and the loop body in `queryRuns`
2. Three new `Scan()` arguments in each site (in the correct order)
3. Three new struct field assignments after each scan

Missing any one of these causes a runtime panic (`sql: expected N destination arguments in Scan, not M`), not a compile error. The plan should add a checklist note that all three sites must be updated and tested together. Adding a helper function `scanRun(rows *sql.Rows) (*Run, error)` would eliminate the duplication entirely.

Evidence: `store.go` lines 65-99 (`Get`), 259-291 (`Current`), 308-332 (`queryRuns`). All mirror the same scan pattern.

---

## Improvements

### Q11. INFO: Plan text implies `strPtrOrNil` needs import — both files are in `package phase`

Task 4, Step 3 says: "Where `strPtrOrNil` already exists in `machine.go`."

Both `machine.go` and `store.go` are in `package phase`, so `strPtrOrNil` is accessible from `store.go` without any import. The sentence will confuse an implementer who wonders if they need to move the function. A one-line clarification suffices: "Both files are in `package phase`; no import needed."

---

### Q12. INFO: `TokenAggregation.TotalCache` should be `*int64` to represent absent data

Task 7, Step 2 uses `COALESCE(SUM(cache_hits), 0)` which returns `0` whether cache_hits is all-NULL (data was never reported) or genuinely zero. The `ic run tokens` output shows a cache ratio line that would display `0.0%` when cache data is simply absent, misleading users into thinking they have a poor cache ratio.

Use `SUM(cache_hits)` without COALESCE, scan into `sql.NullInt64`, and make `TotalCache *int64`:
```go
type TokenAggregation struct {
    TotalIn    int64
    TotalOut   int64
    TotalCache *int64  // nil if no dispatches reported cache_hits
}
```
Omit the cache ratio line in text output when `TotalCache == nil`.

---

### Q13. INFO: `cmdRun` usage string not updated in any task step

`run.go` line 22-24:
```go
fmt.Fprintf(os.Stderr, "ic: run: missing subcommand (create, status, advance, phase, list, events, cancel, set, current, agent, artifact)\n")
```

After Task 4 adds `skip` and Task 7 adds `tokens`, this string needs to include both. No task step mentions updating it. Add to Task 9, Step 1 or Task 10, Step 3 cleanup.

---

### Q14. INFO: `cmdRunSkip` missing explicit `--reason` validation in the plan's code

Task 4, Step 5 shows:
```go
if reason == "" {
    fmt.Fprintf(os.Stderr, "ic: run skip: --reason is required\n")
    return 3
}
```

This is present in the plan and is correct. However, the integration test in Task 9 calls `ic run skip "$RUN_ID" b --reason="complexity 1" --actor="test"` — the flag `--actor` is optional but the test includes it, which is fine. The concern is that the integration test does not have a negative test case for missing `--reason`, leaving coverage incomplete for a required-flag validation path. Add one integration test that calls `ic run skip <id> <phase>` without `--reason` and asserts exit code 3.
