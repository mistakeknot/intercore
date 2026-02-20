# Architecture Review: Wave 2 Event Bus

**Date:** 2026-02-18
**Reviewer:** Flux-drive Architecture & Design Reviewer
**Scope:** 27 files changed, ~1,500 lines added across 7 commits
**Mode:** Codebase-aware (CLAUDE.md + AGENTS.md grounded)

---

## What Was Reviewed

The Wave 2 Event Bus adds a unified, observable event stream over the existing run lifecycle and dispatch lifecycle. Key artifacts:

- `internal/event/` — new package: `Event` type, `Store`, `Notifier`, three handlers (log, spawn, hook)
- `internal/db/schema.sql` v5 — new `dispatch_events` table
- `internal/phase/machine.go` — `PhaseEventCallback` parameter added to `Advance()`
- `internal/dispatch/dispatch.go` — `DispatchEventRecorder` added via constructor injection
- `cmd/ic/events.go` — new CLI: `ic events tail` with dual-cursor support
- `cmd/ic/run.go` — composition root wires the notifier, hooks, and closures
- `lib-intercore.sh` v0.6.0 — four new bash wrappers
- `test-integration.sh` — eight new integration tests

---

## Section 1: Boundaries & Coupling

### Overall Assessment

The package structure is clean. `internal/event` has no imports from `internal/phase`, `internal/dispatch`, or `internal/runtrack` — it only knows about `*sql.DB` (in `store.go`) and the `Event` struct. Coupling flows through named function types (`DispatchEventRecorder`, `PhaseEventCallback`) and the `Handler` func type, which is the correct inversion for a CLI tool that composes everything at the command layer.

The `cmd/ic/run.go` composition root is the one place where all packages meet, and it does so via closures rather than interface types. This is consistent with the project's existing composition pattern (see `cmd/ic/gate.go` with `rtStore`, `dStore`).

### Issue A1: Dispatch event write is outside the dispatch transaction (MEDIUM)

`cmd/ic/run.go` defines `dispatchRecorder` as:

```go
dispatchRecorder := func(dispatchID, runID, fromStatus, toStatus string) {
    e := event.Event{...}
    evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", "")
    notifier.Notify(ctx, e)
}
dStore := dispatch.New(d.SqlDB(), dispatchRecorder)
```

`dispatch.Store.UpdateStatus` (in `internal/dispatch/dispatch.go`) commits its transaction, then fires the recorder outside that transaction:

```go
if err := tx.Commit(); err != nil {
    return fmt.Errorf("dispatch update: commit: %w", err)
}
// Fire event recorder outside transaction (fire-and-forget)
if s.eventRecorder != nil && status != prevStatus {
    s.eventRecorder(id, runID, prevStatus, status)
}
```

The comment says "fire-and-forget" but `AddDispatchEvent` is a DB write. If it fails (disk full, SQLITE_BUSY from a concurrent `ic dispatch poll` or `ic dispatch wait`), the failure is swallowed — the dispatch status transition is committed but no event row is created. This cannot be recovered from without re-querying `dispatches` and back-filling events.

The project's documented constraint is `SetMaxOpenConns(1)`. On the current single writer this is serialized, so SQLITE_BUSY contention between the post-commit `AddDispatchEvent` and a concurrent dispatch command is possible only across separate processes. Given that `dispatch.sh` runs as a forked process that eventually calls `ic dispatch poll`, this race is plausible in normal operation.

**Smallest viable fix:** Move `AddDispatchEvent` into `UpdateStatus`'s transaction before `tx.Commit()`. Accept an `eventStore` dependency in `dispatch.New()` or as a parameter on `UpdateStatus`. The recorder closure in `run.go` would then only need to call `notifier.Notify`, which is the correct split: DB write in the transaction, notification outside.

### Issue A2: Phase callback misses blocked and paused events (MEDIUM)

`internal/phase/machine.go` has three exit paths:

1. **Pause** (lines 71-93): records `EventPause` in `phase_events`, returns. No callback.
2. **Block** (lines 114-139): records `EventBlock` in `phase_events`, returns. No callback.
3. **Advance** (lines 142-170): records `EventAdvance` or `EventSkip`, then fires callback.

An `ic events tail --follow` consumer will only see `advance` and `skip` events. Blocked and paused transitions — which are significant operational states — are invisible. The database audit trail in `phase_events` is authoritative and correct; the event bus gives a false optimistic view.

If blocked/paused events are intentionally excluded because they do not represent a state change that consumers should react to, this should be documented in the `PhaseEventCallback` doc comment and the `ListEvents` query should filter by `event_type`. As written, the asymmetry is invisible to consumers.

