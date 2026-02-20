# Correctness Review: Event Bus Implementation Plan
**Plan:** `docs/plans/2026-02-18-intercore-event-bus.md`
**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-18

## Invariants Established Before Review

1. `phase_events.id` and `dispatch_events.id` are independent AUTOINCREMENT sequences — integer equality between them is meaningless.
2. `SetMaxOpenConns(1)` means there is exactly one live SQLite connection per `ic` invocation; no goroutine can hold two concurrent DB connections.
3. Notifier handlers are called synchronously in registration order; the call stack blocks until all handlers return.
4. `UpdatePhase` and `AddEvent` are separate, un-transacted single-statement executions — there is no atomicity guarantee across them.
5. The CLI is a short-lived process (not a daemon); the Notifier lifecycle is bounded to a single command invocation.
6. Shell hooks are external processes that may themselves call `ic`, creating re-entrant DB access patterns.
7. `PRAGMA foreign_keys = ON` is enforced; any INSERT that violates a FK will fail hard.
8. The pre-migration backup is the only rollback mechanism for schema changes.

---

## High-Severity Findings

### C-01 — HIGH: Unified `--since` cursor collapses two independent ID sequences

**Location:** Task 7, `events.go` lines ~1347–1355, ~1421–1427

**Invariant violated:** Invariant #1. `phase_events.id` and `dispatch_events.id` are independent sequences.

**Failure narrative:**

Consider a database that has accumulated 10 phase events (IDs 1–10) and 50 dispatch events (IDs 1–50). An operator runs:

```
ic events tail --all --consumer=monitor
```

The initial poll calls `ListAllEvents(ctx, 0, 0, 1000)` which returns all 60 events ordered by `created_at ASC`. In the result set, dispatch events dominate by count. After processing, the tracking loop executes:

```go
for _, e := range events {
    enc.Encode(e)
    if e.Source == event.SourcePhase && e.ID > sincePhase {
        sincePhase = e.ID         // correctly advances only for phase events
    }
    if e.Source == event.SourceDispatch && e.ID > sinceDispatch {
        sinceDispatch = e.ID      // correctly advances only for dispatch events
    }
}
```

The per-source guards in the loop are correct. The cursor is saved as `{"phase":10,"dispatch":50}`.

Now consider the user who passes `--since=5`. Lines 1347–1355:

```go
case strings.HasPrefix(args[i], "--since="):
    val := strings.TrimPrefix(args[i], "--since=")
    n, err := strconv.ParseInt(val, 10, 64)
    ...
    sincePhase = n
    sinceDispatch = n
```

Both cursors are set to 5. The user intended "phase events after ID 5." The query runs:
- `phase_events WHERE id > 5` — returns phase events 6–10. Correct.
- `dispatch_events WHERE id > 5` — returns dispatch events 6–50, silently skipping dispatch events 1–5.

The user does not know that "5" in phase_events is unrelated to "5" in dispatch_events. There is no documentation or validation warning. If dispatch events 1–5 contain a critical spawn-failure record, it is silently omitted.

The `loadCursor` function correctly uses separate `cursor.Phase` and `cursor.Dispatch` fields and is not broken. The bug is isolated to the `--since` initialization path and the absence of a clear API contract.

**Fix:** Split `--since` into `--since-phase=N` and `--since-dispatch=N`, or change `--since` to accept a Unix timestamp (comparable across both tables via `created_at`). Document that the integer namespace is per-table and non-comparable.

---

### C-02 — HIGH: Notifier fires after `AddEvent` but outside any DB transaction — phantom state risk

**Location:** Task 3, Step 1 — callback fires after `store.AddEvent` returns in `machine.go`

**Invariant violated:** Invariant #4. `UpdatePhase` and `AddEvent` are separate un-transacted statements.

**Failure narrative:**

After a successful advance in `Advance()`:

```
store.UpdatePhase(ctx, runID, fromPhase, toPhase)  // Statement 1: committed immediately
store.AddEvent(ctx, &PhaseEvent{...})               // Statement 2: committed immediately
callback(runID, eventType, ...)                     // Notifier fires HERE
```

Both writes are committed before the Notifier fires — that part is safe for the phase case.

