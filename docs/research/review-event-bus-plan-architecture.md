# Architecture Review: Intercore Event Bus Plan (iv-egxf)

**Plan reviewed:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-event-bus.md`
**Codebase:** `/root/projects/Interverse/infra/intercore/`
**Reviewer role:** Flux-drive Architecture & Design Reviewer
**Date:** 2026-02-18

---

## Review Mode

Codebase-aware. Documentation read: `CLAUDE.md` (root, projects, intercore), `AGENTS.md` (intercore), `internal/db/schema.sql`, and all existing Go source files in `infra/intercore/`.

---

## What the Plan Adds

The plan introduces an event bus to intercore across 10 tasks:

- **Task 1:** `internal/event/event.go` — unified `Event` type; `internal/event/store.go` — `EventStore` reading from a new `dispatch_events` table (schema v5) UNION'd with existing `phase_events`; tests.
- **Task 2:** `internal/event/notifier.go` — in-process `Notifier` with `Subscribe`/`Notify` and `sync.RWMutex`; tests.
- **Task 3:** Wire the Notifier into `phase.Advance()` via a callback func, and into `dispatch.UpdateStatus` via a recorder field.
- **Tasks 4-6:** Three handler implementations: log, auto-spawn, shell hook.
- **Task 7:** `ic events tail` CLI command with cursor persistence in the existing `state` table.
- **Task 8:** Bash library wrappers in `lib-intercore.sh` (version bump to 0.6.0).
- **Task 9:** Register all handlers at CLI startup and bump binary to 0.3.0.
- **Task 10:** Integration test additions.

---

## Boundaries and Coupling Analysis

### Package dependency graph (proposed)

```
cmd/ic/events.go
  → internal/event (EventStore, Notifier, handlers)
  → internal/state (cursor persistence)

cmd/ic/run.go (Advance wiring)
  → internal/phase (PhaseEventCallback type)
  → internal/event (Event struct, Notifier)
  → internal/runtrack (AgentQuerier impl)
  → internal/dispatch (AgentSpawner impl)

internal/event/
  → database/sql (store.go)
  → no imports from phase, dispatch, runtrack, state

internal/phase/machine.go
  → PhaseEventCallback (func type, no event import)
