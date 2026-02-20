# Correctness Review: Intercore Rollback and Recovery Feature

**Diff:** `/tmp/qg-diff-1771610172.txt`
**Reviewer role:** Julik, Flux-drive Correctness Reviewer
**Date:** 2026-02-20

---

## Invariants That Must Hold

These are the correctness properties I am testing against:

1. **Phase integrity:** A run's `phase` column is always a member of its declared chain. No phase values outside the chain can be written.
2. **Status integrity:** A run with `status='cancelled'` or `status='failed'` cannot be mutated. Only `status='completed'` runs may be rolled back (reverting to `active`).
3. **Atomicity of rollback:** Either the phase pointer moves AND the event is recorded, or neither. A phase rewind with no audit trail is a ghost transition.
4. **Artifact status consistency:** After a rollback to phase P, all artifacts belonging to phases strictly after P (the rolled-back range) must be marked `status='rolled_back'`. Artifacts in phase P and earlier remain `status='active'`.
5. **Dispatch consistency:** All non-terminal dispatches that belong to the rolled-back run must be cancelled before any new dispatches can be spawned.
6. **Agent consistency:** All `status='active'` agents must be marked `failed` before the run re-enters the rolled-back phase.
7. **Schema idempotency:** Running `ic init` twice on a v7 database must be safe and must not corrupt data. The v7→v8 migration must be re-entrant.
8. **Phase order for rollback:** The target phase must be strictly behind (lower index than) the current phase. Rolling "forward" via the rollback path is prohibited.
9. **No double-Get race:** The business logic sees a consistent snapshot of the run at validation time and at write time. If the run's phase changes between validation and write, the write must not silently succeed with stale data.
10. **Partial failure recoverability:** If the CLI handler fails after `phase.Rollback()` but before `MarkArtifactsRolledBack`, the database must not be left in a permanently inconsistent state or must provide clear guidance on recovery.

---

## Analysis by Area

### 1. Schema Migration v7→v8 (`internal/db/db.go`)

**Claim in code:** The migration is labeled "v7 → v8" and is guarded by `currentVersion >= 4 && currentVersion < 8`.

**Finding — Mislabeled version range (low severity, no data risk):**

The migration block is labeled "v7 → v8" in its comment, but its guard condition is `currentVersion >= 4 && currentVersion < 8`. This means it fires for ANY database at version 4, 5, 6, or 7 — not just v7. For a fresh v4 database that never had v5 or v6 applied, the v8 migration block fires first and `ALTER TABLE run_artifacts ADD COLUMN status TEXT NOT NULL DEFAULT 'active'` will execute. The schema DDL is then applied which has the column already, giving a "duplicate column" error. The `isDuplicateColumnError` guard catches this so there is no crash, but it is doing more work than its label implies. This is a documentation/maintenance concern rather than a data correctness bug: the actual behavior is safe because the duplicate-column guard is present.

**Finding — Missing v6→v7 migration block:**

The migration in `db.go` has a block for v5→v6 (line 137) and now v7→v8 (line 157), but there is no explicit v6→v7 block. Looking at `schema.sql`, the `interspect_events` table is labeled `-- v7: interspect evidence events`. There is no migration block that creates `interspect_events` for databases upgrading from v5 or v6. The `CREATE TABLE IF NOT EXISTS` in the final DDL application step covers this, making it safe for a fresh install. However, a database at v5 or v6 would get `interspect_events` created silently by the DDL step without a corresponding comment/block in the migration path. This is a documentation gap that could mislead future maintainers when they add v9 and need to understand what was created at each version step.

**Finding — isDuplicateColumnError is string-matching, not code-matching:**

`isDuplicateColumnError` checks `strings.Contains(err.Error(), "duplicate column name")`. This is a string match against a driver-specific error string from `modernc.org/sqlite`. If the driver is upgraded and the error message changes (e.g., to "column already exists"), the guard silently stops working: subsequent `ic init` calls on already-migrated databases would return an error and be forced to restore from backup. This is a low-probability but high-pain failure.