The dangerous case is the dispatch wiring (Task 3, Step 2). The plan places the event recorder call at the END of `UpdateStatus`:

```go
result, err := s.db.ExecContext(ctx, query, args...)  // UPDATE dispatches ... committed
...
if s.eventRecorder != nil {
    d, _ := s.Get(ctx, id)                            // Second DB call: separate read
    runID := ""
    if d != nil && d.ScopeID != nil {
        runID = *d.ScopeID
    }
    s.eventRecorder(id, runID, "", status)            // INSERT dispatch_events
}
```

There are two commits here: one for the UPDATE and one for the INSERT. They are NOT atomic. Crash between them = status updated, event not recorded. The read (`s.Get`) between the two writes is also a TOCTOU: another process could prune the dispatch record between the UPDATE and the GET, making `d` nil and preventing the event from ever being recorded (runID will be `""`).

**Fix:** Wrap both the `UPDATE dispatches` and the `INSERT dispatch_events` in a single transaction. Since `SetMaxOpenConns(1)` is in use, nested transactions aren't possible, but the entire `UpdateStatus` operation can begin a transaction, execute both statements, and commit. This is consistent with the WAL protocol patterns in `docs/guides/data-integrity-patterns.md`.

---

### C-03 — HIGH: dispatch event recorder always passes empty `fromStatus`

**Location:** Task 3, Step 2, end of `UpdateStatus` wiring

**Invariant violated:** `dispatch_events.from_status TEXT NOT NULL` — the column must contain a meaningful value.

**Analysis:**

The recorder closure as written:

```go
s.eventRecorder(id, runID, "", status)
```

The third argument is the literal empty string `""`. This becomes `from_status = ""` in `dispatch_events`. The column is `TEXT NOT NULL`, so no constraint violation occurs (empty string satisfies NOT NULL), but the audit trail is semantically invalid. Every dispatch event row will show `from_status = ""` regardless of the actual previous state.

The `UpdateStatus` function receives only the new `status` — it does not have the old status. The caller (spawn, poll, collect, kill in CLI dispatch commands) knows the previous state implicitly but does not pass it to `UpdateStatus`.

A consumer replaying the dispatch event log to reconstruct state transitions cannot determine from the log what state each dispatch was in before a transition. The log is an append-only record, and without `from_status`, every event is informationally equivalent to `INSERT INTO dispatches (status=X)`, losing the transition semantics entirely.

**Fix (option A — preferred):** Read the previous status inside `UpdateStatus` within a BEGIN IMMEDIATE transaction:

```go
func (s *Store) UpdateStatus(ctx context.Context, id, status string, fields UpdateFields) error {
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil { return ... }
    defer tx.Rollback()

    var prevStatus string
    tx.QueryRowContext(ctx, "SELECT status FROM dispatches WHERE id = ?", id).Scan(&prevStatus)

    // ... perform UPDATE ...

    if s.eventRecorder != nil {
        s.eventRecorder(id, runID, prevStatus, status)  // prevStatus is now accurate
    }
    return tx.Commit()
}
```

**Fix (option B):** Change the `SetEventRecorder` callback signature and all callers to supply the known previous status. This is more invasive but avoids an extra DB read.

---

## Medium-Severity Findings

### C-04 — MEDIUM: Schema v5 bump is irreversible; concurrent v4 binaries fail at DB open

**Location:** Task 1, Step 3 — `db.go` version constants

**Analysis:**

`db.Open()` returns `ErrSchemaVersionTooNew` if `version > maxSchemaVersion`. During the period between `ic init` (which commits schema v5) and all running `ic` processes being upgraded, any v4 binary invocation fails at open time with no retry and no graceful degradation.

This is a narrow race window (CLI, not daemon), but it applies to cron-driven hooks or long-running `--follow` poll processes started before the upgrade. A `--follow` process that was started with a v4 binary and holds the DB open does NOT block the migration (SQLite WAL allows concurrent readers), so the migration can commit while a `--follow` poll is in progress, after which the next poll attempt by the v4 binary gets `ErrSchemaVersionTooNew` and exits with a confusing error.

No schema downgrade path is planned. The pre-migration backup is the only recovery path, and restoring it discards all v5 events.