```

The dependency graph is correct in the final resolved form. The `phase` package does not import `event`. The `event` package does not import `phase`, `dispatch`, or `runtrack`. The CLI layer (`cmd/ic`) is the integration point, which is consistent with the project's existing pattern where `cmd/ic/run.go` already imports and wires `phase`, `runtrack`, and `dispatch` together.

### Existing patterns confirmed

The project uses constructor injection (`New(db *sql.DB) *Store`) everywhere. It uses store-per-command instantiation (each CLI command creates its own store from `openDB()`). Internal packages expose narrow interfaces (`RuntrackQuerier`, `VerdictQuerier` in `phase/gate.go`) that are satisfied by concrete stores from other packages, bridged at the CLI layer. The plan follows this pattern correctly for the callback and the new handler interfaces.

---

## Issue Analysis

### A1 — EventNotifier interface / PhaseEventCallback: indecisive plan text risks circular import

**Severity: MEDIUM**

Task 3, Step 1 first introduces `EventNotifier interface { Notify(ctx context.Context, e interface{}) error }` in `phase/machine.go`, which would require importing `event.Event` (since the signature is `interface{}`-typed, callers still need `event` to construct the value). The plan then self-corrects to `PhaseEventCallback func(runID, eventType, fromPhase, toPhase, reason string)`.

The correction is right. The risk is that the first approach remains in the plan text as an unretracted option. An implementer reading the plan sequentially could apply the interface definition before noticing the correction, then import `event.Event` into `phase/machine.go`. Since `internal/event/store.go` queries `phase_events` (a table whose schema is owned by the `phase` package's DDL), a `phase` → `event` dependency combined with any future `event` → `phase` import (e.g., importing phase constants) would create a circular dependency.

The plan should retract the interface option and present only the callback approach. The callback approach also respects the project's established pattern: `phase/gate.go` already defines `RuntrackQuerier` and `VerdictQuerier` as narrow string/bool interfaces to avoid cross-package imports.

**Evidence:** Plan Task 3, Step 1 contains two conflicting code blocks for `Advance()` signature and explicitly notes "Actually, the cleaner approach."

**Fix:** Delete the `EventNotifier interface` block from the plan. Keep only `PhaseEventCallback func(runID, eventType, fromPhase, toPhase, reason string)`.

---

### A2 — SetEventRecorder is the only post-construction mutator; violates codebase convention

**Severity: MEDIUM**

Task 3, Step 2 proposes adding `SetEventRecorder(fn func(...))` to `dispatch.Store` as a post-construction setter. This is structurally unlike every other store in the codebase:

```go
// ALL existing stores: fixed struct, no setters
func New(db *sql.DB) *Store { return &Store{db: db} }
```

The `SetEventRecorder` pattern introduces:
1. A window of partial initialization: between `dispatch.New(db)` and `SetEventRecorder(fn)`, the store has a nil recorder. Any `UpdateStatus` call in that window silently skips recording.
2. An inconsistency: call sites that do not pass a recorder (e.g., direct tests) get different behavior than wired-up call sites.
3. A future concurrency hazard: if `SetEventRecorder` is ever called while `UpdateStatus` is running (the single-writer SQLite constraint protects the DB, but not the `eventRecorder` field), a data race exists.

**Fix:** Pass the recorder in the constructor:
```go
func New(db *sql.DB, recorder func(dispatchID, runID, fromStatus, toStatus string)) *Store {
    return &Store{db: db, eventRecorder: recorder}
}
```
All existing `dispatch.New(db)` call sites pass `nil` for the recorder. The CLI call site in `cmd/ic/dispatch.go` and `cmd/ic/run.go` pass the real recorder. This is identical to how `phase.Advance()` accepts a nil-able `PhaseEventCallback`.

**Evidence:** `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch.go:74-76` — `func New(db *sql.DB) *Store { return &Store{db: db} }`.

---

### A3 — Dual-cursor UNION with --since=N is semantically broken

**Severity: MEDIUM**

`EventStore.ListEvents` takes `sincePhaseID int64` and `sinceDispatchID int64` as separate AUTOINCREMENT cursors from two independent tables. In `cmdEventsTail` (Task 7), the `--since=N` flag assigns the same `N` to both:

```go
sincePhase = n
sinceDispatch = n
```

`phase_events` and `dispatch_events` are separate AUTOINCREMENT sequences. There is no guarantee that ID 42 in `phase_events` is temporally or causally related to ID 42 in `dispatch_events`. A deployment that has processed 1000 phase events but only 10 dispatch events would, on `--since=50`, return no dispatch events at all (sinceDispatch=50 exceeds the max ID of 10) and only phase events above ID 50.

The named `--consumer` cursor mechanism correctly handles this: `loadCursor` and `saveCursor` maintain separate `phase` and `dispatch` watermarks in JSON. The `--since` flag is a broken shortcut that cannot be made correct without additional context.

**Fix (option A — minimal):** Remove `--since=N`. Document that consumers use `--consumer=<name>` for stateful tailing. One-shot reads start from ID 0, which is correct.

**Fix (option B — time-based):** Change `--since` to accept a Unix timestamp and filter on `created_at > since` in both tables. Since both tables use `created_at INTEGER NOT NULL DEFAULT (unixepoch())`, this is table-agnostic and the semantics are clear: "events since this wall-clock second."

Option A is the minimal-change path and keeps the implementation simple.

**Evidence:** Plan Task 7 `cmdEventsTail`, `--since=` block; `store.go` `ListEvents(ctx, runID, sincePhaseID, sinceDispatchID, limit)` signature.

---

### A4 — AgentQuerier.ListPendingAgentIDs: method does not exist in runtrack.Store

**Severity: LOW**

Task 5 defines:
```go
type AgentQuerier interface {
    ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error)
}
```

`runtrack.Store` has `ListAgents(ctx, runID)` (returns all agents) and `CountActiveAgents(ctx, runID)` (counts active). Neither returns a `[]string` of agent IDs filtered by "pending" (not yet dispatched). The plan's file list for Task 5 does not include any modification to `internal/runtrack/store.go`, so the concrete implementation of `AgentQuerier` is missing from the plan.

Additionally, the comment "returns agents with status='active' for a run. (In runtrack, 'active' means not yet completed)" is incorrect — an agent with `status='active'` could be in-flight with a live dispatch. The spawn handler should query for agents that have no `dispatch_id` yet (pre-spawn), not agents that are already running.

**Fix:** Add a method `ListUndispatchedAgentIDs(ctx, runID string) ([]string, error)` to `runtrack.Store` querying `WHERE run_id = ? AND status = 'active' AND dispatch_id IS NULL`. Add `internal/runtrack/store.go` to Task 5's file list. Rename the interface method to match.

**Evidence:** `/root/projects/Interverse/infra/intercore/internal/runtrack/store.go` — no `ListPendingAgentIDs` method.

---

### A5 — Task 9 title misleads; handler wiring is incomplete

**Severity: LOW**

Task 9 is titled "Register All Handlers at CLI Startup" but the code is correctly placed inside `cmdRunAdvance()`. The CLI exits after each invocation — there is no persistent startup. The misleading framing could cause an implementer to add initialization in `main()` that serves no purpose.

More significantly, Task 9 Step 1 creates the notifier but comments out the spawn and hook handler registration with "We'll register them after getting the run info." The concrete types that satisfy `AgentQuerier` and `AgentSpawner` are not specified. An implementer must infer that `runtrack.Store` and a closure over `dispatch.Store` satisfy them, but the plan does not show the adapter code. The task is structurally incomplete for compilation.

**Fix:** Step 1 should show the complete handler registration block including the `runtrack.Store` as `AgentQuerier` (with the new `ListUndispatchedAgentIDs` method from A4's fix) and a closure over `dispatch.Store` as `AgentSpawner`. The title should read "Register Handlers in cmdRunAdvance."

---

### A6 — intercore_events_cursor_get reads cursor format directly, not via CLI

**Severity: INFO**

Task 8 defines:
```bash
intercore_events_cursor_get() {
    $INTERCORE_BIN state get "cursor" "$consumer" 2>/dev/null || echo ""
}
```

This reads the cursor's raw JSON (`{"phase":N,"dispatch":N}`) directly via the state API, bypassing any format abstraction. If the cursor format changes (likely if A3's fix adds a `wall_clock` field), this wrapper breaks silently. The correct approach is to expose `ic events cursor get <consumer>` as a proper subcommand that owns the output format. The `ic events cursor list` command already exists in the plan; a `get` variant adds minimal code.

This is informational because direct `state get` access for known keys is already a pattern in the bash library, and the cursor format is stable for the foreseeable scope.

---

## Pattern Analysis

### Patterns correctly followed

- **Store-per-command instantiation:** `evStore := event.NewStore(d.SqlDB())` inside each command function matches the existing style exactly.
- **Interface bridging at CLI layer:** `AgentQuerier`/`AgentSpawner` interfaces defined in `event` package, satisfied by concrete stores in the CLI layer — mirrors how `RuntrackQuerier` and `VerdictQuerier` are bridged in `cmd/ic/run.go`.
- **Fire-and-forget errors from handlers:** Returning `firstErr` from `Notify()` for logging purposes without blocking subsequent handlers matches the project's pattern for non-critical side effects.
- **Nil-safe optional parameters:** `if notifier != nil` / `if callback != nil` guards are consistent with how `rt` and `vq` are handled in `phase.Advance()`.
- **UNION ordering by created_at:** Consistent with the project's use of `INTEGER NOT NULL DEFAULT (unixepoch())` timestamps across all tables.
- **Cursor in state table:** Reusing the existing `state` KV store for cursor persistence (with TTL for auto-cleanup) is exactly the right reuse of existing infrastructure rather than adding a new table.

### Anti-patterns flagged

- `SetEventRecorder` post-construction mutator (see A2).
- Dual-meaning `--since` flag (see A3).

### Naming

The `event` package name is clear and non-colliding. `SourcePhase` / `SourceDispatch` constants follow the project's constant naming style. `PhaseEventCallback` is consistent with `PhaseEvent` already in `phase/store.go`.

---

## Simplicity and YAGNI

### Justified complexity

- Dual-table UNION: required since phase and dispatch events live in separate tables; a single events table would require schema consolidation outside this plan's scope.
- Two-watermark cursor: necessary given two separate AUTOINCREMENT sequences; the named consumer mechanism handles this correctly.
- `sync.RWMutex` in Notifier: justified since handlers could theoretically be subscribed from different goroutines in future; the copy-on-read pattern is standard and correct.

### Potentially premature

- Shell hook handler (Task 6): this is a full-featured extensibility point with filesystem stat, permission check, JSON marshaling, and subprocess execution — for a feature with zero current hook users. It is net-positive if hooks are a first-class Clavain concept (`.clavain/hooks/` directory), but it adds ~80 lines of production code plus 5 tests for speculative use. It is a small surface and the pattern is consistent with how Clavain hooks work elsewhere in the ecosystem, so this is acceptable.
- `AgentSpawner` interface (Task 5): has exactly one planned consumer (the spawn handler). New abstractions with a single concrete caller are borderline YAGNI. However, it correctly decouples `event` from `dispatch`, so the interface is warranted for the layering goal.

### Cursor subcommand (Task 7)

`ic events cursor list` and `ic events cursor reset` are the right operations. No concerns.

---

## Schema Analysis

The `dispatch_events` table design is clean:

```sql
CREATE TABLE IF NOT EXISTS dispatch_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id TEXT NOT NULL,
    run_id      TEXT,
    from_status TEXT NOT NULL,
    to_status   TEXT NOT NULL,
    event_type  TEXT NOT NULL DEFAULT 'status_change',
    reason      TEXT,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
