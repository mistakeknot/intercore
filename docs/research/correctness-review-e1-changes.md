# Correctness Review — Intercore E1 Kernel Primitives

Reviewer: Julik (Flux-drive Correctness Reviewer)
Date: 2026-02-19
Diff: `/tmp/qg-diff-1771519861.txt`
Full findings: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md`

---

## Key Findings

### C1 (MEDIUM) — Migration early-return inside live transaction is never committed

`db.Migrate()` opens a transaction, creates `_migrate_lock`, reads the schema version, and then at line 132 early-returns with `return nil` when `currentVersion >= currentSchemaVersion`. The `defer tx.Rollback()` fires and silently rolls back the `_migrate_lock` DDL. No data is corrupted (the rollback is a no-op from a data standpoint), but the code path is misleading and the lock table creation is not idempotent in intent. Fix: call `tx.Rollback()` explicitly before `return nil`, or restructure the condition so the fast path exits before the transaction begins.

### C2 (MEDIUM) — `budget.Check()` silently maps transient DB errors to "no budget"

```go
run, err := c.phaseStore.Get(ctx, runID)
if err != nil {
    return nil, nil // run not found or error — no budget to check
}
```

Any error — including `SQLITE_BUSY` after `busy_timeout` expiry, context cancellation, or a corrupt row — produces `nil, nil`, which all callers interpret as "no budget set, all good." The dedup flag never gets set on error, so the next successful check will re-emit the warning event, but the immediate check falsely signals safety. Fix: distinguish `phase.ErrNotFound` (truly no budget) from other errors (propagate as error).

### C3 (LOW) — Skip-walk silently halts at a skipped phase on `ChainNextPhase` error

If `ChainNextPhase` returns an error mid-walk (only possible if `toPhase` is not in the chain — a data corruption case), the loop `break`s and `Advance()` proceeds to update the run to the skipped phase with `EventAdvance` type. The audit trail records a misleading advance into a phase that was pre-marked for skipping.

### C4 (LOW) — `cmdDispatchTokens` uses `dispatch.ScopeID` as run ID for budget check

Works by convention when `ScopeID` is always the run ID, but the column has no FK and accepts any string. A dispatch with a non-run `scope_id` silently skips the budget check.

### C5 (LOW) — Cache ratio denominator conflates two token domains

`cacheRatio = TotalCache / (TotalIn + TotalCache) * 100` — the denominator mixes actual sent tokens with cache-served tokens. The value can exceed 100% if any dispatch stores `cache_hits > input_tokens`. Cosmetic but confusing.

---

## Verdict

**needs-changes** — C1 and C2 should be fixed before shipping; C3–C5 are low-risk but worth addressing in a follow-up pass.