**Recommendation:** Document the constraint explicitly in the deployment procedure. Add a `-- v5 ROLLBACK:` comment in `schema.sql` explaining that v5 can only be rolled back by restoring the backup.

---

### C-05 — MEDIUM: Cursor saved only on non-empty batches; handlers must be idempotent but aren't documented as such

**Location:** Task 7, `events.go` lines ~1429–1431

```go
if consumer != "" && len(events) > 0 {
    saveCursor(ctx, stStore, consumer, runID, sincePhase, sinceDispatch)
}
```

**Analysis:**

Consider this sequence in `--follow` mode with a named consumer:

1. T=0: Poll returns events 1–5. In-memory high-water: `{phase:5, dispatch:3}`. Cursor saved.
2. T=500ms: Events 6, 7 arrive. Poll returns them. In-memory: `{phase:7, dispatch:3}`. Cursor save begins.
3. Between reading events and writing the cursor, process receives SIGTERM. Cursor write is cancelled (context cancelled). Cursor in DB is still `{phase:5, dispatch:3}`.
4. Consumer restarts. Loads cursor `{phase:5, dispatch:3}`. Re-delivers events 6 and 7.

Re-delivery is expected behavior for an at-least-once consumer. The correctness problem is that the spawn handler (Task 5) calls `spawner.SpawnByAgentID(ctx, id)` for each agent, and the hook handler (Task 6) executes `hookPath` as a shell command. Neither is idempotent. Spawning an agent twice (once before crash, once after restart) starts two agent instances for the same run phase, causing duplicate work, conflicting output files, and potentially conflicting verdicts.

**Fix:** Either (a) document and enforce handler idempotency (spawn should check agent status before spawning), or (b) save the cursor BEFORE processing each event (record-then-process), accepting that on crash some events may be processed twice but at least the cursor is durable.

---

### C-06 — MEDIUM: `--since=N` semantics are undefined across two tables (also see C-01)

(Documented in C-01. Separate ID here because the fix is a CLI API change, not a logic fix.)

---

### C-07 — MEDIUM: Shell hook runs synchronously on Notifier call stack — risk of DB re-entrancy deadlock

**Location:** Task 6, `handler_hook.go`, `NewHookHandler`

**Failure narrative:**

A hook script at `.clavain/hooks/on-phase-advance` contains:

```sh
#!/bin/sh
IC_RESULT=$(ic run status "$run_id" --json)
echo "$IC_RESULT" | process_somehow
```

Execution path:
1. `ic run advance <id>` opens DB connection C1 (the only connection, `SetMaxOpenConns(1)`).
2. `phase.Advance()` completes. Notifier fires hook handler synchronously.
3. Hook handler calls `exec.CommandContext` → spawns child `ic run status <id>`.
4. Child `ic run status` calls `db.Open()` → tries to open the same DB file.
5. SQLite WAL allows concurrent readers. `sql.Open` succeeds. But `SetMaxOpenConns(1)` is per-connection-pool, not per-file. The child process creates its own pool with its own connection — this is fine.
6. The child runs successfully in ~100ms. No deadlock in this case.

**Actual risk:** The hook handler runs within the parent's `cmdRunAdvance` call. The parent holds the DB connection open for the full duration of `cmdRunAdvance`. Any hook that takes > `busy_timeout` (100ms default) while trying to perform a write operation through `ic` will fail with SQLITE_BUSY. The default busy_timeout is 100ms, far less than the 5-second hook timeout. A hook that writes state (`ic state set`) will reliably fail.

More broadly: the 5-second synchronous execution makes `ic run advance` appear to hang for 5 seconds when a hook times out. This is a UX problem that becomes a correctness problem if the caller (cron job, bash hook) has its own timeout shorter than 5 seconds and kills the `ic run advance` process mid-flight, leaving the phase advanced but the event bus notification undelivered.

**Fix:** Run each hook in a separate goroutine with `go func() { ... }()`, or after the parent `ic run advance` returns by writing the pending hook execution to a state entry and processing it in a subsequent call. Detaching from the call stack is the correct model for fire-and-forget.

---

## Low-Severity Findings

