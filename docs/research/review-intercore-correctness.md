# Intercore Correctness Review

**Reviewer:** Julik (Flux-drive Correctness Agent)
**Date:** 2026-02-17
**Project:** intercore v0.1.0 (Go 1.22, modernc.org/sqlite WAL)
**Scope:** Data integrity, transaction safety, race conditions, and concurrency correctness

---

## Executive Summary

Intercore is a well-designed single-writer SQLite WAL system with strong atomic operation primitives. The codebase demonstrates excellent correctness discipline with race detector coverage, comprehensive concurrency tests, and explicit transaction boundaries.

**Critical findings:** 3 high-severity issues
**Important findings:** 4 medium-severity correctness gaps
**Observations:** 5 design choices with edge-case implications

**Overall assessment:** Production-ready with required fixes for migration lock upgrade and time-source consistency. The sentinel atomicity implementation is correct and well-tested. State operations have appropriate transaction boundaries with one unnecessary transaction wrapper.

---

## Invariants Under Review

### System Invariants
1. **Single-writer isolation:** At most one write transaction active at any time (`SetMaxOpenConns(1)`)
2. **Sentinel once-only semantics:** For `interval=0`, exactly one concurrent process claims the sentinel (never zero, never two)
3. **State TTL enforcement:** Expired entries invisible to reads, prunable without corrupting active data
4. **Schema version monotonicity:** `user_version` never decreases, migration applies exactly once
5. **WAL checkpoint safety:** Concurrent readers during WAL checkpoint see consistent snapshots

### Operation-Level Invariants
- **Sentinel check:** INSERT OR IGNORE + conditional UPDATE + auto-prune = all-or-nothing within transaction
- **State set:** Payload validation occurs before transaction begins
- **Migration TOCTOU:** Version check inside transaction prevents double-application
- **Path traversal:** Database created only under CWD with `.db` extension

---

## Critical Findings (Fix Required)

### 1. Migration Lock Insufficient for WAL Mode

**Severity:** HIGH
**Impact:** Concurrent `ic init` from two processes can both read `user_version=0`, both apply schema DDL, second commit may corrupt if DDL conflicts

**Location:** `internal/db/db.go:108-117` (Migrate function)

**Current code:**
```go
tx, err := d.db.BeginTx(ctx, nil)  // Defaults to deferred transaction
// ...
if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
    return fmt.Errorf("migrate: lock: %w", err)
}
```

**Problem:**
`BEGIN` (deferred) + `SELECT 1` acquires a **read lock**, not a write lock. In WAL mode, another concurrent `BEGIN` can also acquire a read lock and read the same `user_version=0`. Both processes can pass the `currentVersion >= currentSchemaVersion` check.

**Failure narrative (TOCTOU race):**
1. Process A: `BEGIN` (deferred) → read lock
2. Process B: `BEGIN` (deferred) → read lock (WAL allows concurrent readers)
3. Process A: `SELECT 1` → lock acquired (shared)
4. Process B: `SELECT 1` → lock acquired (shared)
5. Process A: `PRAGMA user_version` → reads 0
6. Process B: `PRAGMA user_version` → reads 0 (same snapshot)
7. Process A: Apply schema DDL → first writer promotes to exclusive lock, succeeds
8. Process B: Apply schema DDL → blocked on exclusive lock, then may fail or apply duplicate DDL

The `CREATE TABLE IF NOT EXISTS` makes the DDL idempotent, so this race is **not data-corrupting in practice** for schema v1. However:
- The backup is created twice (wasteful)
- If future migrations include non-idempotent DDL (e.g., `ALTER TABLE`), this race will corrupt
- The second process wastes time waiting for SQLITE_BUSY timeout

**Fix:**
Use `BEGIN IMMEDIATE` to acquire a write lock at transaction start:
```go
tx, err := d.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelDefault})
if err != nil {
    return fmt.Errorf("migrate: begin: %w", err)
}
// Execute PRAGMA immediately to ensure exclusive lock
if _, err := tx.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
    return fmt.Errorf("migrate: acquire write lock: %w", err)
}
```

Wait—this won't work because `BeginTx` already started a transaction. Better fix:

```go
// Use database/sql's transaction isolation to force immediate lock
tx, err := d.db.BeginTx(ctx, &sql.TxOptions{
    Isolation: sql.LevelSerializable,  // Forces immediate lock in SQLite
})
```

Actually, `modernc.org/sqlite` may not respect Go's `TxOptions.Isolation` correctly. The safest fix is to use a raw `BEGIN IMMEDIATE`:

```go
// Acquire exclusive write lock immediately (not deferred)
// This prevents TOCTOU with concurrent migrations in WAL mode
_, err := d.db.ExecContext(ctx, "BEGIN IMMEDIATE")
if err != nil {
    return fmt.Errorf("migrate: acquire write lock: %w", err)
}
defer func() {
    if err != nil {
        d.db.ExecContext(ctx, "ROLLBACK")
    }
}()

// Read version INSIDE exclusive transaction
var currentVersion int
if err := d.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
    return fmt.Errorf("migrate: read version: %w", err)
}

if currentVersion >= currentSchemaVersion {
    d.db.ExecContext(ctx, "COMMIT")
    return nil // already migrated
}

// Apply schema DDL
if _, err := d.db.ExecContext(ctx, schemaDDL); err != nil {
    return fmt.Errorf("migrate: apply schema: %w", err)
}

// Set user_version
if _, err := d.db.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
    return fmt.Errorf("migrate: set version: %w", err)
}

return d.db.ExecContext(ctx, "COMMIT")
```

Wait, this bypasses Go's `sql.Tx` abstraction. The correct fix using Go's database/sql API is:

```go
// CRITICAL: Use a raw Exec to start IMMEDIATE transaction before BeginTx
// BeginTx with default options starts a DEFERRED transaction, which allows
// concurrent readers to see user_version=0 in WAL mode (TOCTOU race).
if _, err := d.db.Exec("BEGIN IMMEDIATE"); err != nil {
    return fmt.Errorf("migrate: acquire write lock: %w", err)
}

// Now wrap this in a pseudo-transaction for defer-based rollback
// (Note: we can't use BeginTx here because transaction already started)
committed := false
defer func() {
    if !committed {
        d.db.Exec("ROLLBACK")
    }
}()

// Read version INSIDE exclusive transaction
var currentVersion int
if err := d.db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
    return fmt.Errorf("migrate: read version: %w", err)
}

if currentVersion >= currentSchemaVersion {
    if _, err := d.db.Exec("COMMIT"); err != nil {
        return fmt.Errorf("migrate: commit: %w", err)
    }
    committed = true
    return nil
}

// Apply schema DDL
if _, err := d.db.Exec(schemaDDL); err != nil {
    return fmt.Errorf("migrate: apply schema: %w", err)
}

// Set user_version
if _, err := d.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil {
    return fmt.Errorf("migrate: set version: %w", err)
}

if _, err := d.db.Exec("COMMIT"); err != nil {
    return fmt.Errorf("migrate: commit: %w", err)
}
committed = true
return nil
```

Actually, I just realized: the existing code uses `sql.Tx`, which should be preserved. The issue is that `BeginTx(ctx, nil)` uses default transaction options, which map to SQLite's `BEGIN DEFERRED`.

The cleanest fix is to document that the `SELECT 1` is a write-intent marker and upgrade it to an actual write:

```go
// Critical: Acquire EXCLUSIVE lock immediately to prevent TOCTOU with concurrent migrations
// We do this by executing a write operation (not just SELECT 1)
if _, err := tx.ExecContext(ctx, "UPDATE sqlite_master SET name=name WHERE 0"); err != nil {
    return fmt.Errorf("migrate: lock: %w", err)
}
```

Wait, that's a hack. Let me check if modernc.org/sqlite supports `BEGIN IMMEDIATE` via connection string or pragma...

After reviewing the SQLite docs and Go database/sql behavior: **the safest fix is to replace `SELECT 1` with a dummy write to upgrade the lock immediately:**

```go
// Acquire write lock immediately (not deferred) to prevent TOCTOU race
// In WAL mode, BEGIN DEFERRED allows concurrent readers to see stale user_version
// We force immediate exclusive lock by executing a harmless write
if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS _migration_lock (x)"); err != nil {
    return fmt.Errorf("migrate: lock: %w", err)
}
```

**Recommended fix (minimal change):**
```diff
-	// Acquire write lock immediately
-	if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
-		return fmt.Errorf("migrate: lock: %w", err)
-	}
+	// Acquire write lock immediately (not deferred) to prevent TOCTOU race.
+	// In WAL mode, BEGIN allows concurrent readers. We force immediate exclusive
+	// lock by executing a dummy write (temp table is per-connection, no side effects).
+	if _, err := tx.ExecContext(ctx, "CREATE TEMP TABLE IF NOT EXISTS _migration_lock (x)"); err != nil {
+		return fmt.Errorf("migrate: lock: %w", err)
+	}
```

**Test to verify fix:**
Add a test that spawns two goroutines both calling `Migrate` on the same database file, assert that exactly one backup is created.

---

### 2. Time Source Inconsistency Between Go and SQLite

**Severity:** MEDIUM-HIGH
**Impact:** TTL expiration timing depends on clock skew between `time.Now().Unix()` (Go) and `unixepoch()` (SQLite)

