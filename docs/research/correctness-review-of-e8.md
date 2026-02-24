# Correctness Review — E8 Portfolio Orchestration

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-21
**Scope:** `internal/portfolio/relay.go`, `internal/portfolio/dbpool.go`, `internal/phase/store.go` (CancelPortfolio, CreatePortfolio, GetChildren, AddEvent), `internal/phase/gate.go` (CheckChildrenAtPhase), `cmd/ic/dispatch.go` (limit check), `internal/db/db.go` (v9→v10 migration)

---

## Invariants That Must Hold

The following invariants are the baseline for this review. Any code path that violates one of these is a correctness defect, not a style concern.

1. **Cursor monotonicity:** a relay cursor for a child project DB must only ever move forward. An event with id N must never be processed twice, and no event with id > cursor must be silently skipped.
2. **Dispatch-count accuracy:** `active-dispatch-count` in the portfolio's state table must never admit a spawn that would push total active dispatches above `max_dispatches`.
3. **Portfolio atomicity:** a portfolio run and all of its initial child runs must either all be created or none be created.
4. **Cancellation completeness:** `CancelPortfolio` must leave no active child runs behind.
5. **Children-at-phase gate correctness:** the gate must block portfolio phase advance when any active child is still behind the target phase. It must not block when a child is done.
6. **Migration idempotency:** running `ic init` twice on the same DB at any version must produce identical schema with no data loss.
7. **Migration transaction safety:** the `user_version` PRAGMA must only advance inside the migration transaction, not after a partial rollback.

---

## Finding 1 — CRITICAL: Cursor saved after relay event inserted but before relay event is committed; AddEvent is not transactional

**File:** `internal/portfolio/relay.go:124-155`
**Severity:** Data integrity — duplicate relay events on crash or poll error

### Description

`poll()` calls `r.store.AddEvent()` for each child event and then calls `r.saveCursor()` after the event loop for that child. `AddEvent` uses a bare `db.ExecContext` with no transaction. `saveCursor` writes the cursor via `stateStore.Set`, also without a transaction binding to the prior insert.

The sequence inside the inner event loop is:

```
for _, evt := range events {
    r.store.AddEvent(ctx, &phase.PhaseEvent{ ... })   // (1) INSERT phase_events — no tx
    cursor = max(cursor, evt.ID)
}
cursors[child.ProjectDir] = cursor
r.saveCursor(ctx, child.ProjectDir, cursor)            // (2) UPDATE state — no tx
```

There is no single transaction that atomically commits both the relay event row and the advanced cursor value. If the process crashes or the context is cancelled between step 1 and step 2, the relay event row is durably written but the cursor is not advanced. On the next poll, `queryChildEvents` will return the same child event again (because `id > old_cursor`) and `AddEvent` will insert a second relay row for the same child transition.

The result is duplicate `child_advanced` / `upstream_changed` rows in `phase_events` for the portfolio run. Any consumer reading those events (e.g., `ic events tail`) will see phantom duplicate transitions. If a downstream hook triggers on those events the duplicate is also acted upon.

**Concrete interleaving:**

```
[relay]  queryChildEvents: returns [id=42, brainstorm→brainstorm-reviewed]
[relay]  AddEvent: INSERT phase_events (run=portfolio, child_advanced, ...) — committed
[relay]  --- process killed / ctx cancelled here ---
[relay]  restart: loadCursors returns cursor=0 (old value)
[relay]  queryChildEvents: returns [id=42, ...] again
[relay]  AddEvent: second INSERT — same transition, second row
```

**Fix:** Wrap the AddEvent call and cursor update in a single portfolio-DB transaction per child. Since `AddEvent` only touches the portfolio DB (not the child DB), a transaction opened on `s.db` can cover both the INSERT and the state REPLACE atomically:

```go
tx, err := r.store.DB().BeginTx(ctx, nil)
// insert relay event via tx
// upsert cursor via tx
tx.Commit()
```

Alternatively, make `AddEvent` accept an optional `*sql.Tx` parameter so the caller can fold it into a cursor-save transaction.

