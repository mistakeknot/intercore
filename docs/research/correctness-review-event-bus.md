# Correctness Review: Wave 2 Event Bus

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-18
**Scope:** 27 Go files, ~1500 lines — new `internal/event` package, `dispatch.go` UpdateStatus transaction,
`phase/machine.go` callback wiring, `cmd/ic/events.go` dual-cursor tracking, schema v5 migration.

---

## Invariants That Must Remain True

1. **Single-writer SQLite** — `SetMaxOpenConns(1)` is the only concurrency protection. No goroutine may open a second connection through the same `*sql.DB` while a transaction is in-flight on the same pool slot.
2. **Atomic dispatch status transition** — a status row must not be read by one operation and updated by another without serializability. The pre-v5 code used bare `ExecContext`; v5 wraps the UPDATE in a transaction.
3. **Event record follows commit** — a dispatch event written to `dispatch_events` must correspond to a dispatch row that is already durably committed. Writing the event before or inside the same transaction that updates the dispatch row is the correct placement.
4. **Phase callback fires after full commit** — the `PhaseEventCallback` must be called only after `UpdatePhase`, `AddEvent`, and `UpdateStatus` (for done) are all durably committed. Calling it mid-sequence would fire notifications for work that may roll back.
5. **No goroutine may outlive the process** — the CLI `ic` is a short-lived process. Any spawned goroutine must complete or be benign if the process exits.
6. **Cursor consistency** — after `ic events tail --consumer=X`, the saved cursor must represent the highest event ID that was successfully emitted to stdout, so a subsequent tail does not re-emit or skip events.
7. **Referential integrity** — `dispatch_events.run_id` is nullable intentionally; `dispatch_events.dispatch_id` must reference a row in `dispatches`. The schema omits the FK, which is intentional but documented.
8. **Migration idempotency** — re-running `ic init` on a schema that is already at v5 must be a no-op with no data loss.

---

## Finding Summary

### Critical

None of the findings rise to the level of data corruption under normal operation. One finding (C1) produces a guaranteed goroutine leak on each hook invocation and will cause the process to exit with a dangling goroutine writing to a closed `io.Writer`; on a long-running embedded caller this is a resource leak. For the CLI use case (process exits immediately), it is cosmetically benign.

### Significant

**C1** — Hook goroutine is completely untracked. No mechanism to drain it before the process exits. For the CLI, this is safe only because the OS kills it. For any future in-process embedding, this leaks.

**C2** — `dispatchRecorder` closure in `cmd/ic/run.go` calls `evStore.AddDispatchEvent`, which does a bare `ExecContext` on the same `*sql.DB` while a transaction from `UpdateStatus` has already committed — so the ordering is correct. But the recorder runs synchronously in the `Notify` call chain, which itself runs synchronously from within `UpdateStatus` after `tx.Commit()`. The `*sql.DB` pool has `MaxOpenConns(1)`. With `MaxOpenConns(1)` and SQLite WAL, `ExecContext` on the recorder acquires the single pool slot; because `tx.Commit()` has already released it, this is safe. However, this sequence is fragile: if a future handler calls any DB operation before `UpdateStatus` fully returns (e.g., inside `Notify` from a synchronous handler), it would attempt to acquire the single pool slot while the pool believes it is free but SQLite WAL may still be in a checkpoint transition. This is not a current bug but a stability risk.

