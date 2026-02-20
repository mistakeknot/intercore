# Correctness Review: 2026-02-18-intercore-run-tracking Plan

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-18
**Plan file:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-run-tracking.md`
**Files reviewed:**
- `/root/projects/Interverse/infra/intercore/internal/db/schema.sql`
- `/root/projects/Interverse/infra/intercore/internal/db/db.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/store.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/phase.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/machine.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/errors.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/main.go`
- `/root/projects/Interverse/infra/intercore/internal/db/db_test.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/store_test.go`
- `/root/projects/Interverse/infra/intercore/lib-intercore.sh`

---

## Invariants That Must Remain True

Before findings, here are the invariants this codebase enforces (derived from reading the code, not the plan):

1. **Schema version monotonicity:** `PRAGMA user_version` only increases. `Open()` rejects any DB where version > `maxSchemaVersion`.
2. **Additive DDL only:** All tables use `CREATE TABLE IF NOT EXISTS`. A migration that re-applies the full `schema.sql` against a v1 or v2 database must not fail on pre-existing tables.
3. **Single connection:** `SetMaxOpenConns(1)` is set on every `*sql.DB`. All concurrent requests serialize through the one connection.
4. **WAL mode:** WAL journal mode is set explicitly after `sql.Open`. No checkpoint races because there is only one connection.
5. **Foreign key enforcement must be explicit:** SQLite does not enforce `REFERENCES` clauses unless `PRAGMA foreign_keys = ON` is set per-connection, per-session.
6. **Timestamps come from Go, not SQL:** Design decision documented in `CLAUDE.md` and `AGENTS.md` — `time.Now().Unix()` is used for inserted rows. `DEFAULT (unixepoch())` in DDL is a fallback only, never relied upon for consistency.
7. **Optimistic concurrency on phase transitions:** `UpdatePhase` uses `WHERE phase = ?` to detect concurrent advances. This is the sole write-level concurrency guard for phase state.
8. **Exit codes are load-bearing:** Bash callers interpret 0=found, 1=not-found, 2=error, 3=usage. New commands must follow this scheme exactly.

---

## Findings

### P0 — BLOCKING

---

#### P0-1: Foreign Key Enforcement is Silently Off for the New Tables

**Severity:** P0 — silent data corruption
**Files:** `internal/db/db.go` (Open function), `internal/db/schema.sql` (proposed DDL)

**The problem:**

SQLite does not enforce `REFERENCES` (foreign key) constraints unless `PRAGMA foreign_keys = ON` is issued on every connection, every session. It is off by default. The existing `Open()` function in `db.go` sets two PRAGMAs explicitly:

```go
// db.go lines 59–66
if _, err := sqlDB.Exec(fmt.Sprintf("PRAGMA busy_timeout = %d", busyTimeout.Milliseconds())); err != nil { ... }
if _, err := sqlDB.Exec("PRAGMA journal_mode = WAL"); err != nil { ... }
```

`PRAGMA foreign_keys` is not there. It is not in the DSN either (the DSN `_pragma` path is noted in comments as unreliable and is intentionally not relied upon).

The existing tables `phase_events` and (proposed) `run_agents` and `run_artifacts` all carry `REFERENCES runs(id)` declarations. Without `foreign_keys = ON`, those declarations are parsed and stored in schema but never checked at insert time.

**Failure narrative:**

1. An integration test or script calls `ic run agent add <stale_run_id>`.
2. The run was previously deleted (direct DB manipulation, failed migration, test cleanup). The run row does not exist.
3. `AddAgent` does `INSERT INTO run_agents ... VALUES (?, ?, ...)` with `run_id = stale_run_id`.
4. SQLite accepts the insert without complaint. FK enforcement is off.
5. `run_agents` now contains a row referencing a non-existent run. `ic run agent list <stale_run_id>` returns a result. The data is orphaned.

The plan's test `TestStore_AddAgent_BadRunID` explicitly expects FK rejection:

> "add agent for non-existent run (FK constraint should reject)"

That test will **pass vacuously** (no error returned) unless `foreign_keys = ON` is set. The plan's correctness guarantee for this test is wrong.

**Fix:**

Add a third explicit PRAGMA in `Open()`:

```go
// internal/db/db.go — after WAL PRAGMA, before version check
if _, err := sqlDB.Exec("PRAGMA foreign_keys = ON"); err != nil {
    sqlDB.Close()
    return nil, fmt.Errorf("open: enable foreign keys: %w", err)
}
```

This must be done in `Open()` rather than in `Migrate()` because FK enforcement is needed for all operations (read-write and migration), not just migration. It is a session-level PRAGMA — it must be set every time a connection opens.

**Note on existing tables:** `phase_events(run_id REFERENCES runs(id))` was already declared with a FK, and it was already not enforced. Fixing this now is the right call but be aware that if any existing data is already orphaned (unlikely in practice since `ic` creates runs before events), enabling FK enforcement would not retroactively break reads. SQLite only checks FKs at write time.

---

#### P0-2: Migration Logic Applies Full schema.sql but Version Check is Monotonic — v3→v4 Will Apply All DDL Including v1/v2 Tables Already There

**Severity:** P0 (conditional) — migration silently becomes a no-op for new tables if executed against a live v3 DB
**Files:** `internal/db/db.go` (`Migrate` function), `internal/db/schema.sql`

**The problem:**

The migration function does this:

```go
// db.go lines 127–141
if currentVersion >= currentSchemaVersion {
    return nil // already migrated
}
// Apply schema DDL
if _, err := tx.ExecContext(ctx, schemaDDL); err != nil { ... }
// Set user_version
if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", currentSchemaVersion)); err != nil { ... }
return tx.Commit()
```

The condition is `currentVersion >= currentSchemaVersion`. For a v3 database being migrated to v4, after bumping `currentSchemaVersion = 4`:

- `currentVersion` = 3 (existing DB)
- `currentSchemaVersion` = 4
- `3 >= 4` is false, so migration proceeds — so far correct.

However, it then executes the full `schemaDDL` (the entire `schema.sql` embedded at compile time). Because all DDL uses `CREATE TABLE IF NOT EXISTS`, all v1/v2/v3 tables are skipped silently (they already exist), and the v4 tables (`run_agents`, `run_artifacts`) are created. This is the "additive DDL" property and it works correctly here.

**The real risk:** `currentSchemaVersion` is read from the Go constant and compared to the DB version. If someone bumps the constant to 4 but forgets to append the v4 DDL to `schema.sql`, the migration "succeeds" — it commits `user_version = 4` — but the new tables were never created. Every subsequent `ic run agent add` then fails with "no such table."

This is not a race condition but a deployment correctness gap. The plan does specify both changes (Task 1), but they must happen atomically — one cannot be done without the other.

**Secondary issue:** The `Open()` function checks at open time:

```go
// db.go lines 70–75
if version > maxSchemaVersion {
    sqlDB.Close()
    return nil, ErrSchemaVersionTooNew
}
```

If you deploy the new binary (with `maxSchemaVersion = 4`) to a machine that still has a v3 DB, the DB opens fine (`3 > 4` is false) and `ic init` migrates. But if you deploy the old binary to a machine that already ran `ic init` with the new binary (v4 DB), it refuses to open. That is the intended safety behavior. The deployment pattern is: ship binary, run `ic init`. Do not run `ic init` on a production DB before shipping the binary. This is correctly documented in `docs/solutions/environment-issues` but should be spelled out in the plan's acceptance criteria.

**Fix:** The plan should add an explicit acceptance criterion: `TestMigrate_V3ToV4` — simulate a v3 DB (apply v1–v3 DDL, set `user_version = 3`), run `Migrate()`, assert version = 4 and both new tables exist. This mirrors the existing `TestMigrate_V2ToV3` test. Without this test, the "both changes happen together" property is not verified automatically.

---

### P1 — MUST FIX BEFORE SHIPPING

---

#### P1-1: Timestamp Conflict Between DDL DEFAULT and Go-Side Insertion for `updated_at` in `run_agents`

**Severity:** P1 — incorrect values under failure or retry conditions
**Files:** `internal/db/schema.sql` (proposed DDL), plan Task 2

**The problem:**

The plan correctly instructs: "use `time.Now().Unix()` for timestamps (not SQL `unixepoch()`)". This follows the existing pattern in all store methods (e.g., `Create` in `store.go` line 47: `now := time.Now().Unix()`).

However, the proposed DDL for `run_agents` includes:

```sql
updated_at  INTEGER NOT NULL DEFAULT (unixepoch())
```

And `run_artifacts` has `created_at INTEGER NOT NULL DEFAULT (unixepoch())`.

The `DEFAULT (unixepoch())` path is only used if the column is not provided in the INSERT. The plan says the store method will supply the value from Go. So in the happy path, there is no conflict.

**The actual failure mode** is in `UpdateAgent`. The plan signature is:

```go
func (s *Store) UpdateAgent(ctx context.Context, id, status string) error
```

If the implementation writes `UPDATE run_agents SET status = ? WHERE id = ?` without also updating `updated_at = ?`, then `updated_at` stays at its original value from INSERT time. It does not use `DEFAULT` on UPDATE (SQLite DEFAULTs only apply on INSERT). The column goes stale.

This is the classic "forgot to update updated_at on UPDATE" mistake. The plan does not show the implementation body of `UpdateAgent`, only its signature.

**Failure narrative:** Agent is added at t=100. At t=200, status changes to `completed`. A consumer reads `updated_at = 100` — cannot tell whether the agent just died or was completed hours ago.

**Fix:** The plan must explicitly state that `UpdateAgent` must include `updated_at = ?` with `time.Now().Unix()`:

```go
result, err := s.db.ExecContext(ctx, `
    UPDATE run_agents SET status = ?, updated_at = ? WHERE id = ?`,
    status, time.Now().Unix(), id,
)
```

This matches how `UpdatePhase` and `UpdateStatus` both set `updated_at` on every write.

---

#### P1-2: `TestStore_AddAgent_BadRunID` Will Pass Vacuously Without FK Enforcement

**Severity:** P1 — test gives false confidence
**Files:** Proposed `internal/phase/agent_test.go`

This is the direct test consequence of P0-1. The plan specifies:

> `TestStore_AddAgent_BadRunID` — add agent for non-existent run (FK constraint should reject)

Without `PRAGMA foreign_keys = ON`, the INSERT succeeds and the test either:
- Passes vacuously (if written as `if err == nil { t.Error(...) }` — it fails to fail), or
- Panics at assertion (if written expecting an error).

The correct fix is to resolve P0-1 first (enable FK enforcement in `Open()`). Then this test will correctly exercise the constraint. The test plan item is architecturally right — it just depends on P0-1 being in place.

**Additionally:** Tests in `setupTestStore(t)` call `db.Open()` then `db.Migrate()`. Once P0-1 is fixed, every test in the phase package automatically gets FK enforcement without any change to the test setup function. This is exactly the right place to fix it.

---

#### P1-3: `ic run current` Has a Soft TOCTOU Window on `project_dir` Match

**Severity:** P1 — incorrect result under concurrent run creation
**Files:** Proposed `cmd/ic/main.go` (`cmdRunCurrent`)

**The problem:**

The plan specifies:

```sql
SELECT id FROM runs WHERE status = 'active' AND project_dir = ? ORDER BY created_at DESC LIMIT 1
```

`project_dir` is stored as a plain string — whatever was passed to `ic run create --project=<dir>`. In the codebase, when `--project` is omitted, `os.Getwd()` is used. On the reader side, `ic run current` also defaults to `os.Getwd()`.

**TOCTOU scenario:**

1. Agent A runs `ic run create --project=/projects/foo` from CWD `/projects/foo`. Run `r1` is created with `project_dir = /projects/foo`.
2. Agent B is in CWD `/projects/foo/subdir`. It runs `ic run current` (no `--project`). CWD resolves to `/projects/foo/subdir`, which does not match `r1.project_dir = /projects/foo`. Returns exit 1 (not found).
3. Agent B was looking for its parent run and got nothing.

This is not a concurrency race in the database sense — the query is atomic. It is a semantic mismatch: `project_dir` is compared as an exact string, but filesystem paths can refer to the same directory via different representations (trailing slashes, symlinks, relative vs. absolute).

**The more serious variant:**

1. Agent A creates run `r1` for `/projects/foo` with a specific goal.
2. Agent A is interrupted mid-phase.
3. Agent C starts fresh and creates run `r2` for `/projects/foo` 500ms later (both are 'active').
4. `ic run current` returns `r2` (most recent). All `agent add` and `artifact add` calls now attach to `r2`. Agent A's run `r1` accumulates no artifacts and looks abandoned.

There is no uniqueness constraint preventing two simultaneous active runs for the same `project_dir`. Whether this is intentional (multi-goal concurrency) or a hazard depends on use. If at-most-one active run per project is an invariant, it should be enforced with a partial unique index:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_runs_one_active_per_project
    ON runs(project_dir) WHERE status = 'active';
```

