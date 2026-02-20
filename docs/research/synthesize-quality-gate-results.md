# Quality Gate Synthesis: intercore-event-bus Plan Review

**Date:** 2026-02-18
**Plan reviewed:** `docs/plans/2026-02-18-intercore-event-bus.md`
**Agents:** fd-architecture, fd-correctness, fd-quality
**Output dir:** `/root/projects/Interverse/.clavain/quality-gates/`

---

## Validation

All 3 agent output files found and validated:
- `/root/projects/Interverse/.clavain/quality-gates/fd-architecture.md` — Valid (Findings Index present, `Verdict: needs-changes`)
- `/root/projects/Interverse/.clavain/quality-gates/fd-correctness.md` — Valid (Findings Index present, `Verdict: needs-changes`)
- `/root/projects/Interverse/.clavain/quality-gates/fd-quality.md` — Valid (Findings Index present, `Verdict: needs-changes`)

**Validation: 3/3 agents valid, 0 failed**

All three agents independently returned `needs-changes`. No agent returned `safe` or `error`.

---

## Verdict

**Overall verdict: needs-changes**
**Gate: FAIL**

2 P1 (HIGH) findings block implementation as written. 5 P2 (MEDIUM) findings should be resolved before implementation begins. No P0 critical findings.

---

## Findings Index

### P1 — Must fix before implementation

**[P1] Unified cursor collapses independent ID spaces — polling silently misses events**
- Convergence: 3/3 agents (fd-correctness C-01/C-06, fd-architecture A3/A6, fd-quality Q8)
- Section: Task 7 `cmdEventsTail`, Task 1 `ListEvents`
- Root cause: `--since=N` sets `sincePhaseID = n` AND `sinceDispatchID = n` from one command-line integer. `phase_events` and `dispatch_events` use separate AUTOINCREMENT sequences. An integer meaningful in one table is meaningless in the other.
- Failure mode: A poll cycle that sees dispatch events with id=50 advances `sincePhase` to 50 even if the highest phase event ID is 10. Next poll uses `WHERE id > 50` on `phase_events`, silently skipping all phase events 11–50. Events are permanently missed, not re-deliverable.
- Secondary failure: `--since` and `--consumer` are mutually incompatible — passing both gives neither the stored cursor nor the `--since` semantics (silently).
- Fix: Split `--since` into `--since-phase=N` and `--since-dispatch=N`, OR replace with timestamp-based filtering on `created_at` (which IS comparable across both tables).
- Evidence: `events.go` lines ~1347–1355 (`--since` initialization); `store.go` `ListEvents` signature `sincePhaseID, sinceDispatchID int64`; cursor poll loop lines ~1421–1427 (per-source tracking is correct here — initialization is the bug)

**[P1] `fromStatus` always empty string — dispatch audit trail permanently corrupted**
- Convergence: 2/3 agents (fd-correctness C-03, fd-quality Q3)
- Section: Task 3 Step 2, `dispatch.UpdateStatus`
- Root cause: The event recorder closure is called with `fromStatus = ""` (literal empty string). `UpdateStatus` takes only the new status as a parameter and never reads the previous state. Every `dispatch_events.from_status` will be `""`.
- Failure mode: The audit trail is permanently corrupted. State-machine replay, diffing, and debugging using `from_status` all receive garbage data. The `NOT NULL` constraint is satisfied by `""` so no error is surfaced — the corruption is entirely silent.
- Fix: Either (a) `UpdateStatus` reads previous status within a transaction before the UPDATE (`SELECT status FROM dispatches WHERE id = ?`), or (b) callers pass `fromStatus` as a parameter. Option (b) preferred — callers often already know the previous state, and it avoids an extra DB round-trip.
- Evidence: `dispatch.go` Task 3 Step 2: `s.eventRecorder(id, runID, "", status)` — third argument is hardcoded `""`; schema `from_status TEXT NOT NULL`

### P2 — Should fix before implementation

**[P2] SetEventRecorder is a post-construction mutator — violates constructor-injection and introduces data race**
- Convergence: 2/3 agents (fd-architecture A2, fd-quality Q2)
- Section: Task 3 Step 2
- Root cause: Every store in the codebase uses fixed-field structs with a single constructor. `SetEventRecorder(fn func(...))` is the only post-construction mutator in the entire codebase. `dispatch.Store` methods are called concurrently; assigning a field after construction without a mutex is a data race under Go's memory model.
- Fix: Pass the recorder function through `dispatch.New`: `func New(db *sql.DB, recorder func(dispatchID, runID, fromStatus, toStatus string)) *Store`. Call sites that do not need recording pass `nil`.
- Evidence: `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch.go:74` — `func New(db *sql.DB) *Store { return &Store{db: db} }` — all other stores follow this shape

**[P2] Shell hook runs synchronously — blocks advance command and risks SQLite deadlock**
- Convergence: 1/3 agents (fd-correctness C-07)
- Section: Task 6, `handler_hook.go`
- Root cause: `NewHookHandler` runs `exec.CommandContext` inline within the synchronous Notifier call stack with a 5-second timeout. `ic` uses `SetMaxOpenConns(1)`. If a hook calls back into `ic` (e.g., `ic run status`), that subprocess hits `SQLITE_BUSY` because the parent holds the one connection. Parent waits for hook; hook waits for DB — deadlock.
- Fix: Run hooks in a detached goroutine with an independent context and timeout. The hook subprocess must not share the parent process's DB connection.
- Evidence: Task 6 `handler_hook.go`, `Notifier.Notify` synchronous call pattern, `SetMaxOpenConns(1)` in `db.go`