**C3** — The hook goroutine uses `context.Background()` and a hard-coded 5-second timeout. The parent context passed to `Notify` (and thus to the hook handler function) is discarded. If the parent context is cancelled (e.g., via `ic run advance`'s `ctx`), the hook subprocess will continue running for up to 5 seconds. This is the intended "fire and forget" design, but the child's `bytes.Buffer stderr` is allocated on the heap and will live for 5 seconds regardless. The `logw io.Writer` captured in the closure is `os.Stderr`, which is always valid for a CLI process, so writing to it after main returns is safe.

**C4** — `dispatch_events.dispatch_id` has no FOREIGN KEY reference to `dispatches.id`. The schema comment says this is intentional (dispatch rows can be pruned independently of events), but there is no documentation of this decision in the schema comments, which will confuse future maintainers. The omission is not a bug but will produce orphaned rows after `ic dispatch prune`.

**C5** — Dual-cursor high-water mark update in `cmd/ic/events.go` lines 202-208 advances each cursor independently in a loop. If the batch contains mixed-source events and `enc.Encode(e)` fails mid-loop (e.g., broken pipe on stdout), the cursor is saved with a partially advanced state. The cursor will then skip events that were never emitted to stdout. In practice, `json.Encoder.Encode` to `os.Stdout` will only fail if the write fails (broken pipe), and the caller's loop does not check the error from `enc.Encode`. Any event whose encode fails silently advances the cursor, making those events permanently invisible on subsequent tails.

**C6** — The `migrate` function in `db.go` line 131-133 has an early-return path: `if currentVersion >= currentSchemaVersion { return nil }`. This returns `nil` without calling `tx.Commit()`, so the deferred `tx.Rollback()` runs, which is correct. The `_migrate_lock` table creation is rolled back, releasing the exclusive lock. This is fine. However, if `currentVersion > currentSchemaVersion`, the same path is taken, and `Open` would have already returned `ErrSchemaVersionTooNew` — so this path is unreachable in practice for the "too new" case. No bug here, just a subtlety worth documenting.

### Minor

**M1** — `TestHookHandler_ExecutesPhaseHook` and `TestHookHandler_DispatchEvent` use `time.Sleep(200 * time.Millisecond)` to wait for the goroutine to complete. This is a classic sleep-based synchronization. It will produce flaky results on a heavily loaded CI machine. The hook goroutine has no signaling mechanism.

**M2** — `ListEvents` query parameter binding: the dispatch subquery uses `(run_id = ? OR ? = '')` with `runID` bound twice. When `runID` is non-empty this correctly filters. When `runID` is empty the condition is always true. But `ListEvents` is documented as requiring a non-empty `runID`; passing empty string to it is undocumented behavior. `ListAllEvents` is the correct API for cross-run queries. The implementation is correct but the two bind positions for `runID` in the dispatch clause could be confusing.

**M3** — `intercore_events_cursor_get` in `lib-intercore.sh` uses `grep "^${consumer}\t"` with a literal tab character. Shell variable interpolation of `\t` in double-quotes does not produce a tab; it produces the two characters `\t`. The correct pattern would require `$'...'` quoting or `printf`. This means the grep will never match, and the wrapper will always return empty string, making named consumer cursors non-functional from bash.

---

## Detailed Analysis

### Q1: Does the hook goroutine risk opening a second SQLite connection?

No, the goroutine spawned by `NewHookHandler` calls `exec.CommandContext` on an external shell script. It does not use the `*sql.DB` at all. The goroutine is fully isolated from the database. The concern in the question is unfounded.

The single-writer constraint is safe here because:
- The goroutine runs after `Notify()` is called.
- `Notify()` is called after `tx.Commit()` in `UpdateStatus` (for the dispatch path) and after all store writes in `Advance` (for the phase path).
- The goroutine itself does no DB I/O.

### Q2: Is the dispatchRecorder called outside the transaction?

Yes, correctly. In `dispatch.go UpdateStatus`:

```
tx.Commit()  ← durable commit first
→ eventRecorder(id, runID, prevStatus, status)  ← then recorder fires
  → evStore.AddDispatchEvent(ctx, ...)  ← bare ExecContext, no transaction
  → notifier.Notify(ctx, e)  ← handlers run synchronously
    → hook handler → spawns goroutine  ← goroutine runs async
    → log handler → writes to stderr  ← synchronous, fine
```

The recorder is invoked only after `tx.Commit()` returns nil. If `tx.Commit()` fails, the recorder is never called. This is correct: no phantom events for rolled-back status changes.

One subtle risk: the recorder is called with `status != prevStatus` guard. This prevents duplicate events for no-op status updates. Correct.

### Q3: Is PhaseEventCallback called after DB commit?

Yes. In `machine.go Advance`, the callback is fired at line 168-170, which is after:
- `store.UpdatePhase` (line 143) — updates the run row
- `store.AddEvent` (line 148) — records the phase event
- `store.UpdateStatus` (line 162) — marks the run completed if done

All three are separate `ExecContext` calls without an enclosing transaction. This means the sequence is NOT atomic: a crash between `UpdatePhase` and `AddEvent` leaves the run in the new phase but with no audit event. This was true before this diff and is a pre-existing design decision documented in AGENTS.md ("dispatch create: No transaction — Single INSERT").

The callback fires last, after all three writes. This is correct for the event bus use case — it will never notify for a transition that failed to persist the run state.

### Q4: Hook goroutine — race conditions with context.Background()

The goroutine captures `logw`, `hookName`, `hookPath`, and `eventJSON` by value (eventJSON is a `[]byte` slice header, but the underlying array was allocated before the goroutine started and is never mutated). These are safe.

`logw` is captured by reference (it is an interface). The goroutine writes to `logw` after the parent handler function has returned. For the CLI use case, `logw` is `os.Stderr`, which is always valid. For a hypothetical embedded use case where `logw` is a `bytes.Buffer`, the goroutine could write to it after the buffer is no longer expected to receive data. This is a latent correctness issue for embedding.

The goroutine uses `context.Background()` with a 5-second timeout. This is correct for "fire and forget" — the parent `ctx` cancellation should not kill the hook subprocess. However, the design comment says "detached goroutine to avoid blocking the single DB connection." This is misleading: the hook subprocess does not interact with the DB. The detachment is to avoid blocking the CLI's stdout and the event notification pipeline, which is a valid reason.

The goroutine has no `WaitGroup` or channel to signal completion. The process will exit after `Advance` or `UpdateStatus` returns, possibly before the subprocess has started or received its stdin. In practice the goroutine will be killed by the OS, and the subprocess (being a separate process) will continue running. If the hook relies on the subprocess completing before making DB changes, that assumption is broken.

Concrete failure sequence:
1. `ic run advance` calls `Advance(...)`.
2. DB commits. Callback fires. Notifier dispatches to hook handler.
3. Hook handler spawns goroutine and returns immediately.
4. `Advance` returns to `cmdRunAdvance`. `cmdRunAdvance` returns `exitCode=0`.
5. `main()` calls `os.Exit(0)`. Process exits.
6. Hook goroutine's `cmd.Run()` may not have started yet, or may be mid-execution.
7. The goroutine is killed. The subprocess may or may not have started.

For hooks that write back to the DB (e.g., `ic run advance` from within the hook), the subprocess will continue running since it is a separate process. The subprocess uses a new `ic` invocation, which opens a fresh DB connection with its own pool. This is safe.

The issue is that the goroutine's lifespan is undefined relative to the parent process. Tests cope with this using `time.Sleep`. Production use relies on OS process exit cleaning up the goroutine. This is acceptable for a short-lived CLI.

### Q5: Dual cursor tracking — correctness

Events from `phase_events` and `dispatch_events` have independent `AUTOINCREMENT` ID spaces. The code tracks them separately with `sincePhase` and `sinceDispatch`. This is correct design.

The UNION ALL query orders by `created_at ASC, source ASC, id ASC`. Two events from different sources at the same `created_at` (second resolution) will be ordered deterministically by source name ("dispatch" < "phase") then by ID. This is stable.

The cursor advance loop:

```go
for _, e := range events {
    enc.Encode(e)
    if e.Source == event.SourcePhase && e.ID > sincePhase {
        sincePhase = e.ID
    }
    if e.Source == event.SourceDispatch && e.ID > sinceDispatch {
        sinceDispatch = e.ID
    }
}
```

The cursor is advanced for each event regardless of whether `enc.Encode` succeeded. `json.Encoder.Encode` to `os.Stdout` returns an error on broken pipe, but the code ignores the error. If the consumer disconnects (broken pipe) mid-batch, some events will be emitted, some will fail silently, but the cursor will advance past all of them. Subsequent tails will miss the events that failed to emit.

This is a **data loss scenario for the consumer**: events are permanently skipped without any indication.

Fix: check `enc.Encode(e)` error and break the loop on failure. Do not save the cursor if encode failed. Only advance the cursor after successful encode.

### Q6: Schema v5 migration — additive or destructive?

Fully additive. The new `dispatch_events` table uses `CREATE TABLE IF NOT EXISTS`. Running `ic init` on a v4 DB will:
1. Create `dispatch_events`.
2. Create three new indexes.
3. Bump `PRAGMA user_version = 5`.

No existing tables are altered. No columns are dropped or changed. No data is moved. The migration is safe for online rolling restarts where old binary (v4) and new binary (v5) coexist briefly:
- Old binary will not see `dispatch_events` table; it does not query it.
- New binary will create the table on first `ic init`; until then, `AddDispatchEvent` will return an error (table not found). This fails silently in the recorder (the error from `evStore.AddDispatchEvent` is discarded in the `dispatchRecorder` closure).

One concern: the `dispatchRecorder` ignores the return value of `evStore.AddDispatchEvent`:

```go
dispatchRecorder := func(dispatchID, runID, fromStatus, toStatus string) {
    ...
    evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", "")
    notifier.Notify(ctx, e)
}
```

If `AddDispatchEvent` fails (e.g., during migration window where schema is at v4), the error is silently dropped. The notifier fires anyway. The event bus notification happens but the persistent record is missing. This is an event loss scenario that is invisible to the operator.

Fix: log or handle the error from `AddDispatchEvent` in the recorder closure.

---

## Concurrency Model Summary

The event bus is fully synchronous in the notification path (Notifier.Notify runs all handlers in sequence). The only async element is the hook subprocess, which runs in a detached goroutine. The `Notifier` struct uses `sync.RWMutex` correctly: `Subscribe` holds a write lock, `Notify` takes a snapshot under read lock then releases it before calling handlers. This prevents handler deadlock when a handler calls `Subscribe`.

The `dispatch.Store.UpdateStatus` transaction correctly serializes the status read and status write. The `SetMaxOpenConns(1)` constraint means only one `*sql.DB` operation can be in-flight at a time. The post-commit recorder fires synchronously after the transaction releases the pool slot, so the recorder's `ExecContext` can acquire the pool without deadlock.

---

## Fixes Required

### C5 — Cursor advance on failed encode (HIGH)

In `/root/projects/Interverse/infra/intercore/cmd/ic/events.go`, replace the encode loop:

```go
for _, e := range events {
    if err := enc.Encode(e); err != nil {
        // stdout broken — stop. Do not advance cursor past events we didn't emit.
        return 2
    }
    if e.Source == event.SourcePhase && e.ID > sincePhase {
        sincePhase = e.ID
    }
    if e.Source == event.SourceDispatch && e.ID > sinceDispatch {
        sinceDispatch = e.ID
    }
}
```

### C2/recorder — Silence on AddDispatchEvent error (MEDIUM)

In `/root/projects/Interverse/infra/intercore/cmd/ic/run.go`, the `dispatchRecorder` closure:

```go
if err := evStore.AddDispatchEvent(ctx, dispatchID, runID, fromStatus, toStatus, "status_change", ""); err != nil {
    fmt.Fprintf(os.Stderr, "[event] AddDispatchEvent %s: %v\n", dispatchID, err)
}
```

### M3 — Shell tab literal in grep (MEDIUM)

In `/root/projects/Interverse/infra/intercore/lib-intercore.sh`:

```bash
$INTERCORE_BIN events cursor list 2>/dev/null | grep "^${consumer}"$'\t' | cut -f2 || echo ""
```

### M1 — Sleep-based test synchronization (LOW)

Add a synchronization file or channel to `NewHookHandler` for testability. Alternatively, add a `WaitForHooks()` method that the test can call. The 200ms sleep is fragile on loaded CI.

---

## Verdict

**needs-changes** — Two functional issues (cursor skip on broken pipe, silent event loss on recorder error) require fixes before this goes to production. Schema migration is safe. Transaction ordering is correct. The hook goroutine design is acceptable for CLI use with known limitations.