This would make `ic run create` fail loudly if a run for that project is already active, rather than silently allowing two and having `ic run current` return whichever was created most recently.

**The plan does not address this.** It should either: (a) add the partial unique index and handle the constraint violation in `Create`, or (b) explicitly document "multiple active runs per project are intentional" and add a test proving `current` returns the most recent.

**Fix (if at-most-one is intended):** Add the partial unique index to the v4 DDL. In `cmdRunCreate`, if the insert fails with a UNIQUE constraint violation, return a clear error: "a run is already active for this project — cancel it first or use --force."

**Fix (if multiple active runs are intentional):** Add a `--list` flag to `ic run current` that returns all active runs, and document that `current` is a "best guess" returning the newest. Add the test from the plan explicitly.

---

#### P1-4: Migration Does Not Rollback Before Creating Backup — Backup May Be Stale on Concurrent `ic init`

**Severity:** P1 — backup integrity
**Files:** `internal/db/db.go` (`Migrate` function)

The backup is created before the transaction starts:

```go
// db.go lines 101–105
if info, err := os.Stat(d.path); err == nil && info.Size() > 0 {
    backupPath := fmt.Sprintf(...)
    if err := copyFile(d.path, backupPath); err != nil { ... }
}
```

Then the transaction begins. This is safe because:
1. `SetMaxOpenConns(1)` means only one connection exists.
2. WAL mode means readers do not block writes and vice versa.
3. There is no OS-level lock preventing `copyFile` from reading a consistent snapshot.

