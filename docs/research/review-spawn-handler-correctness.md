# Correctness Review: SpawnHandler Closure + UpdateAgentDispatch

**Reviewer:** Julik (Correctness Reviewer)
**Date:** 2026-02-18
**Files reviewed:**
- `/root/projects/Interverse/infra/intercore/cmd/ic/run.go` (lines 237–281)
- `/root/projects/Interverse/infra/intercore/internal/runtrack/store.go` (UpdateAgentDispatch)
- `/root/projects/Interverse/infra/intercore/internal/event/notifier.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/machine.go`
- `/root/projects/Interverse/infra/intercore/internal/dispatch/spawn.go`

---

## Invariants That Must Hold

Before naming bugs, we name the rules they break.

1. **Single-connection invariant**: `SetMaxOpenConns(1)` means every SQLite operation — reads, writes, transactions — is serialized through a single connection. Any operation that needs the connection while the connection is already in use will block until the operation times out, or deadlock if the call stack owns the connection non-reentrantly.

2. **Callback-fires-after-commit**: The AGENTS.md documents that `PhaseEventCallback` fires "after DB commit (fire-and-forget)". The implementation must actually enforce this, or the callback fires during a live write or under a lock.

3. **No nested writes on the same connection during a live statement**: SQLite does not support multiplexed use of a single connection. With `MaxOpenConns(1)`, all `ExecContext`/`QueryRowContext` calls queue on the one connection via `database/sql`'s connection pool. If the pool is empty (connection is in use by an ongoing prepared statement execution or `rows.Next()` loop), the next caller blocks until the busy timeout fires.

4. **Spawn is a side-effectful OS operation**: Once `cmd.Start()` is called, the child process is running whether or not the subsequent DB write (`UpdateAgentDispatch`) succeeds. If the DB write fails, the dispatch record exists and the process is running, but the agent record has no `dispatch_id`. This is an orphan spawn.

5. **Agent state machine**: `run_agents.status` transitions from `active` → `completed|failed`. A re-spawned agent may still be in `active` status from a prior dispatch. No guard prevents spawning an already-running agent.

---

## Finding 1: CRITICAL — Synchronous DB Calls Inside Notify, Triggering SQLite Deadlock

**Severity: Critical — will deadlock on every advance that reaches "executing".**

### The call chain

```
phase.Advance()
  └─ store.UpdatePhase()         # ExecContext on the single connection
  └─ store.AddEvent()            # ExecContext on the single connection
  └─ store.UpdateStatus()        # ExecContext (if toPhase == done)
  └─ callback()                  # fires synchronously here
       └─ notifier.Notify()
            └─ SpawnHandler (synchronous)
                 └─ querier.ListPendingAgentIDs()   # QueryContext — NEEDS connection
                 └─ spawner.SpawnByAgentID()
                      └─ rtStore.GetAgent()          # QueryRowContext — NEEDS connection
                      └─ dStore.Get()                # QueryRowContext — NEEDS connection
                      └─ dispatch.Spawn()
                           └─ store.Create()         # ExecContext — NEEDS connection
                           └─ store.UpdateStatus()   # ExecContext — NEEDS connection
                      └─ rtStore.UpdateAgentDispatch() # ExecContext — NEEDS connection
```

The AGENTS.md states:

> "Callbacks fire **after DB commit** (fire-and-forget). Handler errors are logged but never fail the parent operation. **The hook handler runs in a detached goroutine** with `context.Background()` and a 5s timeout to avoid blocking the single DB connection."

The hook handler (`HookHandler`) runs in a goroutine. But `SpawnHandler` is NOT the hook handler. The notifier calls all handlers **synchronously**:

```go
// notifier.go, line 46
for _, nh := range handlers {
    if err := nh.handler(ctx, e); err != nil {
```

`SpawnHandler` returns a plain `Handler` func. It is not wrapped in a goroutine. So when `phaseCallback` calls `notifier.Notify()`, execution enters `SpawnHandler` on the same goroutine, on the same call stack that just finished writing to SQLite.

At this point in `phase.Advance()` (line 175 of machine.go), the call to `callback()` occurs after `store.AddEvent()` and (if needed) `store.UpdateStatus()` have returned — so those individual `ExecContext` calls have completed and released the connection back to the pool.

**This means: the connection IS free at the moment `callback()` fires. There is no deadlock from phase.Advance's own writes.**

However, the analysis does not end there.

### The actual problem: the dispatchRecorder fires INSIDE Spawn

