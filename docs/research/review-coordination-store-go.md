# Correctness Review: internal/coordination/store.go

**File reviewed:** `/home/mk/projects/Demarch/core/intercore/internal/coordination/store.go`
**Reviewer:** Julik (flux-drive correctness)
**Date:** 2026-02-25

---

## Invariants Under Review

Before findings, the invariants that must hold:

1. **Lock exclusivity**: At most one exclusive lock may be active per overlapping pattern+scope pair at any moment, across any two distinct owners.
2. **No phantom reads between conflict-check and insert**: A concurrent Reserve must not slip in between the SELECT and INSERT, defeating conflict detection.
3. **Event ordering**: Events (acquired, conflict, expired, transferred) must be emitted only after the database state change is visible and committed — never while a write transaction is still open on the same DB.
4. **No cursor leaks**: Every `*sql.Rows` opened must be closed on all code paths, including early returns.
5. **Sweep idempotency**: Multiple overlapping Sweep calls must not double-emit expired events or double-decrement counters.
6. **Event emission does not block under the write lock**: Because `modernc.org/sqlite` with `MaxOpenConns(1)` serializes all connections, any write inside `emitEvent` while a transaction is still open would deadlock.
7. **ID collision safety**: IDs must have sufficient entropy that collisions are not practically possible.
8. **Transfer conflict visibility**: The conflict check for Transfer must see the post-sweep state of locks, not a stale view.

---

## Context: SQLite Single-Connection Pool

The DB is opened with `SetMaxOpenConns(1)` (confirmed in `internal/db/db.go:56`), WAL mode, and a `busy_timeout`. This is the standard pattern for modernc.org/sqlite.

With a single connection, SQLite serializes all operations through that one connection. `BeginTx` with `sql.LevelSerializable` maps to `BEGIN IMMEDIATE` in modernc.org/sqlite, which acquires a reserved lock immediately. This prevents any other writer from starting — but reads can still proceed concurrently through WAL snapshots if there were multiple connections. Since there is only one connection here, `BEGIN IMMEDIATE` effectively serializes everything. This is correct and appropriate.

---

## Finding 1 — CRITICAL: Sweep Holds Open Cursor While Calling Release, Which Opens a New Statement (Cursor Leak / Write Blocked)

**Severity: High**
**Location:** `store.go:361–394`

### The problem

`Sweep` does this:

```go
rows, err := s.db.QueryContext(ctx, `SELECT ... FROM coordination_locks WHERE ...`, cutoff)
// ...
defer rows.Close()

var expired []Lock
for rows.Next() {
    // scan into expired
}
if err := rows.Err(); err != nil { ... }

// rows is still open (not closed yet — defer is pending)
for _, l := range expired {
    if _, err := s.Release(ctx, l.ID, "", ""); err != nil {
        continue
    }
    s.emitEvent(...)
}
return result, nil  // <-- rows.Close() fires here via defer
```

The `defer rows.Close()` at line 368 fires only when `Sweep` returns. This means `rows` is closed **after** `Release` is called for all expired locks.

`Release` calls `s.db.ExecContext`, which needs a connection from the pool. With `MaxOpenConns(1)` and the single connection already being held by the cursor (`rows`), this would deadlock on a real multi-connection pool. On the current single-connection `modernc.org/sqlite` setup, the cursor is not a "connection" per se — it is a statement on the single connection. However, SQLite's single-connection model still requires that the read cursor not be active when a write statement runs through the same connection, depending on the driver's internal multiplexing.

More concretely: the code has already fully iterated `rows.Next()` to exhaustion and checked `rows.Err()`. The loop is done. The rows are read. The `defer rows.Close()` is pure cleanup at this point — but it is still technically an open statement handle on the connection. `Release` then issues `UPDATE coordination_locks`, which goes through the same `*sql.DB`. With `modernc.org/sqlite` and `MaxOpenConns(1)`, this is actually safe in practice because the rows have been fully consumed, but it is fragile and misleading. The pattern is also wrong by convention: `database/sql` rows must be closed before issuing any further statements through the same connection if you want to be portable and safe.

**The correct pattern** is to close `rows` explicitly after the scan loop and before the release loop:

```go
for rows.Next() { ... }
if err := rows.Err(); err != nil { return nil, err }
rows.Close()  // explicit close here, not via defer

for _, l := range expired {
    s.Release(...)
    s.emitEvent(...)
}
```

The `defer` approach works but should be replaced with explicit close immediately after the scan. This is not an immediate 3 AM fire under the current single-connection setup, but it is a correctness smell that will bite on any future refactor to a multi-connection pool, and it violates established `database/sql` idiom for this code pattern.

**Fix:** Remove `defer rows.Close()` and add `rows.Close()` explicitly at line 380 after `rows.Err()` is checked, before the release loop.