**Smallest viable fix:** Fire the callback from the pause and block return paths. The `Event.Advanced` field does not exist on the current `Event` struct, but the `Type` field distinguishes them (`"block"`, `"pause"`). If the event bus should remain advance-only, add a comment to `PhaseEventCallback` explicitly stating that and gate the filter at `ListEvents` level.

### Scope and Coupling Observations (No Issue — Healthy)

- `handler_hook.go` correctly uses `context.Background()` for the goroutine's context rather than the parent invocation context. This prevents the hook from being cancelled when the parent command exits, which is the right behavior for a detached side-effect.
- The `events.go` cursor uses the existing `state` package for persistence. This is good reuse and avoids a new table. The 24h TTL for cursor auto-cleanup is appropriate.
- `ListEvents`'s `WHERE (run_id = ? OR ? = '') AND id > ?` pattern handles the `--all` case inline. The `?=''` trick passes `runID` twice. This works but the comment in `store.go` should note that the `allRuns` path should use `ListAllEvents` instead — the `ListEvents` function should not accept empty `runID` as a "match all" sentinel without documentation.

---

## Section 2: Pattern Analysis

### Dual-Cursor Design (Correct)

The design uses separate `sincePhaseID` and `sinceDispatchID` int64 cursors because `phase_events` and `dispatch_events` have independent `AUTOINCREMENT` sequences. This is the correct approach. Using a single cursor across a `UNION ALL` of two AUTOINCREMENT sequences would conflate the ID spaces and produce incorrect filtering. The test `TestListEvents_DualCursorsIndependent` covers this explicitly.

### Constructor Injection Pattern (Consistent)

`dispatch.New(db, recorder)` and `Advance(..., callback)` both use nil-safe optional dependencies. All eight call sites in `cmd/ic/dispatch.go` pass `nil` for the recorder because they do not need event recording (they are read-heavy operations or state-neutral queries). Only `cmdRunAdvance` in `run.go` wires a real recorder. This is clean: the capability is opt-in per call site.

### Issue A3: `dispatch_events.dispatch_id` has no FK to `dispatches` (LOW)

All other child tables in the schema use explicit FK references:

- `phase_events`: `run_id TEXT NOT NULL REFERENCES runs(id)`
- `run_agents`: `run_id TEXT NOT NULL REFERENCES runs(id)`
- `run_artifacts`: `run_id TEXT NOT NULL REFERENCES runs(id)`

`dispatch_events.dispatch_id` has no `REFERENCES dispatches(id)`. The consequence is that `ic dispatch prune --older-than` will delete `dispatches` rows but leave orphaned `dispatch_events` rows. Since `dispatch_events` has no prune command of its own, orphaned rows will accumulate indefinitely.

**Smallest viable fix:** Add `REFERENCES dispatches(id) ON DELETE CASCADE` to `dispatch_id` in the v5 schema DDL and add a migration test for it. If the intent is to retain events as a permanent audit log even after dispatch prune, document that explicitly and add a `dispatch_events` prune command to the CLI.

### Issue A5: SpawnHandler is wired nowhere (LOW — YAGNI)

`internal/event/handler_spawn.go` defines:

```go
type AgentQuerier interface {
    ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error)
}

type AgentSpawner interface {
    SpawnByAgentID(ctx context.Context, agentID string) error
}

func NewSpawnHandler(querier AgentQuerier, spawner AgentSpawner, logw io.Writer) Handler
```

`runtrack.Store.ListPendingAgentIDs` satisfies `AgentQuerier`. No `AgentSpawner` implementation exists anywhere in the codebase. `cmdRunAdvance` in `run.go` does not subscribe a `SpawnHandler`.

The AGENTS.md documents the project's CLI-only design decision: "CLI only (no Go library API in v1) — bash hooks shell out to `ic`". Auto-spawn via an event handler would bypass that design by triggering dispatch from within the `ic` process. The handler tests pass, but the handler is unreachable in practice.

Per the project's YAGNI practice, this handler should either be removed or carry a comment like `// Not yet wired; requires AgentSpawner implementation` with a reference to the work item that will complete it.

### Naming Consistency

The `Event.FromState` / `Event.ToState` field names are generic across both phase and dispatch events. The underlying columns are `from_phase`/`to_phase` (in `phase_events`) and `from_status`/`to_status` (in `dispatch_events`). The unified naming in the Go struct is intentional and correct — the `Source` field distinguishes context. This is documented implicitly in the field comment `// from_phase or from_status`. The tradeoff is acceptable.