`dispatch.Spawn()` calls `store.UpdateStatus()` (lines 115-118 of spawn.go):

```go
store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
    "pid":        pid,
    "started_at": now,
})
```

`dispatch.Store.UpdateStatus` calls the `eventRecorder` after the DB write:

```go
// dispatch.go (not fully read, but documented in AGENTS.md):
// "dispatch.UpdateStatus → DispatchEventRecorder → Notifier.Notify() → handlers"
```

The `dispatchRecorder` closure (run.go lines 223–236) does:

```go
dispatchRecorder := func(dispatchID, runID, fromStatus, toStatus string) {
    // ...
    if err := evStore.AddDispatchEvent(ctx, ...); err != nil {  // DB write
    notifier.Notify(ctx, e)                                      // calls SpawnHandler again
}
```

So the call graph during `SpawnHandler` execution is:

```
SpawnHandler
  └─ spawner.SpawnByAgentID()
       └─ dispatch.Spawn()
            └─ store.Create()          # ExecContext — connection acquired + released
            └─ cmd.Start()             # process started
            └─ store.UpdateStatus(running)
                 └─ dispatchRecorder() # synchronous callback
                      └─ evStore.AddDispatchEvent()   # ExecContext — connection needed
                      └─ notifier.Notify()            # SpawnHandler invoked again
                           └─ querier.ListPendingAgentIDs()  # QueryContext
```

Each individual `ExecContext`/`QueryContext` acquires the single connection, executes, and releases it before the next call can proceed. With `database/sql` and `MaxOpenConns(1)`, calls block on acquiring the connection but do not deadlock as long as no single goroutine holds the connection open while synchronously blocking on another acquire from the same goroutine.

**Go's `database/sql` does NOT reenter the same goroutine's connection.** If goroutine G holds a `*sql.Conn` (or an open transaction), and G then calls `db.ExecContext`, `db` will try to get a free connection. With `MaxOpenConns(1)` and one connection already held by G, this blocks until the busy timeout fires and returns `context deadline exceeded`.

**This scenario arises with `db.Tx` (explicit transactions)**, not with individual `db.ExecContext` calls, which release the connection immediately after execution.

### Verdict on deadlock

For the code as written — no explicit `BEGIN TRANSACTION` wrapping `Advance()`, all writes use individual `ExecContext` calls that each acquire and release the connection atomically — **there is no deadlock from SQLite's single-connection pool**.

Each `ExecContext` completes before the next one runs. `database/sql` queues them sequentially. The "busy_timeout" PRAGMA handles any write-write contention from external processes (there are none here since this is a single CLI process).

**However:** the AGENTS.md claim that callbacks fire "after DB commit" is misleading. The phase writes are autocommit (not explicit transactions). There is no single transaction wrapping `UpdatePhase + AddEvent + UpdateStatus + callback`. Each write is its own implicit transaction. The callback fires after the last autocommit write, not after a unified transaction.

This matters for a different reason (see Finding 3).

---

## Finding 2: HIGH — Orphan Spawn on UpdateAgentDispatch Failure

**Severity: High — silent data corruption. Agent process runs; record is not linked.**

### The sequence

```go
result, err := dispatch.Spawn(ctx, dStore, opts)  // process starts, dispatch record created
if err != nil {
    return err
}

// Link the new dispatch back to the agent record
return rtStore.UpdateAgentDispatch(ctx, agentID, result.ID)
```

`dispatch.Spawn` is not atomic. It does:

1. `store.Create()` — INSERT into dispatches (autocommit)
2. `cmd.Start()` — OS fork, process now running
3. `store.UpdateStatus(running)` — UPDATE dispatches (autocommit)

If `rtStore.UpdateAgentDispatch` fails (DB error, agent row disappeared, context cancelled), the process is running and its dispatch record is in state `running`, but `run_agents.dispatch_id` is still NULL or still pointing to the prior dispatch.

**Consequence:** The agent is running. Nobody knows which agent record it belongs to. When `SpawnHandler` runs again (e.g., on a retry), it will find the agent still in `active` status (since `dispatch_id` was never updated and the old dispatch is still tracked), potentially spawning a second instance of the same agent. Two processes will race to write to the same output file.

**Secondary issue:** `dispatch.Spawn` itself calls `store.UpdateStatus(StatusRunning, ...)` after `cmd.Start()`, but does not handle the error from that call. If that update fails, the dispatch record stays in `spawned` state permanently. The status update call is:

```go
store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{...})
// return value ignored
```