In WAL mode, `copyFile` of the main DB file while a WAL file exists is not a complete backup — the WAL file contains pages not yet checkpointed to the main DB file. If the process crashes between `copyFile` and `Commit`, the backup may be missing committed pages from prior transactions that are in the WAL.

This is an existing issue (not introduced by this plan) but worth noting. The v4 migration does not make it worse. For operational correctness, restoring from the backup would require also copying the WAL file (`intercore.db-wal`). The plan does not mention WAL handling in backups.

**Minimal fix:** Document in `AGENTS.md` that restoring from a migration backup requires copying both `intercore.db.backup-*` AND the `intercore.db-wal` file at the same moment, or use SQLite's online backup API. This is out of scope for this plan but should be a follow-up.

---

### P2 — SHOULD FIX

---

#### P2-1: `UpdateAgent` Missing ErrNotFound Detection Pattern

**Severity:** P2 — incorrect error semantics
**Files:** Proposed `internal/phase/store.go`

The plan specifies `UpdateAgent` returns `ErrNotFound` if the agent doesn't exist. The existing pattern for this is:

```go
// store.go lines 113–124 (UpdatePhase)
n, err := result.RowsAffected()
if n == 0 {
    _, getErr := s.Get(ctx, id)
    if getErr != nil {
        return ErrNotFound
    }
    return ErrStalePhase
}
```