**Idempotency verdict:** The v7→v8 migration is idempotent for the common cases. A partial failure (process killed after ALTER but before `PRAGMA user_version` is updated) leaves the DB at version 7 with the column already added. The next `ic init` re-enters the migration at `currentVersion=7`, fires the v8 block again, hits the duplicate-column guard, and correctly continues. This is safe.

**Backup-before-migrate behavior:** The backup is taken BEFORE the transaction starts. If migration fails, the user has a clean backup. If migration succeeds but the backup copy failed (disk full), the migration is aborted. This is the correct ordering.

---

### 2. RollbackPhase — Direct UPDATE Without Optimistic Concurrency (`internal/phase/store.go`)

**Code path:**

```go
func (s *Store) RollbackPhase(ctx context.Context, id, currentPhase, targetPhase string) error {
    run, err := s.Get(ctx, id)               // Read #1 — separate query
    // ... validation ...
    result, err := s.db.ExecContext(ctx, `
        UPDATE runs SET phase = ?, status = 'active', updated_at = ?, completed_at = NULL
        WHERE id = ?`,                        // Write — no WHERE phase = ? guard
        targetPhase, now, id,
    )
```

**Race analysis:**

The `RollbackPhase` method reads the run (`Get`), validates it, then writes without using the read phase as a predicate. Compare with `UpdatePhase` which uses `WHERE id = ? AND phase = ?` to enforce optimistic concurrency.

For rollback, the SQLite `SetMaxOpenConns(1)` means there is only one physical connection, so two concurrent Go goroutines cannot execute SQL simultaneously. The single-connection serialization means the Read and the Write are not interleaved at the SQL level within the same process. However:

- **Multi-process scenario:** Two separate `ic run rollback` invocations on the same run ID could run concurrently. Because SQLite WAL mode with `busy_timeout` is used, one writer will block until the other commits. If process A reads at phase=`planned`, then process B also reads at phase=`planned`, B commits its rollback to `brainstorm`, then A's UPDATE fires and also writes `brainstorm` — the second rollback silently "succeeds" but was stale. The result is two rollback events in the audit trail for the same effective transition, and artifact/dispatch operations being run twice. This is the double-rollback problem.

