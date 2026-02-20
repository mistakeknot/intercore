# Architecture Review: Intercore E1 Kernel Primitives Implementation Plan

**Plan file:** `/root/projects/Interverse/docs/plans/2026-02-19-intercore-e1-kernel-primitives.md`
**Codebase root:** `/root/projects/Interverse/infra/intercore/`
**Reviewer:** Flux-drive Architecture & Design Reviewer
**Date:** 2026-02-19

---

## Findings Index

| SEVERITY | ID | Section | Title |
|----------|----|---------|-------|
| MEDIUM | A1 | Task 8 — Budget Threshold Events | BudgetChecker couples three stores without interface boundaries |
| MEDIUM | A2 | Task 4 — Skip Logic | Advance's skip-scan reads the full event log on every call |
| MEDIUM | A3 | Task 3 — Terminal Phase Check | `IsTerminalPhase` hardcodes `PhaseDone`, broken for custom chains |
| LOW | A4 | Task 3 — Gate Fallback | `gateRules` silently passes for custom chains that reuse default phase names |
| LOW | A5 | Task 6 — Token Column Semantics | Mixed nullability between token columns creates ambiguous aggregation convention |
| LOW | A6 | Task 1 — Migration Placement | ALTER TABLE block placement relative to DDL re-application needs clarity |
| LOW | A7 | Task 4 — Skip Walk Edge Case | Advance fallback to terminal ignores skip events on the terminal phase itself |
| INFO | A8 | Task 5 — Artifact Hashing | File I/O inside the store layer breaks the store's responsibility boundary |

**Verdict: needs-changes**

---

## Context: What the Plan Changes

The E1 plan delivers six kernel primitives across ten tasks:

1. Schema migration v5→v6 (ALTER TABLE for existing DBs, new columns in CREATE TABLE for fresh)
2. Configurable phase chains (JSON array in `runs.phases`, fallback to `DefaultPhaseChain`)
3. Refactoring `Advance` and `EvaluateGate` to derive transitions from stored chain instead of compile-time maps
4. Explicit `ic run skip` command with audit trail
5. Artifact content hashing (SHA256) and `dispatch_id` linkage
6. `cache_hits` column on dispatches
7. Query-time token aggregation via `ic run tokens`
8. Budget threshold events via a new `internal/budget` package
9. CLI integration (flags, integration tests, lib wrappers)
10. Legacy code removal and documentation

The plan correctly identifies mechanism/policy separation as the architectural goal: the kernel stores mechanism, the OS (Clavain or a shell operator) applies policy. The `BudgetChecker` pattern and the `ic run skip` command are appropriate expressions of this.

---

## Codebase State (Pre-Change)

Key facts extracted from reading the referenced files:

- `/root/projects/Interverse/infra/intercore/internal/phase/phase.go:52-60` — `transitionTable` is a compile-time `map[string]string` for the 8-phase Clavain chain.
- `/root/projects/Interverse/infra/intercore/internal/phase/phase.go:182-184` — `IsTerminalPhase(p string) bool { return p == PhaseDone }` — hardcoded.
- `/root/projects/Interverse/infra/intercore/internal/phase/machine.go:61` — `toPhase := NextRequiredPhase(fromPhase, run.Complexity, run.ForceFull)` — uses compile-time logic.
- `/root/projects/Interverse/infra/intercore/internal/phase/gate.go:74-94` — `gateRules` keyed on hardcoded phase-name pairs.
- `/root/projects/Interverse/infra/intercore/internal/phase/gate.go:19-29` — `RuntrackQuerier` and `VerdictQuerier` interfaces show the established cross-package query pattern.
- `/root/projects/Interverse/infra/intercore/internal/phase/store.go:226` — `Events(ctx, runID)` returns all phase events for a run, no type filter.
- `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch.go:193-198` — `allowedUpdateCols` allowlist for dynamic UPDATE columns.
- `/root/projects/Interverse/infra/intercore/internal/db/db.go:131-132` — `if currentVersion >= currentSchemaVersion { return nil }` early return in `Migrate`.
- `/root/projects/Interverse/infra/intercore/internal/db/schema.sql:40-43` — `input_tokens INTEGER DEFAULT 0`, `output_tokens INTEGER DEFAULT 0` on `dispatches` table.
- `/root/projects/Interverse/infra/intercore/internal/event/notifier.go` — existing `Notifier` with `Subscribe`/`Notify` pattern; budget events should use this, not a new mechanism.
- `/root/projects/Interverse/infra/intercore/internal/state/state.go` — `Store.Get`/`Store.Set` with `(key, scopeID string)` signature — used in budget dedup proposal.

---

## Issues Found