---

## Finding 2 — MEDIUM: Sweep Error Handling in Release Loop Silently Swallows Errors

**Severity: Medium**
**Location:** `store.go:389–394`

```go
for _, l := range expired {
    if _, err := s.Release(ctx, l.ID, "", ""); err != nil {
        continue  // silently skips and does not update result.Total
    }
    s.emitEvent(ctx, "coordination.expired", l.ID, l.Owner, l.Pattern, l.Scope, "sweep", l.RunID)
}
return result, nil
```

When `Release` fails for a lock, `Sweep` silently skips it and continues. The returned `SweepResult.Total` reflects the number of expired locks found by the SELECT, not the number actually released. Callers have no way to distinguish "found 5 expired, released 5" from "found 5 expired, released 2 (3 failed)".

Additionally, `result.Total` is set to `result.Expired` (line 383) before the release loop runs, so it never reflects the actual count of successfully released locks.

This is not a data-corruption risk because `Release` uses `UPDATE ... WHERE id = ? AND released_at IS NULL` which is idempotent — failing to release one lock means it remains active, which is visible through subsequent `Reserve` calls. But callers (e.g., the CLI `ic lock clean`) will see a misleading success count.

**Fix:** Introduce a `result.Released int` field in `SweepResult`, increment it only on successful Release calls, and set `result.Total = result.Released`. Consider returning a partial-error or accumulating errors with `errors.Join`.

---

## Finding 3 — CORRECTNESS: Conflict Check in Reserve Does Not Filter by Exclusive Mode on Both Sides Consistently

**Severity: Medium**
**Location:** `store.go:88–130`

The conflict loop:

```go
rows, err := tx.QueryContext(ctx, `SELECT id, owner, pattern, reason, exclusive
    FROM coordination_locks
    WHERE scope = ? AND released_at IS NULL
      AND (expires_at IS NULL OR expires_at > ?)
      AND owner != ?`, lock.Scope, now, lock.Owner)
```

This pulls ALL active locks by other owners, regardless of whether they are exclusive or not. Then inside the loop:

```go
if !lock.Exclusive && !existing.exclusive {
    continue  // skip shared+shared
}
```

This is correct as far as it goes: shared+shared is allowed, everything else (exclusive+shared, shared+exclusive, exclusive+exclusive) is a conflict. The logic is sound.

However, there is a subtle issue: the SQL query does not filter out locks where `exclusive = 0` AND `lock.Exclusive = false` at the database level. It fetches all locks then filters in Go. This means O(N) database rows fetched even when the requesting lock is shared and most existing locks are also shared, a performance issue at scale. The bigger concern is that this is a missed opportunity for a database-enforced invariant — the conflict check is entirely application-level.

This is not a bug, but it is a correctness fragility: if someone adds a third lock mode without updating the skip condition, shared locks would be incorrectly reported as conflicting. The SQL `WHERE` clause should be tightened to only return rows that could possibly conflict:

```sql
WHERE scope = ? AND released_at IS NULL
  AND (expires_at IS NULL OR expires_at > ?)
  AND owner != ?
  AND (exclusive = 1 OR ?)  -- ? = lock.Exclusive (1 or 0)
```

This is a minor finding but worth noting for correctness completeness.

---

## Finding 4 — CONFIRMED CORRECT: Transaction Rollback Before emitEvent in Reserve

**Location:** `store.go:133–139`

```go
if conflict != nil {
    // Rollback the transaction BEFORE emitting events to avoid deadlock.
    tx.Rollback()
    s.emitEvent(ctx, "coordination.conflict", ...)
    return &ReserveResult{Conflict: conflict}, nil
}
```

This is correct. The comment is accurate. With a single-connection SQLite pool, any write inside `emitEvent` (which calls `s.onEvent`, which ultimately writes to `coordination_events` via a new DB call) would block forever if the IMMEDIATE transaction were still open. Rolling back first, then emitting, is the right ordering.

The `defer tx.Rollback()` at line 79 is also correct: after the explicit `tx.Rollback()` at line 137, the deferred call is a no-op (rollback of an already-rolled-back transaction returns an error that is ignored, which is the standard Go pattern).

---

## Finding 5 — CONFIRMED CORRECT: Transfer Event Emission After Commit

**Location:** `store.go:341–348`

```go
n, _ := res.RowsAffected()
if err := tx.Commit(); err != nil {
    return 0, err
}

if n > 0 {
    s.emitEvent(ctx, "coordination.transferred", "", fromOwner, "", scope, "transferred to "+toOwner, "")
}
return n, nil
```

Event emission happens after `tx.Commit()`. This is correct. The transaction is fully committed before `emitEvent` attempts to write to `coordination_events`. No deadlock risk.