- **Concrete interleaving:**
  1. Process A: `RollbackPhase.Get()` → sees phase=`planned`, validates OK
  2. Process B: `RollbackPhase.Get()` → sees phase=`planned`, validates OK
  3. Process B: `UPDATE runs SET phase='brainstorm' WHERE id=?` → succeeds
  4. Process A: `UPDATE runs SET phase='brainstorm' WHERE id=?` → also succeeds (no phase guard)
  5. Both processes call `MarkArtifactsRolledBack` → both succeed (second call marks 0 rows since first already marked them, but that's benign)
  6. Both processes call `CancelByRun` → second call cancels 0 dispatches (idempotent)
  7. Both processes call `FailAgentsByRun` → second call fails 0 agents (idempotent)
  8. Both processes record a `rollback` event → two rollback events in audit trail for what was one logical operation

**Assessment:** The artifact/dispatch/agent side effects are idempotent, so double-rollback does not corrupt data. The audit trail gets a spurious duplicate event, which is misleading but not a data integrity failure. The intent to make rollback "authoritative" is stated in the comment ("rollback is an authoritative operation"). The real question is whether an operator triggering rollback twice concurrently is a realistic scenario. For a CLI tool invoked manually, this is low-probability. However, if shell hooks or automation systems ever call `intercore_run_rollback`, the risk increases.

**Alternative that would be strictly safer:** `WHERE id = ? AND phase = currentPhase` in the UPDATE would cause the second concurrent rollback to hit 0 rows and return `ErrStalePhase` (or, better, a new `ErrAlreadyRolledBack`). The first rollback's result would stand.

**Verdict:** Low risk in the current use context (CLI, human operator). Documented intentional choice. But the comment justifying "authoritative" without addressing the double-fire scenario is incomplete.

---

### 3. Rollback Machine: Multi-Step Operations Without Transaction (`internal/phase/machine.go`)

**Code path in `Rollback()`:**

```go
// Step 1: Get run state
run, err := store.Get(ctx, runID)

// Step 2: Validate
// ...

// Step 3: Write phase pointer
if err := store.RollbackPhase(ctx, runID, fromPhase, targetPhase); err != nil { ... }

// Step 4: Write audit event
if err := store.AddEvent(ctx, &PhaseEvent{ ... }); err != nil { ... }

// Step 5: Fire callback (no error propagation)
if callback != nil { callback(...) }
```

**Transaction gap analysis:**

Steps 3 and 4 are two separate SQL writes. If step 3 succeeds and step 4 fails (e.g., disk full, connection error), the run's phase is rewound but no rollback event is written to `phase_events`. The audit trail shows the phase jumped from `planned` to `brainstorm` without explanation.

**Severity:** Low-medium. The data is technically consistent (the phase pointer is correct), but the audit trail is silent about what happened. A forensic review of the events would show a brainstorm phase with no prior rollback event, and the next `ic run events` would not show a rollback. If the operator re-runs rollback to recover, a duplicate event is created for what looks like the same transition. This is messy but not catastrophic.

**Compare with Advance():** `Advance` has the same pattern (UpdatePhase, then AddEvent, then UpdateStatus as separate statements). The existing behavior is documented as intentional (the AGENTS.md says "Callbacks fire after DB commit (fire-and-forget)"). So rollback is consistent with the established design.

**Partial failure in AddEvent:** If `AddEvent` returns an error, `Rollback()` returns an error to the CLI handler. The CLI handler then does NOT proceed to `MarkArtifactsRolledBack`, `CancelByRun`, or `FailAgentsByRun`. Result: phase is rewound, audit trail is empty, artifacts still show `active`, dispatches still running, agents still active. The system is in a partially rolled-back state.

**Recovery path:** A subsequent `ic run rollback` call will see phase=`brainstorm` and reject the rollback because the target would not be behind the current phase (same as current or already at target). The operator cannot easily "finish" the rollback cleanup through the same command. They would need to manually invoke artifact marking and dispatch cancellation, which has no exposed CLI interface.

**This is the most consequential issue in the diff.** If the event write fails, artifact and dispatch cleanup is silently skipped with no clear recovery path exposed to the operator.

---

### 4. CLI Handler: Partial Failure Handling (`cmd/ic/run.go`, `cmdRunRollbackWorkflow`)

**The four post-rollback operations:**

```go
// Operation 1 — phase rollback (atomic within machine.Rollback)
result, err := phase.Rollback(ctx, pStore, runID, toPhase, reason, callback)
// ← if this fails, we return immediately. Correct.

// Operation 2 — mark artifacts
markedArtifacts, err := rtStore.MarkArtifactsRolledBack(ctx, runID, result.RolledBackPhases)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run rollback: warning: artifact marking failed: %v\n", err)
    // ← CONTINUES despite failure
}

// Operation 3 — cancel dispatches
cancelledDispatches, err := dStore.CancelByRun(ctx, runID)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run rollback: warning: dispatch cancellation failed: %v\n", err)
    // ← CONTINUES despite failure
}

// Operation 4 — fail agents
failedAgents, err := rtStore.FailAgentsByRun(ctx, runID)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run rollback: warning: agent failure marking failed: %v\n", err)
    // ← CONTINUES despite failure
}
```

**Failure mode 1 — Artifact marking fails (operation 2 fails):**

Phase is rewound. Artifacts remain `status='active'` for phases that no longer exist in the run's future. The gate check `CountArtifacts` filters on `status='active'`, so rolled-back artifacts that were not marked will incorrectly be counted as evidence for gate passage on the next `ic run advance`. If the artifact_exists gate is configured hard, the run can advance from the rolled-back phase because old (now-invalid) artifacts satisfy the gate.

**This is a data integrity violation.** Rolled-back artifact evidence should not gate-pass future advances. The current CLI handler makes this silent — only a warning is emitted, return code is 0, and the JSON output shows `marked_artifacts: 0`.

**Failure mode 2 — Dispatch cancellation fails (operation 3 fails):**

Phase is rewound, artifacts are marked. Non-terminal dispatches remain active. If these dispatches complete successfully, their results may be recorded against the run's scope, and the run may incorrectly be attributed token spend and verdicts from work done in the rolled-back phase. Depending on how downstream consumers use `ic dispatch wait`, they may see a "completed" dispatch for a phase that was explicitly rolled back.

**Failure mode 3 — Agent failure marking fails (operation 4 fails):**

Agents remain `status='active'`. The gate check `CountActiveAgents` used by `agents_complete` will count them, blocking future advance even though these agents' work was for rolled-back phases.

**Severity rating:** Medium-high. These are "warning" paths that silently leave the database in an inconsistent state. The exit code remains 0, so automated scripts calling `intercore_run_rollback` will believe the rollback was clean.

**What would fix this:**

Option A — Wrap all four operations in a single SQL transaction. This is the correct fix but requires the phase store, runtrack store, and dispatch store to accept a `*sql.Tx` parameter. That's a non-trivial refactor.

Option B — Return exit code 1 (expected negative result) rather than 0 when any cleanup operation fails. At minimum, the operator script knows the rollback was partial and can re-invoke with a hypothetical `--cleanup-only` flag.

Option C — Make the cleanup operations part of the machine.Rollback logic rather than the CLI handler, so the semantics are enforced at the library level regardless of the caller.

**Currently implemented:** Warning-only, exit 0. Automation cannot detect partial rollback failure.

---

### 5. Dry-Run Phase Validation Duplication

**In `cmdRunRollbackWorkflow`:**

```go
// Before phase.Rollback():
chain := phase.ResolveChain(run)
rolledBackPhases := phase.ChainPhasesBetween(chain, toPhase, run.Phase)
if rolledBackPhases == nil {
    fmt.Fprintf(os.Stderr, "...")
    return 1
}
```

Then inside `phase.Rollback()`:

```go
// Inside machine.Rollback():
rolledBack := ChainPhasesBetween(chain, targetPhase, fromPhase)
if rolledBack == nil {
    return nil, ErrInvalidRollback
}
```

And inside `store.RollbackPhase()`:

```go
// Inside store.RollbackPhase():
if targetIdx >= currentIdx {
    return ErrInvalidRollback
}
```

The CLI does its own pre-flight validation before calling `phase.Rollback()`, which re-validates. `phase.Rollback()` calls `store.RollbackPhase()` which re-validates a third time. This triple-validation is not harmful but means that changes to the phase chain between the CLI pre-flight read and the actual write could give confusing error messages (ErrInvalidRollback from the store, not the friendly CLI message). In practice this is extremely unlikely given SetMaxOpenConns(1) and the short time window.

---

### 6. CancelByRun Scope Assumption

**In `dispatch.go`:**

```go
func (s *Store) CancelByRun(ctx context.Context, runID string) (int64, error) {
    result, err := s.db.ExecContext(ctx, `
        UPDATE dispatches SET status = ?, completed_at = ?
        WHERE scope_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'timeout')`,
        StatusCancelled, now, runID,
    )
```