At minimum: save the cursor after every individual event (not after the batch) so that partial progress is preserved, reducing the window for duplicates — but this still leaves a crash gap between the INSERT and the state REPLACE without a real transaction.

---

## Finding 2 — CRITICAL: Dispatch limit check is a TOCTOU race — portfolio can exceed max_dispatches

**File:** `cmd/ic/dispatch.go:108-128`
**Severity:** Business rule violation — parallel spawners can bypass the limit

### Description

The portfolio dispatch limit check reads the `active-dispatch-count` state value and then, separately, calls `dispatch.Spawn()`. The read and the spawn are not inside any transaction or lock. Multiple concurrent `ic dispatch spawn` invocations for children of the same portfolio will all read the same stale count, all conclude the limit is not exceeded, and all proceed to spawn.

```
// Simplified dispatch.go:108-128
payload, err := stateStore.Get(ctx, "active-dispatch-count", parentRunID)   // (1) read count=N
// ... parse count ...
if count >= parent.MaxDispatches { return 1 }                                 // (2) check
result, err := dispatch.Spawn(ctx, store, opts)                               // (3) spawn
```

The relay writes `active-dispatch-count` asynchronously on its 2-second poll cycle. The value is stale by design. But even ignoring staleness, steps (1)–(3) are not atomic: if two child agents both execute step (1) concurrently and both read `count=N-1` where `MaxDispatches=N`, both pass step (2) and both execute step (3), yielding `count=N+1` active dispatches in reality.

**Concrete interleaving (MaxDispatches=5, current active=4):**

```
Agent A:  stateStore.Get → "4"
Agent B:  stateStore.Get → "4"
Agent A:  4 < 5 → passes check, calls Spawn → spawns dispatch #5
Agent B:  4 < 5 → passes check, calls Spawn → spawns dispatch #6
Relay:    countActiveDispatches → 6, writes "6" to state
```

The 3 AM scenario: a portfolio with `max_dispatches=10` running 10 parallel codex agents. All 10 complete. A batch of 10 new spawns is triggered almost simultaneously. All 10 read count=0 and pass. 10 agents spawn against a limit meant to throttle to 3. Token budget explodes.

**Fix:** The correct design is one of:

- Replace the relay-maintained cache with an atomic counter in the portfolio DB (a `dispatches` count column on the portfolio run row, incremented with `UPDATE ... SET active_dispatches = active_dispatches + 1 WHERE active_dispatches < max_dispatches` and checking rows-affected). This is a compare-and-increment, atomic in SQLite under WAL with a single-writer connection.
- Alternatively, take a named lock (the existing `ic lock` mechanism) on the portfolio ID before spawn and release it after. This serializes spawns for the same portfolio.

The current "best-effort" comment in the code acknowledges the approximation, but the invariant document must then explicitly say this limit is advisory, not enforced. Right now there is neither a hard enforcement mechanism nor a documented acknowledgment that the limit is approximate.

---

## Finding 3 — MODERATE: IsTerminalStatus logic inversion in CheckChildrenAtPhase

**File:** `internal/phase/gate.go:226`
**Severity:** Gate correctness — terminal children may incorrectly block portfolio phase advance

### Description

The gate logic to skip terminal children reads:

```go
if IsTerminalStatus(child.Status) && child.Status != StatusActive {
    continue // completed/cancelled children don't block
}
```

`IsTerminalStatus` returns `true` for `StatusCompleted`, `StatusCancelled`, and `StatusFailed`. `StatusActive` is never returned by `IsTerminalStatus` — it returns `false` for `StatusActive`.

So the condition `IsTerminalStatus(child.Status) && child.Status != StatusActive` is equivalent to `IsTerminalStatus(child.Status)`, because `StatusActive` is never terminal. The `&& child.Status != StatusActive` clause is always true when the outer condition is true. It adds no filtering.

The stated intent is "completed/cancelled children don't block." The implementation achieves that, but the `StatusFailed` case is included in the skip. A failed child silently does not block portfolio advance. Depending on the business intent:

- If a child fails, the portfolio should probably halt or require explicit override, not silently advance past the stalled child.
- The skip-over-failed behavior is currently indistinguishable from skip-over-completed.

Additionally: the redundant `&& child.Status != StatusActive` suggests the author may have intended a different condition — perhaps `IsTerminalStatus(child.Status) && child.Status != StatusFailed` (skip completed/cancelled but not failed). As written, a failed child is treated identically to a completed one.

**Fix:** Make the intent explicit. If failed children should block:

```go
if child.Status == StatusCompleted || child.Status == StatusCancelled {
    continue
}
```

If failed children should also be ignored, add a comment explaining why. Remove the dead `child.Status != StatusActive` clause either way to prevent future readers from treating it as load-bearing.

---

## Finding 4 — MODERATE: Relay eventType mapping loses information — all non-cancel child events become child_advanced

**File:** `internal/portfolio/relay.go:119-122`
**Severity:** Audit integrity — incorrect event classification

### Description

```go
eventType := EventChildAdvanced
if evt.EventType == phase.EventCancel {
    eventType = EventChildCompleted
}
```

Child phase events include: `advance`, `skip`, `pause`, `block`, `cancel`, `set`, `rollback`. The relay collapses all of these into `child_advanced` except `cancel` (which becomes `child_completed`).

Concretely: if a child is blocked by a hard gate (`block` event), the portfolio records `child_advanced`, not a block. If a child is rolled back, the portfolio records `child_advanced`. If a child's phase is `set` administratively, the portfolio records `child_advanced`. The portfolio event log is misleading. More importantly, `EventChildCompleted` is used to signal child completion, but `EventCancel` is a cancellation, not a completion — these are different terminal states.

This does not corrupt data directly, but any downstream consumer reacting to portfolio events (e.g., a progress dashboard or trigger hook) will see incorrect signals and may act on them wrongly.

**Fix:** Map all child event types explicitly:

```go
switch evt.EventType {
case phase.EventCancel:
    eventType = EventChildCompleted // or add EventChildCancelled
case phase.EventBlock, phase.EventPause:
    eventType = EventChildBlocked   // add this constant
case phase.EventRollback:
    eventType = EventChildRolledBack
default:
    eventType = EventChildAdvanced
}
```

At minimum, distinguish `EventChildCompleted` (the child reached terminal done) from `EventChildCancelled`.

---

## Finding 5 — MODERATE: Migration early-return skips tx.Rollback on the deferred path when already at current version

**File:** `internal/db/db.go:132-134`
**Severity:** Transaction hygiene — benign but worth fixing

### Description

```go
tx, err := d.db.BeginTx(ctx, nil)
// ...
defer tx.Rollback()

// ...
if currentVersion >= currentSchemaVersion {
    return nil // already migrated
}
```

When `return nil` fires at line 133, the deferred `tx.Rollback()` runs against a committed-or-open transaction. Since no DML has run yet except `CREATE TABLE IF NOT EXISTS _migrate_lock`, this rollback is effectively a no-op, but it leaves the `_migrate_lock` table created and then rolled back. SQLite DDL inside a transaction is rolled back with the transaction, so this is clean. However, the pattern is fragile: the `_migrate_lock` create statement is the write that escalated the deferred transaction to exclusive. Rolling it back means future migrations rely on `CREATE TABLE IF NOT EXISTS` being idempotent — which it is, but only because of `IF NOT EXISTS`.

The more significant concern: the early return does NOT call `tx.Commit()`. The `defer tx.Rollback()` is the only cleanup. In SQLite, a rolled-back writable transaction holds the write lock until it returns. With `SetMaxOpenConns(1)`, this is fine — the connection pool has one connection and it will be returned on function exit. But the pattern `return nil` without `tx.Rollback()` called explicitly reads as if the caller thinks there is nothing to roll back, which obscures the cleanup path.

**Minimal fix:** Call `tx.Rollback()` explicitly before the early return, or restructure to skip beginning a transaction if no migration is needed (check `user_version` outside the transaction first, then re-read inside if it appears stale).

---