(spawn.go line 115). The error is silently dropped.

### Minimal fix

Wrap the spawn + link in a compensating action:

```go
result, err := dispatch.Spawn(ctx, dStore, opts)
if err != nil {
    return err
}

if err := rtStore.UpdateAgentDispatch(ctx, agentID, result.ID); err != nil {
    // Compensate: kill the spawned process before returning
    if result.Cmd != nil && result.Cmd.Process != nil {
        _ = result.Cmd.Process.Kill()
    }
    // Mark the dispatch failed in DB
    _ = dStore.UpdateStatus(ctx, result.ID, dispatch.StatusFailed, dispatch.UpdateFields{
        "error_message": "agent link failed: " + err.Error(),
    })
    return fmt.Errorf("spawn: link agent dispatch: %w", err)
}
```

This is not perfect (the kill can race), but it is dramatically better than leaving an orphaned process running indefinitely.

---

## Finding 3: HIGH — Callback Fires Before Phase Transition Is Durable (No Transaction)

**Severity: High — observable inconsistency window where agents spawn for a phase the run is not yet in.**

### The sequence in phase.Advance()

```
store.UpdatePhase(ctx, runID, fromPhase, toPhase)  // autocommit write 1
store.AddEvent(ctx, &PhaseEvent{...})              // autocommit write 2
// optional:
store.UpdateStatus(ctx, runID, StatusCompleted)    // autocommit write 3

callback(runID, eventType, fromPhase, toPhase, reason)
  // → SpawnHandler → agents spawned here
```

Each of the three writes is its own autocommit transaction. If the process crashes after write 1 but before write 2, the run's phase is `executing` but there is no event record. If the process crashes after write 2 but before the callback fires, no agents are spawned, and a subsequent manual `ic run advance` may fail with `ErrStalePhase` (correct behaviour).

The more dangerous scenario: the callback fires, `dispatch.Spawn` succeeds, the child process starts — and **then** the process crashes before `UpdateAgentDispatch` completes. Now you have a running child with no DB record linking it back to the agent. This is the orphan scenario from Finding 2 compounded by crash-between-writes.

The fix for the durability concern is to wrap all three writes in a single explicit transaction, and fire the callback only after that transaction commits:

```go
tx, err := db.BeginTx(ctx, nil)
// ... all writes on tx ...
if err := tx.Commit(); err != nil { ... }
// only now:
callback(...)
```

This is a larger refactor to `phase.Store`, but it is the correct fix. The current autocommit-per-statement design is documented as intentional in AGENTS.md ("dispatch create: No transaction — Single INSERT"), which suggests single writes are left unguarded intentionally for simplicity. That is acceptable for individual writes but not for a sequence of writes whose combined result must be atomic before triggering side effects.

---

## Finding 4: MEDIUM — Double-Spawn Risk: No Active-Agent Guard in SpawnHandler

**Severity: Medium — can produce duplicate agents if Notify is called twice for the same run.**

`SpawnHandler` calls `querier.ListPendingAgentIDs()` which returns agents with `status = 'active'`. At the point the spawn closure runs, the agent is still `active` (status only changes to `completed`/`failed` when the external process finishes, which is tracked by `ic dispatch poll`).

If `ic run advance` is called twice in rapid succession — which is possible if a human runs it concurrently, or a bash hook fires the command and a retry fires — both invocations will:

1. Both see the agent as `active`
2. Both invoke `SpawnByAgentID`
3. Both call `dispatch.Spawn`
4. Two processes start

Neither invocation sees the other's spawn because `dispatch_id` is only written back after `Spawn` returns, and the two invocations race on the single connection, each reading the old `dispatch_id = NULL` before either writes the new one.

The minimal guard is a check at the top of the spawn closure:

```go
agent, err := rtStore.GetAgent(ctx, agentID)
// ...
if agent.DispatchID != nil {
    // Already has a dispatch assigned — check if it's still running before re-spawning
    prior, err := dStore.Get(ctx, *agent.DispatchID)
    if err == nil && !prior.IsTerminal() {
        return fmt.Errorf("spawn: agent %s already has active dispatch %s", agentID, *agent.DispatchID)
    }
}
```

This does not eliminate the TOCTOU race (two concurrent callers can both see `DispatchID == nil` before either writes), but it reduces the window dramatically for the common case. A true fix requires a compare-and-swap on `dispatch_id`: `UPDATE run_agents SET dispatch_id = ? WHERE id = ? AND dispatch_id IS NULL`, checking rows affected.