For `UpdateAgent`, the logic is simpler (no optimistic concurrency — just status update), so the pattern should be:

```go
n, err := result.RowsAffected()
if n == 0 {
    return ErrNotFound
}
```

This is straightforward. The risk: if the implementer uses the `UpdatePhase` pattern verbatim and calls `s.GetAgent(ctx, id)` to distinguish not-found from stale, but `GetAgent` itself is correct, this works fine. If the implementer forgets `RowsAffected` entirely and just checks `err != nil`, a silent no-op update returns success even when the agent ID does not exist.

The plan test `TestStore_UpdateAgent_NotFound` guards against this. Make sure the test does NOT pass vacuously (verify the test would fail if `RowsAffected` check is omitted).

---

#### P2-2: `ListArtifacts` Phase Filter Uses SQL NULL Differently Than Expected

**Severity:** P2 — silent wrong results
**Files:** Proposed `internal/phase/store.go` (`ListArtifacts`)

The plan signature:

```go
func (s *Store) ListArtifacts(ctx context.Context, runID string, phase *string) ([]*RunArtifact, error)
```

When `phase == nil`, list all artifacts. When `phase != nil`, filter by phase. This is the same pattern as `List` for runs.

The SQL must handle this correctly. A common mistake:

```sql
-- Wrong: if phase is nil, this becomes WHERE phase = NULL which matches nothing
WHERE run_id = ? AND phase = ?
```