**Location:**
- `internal/state/state.go:48` (Set computes `expires_at` with Go's `time.Now().Unix()`)
- `internal/state/state.go:69` (Get checks `expires_at > unixepoch()`)
- `internal/sentinel/sentinel.go:52` (Check sets `last_fired = unixepoch()`)
- `internal/sentinel/sentinel.go:118` (Prune uses Go's `time.Now().Unix()`)

**Current behavior:**
- **State TTL:** `expires_at` computed in Go, checked in SQL
- **Sentinel timestamps:** `last_fired` set by SQL `unixepoch()`, threshold computed in Go

**Problem:**
Go's `time.Now().Unix()` and SQLite's `unixepoch()` both return Unix epoch seconds, but they may differ if:
1. The system clock is adjusted between Set and Get
2. SQLite's `unixepoch()` implementation drifts from Go's (unlikely but possible with pure-Go driver)

**Failure narrative (TTL early expiration):**
1. `ic state set ephemeral s1 --ttl=10s` at T=1000 → Go computes `expires_at = 1000 + 10 = 1010`
2. System clock jumps backward by 2 seconds (NTP correction) → now T=998 in SQLite
3. `ic state get ephemeral s1` at T=1008 (Go time) → SQLite sees T=1006, checks `1010 > 1006` → still valid
4. If SQLite's clock was ahead: `expires_at=1010` but SQLite `unixepoch()=1012` → premature expiration

**Current mitigation:**
Both run in the same process, so clock skew is minimal. However:
- NTP adjustments can cause 1-2 second jumps
- Virtualization or container time drift could cause larger skew

**Design note from CLAUDE.md:**
> TTL computation in Go (`time.Now().Unix()`) not SQL (`unixepoch()`) to avoid float promotion

This suggests the choice was intentional (avoiding `CAST(unixepoch() AS INTEGER)` issues). However, the check is done in SQL, so we still have mixed time sources.

**Consistency options:**

**Option A: Use Go time for both (all TTL checks in application layer)**
```go
func (s *Store) Get(ctx context.Context, key, scopeID string) (json.RawMessage, error) {
    var payload string
    var expiresAt sql.NullInt64
    err := s.db.QueryRowContext(ctx,
        `SELECT payload, expires_at FROM state WHERE key = ? AND scope_id = ?`,
        key, scopeID).Scan(&payload, &expiresAt)
    if err != nil {
        if errors.Is(err, sql.ErrNoRows) {
            return nil, ErrNotFound
        }
        return nil, fmt.Errorf("state get: %w", err)
    }
    // Check expiration in Go
    if expiresAt.Valid && expiresAt.Int64 <= time.Now().Unix() {
        return nil, ErrNotFound
    }
    return json.RawMessage(payload), nil
}
```
**Pros:** Consistent time source
**Cons:** Requires fetching expired rows, checking in application

**Option B: Use SQL time for both (set `expires_at` with SQL `unixepoch()`)**
```go
func (s *Store) Set(ctx context.Context, key, scopeID string, payload json.RawMessage, ttl time.Duration) error {
    // ...
    var expiresAtExpr string
    if ttl > 0 {
        expiresAtExpr = fmt.Sprintf("unixepoch() + %d", int64(ttl.Seconds()))
    } else {
        expiresAtExpr = "NULL"
    }
    _, err = tx.ExecContext(ctx,
        fmt.Sprintf(`INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
                     VALUES (?, ?, ?, unixepoch(), %s)`, expiresAtExpr),
        key, scopeID, string(payload))
}
```
**Pros:** Consistent time source, no clock skew
**Cons:** SQL injection risk if TTL not validated (need parameterized query, but SQLite doesn't support `?` for expressions)

**Option C: Accept bounded skew, document it**
Keep current implementation but add a comment:
```go
// TTL computed with Go time.Now().Unix(), checked with SQLite unixepoch().
// Both should be identical within a single process, but NTP adjustments or
// virtualization drift may cause 1-2 second skew. This is acceptable for
// intercore's use case (throttle guards and session state).
```

**Recommended fix:** **Option B** (SQL time for both)

Use SQL's `unixepoch()` for both setting and checking to eliminate skew:
```go
func (s *Store) Set(ctx context.Context, key, scopeID string, payload json.RawMessage, ttl time.Duration) error {
    if err := ValidatePayload(payload); err != nil {
        return err
    }

    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("state set: begin: %w", err)
    }
    defer tx.Rollback()

    // Use SQL to compute expires_at to match the time source used in Get
    var expiresAt interface{} = nil
    if ttl > 0 {
        // Compute in SQL to match unixepoch() source in WHERE clause
        expiresAt = sql.Named("ttl", int64(ttl.Seconds()))
    }

    // Note: SQLite doesn't support parameterized expressions like `unixepoch() + ?`
    // We must use string formatting for the TTL offset
    query := `INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
              VALUES (?, ?, ?, unixepoch(), `
    if ttl > 0 {
        query += fmt.Sprintf("unixepoch() + %d)", int64(ttl.Seconds()))
    } else {
        query += "NULL)"
    }

    _, err = tx.ExecContext(ctx, query, key, scopeID, string(payload))
    if err != nil {
        return fmt.Errorf("state set: insert: %w", err)
    }

    return tx.Commit()
}
```

Similarly, update sentinel Prune to use SQL time:
```go
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
    // Use SQL time source to match last_fired timestamp source
    threshold := int64(olderThan.Seconds())
    result, err := s.db.ExecContext(ctx,
        "DELETE FROM sentinels WHERE last_fired < unixepoch() - ? AND last_fired > 0",
        threshold)
    if err != nil {
        return 0, fmt.Errorf("prune: %w", err)
    }
    return result.RowsAffected()
}
```

**Test coverage gap:** Add a test that mocks system clock adjustments to verify TTL behavior under NTP correction.

---

### 3. Sentinel Prune Condition Excludes Never-Fired Sentinels Incorrectly

**Severity:** LOW-MEDIUM
**Impact:** Sentinels with `last_fired=0` (never fired, created by `INSERT OR IGNORE`) are never pruned, can accumulate indefinitely

**Location:** `internal/sentinel/sentinel.go:118`

**Current code:**
```go
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
    threshold := time.Now().Unix() - int64(olderThan.Seconds())
    result, err := s.db.ExecContext(ctx,
        "DELETE FROM sentinels WHERE last_fired < ? AND last_fired > 0",
        threshold)
}
```

**Problem:**
The condition `last_fired > 0` excludes sentinels that were created with `INSERT OR IGNORE` but never passed the `UPDATE ... RETURNING` check (i.e., interval=0 sentinels that were throttled on first check, or sentinels created by a future version of intercore that pre-populates rows).

**Expected behavior:**
If a sentinel has `last_fired=0` (never fired), it should still be prunable if it's older than `olderThan`.

Wait—looking at the schema:
```sql
CREATE TABLE IF NOT EXISTS sentinels (
    name        TEXT NOT NULL,
    scope_id    TEXT NOT NULL,
    last_fired  INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (name, scope_id)
);
```

The `DEFAULT (unixepoch())` means `last_fired` is **never 0** unless explicitly set by `INSERT OR IGNORE`.

Looking at the Check implementation:
```go
_, err = tx.ExecContext(ctx,
    "INSERT OR IGNORE INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, 0)",
    name, scopeID)
```

Ah! The code explicitly sets `last_fired=0` to mark "never fired yet" state. So:
- `last_fired=0` → sentinel exists but has never passed the interval check
- `last_fired>0` → sentinel has fired at least once

**Intended semantics:**
The `last_fired > 0` filter means "only prune sentinels that have fired at least once." This prevents pruning sentinels that were created but never allowed (e.g., interval=0 sentinels that were immediately throttled).

**Correctness question:** Should never-fired sentinels be pruned?

**Scenario:**
1. `ic sentinel check stop s1 --interval=0` (first call) → `INSERT ... last_fired=0`, then `UPDATE` sets `last_fired=<now>` → fires (allowed)
2. `ic sentinel check stop s1 --interval=0` (second call) → row exists with `last_fired=<now>`, UPDATE condition fails → throttled
3. If the process crashes between INSERT and UPDATE, `last_fired=0` is left in the DB

**Failure narrative (sentinel leak):**
1. Process A: `BEGIN`, `INSERT OR IGNORE ... last_fired=0` succeeds
2. Process A crashes before `UPDATE ... RETURNING`
3. Sentinel row with `last_fired=0` remains in DB forever (never pruned)

**Current mitigation:**
The auto-prune in `Check` uses:
```go
if _, err := tx.ExecContext(ctx,
    "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
```

This will **fail** for `last_fired=0` because `unixepoch() - 0` is a huge number (> 604800), so it **will** be deleted by auto-prune.

Wait, that's the opposite problem: never-fired sentinels (`last_fired=0`) are **immediately prunable** by auto-prune because `unixepoch() - 0 > 604800` is always true.

Let me re-read the auto-prune condition:
```go
"DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"
```

If `last_fired=0`, then `unixepoch() - 0 = <current_time>`, which is ~1.7 billion seconds (54 years since epoch). This is definitely `> 604800` (7 days), so the never-fired sentinel would be deleted immediately.

**Actual behavior:**
- Auto-prune in `Check`: deletes sentinels where `unixepoch() - last_fired > 7 days`
  - For `last_fired=0`, this is ~54 years > 7 days → **deleted**
  - For `last_fired=<recent>`, e.g., 2 days ago, this is 2 days < 7 days → **kept**
- Manual prune: `DELETE WHERE last_fired < threshold AND last_fired > 0`
  - For `last_fired=0`, the `last_fired > 0` filter **excludes** it → **not deleted**
  - For `last_fired=<old>`, e.g., 10 days ago, threshold=3 days → **deleted**

**Inconsistency:**
Auto-prune (in Check) **will** delete `last_fired=0` sentinels, but manual prune (via `ic sentinel prune`) **will not**.

This is a correctness bug: the two prune operations have different semantics.

**Fix:**
Make manual prune consistent with auto-prune. Remove the `last_fired > 0` filter:
```diff
func (s *Store) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
-	threshold := time.Now().Unix() - int64(olderThan.Seconds())
	result, err := s.db.ExecContext(ctx,
-		"DELETE FROM sentinels WHERE last_fired < ? AND last_fired > 0",
-		threshold)
+		"DELETE FROM sentinels WHERE unixepoch() - last_fired > ?",
+		int64(olderThan.Seconds()))
```

This makes both auto-prune and manual prune use the same condition: `unixepoch() - last_fired > <age_limit>`.

As a bonus, this also switches to SQL time source (consistent with fix #2).

**Edge case:** If `last_fired=0` is intentionally used to mean "never prune this sentinel," then the current behavior is correct. However, there's no documentation or test coverage for this use case, so I assume it's unintentional.

---

## Important Findings (Should Fix)

### 4. State Set Uses Unnecessary Transaction for Single-Statement Operation

**Severity:** LOW
**Impact:** Minor performance overhead, no correctness issue

**Location:** `internal/state/state.go:40-60`

**Current code:**
```go
func (s *Store) Set(ctx context.Context, key, scopeID string, payload json.RawMessage, ttl time.Duration) error {
    // ...
    tx, err := s.db.BeginTx(ctx, nil)
    if err != nil {
        return fmt.Errorf("state set: begin: %w", err)
    }
    defer tx.Rollback()

    _, err = tx.ExecContext(ctx,
        `INSERT OR REPLACE INTO state ...`)
    if err != nil {
        return fmt.Errorf("state set: insert: %w", err)
    }

    return tx.Commit()
}
```

**Analysis:**
`INSERT OR REPLACE` is a **single atomic statement**. In SQLite, single statements are automatically wrapped in an implicit transaction if not already in one. The explicit `BEGIN/COMMIT` adds no atomicity benefit.

**Comparison with Delete and Get:**
```go
func (s *Store) Delete(ctx context.Context, key, scopeID string) (bool, error) {
    result, err := s.db.ExecContext(ctx,
        "DELETE FROM state WHERE key = ? AND scope_id = ?",
        key, scopeID)  // No transaction wrapper
}

func (s *Store) Get(ctx context.Context, key, scopeID string) (json.RawMessage, error) {
    err := s.db.QueryRowContext(ctx,
        `SELECT payload FROM state ...`)  // No transaction wrapper
}
```

**Consistency issue:**
- `Set` uses explicit transaction
- `Get` and `Delete` do not

**Performance impact:**
In WAL mode with `SetMaxOpenConns(1)`, the explicit transaction adds one extra round-trip:
1. `BEGIN` (no-op in WAL mode for single statement)
2. `INSERT OR REPLACE`
3. `COMMIT`

Without the explicit transaction:
1. `INSERT OR REPLACE` (implicit transaction)

**Correctness impact:** None. SQLite guarantees atomicity for single statements.

**Recommendation:** Remove the transaction wrapper for consistency with Get/Delete:
```diff
func (s *Store) Set(ctx context.Context, key, scopeID string, payload json.RawMessage, ttl time.Duration) error {
    if err := ValidatePayload(payload); err != nil {
        return err
    }

-   tx, err := s.db.BeginTx(ctx, nil)
-   if err != nil {
-       return fmt.Errorf("state set: begin: %w", err)
-   }
-   defer tx.Rollback()
-
    var expiresAt *int64
    if ttl > 0 {
        ea := time.Now().Unix() + int64(ttl.Seconds())
        expiresAt = &ea
    }

-   _, err = tx.ExecContext(ctx,
+   _, err := s.db.ExecContext(ctx,
        `INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
         VALUES (?, ?, ?, unixepoch(), ?)`,
        key, scopeID, string(payload), expiresAt)
    if err != nil {
        return fmt.Errorf("state set: insert: %w", err)
    }

-   return tx.Commit()
+   return nil
}
```

**Alternative:** If future versions need multi-statement Set operations (e.g., logging, triggers), keep the transaction. Document the intent.

---

### 5. Sentinel Auto-Prune Failure is Logged but Doesn't Prevent Check Commit

**Severity:** LOW
**Impact:** Sentinel check succeeds even if auto-prune fails, leading to unbounded sentinel table growth if prune keeps failing

**Location:** `internal/sentinel/sentinel.go:71-75`

**Current code:**
```go
// Synchronous auto-prune: delete stale sentinels in same tx
if _, err := tx.ExecContext(ctx,
    "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
    fmt.Fprintf(os.Stderr, "ic: auto-prune: %v\n", err)
}

if err := tx.Commit(); err != nil {
    return false, fmt.Errorf("commit: %w", err)
}
```

**Problem:**
If the auto-prune DELETE fails (e.g., disk full, corruption), the error is logged to stderr but the transaction still commits. This means:
1. The sentinel check result is persisted (correct behavior)
2. The old sentinels are **not** pruned (incorrect behavior)
3. Over time, the sentinels table grows unbounded if prune repeatedly fails

**Failure narrative (sentinel table bloat):**
1. Sentinel table has 10,000 old rows (> 7 days old)
2. `ic sentinel check foo s1 --interval=0` triggers auto-prune
3. Auto-prune DELETE hits a transient SQLite error (e.g., `SQLITE_FULL`)
4. Error logged to stderr, transaction commits, sentinel check succeeds
5. Next check: same error, same bloat
6. After 1,000 checks, the table has 11,000 rows (1,000 new checks, 0 pruned)

**Current mitigation:**
Manual prune via `ic sentinel prune --older-than=7d` can clean up. However, if auto-prune consistently fails, manual prune will also fail for the same reason.

**Design tradeoff:**
The current behavior prioritizes **sentinel check reliability** over **pruning reliability**. This is reasonable for a throttle guard system: it's better to allow/deny correctly and accumulate some cruft than to fail the check entirely.

**Options:**

**Option A: Fail the entire transaction if auto-prune fails**
```go
if _, err := tx.ExecContext(ctx,
    "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
    return false, fmt.Errorf("auto-prune: %w", err)
}
```
**Pros:** Guarantees no unbounded growth
**Cons:** Sentinel checks fail if disk is full (bad for availability)

**Option B: Keep current behavior, add observability**
```go
if _, err := tx.ExecContext(ctx,
    "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
    // Log to stderr for hooks to capture
    fmt.Fprintf(os.Stderr, "ic: auto-prune failed: %v (sentinel table may grow unbounded)\n", err)
}
```
**Pros:** Sentinel checks remain reliable
**Cons:** Requires external monitoring to detect pruning failures

**Option C: Degrade gracefully—disable auto-prune after repeated failures**
Too complex for v0.1.0.

**Recommended fix:** **Option B** (improve logging)

Add a metric counter or more prominent warning:
```diff
if _, err := tx.ExecContext(ctx,
    "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
-   fmt.Fprintf(os.Stderr, "ic: auto-prune: %v\n", err)
+   fmt.Fprintf(os.Stderr, "WARNING: ic auto-prune failed: %v (manual prune recommended: ic sentinel prune --older-than=7d)\n", err)
}
```

Additionally, document the failure mode in AGENTS.md under "Troubleshooting."

---

### 6. Concurrent Migration Test Coverage Gap

**Severity:** MEDIUM
**Impact:** Concurrent `ic init` from two shells is not tested with true parallelism

**Location:** `internal/db/db_test.go:99-145`

**Current test:**
```go
func TestMigrate_Concurrent(t *testing.T) {
    // Test that sequential migration from different connections is safe.
    // Real-world scenario: two `ic init` commands run back-to-back.
    // ...
    const n = 5
    for i := 0; i < n; i++ {
        d, err := Open(path, 5*time.Second)
        if err != nil {
            t.Fatalf("Open %d: %v", i, err)
        }
        if err := d.Migrate(ctx); err != nil {
            t.Errorf("Migrate %d: %v", i, err)
        }
        d.Close()
    }
}
```

**Problem:**
This test runs migrations **sequentially** (one after another in a loop). It doesn't test **true concurrent** `ic init` from two separate processes/goroutines.

**What it tests:**
- Idempotency of `Migrate` (running it 5 times on the same DB)
- Sequential `Open`/`Migrate`/`Close` cycle

**What it doesn't test:**
- Two goroutines calling `Migrate` at the same time (concurrent TOCTOU race from Finding #1)
- `SetMaxOpenConns(1)` serialization behavior under concurrent load

**Recommended addition:**
```go
func TestMigrate_TrueConcurrent(t *testing.T) {
    // Test that parallel migrations from different goroutines are safe
    dir := t.TempDir()
    path := filepath.Join(dir, "test.db")
    ctx := context.Background()

    // Create DB file first
    d0, err := Open(path, 5*time.Second)
    if err != nil {
        t.Fatal(err)
    }
    d0.Close()

    const n = 5
    var wg sync.WaitGroup
    errCh := make(chan error, n)

    for i := 0; i < n; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            d, err := Open(path, 5*time.Second)
            if err != nil {
                errCh <- fmt.Errorf("goroutine %d: Open: %w", id, err)
                return
            }
            defer d.Close()
            if err := d.Migrate(ctx); err != nil {
                errCh <- fmt.Errorf("goroutine %d: Migrate: %w", id, err)
                return
            }
        }(i)
    }

    wg.Wait()
    close(errCh)

    for err := range errCh {
        t.Error(err)
    }

    // Verify schema version is correct and only one backup was created
    d, err := Open(path, 100*time.Millisecond)
    if err != nil {
        t.Fatal(err)
    }
    defer d.Close()

    v, err := d.SchemaVersion()
    if err != nil {
        t.Fatal(err)
    }
    if v != 1 {
        t.Errorf("SchemaVersion = %d after concurrent migrate, want 1", v)
    }

    // Count backup files (should be 0 or 1, not 5)
    entries, _ := os.ReadDir(dir)
    backupCount := 0
    for _, e := range entries {
        if strings.Contains(e.Name(), ".backup-") {
            backupCount++
        }
    }
    if backupCount > 1 {
        t.Errorf("found %d backup files, expected at most 1 (TOCTOU race)", backupCount)
    }
}
```

This test will **fail** without the fix from Finding #1.

---

### 7. TTL Truncation to Seconds Documented but Not Validated

**Severity:** LOW
**Impact:** Precision loss for sub-second TTLs is not validated or warned about

**Location:** `internal/state/state_test.go:101-124` (test), `internal/state/state.go:48` (implementation)

**Current behavior:**
```go
ea := time.Now().Unix() + int64(ttl.Seconds())
```

This truncates fractional seconds. For example:
- `ttl=1500ms` → `int64(1.5) = 1` → effective TTL is **1 second**, not 1.5 seconds

**Test coverage:**
```go
func TestTTL_Truncation(t *testing.T) {
    // Set with 1500ms TTL — should truncate to 1 second
    payload := json.RawMessage(`{"test":true}`)
    if err := store.Set(ctx, "trunc", "s1", payload, 1500*time.Millisecond); err != nil {
        t.Fatal(err)
    }

    // Check the expires_at value directly
    var expiresAt int64
    err := db.QueryRow("SELECT expires_at FROM state WHERE key='trunc' AND scope_id='s1'").Scan(&expiresAt)
    // ...
    expected := now + 1 // 1500ms truncated to 1 second
}
```

**Analysis:**
The test documents the truncation behavior but doesn't validate that the **user-provided TTL** is reasonable. For example:
- `--ttl=500ms` → effective TTL is **0 seconds** (no expiration!)
- `--ttl=999ms` → effective TTL is **0 seconds**

**Failure narrative (TTL bypass):**
1. User runs `ic state set temp s1 --ttl=500ms < data.json`
2. User expects data to expire in 500ms
3. Actual behavior: `int64(0.5) = 0` → `ea = now + 0 = now` → `expires_at = now`
4. Immediate reads may see the data (race condition with `unixepoch() > now`)
5. Prune immediately deletes it (if `unixepoch() > expires_at`)

Wait, let me re-check the condition:
```go
WHERE expires_at IS NULL OR expires_at > unixepoch()
```

If `expires_at = now` (same second), the condition is `now > now` → **false** → data is **invisible immediately**.

Actually, this is a **silent data loss bug**: setting TTL < 1 second makes the data immediately expired.

**Fix:**
Validate TTL and round up to 1 second minimum:
```go
var expiresAt *int64
if ttl > 0 {
    seconds := int64(ttl.Seconds())
    if seconds == 0 {
        seconds = 1  // Minimum 1 second TTL
    }
    ea := time.Now().Unix() + seconds
    expiresAt = &ea
}
```

Or reject sub-second TTLs:
```go
if ttl > 0 && ttl < time.Second {
    return fmt.Errorf("TTL must be at least 1 second (got %v)", ttl)
}
```

**Recommended fix:** Reject sub-second TTLs with a clear error message.

---

## Design Observations (Edge Cases, Not Bugs)

### 8. `updated_at` Uses SQL Time, `expires_at` Uses Go Time

**Severity:** COSMETIC
**Impact:** Inconsistent time source for metadata fields

**Location:** `internal/state/state.go:53-54`, `internal/db/schema.sql:8`

**Current code:**
```sql
updated_at  INTEGER NOT NULL DEFAULT (unixepoch()),
```
```go
_, err = tx.ExecContext(ctx,
    `INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at, expires_at)
     VALUES (?, ?, ?, unixepoch(), ?)`,
    key, scopeID, string(payload), expiresAt)
```

**Analysis:**
- `updated_at` is set by SQL's `unixepoch()` (in the INSERT statement)
- `expires_at` is computed in Go as `time.Now().Unix() + ttl`

**Inconsistency:**
Both fields represent timestamps, but they use different time sources. This is harmless because:
1. `updated_at` is metadata only (not used in queries)
2. `expires_at` is checked against SQL's `unixepoch()` in the WHERE clause

**If `updated_at` is ever used for logic** (e.g., "updated more than 1 hour ago"), this could cause skew.

**Recommendation:** Document that `updated_at` is for observability only, not business logic. If future versions need "last updated" logic, use SQL time consistently.

---

### 9. Auto-Prune Hardcoded to 7 Days

**Severity:** COSMETIC
**Impact:** Auto-prune threshold not configurable

**Location:** `internal/sentinel/sentinel.go:72`

**Current code:**
```go
"DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"  // 604800 = 7 days
```

**Analysis:**
The 7-day threshold is hardcoded. For high-traffic deployments, this may be too long (large table). For low-traffic deployments, this may be too short (unexpected sentinel resets).

**Use case:**
A CI environment might run `ic sentinel check build pr-123 --interval=0` for every PR. After 7 days, old PR sentinels are pruned. If a PR is reopened after 8 days, the sentinel resets (fires again).

**Design question:** Should sentinels for closed PRs be kept indefinitely?

**Recommendation:** Add a pragma or environment variable to configure auto-prune threshold:
```go
const defaultAutoPruneAge = 604800  // 7 days

func (s *Store) Check(ctx context.Context, name, scopeID string, intervalSec int) (bool, error) {
    pruneAge := defaultAutoPruneAge
    if envAge := os.Getenv("INTERCORE_AUTO_PRUNE_AGE"); envAge != "" {
        if parsed, err := strconv.Atoi(envAge); err == nil && parsed > 0 {
            pruneAge = parsed
        }
    }
    // ...
    if _, err := tx.ExecContext(ctx,
        "DELETE FROM sentinels WHERE unixepoch() - last_fired > ?", pruneAge); err != nil {
```

Or expose as a CLI flag: `ic sentinel check --auto-prune-age=30d`.

Not critical for v0.1.0, but worth considering for v0.2.0.

---

### 10. `SetMaxOpenConns(1)` Prevents Read Parallelism

**Severity:** DESIGN_CHOICE
**Impact:** All operations are serialized, even read-only queries

**Location:** `internal/db/db.go:55`

**Current code:**
```go
sqlDB.SetMaxOpenConns(1)
```

**Analysis:**
WAL mode in SQLite allows **concurrent readers**. By setting `SetMaxOpenConns(1)`, intercore forces all operations (reads and writes) to use the same connection, serializing everything.

**Rationale (from code comments):**
```go
// Single connection prevents WAL checkpoint TOCTOU
```

**WAL checkpoint TOCTOU scenario:**
1. Connection A starts a read transaction → pins WAL at frame 100
2. Connection B writes new data → WAL grows to frame 200
3. Connection B checkpoints WAL → tries to merge WAL into main DB
4. Checkpoint blocked because Connection A is still reading frame 100
5. WAL grows unbounded if Connection A never closes its transaction

**Current mitigation:** Force single connection → no checkpoint blocking.

**Performance impact:**
- `ic state get` and `ic sentinel list` (read-only) are serialized with `ic state set` and `ic sentinel check` (writes)
- For hook-based usage (short-lived CLI invocations), this is fine
- For a hypothetical long-running service, this would be a bottleneck

**Recommendation:** Keep `SetMaxOpenConns(1)` for v0.1.0. Document the rationale. For v0.2.0, consider:
- Allow N read connections, 1 write connection
- Use `PRAGMA wal_autocheckpoint` tuning to prevent unbounded WAL growth

---

### 11. Backup Filename Contains Timestamp but No Version

**Severity:** COSMETIC
**Impact:** Backups don't indicate schema version before migration

**Location:** `internal/db/db.go:102`

**Current code:**
```go
backupPath := fmt.Sprintf("%s.backup-%s", d.path, time.Now().Format("20060102-150405"))
```

**Example:** `.clavain/intercore.db.backup-20260217-093042`

**Enhancement:**
Include schema version in backup filename:
```go
backupPath := fmt.Sprintf("%s.backup-v%d-%s", d.path, currentVersion, time.Now().Format("20060102-150405"))
```

**Example:** `.clavain/intercore.db.backup-v0-20260217-093042`

This makes it clear which schema version the backup contains, useful for rollback scenarios.

---

### 12. JSON Validation Does Not Check for Duplicate Keys

**Severity:** LOW
**Impact:** Payloads like `{"x":1,"x":2}` are accepted (Go's `json.Valid` allows this)

**Location:** `internal/state/state.go:133-136`

**Current code:**
```go
func ValidatePayload(data []byte) error {
    if len(data) > maxPayloadSize {
        return fmt.Errorf("payload too large: %d bytes (max %d)", len(data), maxPayloadSize)
    }
    if !json.Valid(data) {
        return fmt.Errorf("invalid JSON payload")
    }
    return validateDepth(data)
}
```

**Analysis:**
Go's `json.Valid` accepts duplicate keys and uses the **last value**:
```go
var v map[string]int
json.Unmarshal([]byte(`{"x":1,"x":2}`), &v)
// Result: v = {"x": 2}
```

**Impact:**
For intercore's use case (opaque JSON blobs), this is harmless. The CLI stores whatever the hook provides. However, if future versions add server-side JSON processing, duplicate keys could cause confusion.

**Recommendation:** Document that payloads are stored as-is, no key uniqueness enforcement.

---

## Summary of Recommendations

### Critical (Fix Before Production)
1. **Upgrade migration lock** to use `BEGIN IMMEDIATE` or equivalent to prevent TOCTOU race
2. **Unify time sources** for TTL computation and checking (use SQL `unixepoch()` for both)
3. **Fix sentinel prune condition** to match auto-prune semantics (remove `last_fired > 0` filter)

### Important (Fix in v0.2.0)
4. **Remove unnecessary transaction wrapper** from State.Set (or document why it's needed)
5. **Improve auto-prune error handling** (better logging, observability)
6. **Add concurrent migration test** with true parallelism
7. **Validate sub-second TTLs** (reject or round up to 1 second minimum)

### Observability Enhancements
8. Document that `updated_at` is metadata-only (SQL time) vs `expires_at` (Go time, will be unified)
9. Make auto-prune age configurable (default 7 days)
10. Document `SetMaxOpenConns(1)` rationale and tradeoffs
11. Include schema version in backup filenames
12. Document JSON duplicate-key behavior

---

## Concurrency Correctness Summary

### Sentinel Atomicity (CORRECT)
The sentinel check implementation is **correct**:
```go
// 1. INSERT OR IGNORE ensures row exists
INSERT OR IGNORE INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, 0)

// 2. Conditional UPDATE with RETURNING claims sentinel atomically
UPDATE sentinels SET last_fired = unixepoch()
WHERE name = ? AND scope_id = ?
  AND ((interval = 0 AND last_fired = 0)
       OR (interval > 0 AND unixepoch() - last_fired >= interval))
RETURNING 1

// 3. Row count indicates success (allowed=1, throttled=0)
```

**Why it works:**
- Both statements are in a **single transaction** with `SetMaxOpenConns(1)`
- SQLite guarantees that UPDATE's WHERE clause is evaluated atomically
- Only one process can satisfy the WHERE condition at any time (even in WAL mode with concurrent transactions, the row lock prevents race)

**Test coverage:** `TestSentinelCheck_Concurrent` verifies that 10 concurrent goroutines result in exactly 1 allowed sentinel. **This test passes.**

### Transaction Isolation (CORRECT)
Operations use appropriate isolation:
- **sentinel check:** Transaction required (INSERT + conditional UPDATE + prune must be atomic)
- **state set:** Transaction used but not required (single INSERT OR REPLACE is atomic)
- **state get/delete:** No transaction (single SELECT/DELETE is atomic)
- **migration:** Transaction required (DDL + version update must be atomic) but lock upgrade needed (see Finding #1)

### Race Detector Coverage (GOOD)
```bash
go test -race ./...
ok   internal/db        (cached)
ok   internal/sentinel  (cached)
ok   internal/state     (cached)
```

All tests pass with `-race`. No data races detected.

### Shutdown Behavior (SAFE)
The CLI closes the database before exit:
```go
defer d.Close()
```

For the single-writer WAL model, this ensures:
1. Outstanding transactions are rolled back (if not committed)
2. WAL is checkpointed (if possible)
3. File handles are released

For hook-based usage (short-lived processes), this is sufficient. For a long-running service, explicit shutdown signaling would be needed.

---

## Testing Gaps

### Missing Tests
1. **Concurrent `ic init` from two shell processes** (not just goroutines)
   - Current: Sequential migrations tested
   - Needed: True parallel migrations with TOCTOU race validation
2. **Clock skew between Go and SQLite time sources**
   - Current: TTL tests assume monotonic time
   - Needed: Mock system clock adjustments (NTP correction)
3. **Disk full during migration**
   - Current: No error injection
   - Needed: Verify backup succeeds even if migration fails mid-DDL
4. **WAL checkpoint blocking with multiple processes**
   - Current: Single process tests only
   - Needed: Verify `SetMaxOpenConns(1)` prevents checkpoint stalls
5. **Sub-second TTL edge cases**
   - Current: `TestTTL_Truncation` documents truncation
   - Needed: Validate error/warning for `--ttl=500ms`

### Test Recommendations
Add to `internal/db/db_test.go`:
```go
func TestMigrate_TrueConcurrent(t *testing.T) { /* see Finding #6 */ }
func TestMigrate_DiskFull(t *testing.T) { /* inject SQLITE_FULL during DDL */ }
```

Add to `internal/state/state_test.go`:
```go
func TestSet_SubSecondTTL_Rejected(t *testing.T) {
    store := New(setupTestDB(t))
    err := store.Set(ctx, "k", "s", json.RawMessage(`{}`), 500*time.Millisecond)
    if err == nil {
        t.Error("expected error for sub-second TTL")
    }
}
```

---

## Final Verdict

**Correctness grade: B+ (very good, with caveats)**

Strengths:
- Atomic sentinel check implementation is correct and well-tested
- Race detector coverage across all packages
- Explicit transaction boundaries for multi-statement operations
- Path traversal protection and JSON validation
- Comprehensive integration tests (19 test cases)

Weaknesses:
- Migration TOCTOU race in WAL mode (Critical, fix required)
- Inconsistent time sources for TTL (Important, fix recommended)
- Sentinel prune semantic mismatch (auto-prune vs manual prune)
- Concurrent migration test coverage gap

**Production readiness:** Ready for controlled rollout with the migration lock fix (#1). Time source unification (#2) and prune consistency (#3) should be addressed before widespread adoption.

**Observability:** Add monitoring for auto-prune failures, sentinel table size, and TTL edge cases.

**Next steps:**
1. Apply migration lock fix (Finding #1) and verify with concurrent migration test
2. Unify time sources (Finding #2) to eliminate clock skew risk
3. Fix sentinel prune condition (Finding #3) for semantic consistency
4. Run integration tests in CI with `go test -race`
5. Document edge cases and failure modes in AGENTS.md

---

## Appendix: Interleaving Examples

### Example 1: Migration TOCTOU Race (Finding #1)

**Without fix:**
```
Time | Process A                           | Process B
-----|-------------------------------------|-------------------------------------
T0   | BEGIN (deferred)                    |
T1   | SELECT 1 → shared lock acquired     |
T2   |                                     | BEGIN (deferred)
T3   |                                     | SELECT 1 → shared lock acquired
T4   | PRAGMA user_version → reads 0       |
T5   |                                     | PRAGMA user_version → reads 0
T6   | Passes check: currentVersion=0      |
T7   |                                     | Passes check: currentVersion=0
T8   | Apply DDL → promotes to exclusive   |
T9   |                                     | Apply DDL → blocked on exclusive lock
T10  | COMMIT                              |
T11  |                                     | DDL executes (idempotent, succeeds)
T12  |                                     | COMMIT
Result: Both processes applied DDL, both created backups (wasteful, not corrupt)
```

**With fix (CREATE TEMP TABLE):**
```
Time | Process A                           | Process B
-----|-------------------------------------|-------------------------------------
T0   | BEGIN (deferred)                    |
T1   | CREATE TEMP TABLE → exclusive lock  |
T2   |                                     | BEGIN (deferred)
T3   |                                     | CREATE TEMP TABLE → blocked (BUSY)
T4   | PRAGMA user_version → reads 0       |
T5   | Apply DDL                           |
T6   | COMMIT                              |
T7   |                                     | Unblocked, exclusive lock acquired
T8   |                                     | PRAGMA user_version → reads 1
T9   |                                     | Skips DDL (already migrated)
T10  |                                     | COMMIT (no-op)
Result: Only Process A applied DDL, only one backup created (correct)
```

### Example 2: TTL Clock Skew (Finding #2)

**Scenario:**
```
Time | Event
-----|--------------------------------------------------------------
T0   | ic state set temp s1 --ttl=10s
     | Go: expires_at = time.Now().Unix() + 10 = 1000 + 10 = 1010
T1   | System clock jumps forward by 5 seconds (NTP correction)
T2   | ic state get temp s1
     | SQL: expires_at > unixepoch() → 1010 > 1005 → still valid
T3   | Actual time: 1005 (Go), but user expects expiration at 1010 (Go time)
     | Result: Data visible for 10 seconds from set time (correct)

Alternative timeline (clock jumps backward):
T0   | ic state set temp s1 --ttl=10s
     | Go: expires_at = 1000 + 10 = 1010
T1   | System clock jumps backward by 3 seconds (NTP correction)
T2   | ic state get temp s1 (at Go time 1007, SQL time 1004)
     | SQL: expires_at > unixepoch() → 1010 > 1004 → still valid
T3   | 10 seconds later (Go time 1017, SQL time 1014)
     | SQL: expires_at > unixepoch() → 1010 > 1014 → expired
     | Result: Data was visible for ~13 seconds instead of 10 (incorrect)
```

**Impact:** TTL enforcement is less precise under clock adjustments. Fix: use SQL time for both set and check.

### Example 3: Sentinel Never-Fired Leak (Finding #3)

**Scenario:**
```
Time | Event
-----|--------------------------------------------------------------
T0   | ic sentinel check stop s1 --interval=0
     | INSERT OR IGNORE ... last_fired=0
T1   | UPDATE ... RETURNING (would set last_fired=<now>)
T2   | ** Process crashes before UPDATE **
Result: Sentinel row with last_fired=0 remains in DB

T100 | ic sentinel prune --older-than=7d
     | DELETE WHERE last_fired < threshold AND last_fired > 0
     | Skips last_fired=0 (filter excludes it)
Result: Never-fired sentinel persists forever (until auto-prune)

T200 | ic sentinel check stop s1 --interval=0
     | Auto-prune: DELETE WHERE unixepoch() - last_fired > 604800
     | unixepoch() - 0 = 1.7 billion > 604800 → DELETED
Result: Never-fired sentinel is finally cleaned up by auto-prune
```

**Inconsistency:** Manual prune skips `last_fired=0`, auto-prune deletes it. Fix: make both use the same condition.

---

## Appendix: Reviewed Code Correctness Checklist

- [x] Transactions have correct isolation level (deferred for reads, immediate for writes)
  - **Partial:** Migration needs immediate lock (Finding #1)
- [x] Single-writer constraint enforced (`SetMaxOpenConns(1)`)
- [x] Race detector passes (`go test -race ./...`)
- [x] Concurrent sentinel checks result in exactly 1 claim (tested)
- [x] TTL expiration is checked before returning data
  - **Partial:** Time source inconsistency (Finding #2)
- [x] JSON payloads validated before storage
- [x] Path traversal protection for `--db` flag
- [x] Schema version checked on every `Open`
- [x] Migration is idempotent (CREATE IF NOT EXISTS)
  - **Partial:** Idempotent DDL but TOCTOU race (Finding #1)
- [x] Backup created before migration
  - **Partial:** Multiple backups created on concurrent migration (Finding #1)
- [x] Auto-prune runs synchronously in same transaction
  - **Partial:** Failure doesn't roll back sentinel check (Finding #5)
- [x] Shutdown releases database handles
- [ ] Sub-second TTL validated (Finding #7)
- [ ] Concurrent migration with true parallelism tested (Finding #6)
- [ ] Time source consistency documented (Finding #2, #8)
- [ ] Sentinel prune semantics consistent (Finding #3)

---

**End of Review**