### A1. MEDIUM: BudgetChecker couples three stores without interface boundaries

**Location:** Plan Task 8, lines 1207-1213 (proposed `internal/budget/budget.go`).

The plan introduces a `Checker` struct that holds concrete `*phase.Store`, `*dispatch.Store`, and `*state.Store` pointers. This creates a three-way cross-package dependency: `budget` imports `phase`, `dispatch`, and `state` simultaneously. Every concrete store signature change cascades into the budget package. More critically, the budget package becomes impossible to unit test in isolation — any test must construct all three stores with a real SQLite database.

The existing codebase has already solved this problem. `/root/projects/Interverse/infra/intercore/internal/phase/gate.go:19-29` defines `RuntrackQuerier` and `VerdictQuerier` as narrow interfaces — one or two methods each — that the stores implement without knowing about the gate package. The budget package should follow the same pattern:

```go
// In internal/budget/budget.go

type RunBudgetQuerier interface {
    GetBudgetConfig(ctx context.Context, runID string) (*BudgetConfig, error)
}

type TokenAggregator interface {
    AggregateTokens(ctx context.Context, scopeID string) (*TokenAggregation, error)
}

type BudgetFlagStore interface {
    Exists(ctx context.Context, key, scopeID string) (bool, error)
    Set(ctx context.Context, key, scopeID string) error
}

type Checker struct {
    runQuerier   RunBudgetQuerier
    tokenAgg     TokenAggregator
    flagStore    BudgetFlagStore
}
```

The `*phase.Store`, `*dispatch.Store`, and `*state.Store` concrete types satisfy these interfaces without modification. Tests can use stub implementations. This is the minimum viable change: no new packages, no new abstractions beyond what the gate pattern already establishes.

### A2. MEDIUM: Advance skip-scan reads the full event log on every advance call

**Location:** Plan Task 4 Step 6, lines 724-765.

The proposed `skippedPhases` helper calls `store.Events(ctx, runID)` which executes `SELECT id, run_id, from_phase, to_phase, event_type, gate_result, gate_tier, reason, created_at FROM phase_events WHERE run_id = ? ORDER BY id ASC` (`store.go:226-228`). This returns every phase event ever recorded for the run. For a Clavain run that has been advancing through phases for hours, recording gate results, blocks, pauses, and advances, this materializes a potentially large result set just to filter for events where `event_type = 'skip'`.

The correct fix is a dedicated store method:

```go
// In phase/store.go
func (s *Store) SkippedPhases(ctx context.Context, runID string) (map[string]bool, error) {
    rows, err := s.db.QueryContext(ctx,
        "SELECT to_phase FROM phase_events WHERE run_id = ? AND event_type = 'skip'",
        runID)
    // ... scan into map[string]bool
}
```

This query uses the existing `idx_phase_events_run` index on `(run_id)` and returns only the minimal data needed. It also gives the operation a clear name in the store's API surface. The `Events` method remains available for audit trail display — it is the wrong tool for this specific lookup.

### A3. MEDIUM: `IsTerminalPhase` hardcodes `PhaseDone`, broken for custom chains

**Location:** `phase.go:182-184`, `gate.go:217`, `machine.go:56`.

The current code:
```go
func IsTerminalPhase(p string) bool {
    return p == PhaseDone  // hardcoded "done"
}
```

This function is called at `machine.go:56` (`if IsTerminalPhase(run.Phase)`) and `gate.go:217` (`if IsTerminalPhase(run.Phase)`). The plan updates `machine.go` to use `ChainIsTerminal(chain, run.Phase)` (Task 3 Step 4, plan line 475). But `gate.go:217` in `EvaluateGate` is addressed only in a comment at plan Task 3 Step 5 ("Update `EvaluateGate`..."), without showing the `IsTerminalPhase` call being removed.

For a run with a custom chain `["draft", "review", "publish"]`, once the run reaches `"publish"`:
- `Advance` correctly returns `ErrTerminalPhase` via `ChainIsTerminal` (if Task 3 Step 4 is implemented)
- `EvaluateGate` (called via `ic gate check`) still calls `IsTerminalPhase("publish")`, which returns `false`, proceeding to compute a next phase that doesn't exist

The minimum fix is to audit all four call sites of `IsTerminalPhase` and replace them with a chain-aware version. Since `IsTerminalPhase` is also part of the public API (exported), the safest approach is to add a new function:

```go
// ChainIsTerminal should be used when a chain is available.
// IsTerminalPhase is kept for legacy compatibility but only correct for DefaultPhaseChain.
func IsTerminalPhase(p string) bool {
    return p == PhaseDone
}
```