## Finding 6 — MODERATE: v6 migration fires for any DB at v5+, even if already at v7, v8, or v9

**File:** `internal/db/db.go:137`
**Severity:** Migration correctness — redundant ALTER TABLE calls, relying on isDuplicateColumnError to paper over them

### Description

```go
if currentVersion >= 5 {
    // v5→v6 ADD COLUMN statements
}
if currentVersion >= 4 && currentVersion < 8 {
    // v7→v8 ADD COLUMN statements
}
if currentVersion >= 3 && currentVersion < 10 {
    // v9→v10 ADD COLUMN statements
}
```

A DB at v8 will execute both the v5→v6 block and the v9→v10 block. The v5→v6 block is NOT guarded with an upper bound (`currentVersion < 6`). It will attempt to `ALTER TABLE runs ADD COLUMN phases TEXT` on a v8 DB where that column already exists. The `isDuplicateColumnError` guard catches this and continues, so the migration succeeds — but only because the error suppression is in place.

If SQLite ever changes the error message for duplicate columns (it currently produces "duplicate column name: X"), `isDuplicateColumnError` would silently swallow a real error on the v5→v6 block instead. The guard is a safety net that masks a structural migration authoring mistake.

The v9→v10 block has a correct upper bound (`< 10`). The v5→v6 block is missing the corresponding `< 6` guard.

**Fix:** Add upper bounds to all migration blocks:

```go
if currentVersion >= 5 && currentVersion < 6 {  // was: >= 5
```

This makes the migration self-documenting (each block runs exactly once, for the version it was written for) and removes the reliance on `isDuplicateColumnError` as a correctness backstop for the v5→v6 path.

---

## Finding 7 — LOW: DBPool cached handles survive child DB rotation or deletion without invalidation

**File:** `internal/portfolio/dbpool.go:33-62`
**Severity:** Operational correctness — stale reads from a replaced or deleted child DB file

### Description

The pool caches `*sql.DB` handles keyed by `projectDir`. Once a handle is cached, it is never evicted except by `Close()`. If a child project's `intercore.db` file is replaced (e.g., after a schema migration or `ic init` on that project), the relay continues to hold the old file descriptor, reading from the old WAL. On Linux, because `unlink()` on an open file does not destroy it until all file descriptors are closed, the relay reads stale pre-migration data silently.

This is unlikely to corrupt data (the relay is read-only against child DBs), but it can cause the relay to see a child as stuck at a phase it already advanced past, which in turn blocks the `CheckChildrenAtPhase` gate from ever passing for that child.

**Fix:** Add a per-handle schema-version check on each poll cycle (or every N cycles). If the version has changed, evict and reopen the handle. Alternatively, add an explicit `Evict(projectDir string)` method to `DBPool` and call it from any path that knows a child DB was recreated.

---

## Finding 8 — LOW: Relay writes active-dispatch-count with TTL=0 but no expiry semantics; stale count persists after relay stops

**File:** `internal/portfolio/relay.go:165-166`
**Severity:** Operational correctness — limit check reads stale count after relay death

### Description

```go
r.stateStore.Set(ctx, "active-dispatch-count", r.portfolioID,
    json.RawMessage(strconv.Quote(strconv.Itoa(totalActive))), 0)
```

TTL=0 means no expiry. If the relay process stops (crash, signal), the last-written dispatch count sits in the state table indefinitely. The dispatch limit check in `cmd/ic/dispatch.go` reads this value and may see a count from minutes or hours ago. If the true active count has dropped (dispatches completed) and the stale count is high, the limit check blocks legitimate spawns. If the true count is higher (dispatches spawned without the relay running), the limit check under-blocks.

**Fix:** Set a TTL of 2–3x the relay poll interval (e.g., `10 * time.Second` for a 2s interval). `ic state prune` already runs on a schedule; the dispatch limit check should treat a missing or expired value as "count unknown, proceed" rather than "count=last_known_value".

Alternatively, the dispatch limit check should log a warning when it reads a value older than N seconds and treat it as stale.

---

## Finding 9 — LOW: saveCursor ignores error silently