This uses `scope_id = runID` to find dispatches. The AGENTS.md confirms "Dispatches are scoped to runs via scope_id = run_id." However, this coupling is implicit. If a dispatch was created with a different scope_id (e.g., a fanout dispatch parented to a sub-scope), it would not be cancelled by this operation.

Looking at the schema: `dispatches.scope_id TEXT` has no FK and no documented constraint that `scope_id` must equal a `runs.id` value. The dispatch spawn command allows arbitrary scope_id via `--scope-id`. A dispatch spawned with `--scope-id=custom-value` and associated with the run only through `run_agents.dispatch_id` would not be cancelled by `CancelByRun`. This is a semantic gap but not a new regression introduced by this diff — the behavior was pre-existing.

**Implication for rollback:** If dispatches were spawned with custom scope IDs, rollback will not cancel them, and they may continue running against a rolled-back run. The text format output `cancelled_dispatches: 0` will not alert the operator.

---

### 7. MarkArtifactsRolledBack: Phases Derived from CLI vs. Machine

**In `cmdRunRollbackWorkflow`:**

```go
result, err := phase.Rollback(ctx, pStore, runID, toPhase, reason, callback)
// ...
markedArtifacts, err := rtStore.MarkArtifactsRolledBack(ctx, runID, result.RolledBackPhases)
```