The safe pattern used by the existing codebase (`List` in store.go) branches in Go:

```go
if phase != nil {
    return s.queryArtifacts(ctx,
        "SELECT ... FROM run_artifacts WHERE run_id = ? AND phase = ? ORDER BY created_at ASC",
        runID, *phase)
}
return s.queryArtifacts(ctx,
    "SELECT ... FROM run_artifacts WHERE run_id = ? ORDER BY created_at ASC",
    runID)
```

This is safe but must be explicit. If the implementer uses a `COALESCE` trick or passes a nil pointer directly to the SQL driver, the behavior depends on driver internals and may return 0 rows silently when unfiltered list is requested.

**Fix:** The plan should specify "use Go-level branch for nil phase filter, following the existing `List` pattern at store.go lines 198–205." This avoids the `WHERE phase = ?` with nil gotcha.

---

#### P2-3: `run_agents` Partial Index on `status = 'active'` May Not Accelerate `ListAgents`

**Severity:** P2 — performance footgun, not correctness
**Files:** Proposed `internal/db/schema.sql`

The proposed DDL includes:

```sql
CREATE INDEX IF NOT EXISTS idx_run_agents_status ON run_agents(status) WHERE status = 'active';
```

This partial index only covers rows where `status = 'active'`. `ListAgents` in the plan lists ALL agents for a run (not just active ones). The query will be:

```sql
SELECT ... FROM run_agents WHERE run_id = ?
```

This query hits `idx_run_agents_run ON run_agents(run_id)`, which is the correct index. The partial index `idx_run_agents_status` would only be useful for a query like `SELECT ... WHERE status = 'active' AND ...`. The plan does not include such a query.

This is harmless but wasteful. If the use case for the status index is to efficiently find all active agents across all runs (e.g., a future `ic run agent list --active`), the index is forward-looking. That should be documented. If it is not needed for any current query, remove it to keep the schema clean.

---

#### P2-4: `ic run agent update` Argument Order Inconsistency

**Severity:** P2 — CLI usability, slight correctness risk
**Files:** Proposed `cmd/ic/main.go`

The plan specifies:

```
ic run agent update <run_id> <agent_id> --status=<status>
```

But `run_id` is redundant here — `agent_id` is a globally unique 8-char ID. To update an agent, you only need `agent_id`. The `run_id` serves as a namespace sanity check. This is fine as a design choice, but:

1. The implementation must validate that `agent_id` actually belongs to `run_id`. If it does not, updating an agent from a different run using the wrong `run_id` should return an error, not silently succeed on the correct agent.
2. The validation requires an extra SELECT or a `WHERE id = ? AND run_id = ?` in the UPDATE.

The plan does not mention this cross-validation. If `UpdateAgent` in the store only takes `(ctx, id, status)` and the CLI passes `run_id` separately but does not use it in the WHERE clause, the `run_id` argument is theater — it provides no safety.

**Fix:** Either drop `run_id` from `ic run agent update` (make it `ic run agent update <agent_id> --status=<status>`) for simplicity, or ensure the store method takes `runID` in the WHERE clause: `UPDATE run_agents SET status = ?, updated_at = ? WHERE id = ? AND run_id = ?` — and `RowsAffected() == 0` means either not-found or run_id mismatch (return `ErrNotFound` for both).

---

### P3 — MINOR / CLEANUP

---

#### P3-1: `ic run current` Should Use `--project` Not Positional Arg

**Severity:** P3 — style/consistency
**Files:** Proposed `cmd/ic/main.go`

The plan says: "Accepts `--project=<dir>` flag (defaults to CWD)." This matches the existing `ic run create --project=<dir>` pattern. Good. Make sure the bash wrapper `intercore_run_current()` passes `--project` from `$1` when provided, using the same pattern as `intercore_dispatch_spawn` uses `--project=`. No positional project argument should exist for consistency with the rest of the CLI.