---

## Finding 5: LOW — dispatchRecorder Closes Over `ctx` From Advance Callsite

**Severity: Low — context cancellation during `ic run advance` will fail dispatch event recording.**

```go
// run.go lines 223–236
dispatchRecorder := func(dispatchID, runID, fromStatus, toStatus string) {
    // ...
    if err := evStore.AddDispatchEvent(ctx, ...); err != nil {
```

`ctx` here is the context passed to `cmdRunAdvance`, which is the root context of the CLI invocation. If the parent context is cancelled (e.g., user hits Ctrl+C during advance), the `dispatchRecorder` will fail to write dispatch events for any status change that happens after cancellation — including the `spawned → running` status update that fires synchronously inside `dispatch.Spawn`.

For a CLI that runs and exits, this is low severity (the context lives for the lifetime of the command). But if this pattern is ever reused in a long-running daemon or test harness that cancels contexts early, dispatch events will be silently dropped.

The `HookHandler` correctly works around this by using `context.Background()` in its goroutine. The `dispatchRecorder` should do the same for the DB write portion:

```go
evCtx := context.Background() // outlives the advance call
if err := evStore.AddDispatchEvent(evCtx, ...); err != nil {
```

---

## Finding 6: LOW — `UpdateAgentDispatch` Does Not Check for Agent Status Precondition

**Severity: Low — bookkeeping only, but worth noting.**

```go
// store.go lines 89–106
func (s *Store) UpdateAgentDispatch(ctx context.Context, agentID, dispatchID string) error {
    now := time.Now().Unix()
    result, err := s.db.ExecContext(ctx, `
        UPDATE run_agents SET dispatch_id = ?, updated_at = ? WHERE id = ?`,
        dispatchID, now, agentID,
    )
```

This unconditionally overwrites `dispatch_id` for any agent, regardless of whether the agent is `completed` or `failed`. If an agent finishes and its status is updated to `completed`, then a late-arriving `UpdateAgentDispatch` call from a parallel spawn attempt can clobber the `dispatch_id` with a new value, making it appear the completed agent was associated with a different dispatch than the one that actually produced its result.

Consider adding `AND status = 'active'` to the WHERE clause and returning `ErrAgentNotFound` (or a more specific error) if 0 rows are affected. This prevents retroactive link rewrites on terminal agents.

---

## Summary Table

| # | Severity | Description | File | Line |
|---|----------|-------------|------|------|
| 1 | — (mitigated) | No deadlock with autocommit writes; AGENTS.md doc is misleading but code is safe | notifier.go | 46 |
| 2 | **Critical** | Orphan spawn: process starts but `UpdateAgentDispatch` can fail, leaving agent unlinked | run.go | 273–279 |
| 3 | **High** | Phase writes not atomic: callback fires between autocommit writes, agents can spawn before event is recorded | machine.go | 149–175 |
| 4 | **Medium** | TOCTOU double-spawn: two concurrent advances both see agent as active, both spawn | handler_spawn.go | 38–53 |
| 5 | Low | `dispatchRecorder` closes over cancellable ctx; dispatch events dropped if ctx cancelled | run.go | 232 |
| 6 | Low | `UpdateAgentDispatch` has no status precondition; can rewrite `dispatch_id` on completed agents | store.go | 89 |

---

## Priority Recommendations

**Fix immediately (blocks correctness):**

1. **Compensate on `UpdateAgentDispatch` failure** (Finding 2). Kill the spawned process and mark the dispatch failed when the DB link cannot be written. This is a 10-line addition to the spawn closure in run.go.

2. **Guard against double-spawn** (Finding 4). Add `AND dispatch_id IS NULL` to the `UpdateAgentDispatch` UPDATE, check RowsAffected, and return a clear error if already linked. Use this as the linearisation point.

**Fix before the SpawnHandler is promoted from "scaffolded" to production:**

3. **Wrap UpdatePhase + AddEvent in a single transaction** (Finding 3). The callback should fire only after the combined transaction commits. This is the correct semantic for "after DB commit" as documented.

4. **Add status precondition to `UpdateAgentDispatch`** (Finding 6). Protect terminal agents from being retroactively relinked.

**Low priority / documentation:**

5. **Use `context.Background()` in dispatchRecorder DB write** (Finding 5).

6. **Correct AGENTS.md**: The description "after DB commit" implies a single transaction. The current implementation uses separate autocommits. Either fix the code to match the doc, or fix the doc to match the code.