`result.RolledBackPhases` comes from `ChainPhasesBetween(chain, targetPhase, fromPhase)` where `chain` and `fromPhase` are the state at the time `phase.Rollback()` was called. Between the `Get` inside `Rollback()` and the `MarkArtifactsRolledBack` call, the run's state could have changed (in theory). In practice, with the single-connection SQLite, this cannot happen within the same process. But if another process advanced the run between rollback commits (extremely unlikely but theoretically possible), the phase list used to mark artifacts could be wrong.

More realistically: the `RolledBackPhases` list correctly represents what phases were behind the new current phase. The artifact marking is correct for those phases. No bug here, but noting the dependency on stale in-memory data.

---

### 8. AddArtifact Does Not Insert `status` Column

**In `runtrack/store.go`, `AddArtifact`:**

```go
_, err = s.db.ExecContext(ctx, `
    INSERT INTO run_artifacts (
        id, run_id, phase, path, type, content_hash, dispatch_id, created_at
    ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
```

The `status` column is NOT in the INSERT. This relies on the schema default `status TEXT NOT NULL DEFAULT 'active'` to set the value. After the v7→v8 migration, new inserts will receive `status='active'` automatically via the column default. Pre-migration rows (written before the migration) also get `status='active'` because `ALTER TABLE ... ADD COLUMN status TEXT NOT NULL DEFAULT 'active'` backfills the default to all existing rows in SQLite.

**Correctness verdict:** Safe. SQLite applies the default to existing rows when using `NOT NULL DEFAULT`, and new inserts get the default automatically. No explicit value is needed in the INSERT.

**Consistency note:** The `ListArtifacts` query now reads 9 columns (`id, run_id, phase, path, type, content_hash, dispatch_id, status, created_at`), but `AddArtifact` writes 8 explicit columns relying on the default for `status`. The read/write column count mismatch is fine because the INSERT omits `status` intentionally. However, future refactors that add another column should check both the INSERT and SELECT sides.

---

### 9. ListArtifactsForCodeRollback — No Status Filter

**The query:**

```sql
SELECT a.dispatch_id, d.name, a.phase, a.path, a.content_hash, a.type
FROM run_artifacts a
LEFT JOIN dispatches d ON a.dispatch_id = d.id
WHERE a.run_id = ?
ORDER BY a.phase, a.created_at ASC
```

This returns ALL artifacts for the run, including those with `status='rolled_back'`. The code rollback query is intended to show what files were changed during the run for potential code reversal. Returning rolled-back artifacts is arguably correct — an operator doing a code rollback wants to know what was produced in each phase, including what was subsequently rolled back. However, there is no status column in the `CodeRollbackEntry` struct or the returned JSON, so the consumer cannot tell which artifacts are still active vs. rolled-back.

**Consequence:** If an operator uses this query to identify files to revert after a workflow rollback, they may revert files that were already "un-active" (from a prior rollback), leading to unnecessary or incorrect file reversions.

**Improvement:** Either include `status` in the `CodeRollbackEntry` struct and the SELECT, or add a `status` filter parameter to allow callers to query only active artifacts.

---

### 10. Event Recording After Dispatch Cancellation

**In `cmdRunRollbackWorkflow`:**

```go
if cancelledDispatches > 0 {
    evStore.AddDispatchEvent(ctx, "", runID, "", dispatch.StatusCancelled, "rollback", reason)
}
```

The `AddDispatchEvent` return value (error) is discarded. If the event write fails, the cancellation happened but the event bus has no record of it. This is consistent with the existing fire-and-forget event bus design, but it means the rollback event in `dispatch_events` may be missing if the DB is under write pressure.

More specifically, the `dispatch_id` argument is `""` (empty string). Looking at the event store's `AddDispatchEvent`, an empty dispatch ID is recorded literally. This means the dispatch_events entry has `dispatch_id=""` rather than a real dispatch ID. Event consumers filtering by dispatch_id would not find this event, and the empty string may not be meaningful to downstream processors.

---

## Summary of Findings

### Critical / High

None found. No data corruption path exists under normal single-process operation.

### Medium

1. **Partial rollback leaves inconsistent state:** If `MarkArtifactsRolledBack`, `CancelByRun`, or `FailAgentsByRun` fail after `phase.Rollback()` succeeds, the system silently enters a partially rolled-back state. Old artifact evidence can satisfy gates incorrectly. Active agents block gate advance for rolled-back work. Exit code is 0, masking the failure from automation.

2. **Missing status in CodeRollbackEntry:** The code rollback query returns all artifacts without a `status` field. Operators cannot distinguish active from rolled-back artifacts in the JSON output, risking unnecessary file reversions.

### Low

3. **Double-rollback creates duplicate audit events:** Two concurrent `ic run rollback` invocations succeed because the UPDATE has no phase guard. The run ends up at the correct phase, but two rollback events are recorded. Automation cannot distinguish this from two legitimate sequential rollbacks.

4. **AddEvent failure leaves phase rewound with no audit trail:** If `store.AddEvent` fails after `store.RollbackPhase` succeeds in `machine.Rollback()`, the audit trail is silent about the rollback. The returned error causes the CLI to skip all cleanup, leaving artifacts and dispatches in pre-rollback state. The recovery path is not exposed.

5. **Empty dispatch_id in dispatch event:** `evStore.AddDispatchEvent` is called with `dispatch_id=""` for the bulk cancellation event. This is a semantic mismatch — the event represents many dispatches, not one with an empty ID.

### Informational

6. **Migration comment "v7→v8" fires for v4+ databases:** The guard `currentVersion >= 4 && currentVersion < 8` covers four version ranges, not just v7. The label is misleading.

7. **isDuplicateColumnError is driver-string-dependent:** The duplicate column guard relies on a driver-specific error message. An upstream driver update could break idempotency.

8. **Triple validation of rollback direction:** CLI, machine, and store each independently validate that target phase is behind current. Benign but adds maintenance surface.

9. **CancelByRun only cancels dispatches with scope_id=runID:** Dispatches with custom scope IDs remain running.

---

## Concrete Fix Recommendations

**For issue 1 (partial rollback, highest priority):**

Change the cleanup error handling from warning+continue to warning+return-nonzero:

```go
markedArtifacts, err := rtStore.MarkArtifactsRolledBack(ctx, runID, result.RolledBackPhases)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run rollback: artifact marking failed: %v\n", err)
    fmt.Fprintf(os.Stderr, "ic: run rollback: phase was rewound to %q — re-run rollback to retry cleanup\n", toPhase)
    return 2  // was: continue
}
```

Or, ideally, wrap the four operations in a single transaction.

**For issue 2 (CodeRollbackEntry missing status):**

Add `Status string` to `CodeRollbackEntry`, include `a.status` in the SELECT, and let the consumer filter or display accordingly.

**For issue 3 (double-rollback):**

Add `AND phase = ?` to the UPDATE in `RollbackPhase`, passing `currentPhase` as the predicate, consistent with `UpdatePhase`. Return a new sentinel error if 0 rows affected and the run exists at the target phase already.

**For issue 5 (empty dispatch_id):**

Pass `"rollback-bulk"` or generate a synthetic ID for the multi-dispatch cancellation event, or use a dedicated event type. Alternatively, suppress the event and rely on the individual dispatch status changes.