And update `EvaluateGate` to load the run's chain and use `ChainIsTerminal`, matching what `Advance` does after Task 3.

### A4. LOW: `gateRules` silently passes for custom chains that reuse default phase names

**Location:** `gate.go:74-94`, plan Task 3 Step 5.

The plan correctly documents that custom chains get no gate rules. However, if a custom chain contains phases whose names match the default set (e.g., `["brainstorm", "executing", "done"]`), the `gateRules` lookup at `gate.go:114` (`rules, ok := gateRules[[2]string{from, to}]`) will find rules for `{brainstorm, executing}` — but that pair does not appear in the hardcoded `gateRules` map, so `ok` is false and the gate passes. This is correct behavior for that specific pair.

The risk is the opposite: a custom chain like `["planned", "executing", "done"]` would trigger the `gateRules` entry for `{PhasePlanned, PhaseExecuting}` (which requires `CheckArtifactExists`). An operator building a new workflow with those phase names gets unexpected gate enforcement.

This is a documentation issue, not a code defect. The fix is a comment at `gate.go:72`:

```go
// IMPORTANT: gateRules apply to any run whose stored chain produces these exact (from, to) pairs.
// Custom chains that happen to include default phase names will get these gate checks applied.
// To disable gate checks for custom chains, use Priority >= 4 or DisableAll.
```

No code change required if this is accepted as by-design, but the comment must be present before custom chains ship.

### A5. LOW: Mixed nullability creates ambiguous token aggregation convention

**Location:** `schema.sql:40-43`, plan Task 1 lines 95-99, plan Task 7 lines 1094-1114.

`input_tokens INTEGER DEFAULT 0` and `output_tokens INTEGER DEFAULT 0` mean "zero tokens reported" and "not yet reported" are indistinguishable. The plan adds `cache_hits INTEGER` (no DEFAULT, nullable) to distinguish "not reported" from "zero cache hits."

The aggregation query at plan line 1103-1108:
```sql
SELECT COALESCE(SUM(input_tokens), 0),
       COALESCE(SUM(output_tokens), 0),
       COALESCE(SUM(cache_hits), 0)
FROM dispatches WHERE scope_id = ?
```

SQLite's `SUM` ignores NULLs, so a mix of reported and unreported cache_hits gives the sum of only the reported ones — which is the correct semantic. This is not a bug. The bug risk is in the Go scan: if `cache_hits` is scanned into a non-pointer `int`, a NULL row causes a scan error. The plan sketch at line 992 shows `CacheHits *int`, which is consistent with the existing pattern for nullable integer fields (`PID *int`, `ExitCode *int` in `dispatch.go:38-39`). The scan must use `sql.NullInt64` as an intermediate, same as `pid` and `exit_code`. The plan does not explicitly show the scan path for `CacheHits` in `queryDispatches` — implementers should follow the `pid` pattern at `dispatch.go:127-134,176`.

The convention note in the plan ("0 means not reported when `cache_hits` is also NULL") is fragile for future readers. The correct framing is: `input_tokens = 0` and `output_tokens = 0` mean either "not reported" or "genuinely zero" (ambiguous by original design); `cache_hits = NULL` means "not reported" and `cache_hits = 0` means "zero cache hits." This distinction should be in the schema comment.

### A6. LOW: Migration placement comment is correct but the implementation risk is real

**Location:** `db.go:131-146`, plan Task 1 lines 116-137.

The plan's description ("Add after `currentVersion >= currentSchemaVersion` check but before `schemaDDL` application") correctly places the ALTER TABLE block for v5→v6. For an existing v5 database:

1. `currentVersion` = 5, `currentSchemaVersion` = 6 → 5 >= 6 is false → no early return
2. ALTER TABLE block runs (adds new columns to existing tables)
3. `schemaDDL` is applied (`CREATE TABLE IF NOT EXISTS` — no-op for existing tables)
4. `PRAGMA user_version = 6` is set

For a fresh database:

1. `currentVersion` = 0 → no early return
2. ALTER TABLE block: the condition `if currentVersion == 5` is false → skip
3. `schemaDDL` creates all tables with new columns inline
4. `PRAGMA user_version = 6` is set

This is correct. The risk is the transaction structure: the existing `Migrate` function does `tx.Rollback()` via `defer` and then returns without committing if the ALTER TABLE fails. Since the ALTER TABLE block and the DDL application are inside the same transaction, a failure in ALTER TABLE correctly rolls back everything. But the backup is created before the transaction starts (`db.go:105-109`), so a failed v5→v6 migration leaves a backup with the old schema — which is correct behavior.