The `res.RowsAffected()` error is silently ignored (`n, _ :=`). This is acceptable for SQLite since `RowsAffected()` on a pure UPDATE always returns a valid count; the error path is essentially unreachable for this driver.

---

## Finding 6 — CORRECTNESS: Transfer Conflict Check Has TOCTOU Window

**Severity: Medium**
**Location:** `store.go:281–330`

Transfer is wrapped in a `BEGIN IMMEDIATE` transaction (line 273), which serializes it against other writers. The conflict check for `!force` mode (lines 281–331) reads `fromOwner`'s exclusive locks and `toOwner`'s exclusive locks, then checks overlap in Go.

However, **the conflict check only examines exclusive locks on both sides** (both queries have `AND exclusive = 1`). A shared lock held by `toOwner` that would conflict with an exclusive lock being transferred from `fromOwner` is invisible to this check.

Concrete failure case:
1. Agent A holds an exclusive lock on `pkg/**` (fromOwner = A).
2. Agent B holds a shared lock on `pkg/foo.go` (toOwner = B).
3. `Transfer(ctx, A, B, scope, false)` is called.
4. The conflict check reads A's exclusive locks: finds `pkg/**`. Reads B's exclusive locks: finds nothing (B's lock is shared). No overlap detected.
5. Transfer proceeds. B now holds an exclusive lock on `pkg/**` AND a shared lock on `pkg/foo.go` for the same pattern — which is internally consistent but the exclusive lock's exclusivity was meant to prevent others from acquiring, not to accumulate inside the same owner.

Actually, after transfer, B owns both locks. Since the conflict check in `Reserve` uses `AND owner != ?`, B's own locks don't block B's future acquires. So the transferred exclusive lock wouldn't block B, which is arguably correct: the transfer is about ownership change. The issue is that the conflict check is incomplete — it should check whether `fromOwner`'s exclusive locks conflict with `toOwner`'s shared locks (which would matter if we later validate that an owner cannot hold a shared lock and an exclusive lock on overlapping patterns simultaneously).

This requires a specification decision: does an owner being both the shared holder and the exclusive holder of the same pattern constitute a conflict? If yes, the transfer conflict check is incomplete. If no (intra-owner conflicts are allowed), this is fine. Mark as a spec clarification needed rather than a hard bug.

---

## Finding 7 — CONFIRMED CORRECT: rows.Close() Handling in Reserve

**Location:** `store.go:87–131`

Reserve uses explicit `rows.Close()` calls in the scan loop, both on scan error (line 105) and on overlap-check error (line 114). After the loop, `rows.Err()` is checked (lines 127–130), followed by `rows.Close()` at line 131 on the happy path.

This is the correct idiomatic Go pattern for `database/sql` cursor handling within a transaction. The cursor is closed before the INSERT executes (line 143), which is correct.

There is no cursor leak in Reserve.

---

## Finding 8 — CONFIRMED CORRECT: generateID() Cryptographic Randomness

**Location:** `store.go:15–26`

```go
func generateID() (string, error) {
    b := make([]byte, idLen)
    max := big.NewInt(int64(len(idChars)))
    for i := range b {
        n, err := rand.Int(rand.Reader, max)
        if err != nil {
            return "", fmt.Errorf("generate id: %w", err)
        }
        b[i] = idChars[n.Int64()]
    }
    return string(b), nil
}
```

This uses `crypto/rand` (imported as `rand`) with `rand.Reader` and `big.Int` for unbiased selection. The alphabet is 36 characters (26 lowercase + 10 digits), length 8. Entropy: log2(36^8) ≈ 41.4 bits. Collision probability for 1 million IDs: ~1 in 2 million. Acceptable for lock IDs within a single project's DB.

The error is propagated correctly. This is correct.

---

## Finding 9 — MINOR: SetEventFunc Is Not Concurrency-Safe

**Severity: Low**
**Location:** `store.go:43–46`

```go
func (s *Store) SetEventFunc(fn EventFunc) {
    s.onEvent = fn
}
```

`onEvent` is a plain field write with no synchronization. The comment says "Call after NewStore, before Reserve/Release", which implies this is only meant to be called once during setup. If that contract is honored, this is safe. If a caller ever tries to swap the event function while operations are in flight, there is a data race on `s.onEvent`.

Given the documented "call once" contract, this is acceptable but should be noted. Using `sync/atomic.Value` or a mutex would eliminate the risk entirely with negligible overhead.

---

## Finding 10 — MEDIUM: Sweep olderThan Logic Is Counterintuitive and Likely Wrong for Its Primary Use Case

**Severity: Medium**
**Location:** `store.go:354–358`

```go
now := time.Now().Unix()
cutoff := now
if olderThan > 0 {
    cutoff = now - int64(olderThan.Seconds())
}
```

When `olderThan == 0`, `cutoff = now` — Sweep releases all locks whose `expires_at < now`. This is the correct TTL-based cleanup: locks that have already expired by wall-clock time.

When `olderThan > 0`, say `olderThan = 1h`, then `cutoff = now - 3600`. This means Sweep only releases locks that expired MORE THAN 1 hour ago. This is intended for "clean up only stale-old expired locks, leave recently expired ones alone". But the SQL query is `WHERE expires_at < cutoff`, not `WHERE expires_at < now AND created_at < cutoff`. Locks that expire in the future are never touched (correct), but the `olderThan` parameter shifts when something is considered "expired enough to sweep", not "old enough". The parameter name suggests age of the lock, but it actually controls age-of-expiry, which is confusing.

A CLI call like `ic lock clean --older-than=5s` would silently NOT clean locks that expired 3 seconds ago, even if they have long since been abandoned. This could be the intended design, but it is worth confirming that the semantics match operator expectations.

This is a design correctness issue, not a crash risk.

---

## Finding 11 — MINOR: Conflict Event Carries Unassigned Lock ID When ID Was Already Generated

**Location:** `store.go:138`

```go
s.emitEvent(ctx, "coordination.conflict", lock.ID, lock.Owner, lock.Pattern, lock.Scope, conflict.BlockerOwner, lock.RunID)
```

When a conflict occurs, `lock.ID` was already generated (line 62–66) or was provided by the caller. The conflict event therefore carries the ID of the lock that was REJECTED, not the ID of the existing blocker. The `BlockerID` is in `conflict.BlockerID` but is not passed to `emitEvent` — `emitEvent`'s signature takes `lockID` (5th positional parameter), not `blockerID`. The event records the would-be acquirer's ID, not the blocker's ID.

Looking at the `emitEvent` signature:
```go
func (s *Store) emitEvent(ctx context.Context, eventType, lockID, owner, pattern, scope, reason, runID string)
```

For `coordination.conflict`, `lockID` = the rejected lock's ID, `reason` = `conflict.BlockerOwner`. The blocker's ID (`conflict.BlockerID`) is not included in the event at all.

This means `coordination.conflict` events cannot be joined to the blocker lock by ID without a separate query. This is an observability gap. The `reason` field encodes only `BlockerOwner` (a string), not `BlockerID`. Operators debugging contention via event logs cannot directly trace which existing lock caused the conflict.

**Fix:** Include `conflict.BlockerID` in the `reason` field (e.g., `"blocked by "+conflict.BlockerID`) or extend the event schema to carry `blocker_id` as a separate field.

---

## Summary Table

| # | Severity | Location | Finding |
|---|----------|----------|---------|
| 1 | High | Sweep L368–394 | Open cursor held during Release calls; correct under current single-conn setup but fragile and violates database/sql idiom |
| 2 | Medium | Sweep L389–394 | Release errors silently ignored; SweepResult.Total is misleading |
| 3 | Medium | Reserve L88–92 | Conflict SQL fetches all locks; missed DB-level exclusion mode filter (performance + future correctness) |
| 4 | Correct | Reserve L133–139 | Rollback before emitEvent — correct, deadlock prevented |
| 5 | Correct | Transfer L341–348 | emitEvent after Commit — correct |
| 6 | Medium | Transfer L281–330 | Conflict check ignores shared locks on toOwner (spec clarification needed) |
| 7 | Correct | Reserve L87–131 | rows.Close() calls on all paths — correct |
| 8 | Correct | L15–26 | generateID() uses crypto/rand correctly |
| 9 | Low | L43–46 | SetEventFunc not concurrency-safe (documented single-call contract mitigates) |
| 10 | Medium | Sweep L354–358 | olderThan semantics counterintuitive: shifts expiry cutoff, not lock creation age |
| 11 | Minor | Reserve L138 | Conflict event does not carry BlockerID; observability gap |

---

## Priority Fixes

**Do now (before production load):**

1. **Finding 1** — Move `rows.Close()` in `Sweep` from `defer` to explicit call after `rows.Err()` and before the release loop. One-line fix, no behavior change under current setup, eliminates future footgun.

2. **Finding 2** — Add `result.Released` counter to `SweepResult`, only increment on successful `Release` calls. Return the real count to callers.

**Do before next feature work on coordination:**

3. **Finding 11** — Include `conflict.BlockerID` in the `coordination.conflict` event reason string. Zero-risk change, high observability payoff.

4. **Finding 10** — Add a comment to `Sweep` clarifying that `olderThan` controls the expiry cutoff window, not the lock creation age. If the intent is to also gate on creation age, fix the SQL.

**Spec clarification before next Transfer use:**

5. **Finding 6** — Decide and document: does `Transfer` need to check `fromOwner`'s exclusive locks against `toOwner`'s shared locks? Update the conflict check accordingly.