### C-08 — LOW: No context cancellation check between Notifier handlers

**Location:** Task 2, `notifier.go`, `Notify()` method

The handler loop does not check `ctx.Done()` between iterations. If context is cancelled mid-dispatch (SIGTERM, deadline), all remaining handlers still execute. For fire-and-forget semantics this is documented as intentional, but it should be explicit.

---

### C-09 — LOW: `AddDispatchEvent` and `UpdateStatus` are separate un-transacted writes

(Secondary aspect of C-02. Tracked separately because it exists regardless of whether the Notifier approach is changed — the recorder pattern itself is non-atomic.)

The existing `UpdateStatus` in `dispatch.go` is a single un-transacted UPDATE. Adding a second write (the event recorder call) after the UPDATE without a transaction wrapper violates the data integrity pattern: the audit event can be lost if the process crashes between the two writes.

---

### C-10 — LOW: `saveCursor` ignores `state.Set` error

**Location:** Task 7, `events.go`, `saveCursor` function

```go
func saveCursor(ctx context.Context, store *state.Store, ...) {
    ...
    store.Set(ctx, "cursor", key, json.RawMessage(payload), 24*time.Hour)
    // error return discarded
}
```

If `state.Set` fails (disk full, DB locked, context cancelled), the cursor position is silently lost. The next run of the consumer re-processes events from the last successfully-saved cursor position. Combined with non-idempotent handlers (C-05), this causes duplicate side effects without any operator visibility.

**Fix:** Return `error` from `saveCursor` and log/surface it in `cmdEventsTail`.

---

## Improvements

**I-01.** Split `--since=N` into per-table flags `--since-phase=N` / `--since-dispatch=N`, or use a Unix timestamp that is comparable across tables. Document that the integer cursor namespace is per-table.

**I-02.** Wrap `UpdateStatus` + `AddDispatchEvent` in a single DB transaction. This matches the WAL protocol guidance in `docs/guides/data-integrity-patterns.md` and makes the dispatch audit trail atomic with the status change.

**I-03.** Capture `prevStatus` inside `UpdateStatus` before the UPDATE, within the same transaction. Eliminate the second `s.Get` call from the recorder closure.

**I-04.** Launch hook handlers in a goroutine detached from the Notifier call stack. Use a copied context with the 5-second timeout applied independently per hook. This unblocks `ic run advance` immediately and prevents the synchronous-call-stack risk described in C-07.

**I-05.** Add an integration test with interleaved phase and dispatch events that verifies: (a) cursor persistence and reload, (b) re-delivery behavior after simulated crash (truncate cursor, re-run), (c) `--since` filtering correctness for both tables independently.

**I-06.** Document the idempotency requirement for all registered handlers in a comment on the `Handler` type and in AGENTS.md. The spawn handler should guard against re-spawning already-running agents; the hook handler should be documented as non-idempotent by default.

**I-07.** Add `go test -race ./internal/event/...` to the CI gate. The `Notifier` uses `sync.RWMutex` correctly, but the handler slice copy under RLock is sound only if `namedHandler` fields are never mutated after registration. A race test would catch any future mutation.

---

## Summary

Three high-severity issues require changes before the plan is safe to implement as written:

1. **C-01** (cursor collapse): The `--since` flag and the polling high-water-mark tracking are semantically broken when applied across two tables with independent ID sequences. Silent event loss is the guaranteed outcome for any user of `--since`.

2. **C-02 + C-09** (non-atomic dispatch recording): The dispatch event recorder is outside the UpdateStatus transaction boundary. A crash between the status update and the event insert produces a status change with no audit record — exactly the scenario the event bus is designed to prevent.

3. **C-03** (empty fromStatus): The recorder always passes `fromStatus=""`, making the entire `dispatch_events` audit trail useless for state-machine reconstruction.

The medium-severity issues (C-07 especially) are real production failure modes under normal hook usage. The low-severity issues are correctness gaps that will surface under operator error conditions (disk full, cursor not saved).

None of these require architectural changes to the plan — they are all fixable within the existing design by: correcting the cursor ID isolation, wrapping the dispatch update in a transaction, capturing `prevStatus`, and detaching hook execution from the synchronous call stack.