The recommendation is to add a test case for the v5→v6 path specifically: create a v5 DB, insert rows, call `Migrate`, verify rows survive and new columns are present. The plan's `TestMigrateV6Columns` only tests fresh DBs (starts at version 0). The migration test for existing v5 DBs is absent.

### A7. LOW: Terminal fallback in skip-walk ignores skip events on the terminal phase

**Location:** Plan Task 4 Step 6, lines 757-765.

```go
if toPhase == "" {
    // All remaining phases skipped — go to terminal
    toPhase = chain[len(chain)-1]
}
```

If the operator calls `ic run skip <id> done --reason="never reach done"` (or whatever the last phase is named), `SkipPhase` succeeds because `ChainIsValidTransition("a", "done")` returns true. Then `Advance` walks the chain, finds all phases skipped, falls back to `chain[len(chain)-1]` — which is the terminal phase — and transitions to it, ignoring the skip event. The skip record exists in `phase_events` but has no effect.

The clean fix is to disallow skipping the terminal phase in `SkipPhase`:
```go
if ChainIsTerminal(chain, targetPhase) {
    return fmt.Errorf("skip: cannot skip terminal phase %q", targetPhase)
}
```

This is a one-line addition to the `SkipPhase` validation block (plan Task 4 Step 3, around plan line 614). It maintains the invariant that the terminal phase is always reachable via advance.

### A8. INFO: Filesystem I/O inside the store layer

**Location:** Plan Task 5 Step 4, lines 860-911 (`hashFile` called from `AddArtifact`).

The existing stores — `phase/store.go`, `dispatch/dispatch.go`, `runtrack/store.go` — are pure SQL wrappers. They hold a `*sql.DB` and perform no I/O beyond database operations. The proposed `AddArtifact` change adds `os.Open` and `io.Copy` calls into the store layer.

The established pattern in the codebase is that the CLI layer (`cmd/ic/`) handles I/O (reads files, computes hashes, formats output) and passes computed values to the store. This matches how `prompt_hash` works in `dispatch.go:33` — the CLI computes the hash before calling `Create`, not the store.

The minimum fix: compute the hash in `cmdRunArtifactAdd` (`cmd/ic/run.go`) before calling `store.AddArtifact`, and pass `ContentHash *string` as a pre-computed field. The store simply stores it. The `hashFile` helper can live in a `cmd/ic/` utility file. This keeps `runtrack/store.go` testable without real files and consistent with every other store in the package.

---

## Improvements

**I1. Add `SkippedPhases(ctx, runID) (map[string]bool, error)` as a dedicated store method**
A targeted query `SELECT to_phase FROM phase_events WHERE run_id = ? AND event_type = 'skip'` gives the skip lookup a clear API name and avoids materializing the full event list. Consistent with how `CountArtifacts` and `CountActiveAgents` are dedicated narrow queries in `runtrack/store.go:256-277`.

**I2. Add a composite index `idx_phase_events_run_type ON phase_events(run_id, event_type)`**
The skip query and any future event-type-filtered queries benefit from this. The v6 migration is the right moment to add it. One additional `CREATE INDEX IF NOT EXISTS` statement in `schema.sql`.

**I3. Consolidate `generateID` before budget package adds a fourth copy**
The function is duplicated verbatim in `phase/store.go:27-38`, `dispatch/dispatch.go:84-95`, and `runtrack/store.go:28-39`. A shared `internal/idgen` package with `func Generate() (string, error)` eliminates drift. The budget package will likely need IDs for event records. E1 is the right opportunity to consolidate before adding a fourth copy.

**I4. Document the `phases = NULL` sentinel in a package-level constant or comment**
The convention that `phases IS NULL` means "use DefaultPhaseChain" is load-bearing but implicit. Task 10 (cleanup) should add either a constant `PhasesLegacyChain = ""` or a package-level comment naming this sentinel, preventing future maintainers from treating NULL as "unset" versus "legacy chain."

---

## Pattern Compliance Summary

The plan is well-aligned with established codebase patterns in five of six concerns:

| Concern | Status |
|---------|--------|
| TDD discipline (write test first) | Compliant — every task has failing-test step |
| Store layer purity (SQL only) | Violated in Task 5 (hashFile) |
| Interface-based cross-package queries | Violated in Task 8 (concrete store pointers) |
| Mechanism/policy separation | Compliant — `ic run skip` is policy, store records mechanism |
| Schema migration safety | Compliant — ALTER TABLE + backup + transaction |
| Event bus usage | Compliant — budget events via existing dispatch_events infrastructure |

The two pattern violations (A1 and A8) are straightforward to fix without redesign: apply the `RuntrackQuerier` interface pattern to the budget package, and move `hashFile` to the CLI layer.