---

## Section 3: Simplicity & YAGNI

### `ListEvents` SQL `OR ? = ''` Pattern

```sql
WHERE (run_id = ? OR ? = '') AND id > ?
```

The same `runID` argument is bound twice. This works with the `modernc.org/sqlite` driver and is a documented pattern for "optional filter" in SQLite. However, the function signature `ListEvents(ctx, runID, sincePhase, sinceDispatch, limit)` with `runID = ""` as "match all" is not obvious. The more explicit alternative is to use `ListAllEvents` and never call `ListEvents` with an empty `runID`. The CLI code in `events.go` (line 190-194) already routes correctly:

```go
if allRuns || runID == "" {
    events, err = evStore.ListAllEvents(...)
} else {
    events, err = evStore.ListEvents(ctx, runID, ...)
}
```

The `OR ? = ''` path in `ListEvents` is therefore never reached in practice. It is dead code in the current composition. The fix is to remove the `OR ? = ''` branch from `ListEvents` and enforce non-empty `runID` with a guard.

### Issue A4: Cursor key format not documented for `cursor reset` (LOW)

`saveCursor` stores the cursor under `state` key `"cursor"`, scope `"consumer:runID"`. The `cmdEventsCursorReset` command accepts `args[0]` as the full scope and calls `stStore.Delete(ctx, "cursor", args[0])`. This is correct, but the help text says:

```
events cursor reset <name>     Reset a named cursor
```

The `<name>` here is the full composite `consumer:runID` key, not just the consumer name. A user who tries `ic events cursor reset integ-consumer` (without the run ID) will receive "not found" rather than resetting all cursors for that consumer. The integration test at line 2201 already passes the composite key explicitly:

```bash
ic --db="$TEST_DB" events cursor reset "integ-consumer:$EVT_RUN"
```

The fix is either: (a) update the help text to show `<consumer>[:<run_id>]`, or (b) implement a `cursor reset <consumer>` that lists all cursor scopes matching the consumer prefix and deletes them all.

### Event Notifier Error Handling

`Notifier.Notify` returns the first error encountered but callers in `run.go` discard the return value:

```go
evStore.AddDispatchEvent(ctx, ...)
notifier.Notify(ctx, e)   // return value ignored
```

And in `phaseCallback`:

```go
notifier.Notify(ctx, e)   // return value ignored
```

This is intentional and correct per the design: handler errors should not fail the parent operation. The only concern is that silent hook failures are only surfaced via the log handler's stderr output, which is suppressed when `!flagVerbose`. A user who runs `ic run advance` without `--verbose` will never see hook failures. This is a UX tradeoff, not an architectural violation, but worth noting.

### `handler_hook.go` Timeout Constant

`hookTimeout = 5 * time.Second` is a package-level constant with no way to configure it. Given that hooks can call other `ic` commands (which themselves have a `--timeout` flag defaulting to 100ms), 5 seconds is generous. If the intent is to allow hooks to call `ic` sub-commands, the timeout is fine. If future hooks call external services, 5 seconds may be too short. The constant is reasonable for now.

---

## Summary of Findings

| ID | Severity | Title |
|----|----------|-------|
| A1 | MEDIUM | Dispatch event write fires outside transaction — silent loss on DB error |
| A2 | MEDIUM | Phase callback omits block/pause transitions — consumer sees incomplete event stream |
| A3 | LOW | `dispatch_events.dispatch_id` no FK — orphaned rows after `dispatch prune` |
| A4 | LOW | `cursor reset <name>` help text misleading about composite key format |
| A5 | LOW | SpawnHandler is untethered dead code in production path |

### Recommended Sequencing

1. **A1 first:** Move `AddDispatchEvent` inside `UpdateStatus` transaction. This is a correctness fix before any consumers depend on a complete event stream.
2. **A2 second:** Decide whether block/pause events belong on the bus (fire callback from all exit paths) or are explicitly excluded (document and filter). Either answer is valid; the current silence is the problem.
3. **A3, A4, A5:** Low-risk, address in the next schema/doc pass.

### What Is Working Well

- Dual-cursor design is correct and well-tested.
- `internal/event` has no upward dependencies — it knows nothing about phase or dispatch internals.
- Hook handler goroutine detachment is appropriate for the SQLite single-connection constraint.
- Cursor persistence via the existing `state` package avoids a new table.
- Constructor injection pattern is consistent with the rest of the codebase.
- Eight new integration tests cover the primary consumer scenarios.
- The `dispatch.New(db, nil)` nil-safe pattern ensures all existing dispatch callsites are unaffected.