**File:** `internal/portfolio/relay.go:155`
**Severity:** Operational correctness — cursor drift undetected

### Description

`r.saveCursor` is a void return function:

```go
func (r *Relay) saveCursor(ctx context.Context, projectDir string, cursor int64) {
    r.stateStore.Set(ctx, "relay-cursor", projectDir, ...)
}
```

`stateStore.Set` returns an error that is discarded. If cursor persistence fails (e.g., portfolio DB write contention, disk full), the relay silently continues with the in-memory cursor advanced but the durable cursor stale. On next restart, the cursor is reloaded from the stale persisted value, and events already processed (and whose relay rows are already in `phase_events`) are processed again.

This compounds Finding 1: even without a crash, transient write failures cause event duplication.

**Fix:** Return the error from `saveCursor` and handle it in `poll`. At minimum, log a persistent warning with enough context to know the cursor is drifting.

---

## Finding 10 — LOW: CheckChildrenAtPhase uses a stale in-process snapshot of child run state

**File:** `internal/phase/gate.go:220`
**Severity:** Gate freshness — gate may pass or fail based on obsolete child state

### Description

`GetChildren` queries the portfolio DB's `runs` table for child rows. Child run state in the portfolio DB is only updated by the relay when it observes events in the child DB. The relay runs on a 2s cycle. The gate check reads child phase directly from `runs.phase`.

This means there is up to a 2-second window where:
- A child has already advanced in its own project DB
- The relay has not yet observed the event
- The gate check queries `runs.phase` and sees the old phase
- The portfolio is incorrectly blocked from advancing

This is an inherent consequence of the polling architecture and is acceptable for most use cases. It becomes a correctness concern only if the gate tier is Hard and the portfolio operator expects real-time gating rather than eventually-consistent gating.

**Recommendation:** Document explicitly in `AGENTS.md` that `children_at_phase` gate freshness is bounded by the relay poll interval. Gate overrides (`ic gate override`) are the escape hatch for manual advancement past a stale check.

---

## Summary Table

| # | File | Severity | Class | Finding |
|---|------|----------|-------|---------|
| 1 | relay.go:124-155 | CRITICAL | Data integrity | Cursor saved non-atomically with relay event; events duplicated on crash |
| 2 | dispatch.go:108-128 | CRITICAL | Race condition | TOCTOU between dispatch-count read and spawn; max_dispatches not enforced |
| 3 | gate.go:226 | MODERATE | Logic error | Dead clause in terminal-child filter; failed children silently non-blocking |
| 4 | relay.go:119-122 | MODERATE | Audit integrity | Non-advance child events misclassified as child_advanced |
| 5 | db.go:132-134 | MODERATE | Transaction hygiene | Early return leaves implicit rollback without Rollback() call |
| 6 | db.go:137 | MODERATE | Migration correctness | v5→v6 migration block missing upper bound; relies on error suppression |
| 7 | dbpool.go:42-62 | LOW | Operational | Stale cached DB handles survive child DB file rotation |
| 8 | relay.go:165-166 | LOW | Operational | TTL=0 on dispatch count; stale value persists after relay dies |
| 9 | relay.go:155 | LOW | Operational | saveCursor silently discards write errors; cursor drift undetected |
| 10 | gate.go:220 | LOW | Freshness | children_at_phase gate bounded by relay poll interval; not documented |

---

## Recommended Fix Priority

1. **Finding 2 (dispatch limit race)** — implement atomic compare-and-increment in the portfolio DB before next production use of `max_dispatches`. The current code enforces nothing.
2. **Finding 1 (cursor/event atomicity)** — wrap AddEvent + saveCursor in a single transaction per child. This prevents duplicate relay events on any restart.
3. **Finding 3 (failed-child gate behavior)** — clarify and implement the intended semantics for failed children before any portfolio reaches a state with a failed child.
4. **Finding 6 (migration upper bounds)** — mechanical fix, low risk, should be in the same PR as any future schema version bump.
5. **Findings 4, 5, 7, 8, 9** — fix in a follow-up hardening pass.