**[P2] EventNotifier interface left in plan alongside callback type — risks circular import**
- Convergence: 2/3 agents (fd-architecture A1, fd-quality Q1)
- Section: Task 3 Step 1
- Root cause: The plan defines `EventNotifier interface { Notify(ctx context.Context, e interface{}) error }` in `phase/machine.go`, then immediately discards it in favor of `PhaseEventCallback`. Both variants remain in plan text. If an implementor uses the interface variant and imports `event.Event` into `phase`, a `phase` → `event` circular import is created (since `event.Store` queries `phase`-owned tables).
- Additional issue: The interface uses `interface{}` instead of a concrete type — a code quality defect independent of the circular import risk.
- Fix: Delete the interface variant from the plan entirely. Only `PhaseEventCallback` should remain.
- Evidence: Task 3 Step 1 plan text contains both definitions

**[P2] AddDispatchEvent executes outside UpdateStatus transaction — silent audit gap on crash**
- Convergence: 1/3 agents (fd-correctness C-09, with C-02 as context)
- Section: Task 3 `dispatch.UpdateStatus`
- Root cause: `UPDATE dispatches SET status = ?` and `INSERT INTO dispatch_events` are two separate un-transacted operations. Process crash between them leaves status updated but no event recorded.
- Fix: Wrap both operations in a single transaction inside `UpdateStatus`. `SetMaxOpenConns(1)` means this is serialized and safe.
- Evidence: `docs/guides/data-integrity-patterns.md` WAL protocol; Task 3 Step 2 implementation — no transaction wrapping shown

**[P2] Schema v5 bump has no downgrade path — old binary on new DB panics at open**
- Convergence: 1/3 agents (fd-correctness C-04)
- Section: Task 1, schema migration
- Root cause: `db.Open()` returns `ErrSchemaVersionTooNew` for old binaries. During rolling upgrade, any cron job or hook using the old binary fails at DB open. The migration is not gated on checking for concurrent writers. Pre-migration backup is the only recovery; restoring it discards all v5 events.
- Fix: This is acceptable risk for a single-process CLI tool. Document the deployment requirement (no concurrent processes during `ic init`) explicitly in the plan and ensure the error message provides actionable guidance.
- Evidence: `db.go` schema version check logic

### P3/IMP — Nice to have

| ID | Convergence | Title |
|----|-------------|-------|
| C-05 | 1/3 | Cursor saved only on non-empty batches — handlers must be idempotent, not documented |
| C-10/Q7 | 2/3 | saveCursor silently ignores state.Set error |
| A4 | 1/3 | AgentQuerier.ListPendingAgentIDs requires new runtrack.Store method not in plan scope |
| A5 | 1/3 | Task 9 title misleads — "Register at CLI Startup" means "register before calling Advance" |
| Q6 | 1/3 | PhaseEventCallback omits gate result and gate tier from AdvanceResult |
| Q5 | 1/3 | MaxPhaseEventID / MaxDispatchEventID exported but only used by CLI |
| Q9 | 1/3 | Hook handler serializes event.Event as map[string]string — loses type fidelity |
| A7 | 1/3 | intercore_events_cursor_get bash wrapper bypasses ic CLI, reads state directly |

---

## Conflicts

**Partial conflict: Notifier transaction semantics (fd-correctness C-02 vs fd-architecture intent)**

fd-correctness flags that the Notifier fires outside a transaction as a correctness risk. fd-architecture notes this is intentional fire-and-forget design but flags the dispatch event recorder (separate INSERT) as the real atomicity problem.

Resolution: Both are right. The Notifier's fire-and-forget is acceptable design; the dispatch event INSERT not being in the same transaction as the UPDATE is the real issue (see P2 finding C-09 above). Address C-09 / I-02 from fd-correctness.

No other agent disagreements detected.

---

## Compact Summary (host agent return value)

```
Validation: 3/3 agents valid
Verdict: needs-changes
Gate: FAIL
P0: 0 | P1: 2 | P2: 5 | IMP: 8
Conflicts: 1 (resolved — C-02 vs intent, see synthesis)
Top findings:
- P1 Unified cursor collapses independent ID spaces — polling silently misses events — fd-correctness/fd-architecture/fd-quality (3/3)
- P1 fromStatus always empty string — dispatch audit trail permanently corrupted — fd-correctness/fd-quality (2/3)
- P2 SetEventRecorder post-construction mutator — data race + violates constructor-injection — fd-architecture/fd-quality (2/3)
- P2 Shell hook synchronous on call stack — SQLite deadlock risk — fd-correctness (1/3)
- P2 EventNotifier interface left in plan — circular import trap + interface{} type — fd-architecture/fd-quality (2/3)
```

---

## Output Files Written

- `/root/projects/Interverse/.clavain/quality-gates/synthesis.md` — full human-readable synthesis report
- `/root/projects/Interverse/.clavain/quality-gates/findings.json` — structured findings data
- `/root/projects/Interverse/.clavain/verdicts/fd-architecture.json` — NEEDS_ATTENTION
- `/root/projects/Interverse/.clavain/verdicts/fd-correctness.json` — NEEDS_ATTENTION
- `/root/projects/Interverse/.clavain/verdicts/fd-quality.json` — NEEDS_ATTENTION