---

#### P3-2: `intercore_run_phase` Wrapper Shells Out to `ic run phase <id>` — No JSON, Just String

**Severity:** P3 — usability
**Files:** Proposed `lib-intercore.sh`

The plan adds `intercore_run_phase <run_id>` that "returns the current phase of a run." The existing `ic run phase <id>` just prints the phase string with no JSON option. This is fine for bash scripting. The wrapper can simply be:

```bash
intercore_run_phase() {
    local run_id="$1"
    if ! intercore_available; then return 1; fi
    "$INTERCORE_BIN" run phase "$run_id"
}
```

No issue here, just confirming the intent matches the CLI. Note that `intercore_available` calls `ic health` which opens and closes a DB connection. For callers that invoke both `intercore_run_current` and `intercore_run_phase` in sequence, this is two health checks and two DB opens. With SQLite single-connection and WAL, these are fast, but if performance matters in hot paths, cache the availability check (which `lib-intercore.sh` already does via `INTERCORE_BIN` caching — `intercore_available()` returns early once `INTERCORE_BIN` is set).

---

#### P3-3: `db_test.go` TestMigrate_Concurrent Is Sequential, Not Truly Concurrent

**Severity:** P3 — test gap documentation
**Files:** `internal/db/db_test.go`

The existing `TestMigrate_Concurrent` test is correctly documented in a comment:

> "With SetMaxOpenConns(1), each connection serializes, so concurrent open is the bottleneck (not the migration itself)."

The test runs 5 sequential `Open + Migrate + Close` calls. This exercises idempotency but not actual concurrency (two goroutines doing `Migrate` simultaneously). The MEMORY.md note confirms this is intentional:

> "Concurrent sql.Open from goroutines: first connection claims lock, others get SQLITE_BUSY before busy_timeout is set. Don't test concurrent migration from goroutines; test sequentially."

The new `TestMigrate_V3ToV4` test should follow the same sequential pattern. No action required, but the plan should explicitly reference this constraint for whoever writes the test.

---

#### P3-4: `copyFile` Does Not `Sync` the Backup File

**Severity:** P3 — durability on crash
**Files:** `internal/db/db.go` (`copyFile` function)

```go
func copyFile(src, dst string) error {
    ...
    if _, err := io.Copy(out, in); err != nil { ... }
    return out.Close()
}
```

`out.Close()` flushes the OS file buffer but does not call `fsync`. On a power loss between close and kernel write-back, the backup file may be incomplete or zero-length. Given that backups are created as a pre-migration safety net, an incomplete backup provides false assurance.

This is an existing issue, not introduced by this plan. Adding `out.Sync()` before `out.Close()` would fix it. Low-priority for a dev tool, high-priority for a production-grade sentinel store.

---

### P4 — INFORMATIONAL

---

#### P4-1: `run_artifacts.path` Has No Uniqueness Constraint Per Run

**Severity:** P4 — possible duplicate artifact records
**Files:** Proposed `internal/db/schema.sql`

The plan does not add a `UNIQUE(run_id, path)` constraint to `run_artifacts`. The same file can be registered as an artifact multiple times for the same run. Whether this is intentional (different phases may produce the same file) or accidental is unclear. If duplicate artifact paths for the same run and phase are not intended, add:

```sql
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_artifacts_unique_path
    ON run_artifacts(run_id, phase, path);
```

This would make `ic run artifact add` fail loudly on exact duplicates rather than silently accumulating them.

---

#### P4-2: Agent `dispatch_id` Has No Foreign Key to `dispatches.id`

**Severity:** P4 — referential integrity gap
**Files:** Proposed `internal/db/schema.sql`

```sql
dispatch_id TEXT,  -- no REFERENCES dispatches(id)
```

The plan notes `dispatch_id` as optional (`*string` in Go, `TEXT` nullable in SQL). If a `dispatch_id` is provided, it should reference a real dispatch. Without a FK (or with FK enforcement off — see P0-1), the link between `run_agents` and `dispatches` is advisory only. A deleted dispatch leaves a dangling `dispatch_id` in `run_agents`.