```

- No FK on `dispatch_id`: correct, dispatches can be pruned while events are retained.
- Nullable `run_id`: correct, not all dispatches have a run scope.
- Three indexes match the query patterns in `ListEvents` and `ListAllEvents`.
- `DEFAULT 'status_change'` for `event_type`: consistent with `phase_events` defaulting to `'advance'`.

One observation: the UNION query in `ListAllEvents` filters `dispatch_events` on `WHERE id > ?` but does not filter by `run_id` (correct — it's the "all runs" variant). The `ORDER BY created_at ASC, source ASC, id ASC` secondary tiebreak on `source` causes all dispatch events to sort before phase events within the same second (`d` < `p`). This is a minor ordering inconsistency since phase transitions logically precede dispatch spawns, but it does not affect correctness.

---

## Integration Risk Assessment

The plan is low integration risk with the three MEDIUM issues resolved:

1. If A1 is resolved (callback only, no interface): zero circular import risk.
2. If A2 is resolved (constructor injection): existing `dispatch.New` call sites require a one-line change to add `nil` parameter; no behavior change.
3. If A3 is resolved (remove `--since` or use wall-clock): the CLI contract is unambiguous.

The schema migration from v4 to v5 is append-only (`CREATE TABLE IF NOT EXISTS` + three indexes) and is safe under the existing migration pattern. No existing queries are affected.

The `Advance()` signature change (adding callback parameter) requires updating all call sites. The plan identifies `cmd/ic/run.go` and `internal/phase/machine_test.go`. A grep for additional call sites is warranted before implementation — `gate.go` (`EvaluateGate`) does not call `Advance` directly (confirmed by reading `gate.go`), so the plan's call site list is complete.

---

## Verdict

**needs-changes**

Three issues require resolution before implementation: A1 (retract interface option from plan), A2 (constructor injection for recorder), A3 (fix or remove `--since` flag). A4 (missing runtrack method) requires adding one method to `runtrack/store.go` to the file list. Once addressed, the plan is architecturally sound and consistent with the intercore codebase's conventions.
