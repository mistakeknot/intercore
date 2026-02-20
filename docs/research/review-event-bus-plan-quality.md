# Quality Review: Event Bus Implementation Plan (iv-egxf)

**Reviewed:** `docs/plans/2026-02-18-intercore-event-bus.md`
**Codebase:** `infra/intercore/` (Go 1.22, modernc.org/sqlite, schema v4 → v5)
**Date:** 2026-02-19

---

## Overview

The plan adds a unified event bus to intercore across 10 tasks: a new `internal/event/` package, in-process `Notifier`, wiring into `phase.Advance` and `dispatch.UpdateStatus`, three handlers (log, spawn, hook), an `ic events tail` CLI command, Bash wrappers, and handler registration at startup. The plan is well-scoped and the new package structure is appropriate.

The plan largely follows existing project conventions: `setupTestStore` test pattern with `t.TempDir()` + `db.Open` + `Migrate`, `fmt.Errorf("context: %w", err)` error wrapping, manual flag parsing, and stdlib-only testing. The schema migration pattern (idempotent `CREATE TABLE IF NOT EXISTS`, `PRAGMA user_version`) is correctly followed.

---

## Findings Summary

Full findings written to: `/root/projects/Interverse/.clavain/quality-gates/fd-quality.md`

**Verdict: needs-changes**

Three medium-severity issues must be resolved before implementation, and five low-severity issues should be addressed. Two info-level improvements are noted.

---

## Key Findings

### Medium Severity

**Q1 — Task 3 Step 1: `EventNotifier` interface uses `interface{}`**

The plan opens Task 3 with an `EventNotifier` interface using `interface{}` as the event parameter, then immediately pivots to `PhaseEventCallback`. The interface definition should be removed entirely; only the callback should be implemented. If the interface variant is accidentally retained during implementation, the `interface{}` type bypasses Go's type system and breaks the usefulness of the `Event` struct. The plan needs to clearly discard the interface variant, not leave both in the text.

**Q2 — Task 3 Step 2: `SetEventRecorder` mutates a live `dispatch.Store` without synchronization**

```go
func (s *Store) SetEventRecorder(fn func(...)) {
    s.eventRecorder = fn
}
```
`dispatch.Store` is shared and its methods are called concurrently. Writing a function field post-construction without a mutex or atomic store is a data race. The project pattern is constructor injection (`New(db *sql.DB) *Store`). Fix: accept the recorder as a constructor parameter or use a wrapper type.

**Q3 — Task 3 Step 2: `UpdateStatus` event recorder re-queries DB after commit and passes empty `from_status`**

The plan's recorder snippet calls `s.Get(ctx, id)` inside `UpdateStatus` to find `runID` after the UPDATE has already committed, discarding the error from `Get`. More critically, `from_status` is hardcoded as `""` — so `dispatch_events.from_status` will always be empty for every recorded event. The previous status must be captured before the UPDATE, not inferred after it.

### Low Severity

**Q4 — Task 1 Step 4: Cross-table UNION ordering by `created_at` INTEGER is brittle at sub-second granularity.** Both tables use `unixepoch()` (1-second resolution). The tiebreak `id ASC` operates on independent AUTOINCREMENT sequences. This is displayable but is not a stable total order. Needs a comment.

**Q5 — Task 1 Step 4: `MaxPhaseEventID` / `MaxDispatchEventID` are exported unnecessarily.** Only consumed by `cmdEventsTail`. Could be unexported helpers or combined into a single `InitCursors` method.

**Q6 — Task 3 Step 1: `PhaseEventCallback` drops `GateResult` and `GateTier`.** These are recorded in `phase_events` but not forwarded to the callback, making it impossible for handlers to distinguish gate-soft-pass from no-gate advances.

**Q7 — Task 7 `saveCursor`: error from `store.Set` is silently discarded.** On DB contention, the cursor silently does not advance, causing event re-delivery on the next `--follow` poll. Should log to `os.Stderr` at minimum.

**Q8 — Task 7 `--since=N`: sets both cursor values to the same integer.** `phase_events` and `dispatch_events` have independent AUTOINCREMENT sequences. `--since=10` skipping both table cursors to 10 is only correct by coincidence. Split into `--since-phase=N` / `--since-dispatch=N` or disallow `--since` with `--consumer`.

### Info

**Q9** — Hook handler uses `map[string]string` instead of `json.Marshal(e)` directly, requiring manual synchronization if `Event` gains new fields.

**Q10** — `Notify` docstring says errors are "collected" but only the first is returned. Should use `errors.Join` (Go 1.20+) or update the documentation.

---

## Consistency with Existing Codebase

| Area | Plan | Codebase | Match |
|------|------|----------|-------|
| Error wrapping | `fmt.Errorf("ctx: %w", err)` | Same throughout | Yes |
| Test setup | `setupTestStore(t)` + `t.TempDir()` | Same in `phase`, `dispatch`, `state` | Yes |
| CLI flag parsing | Manual `strings.HasPrefix` loop | Same in all cmd files | Yes |
| Exit codes | 0/1/2/3 | Established pattern | Yes |
| Store constructor | `New(db *sql.DB) *Store` | Same in all packages | Partial — `SetEventRecorder` breaks this |
| Schema migration | `CREATE TABLE IF NOT EXISTS` + version bump | Matches schema.sql | Yes |
| `rows.Err()` check | Present in `ListEvents` | Same in dispatch, state | Yes |
| `defer rows.Close()` | Present | Same | Yes |
| Unexported `namedHandler` type | Present in notifier | Consistent with `dispatch.go` helpers | Yes |