Since `dispatch_id` is nullable and its use is informational (link an agent record back to the dispatch that spawned it), this may be acceptable. But if FK enforcement is enabled (P0-1 fix), a `REFERENCES dispatches(id)` should be added or the column should be documented explicitly as "advisory, not enforced."

---

#### P4-3: No Cascade Delete Defined — Orphan Artifacts/Agents on Run Deletion

**Severity:** P4 — operational
**Files:** Proposed `internal/db/schema.sql`

The proposed tables:

```sql
run_id TEXT NOT NULL REFERENCES runs(id)
```

No `ON DELETE CASCADE`. If a run is ever deleted (currently not possible via CLI — there is no `ic run delete`), `run_agents` and `run_artifacts` rows would be left orphaned. FK enforcement (P0-1) would actually block such a delete unless cascade or explicit pre-delete is performed.

Since there is no `DELETE FROM runs` in the codebase today, this is P4. If a future `ic run purge` or DB cleanup command is added, cascade semantics must be defined at that point. Document this in AGENTS.md so the future implementer knows to add `ON DELETE CASCADE` or explicit child-table cleanup.

---

## Summary Table

| ID    | Severity | Area                       | Issue                                                           | Fix Required Before Ship |
|-------|----------|----------------------------|-----------------------------------------------------------------|--------------------------|
| P0-1  | P0       | FK enforcement             | `PRAGMA foreign_keys` never set — new FK constraints are inert | Yes                      |
| P0-2  | P0       | Migration correctness      | No test for v3→v4 upgrade path; schema/constant must be co-deployed | Yes (add test)           |
| P1-1  | P1       | Timestamp consistency      | `UpdateAgent` must update `updated_at` or column goes stale    | Yes                      |
| P1-2  | P1       | Test correctness           | `TestStore_AddAgent_BadRunID` passes vacuously without P0-1    | Blocked by P0-1          |
| P1-3  | P1       | TOCTOU / uniqueness        | No constraint on one active run per project; `current` may return wrong run | Decision required        |
| P1-4  | P1       | Backup integrity           | Pre-migration backup does not copy WAL file                    | Document (existing issue)|
| P2-1  | P2       | Error semantics            | `UpdateAgent` must use RowsAffected to detect ErrNotFound      | Yes                      |
| P2-2  | P2       | SQL null safety            | `ListArtifacts` nil phase filter must branch in Go, not use SQL NULL | Yes                      |
| P2-3  | P2       | Index utility              | Partial index on `status = 'active'` has no current query      | Optional                 |
| P2-4  | P2       | CLI safety                 | `ic run agent update` run_id arg is theater without WHERE guard | Yes                      |
| P3-1  | P3       | CLI consistency            | `--project` flag, not positional arg                           | Follow existing pattern  |
| P3-2  | P3       | Wrapper performance        | Health check per call — acceptable, already cached             | No action needed         |
| P3-3  | P3       | Test gap                   | Migration test must be sequential per MEMORY.md                | Document                 |
| P3-4  | P3       | Durability                 | `copyFile` does not fsync backup                               | No (existing issue)      |
| P4-1  | P4       | Data integrity             | No UNIQUE constraint on (run_id, phase, path) in artifacts     | Optional                 |
| P4-2  | P4       | Referential integrity      | `dispatch_id` not a FK to dispatches                           | Document intent          |
| P4-3  | P4       | Future-proofing            | No cascade delete for child tables                             | Document for future      |

---

## Recommended Action Order

1. Fix P0-1 first: add `PRAGMA foreign_keys = ON` to `Open()` in `db.go`. One line, immediately fixes P1-2 and validates FK tests.
2. Add P0-2 test (`TestMigrate_V3ToV4`) before any other Task 1 work. Run it first — it acts as the integration gate for the migration.
3. Add explicit `updated_at` update in `UpdateAgent` implementation (P1-1). This should be part of the Task 2 implementation spec, not left to the implementer to discover.
4. Decide on P1-3 (one active run per project, or multiple allowed). Add the partial unique index to v4 DDL, or document the multi-run-per-project behavior explicitly. This is a product question, not just a code question.
5. Fix P2-2 (ListArtifacts nil filter) and P2-4 (run_id cross-validation in UpdateAgent) at implementation time.
6. Everything P3 and P4: address in follow-up or document as known limitations in AGENTS.md.
