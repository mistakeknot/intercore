# Architecture Review: Intercore Rollback & Recovery Feature

**Date:** 2026-02-20
**Diff:** `/tmp/qg-diff-1771610172.txt`
**Files changed:** 18 files, 1126 lines added
**Schema version:** v7 to v8

---

## 1. Boundaries & Coupling

### Layer Map

The feature touches all four layers of the intercore stack in the correct direction:

```
cmd/ic/run.go               CLI handler (delivery)
  └─ internal/phase/        State machine (domain)
       ├─ machine.go        Rollback() orchestration
       ├─ store.go          RollbackPhase() SQL operation
       └─ phase.go          ChainPhaseIndex, ChainPhasesBetween helpers
  └─ internal/runtrack/     Agent/artifact tracking (domain)
       └─ store.go          MarkArtifactsRolledBack, FailAgentsByRun, ListArtifactsForCodeRollback
  └─ internal/dispatch/     Dispatch lifecycle (domain)
       └─ dispatch.go       CancelByRun
  └─ internal/db/           Persistence (infrastructure)
       └─ db.go + schema    Migration v7→v8, status column
```

Dependency direction is correct: CLI → domain stores → db. No domain layer reaching into the CLI.

### Cross-Package Orchestration in the CLI Handler

`cmdRunRollbackWorkflow` (in `cmd/ic/run.go`) constructs stores from three packages (`phase`, `runtrack`, `dispatch`) and coordinates them after `phase.Rollback()` returns. This matches the established pattern in `cmdRunAdvance`, which similarly wires `pStore`, `rtStore`, `dStore`, and `evStore`. The rollback workflow follows the same coordination style, so this is not a new coupling pattern.

### Atomicity Gap (Critical)

The rollback sequence in `cmdRunRollbackWorkflow` performs four separate SQL operations with no enclosing transaction:

1. `phase.Rollback()` → writes phase rewind + event (two operations inside `phase` package, also without an explicit outer transaction)
2. `rtStore.MarkArtifactsRolledBack()` → updates run_artifacts
3. `dStore.CancelByRun()` → updates dispatches
4. `rtStore.FailAgentsByRun()` → updates run_agents

If the process crashes or the database becomes unavailable between steps 1 and 2, the run is in "brainstorm" phase with artifacts still marked "active" and agents still marked "active" — a partially-rolled-back state with no self-healing mechanism. Steps 2–4 are treated as warnings in the CLI (`warning: artifact marking failed`) but the phase pointer has already moved. On restart, `ListArtifacts` for the new phase would still show old-phase artifacts as "active", and `CountArtifacts` (used by gate checks) would count them, potentially allowing gates to pass on stale data.

The existing `Advance()` operation avoids this problem by doing only a single atomic UpdatePhase+AddEvent within the phase package (SQLite's WAL mode guarantees that pair as effectively atomic). Rollback has three additional cross-table updates that were left unprotected.

The `Advance()` machinery does not face this because the supplemental cleanup (MarkArtifactsRolledBack, FailAgentsByRun) is new to rollback. No existing operation does multi-table cleanup outside a transaction.

### Duplicate Validation Between Machine and Store

`phase.Rollback()` in `machine.go` and `phase.RollbackPhase()` in `store.go` both:
- Fetch the run with `store.Get()`
- Check for cancelled/failed terminal status
- Call `ResolveChain()`
- Validate target-vs-current ordering

This means two database reads and two identical validation paths execute for every workflow rollback. `Advance()` does not do this — `machine.Advance()` performs its own validation and calls `store.UpdatePhase()` which does no pre-read validation. The extra `Get()` in `store.RollbackPhase()` is a design inconsistency that also creates a TOCTOU window: the status check in the store can see a different state than the status check in the machine if another operation runs concurrently. The machine guards first, but the store guard then re-reads. The second read is redundant but could introduce confusion if the two checks ever diverge.

The store's `RollbackPhase` also validates phase chain membership (ChainContains for both currentPhase and targetPhase), which `Rollback()` in machine.go does not explicitly duplicate. So the store adds net-new validation on top of what the machine already computed, rather than replacing it.

### No `CancelByRunAndPhases` — Full-Run Cancellation

The task description mentions `CancelByRunAndPhases` as a new dispatch method, but the diff shows only `CancelByRun` was added. The implementation cancels ALL non-terminal dispatches for a run, not just those scoped to the rolled-back phases. This is semantically overbroad: a run that was at "planned" and rolled back to "brainstorm" cancels every dispatch regardless of phase. In practice today, dispatches are phase-scoped through the sprint pipeline, so this is likely harmless. However, if a future run has overlapping dispatches across multiple concurrent phases, this would cancel dispatches belonging to phases that were not rolled back.

### `--layer=code` is a Query, Not a Recovery Command

`cmdRunRollbackCode` is named under the `rollback` subcommand but performs a read-only query of artifacts joined with dispatch metadata. It does not roll back anything. The naming is a semantic boundary problem: callers expecting `ic run rollback --layer=code` to perform a rollback will get a status report instead. The existing CLI naming convention in this codebase separates query operations (`ic run status`, `ic run events`, `ic run artifact list`) from mutations. This operation belongs closer to `ic run artifact list --with-dispatch` or `ic run status --code-rollback` than to the `rollback` subcommand.

---

## 2. Pattern Analysis

### Naming and Semantic Drift: `--layer=code`

The `rollback` subcommand is overloaded with two entirely different operations controlled by a `--layer` flag:
- `--to-phase` → performs a state mutation (the actual rollback)
- `--layer=code` → performs a read-only query (a report for operators to act on manually)

This is a hidden fork in the command surface. No other `ic run` subcommand multiplexes mutation and query through a flag. The routing logic at line 79-90 of the diff makes this branching explicit:

```go
// Route: --layer=code → code rollback query
if layer == "code" {
    return cmdRunRollbackCode(ctx, runID, filterPhase, format)
}
// Route: --to-phase → workflow rollback
```

This is scope creep within the command: the code query feature is useful, but it is not a rollback operation and does not belong in `cmdRunRollback`.

### Consistent Event Recording

The rollback event (`EventRollback`) is correctly recorded in the phase audit trail via `store.AddEvent()`, matching the pattern established by `EventAdvance`, `EventBlock`, `EventPause`, and `EventCancel`. The `PhaseEventCallback` is fired after the event is recorded, consistent with `Advance()`.

The dispatch cancellation event is recorded at line 184-186 of the diff:
```go
evStore.AddDispatchEvent(ctx, "", runID, "", dispatch.StatusCancelled, "rollback", reason)
```
The empty first argument is the `dispatch_id`. Looking at `dispatch_events` schema, this field has no FK constraint, so storing an empty string is syntactically valid but produces a row with no dispatch reference. `ic events tail` consumers would see a dispatch event with no dispatch ID, which may confuse reactors that expect this field. The existing `dispatch.UpdateStatus` path always sets a real dispatch_id here.

### Status Column: Soft-Deletion Pattern

Adding `status TEXT NOT NULL DEFAULT 'active'` to `run_artifacts` and marking rolled-back entries as `'rolled_back'` rather than deleting them is consistent with the project's audit-first philosophy. Artifacts are never deleted elsewhere. The `CountArtifacts` change to filter `status = 'active'` is the correct corresponding gate query update.

### `ChainPhasesBetween` Doc Comment Mismatch

The doc comment reads:
> "returns the phases strictly after from up to and including to"

But in the rollback context it is called as `ChainPhasesBetween(chain, targetPhase, fromPhase)` where `targetPhase < fromPhase` in chain order. The function thus returns phases after the target up to and including the current (from) phase. In the rollback context this correctly yields the phases being abandoned. The comment describes the general function correctly, but the doc should be clearer that `from` and `to` are positional (lower-to-higher index), not semantic (source-to-destination in the rollback direction). A future caller who reads the comment may pass arguments in the wrong order for a rollback use case and get nil back unexpectedly.

### Wrapper Version Bump

`INTERCORE_WRAPPER_VERSION` jumps from `0.6.0` to `1.1.0`. The AGENTS.md still references `lib-intercore.sh (v0.6.0)`. This is a stale reference that will cause confusion when a caller checks the wrapper version.

---

## 3. Simplicity & YAGNI

### `RollbackPhase` Store Method: Necessary vs Redundant

`store.RollbackPhase` adds validation (chain membership, ordering) that `machine.Rollback()` already computed. The store method does a pre-read `Get()` that machine already did. The minimum viable store operation would be:

```go
func (s *Store) RollbackPhase(ctx context.Context, id, targetPhase string) error {
    now := time.Now().Unix()
    result, err := s.db.ExecContext(ctx, `
        UPDATE runs SET phase = ?, status = 'active', updated_at = ?, completed_at = NULL
        WHERE id = ? AND status NOT IN ('cancelled', 'failed')`,
        targetPhase, now, id)
    // check n==0 → ErrNotFound or ErrTerminalRun
}
```

This would be smaller and avoid the duplicate read, at the cost of a less descriptive error message when the run is terminal. The current implementation's extra validation is defensive but creates the TOCTOU window described above.

### `cmdRunRollbackCode` Can Be Simpler

The format branching at line 238-260 of the diff implements a text formatter directly in the CLI handler. `ic run artifact list` does not have a `--format=text` equivalent; JSON is the standard output contract for `ic` commands. Adding a human-readable format here introduces a one-off output path not shared with any other command. The `--format=text` option is premature: no consumer of this output has been identified in the codebase or tests. The integration tests pass for the text format but never actually parse or act on it.

### Dry-Run Output Includes `rolled_back_phases` But Not Artifact/Agent Counts

The dry-run output reports the phases that would be rolled back but not the count of artifacts or agents that would be affected. This is incomplete for an operator trying to assess impact before running the actual rollback. The live output provides all four counts (`cancelled_dispatches`, `marked_artifacts`, `failed_agents`, `rolled_back_phases`). If dry-run is meant to be a preview, it should preview the same fields, which would require read-only count queries during dry-run. Currently, dry-run exits before touching the stores (`pStore` only, no `rtStore`/`dStore`), so this is a functional gap that makes the dry-run less useful for pre-flight assessment.

---

## 4. Summary of Findings

### Critical

**F01 — No transaction wrapping the rollback compound operation.** `cmdRunRollbackWorkflow` calls `phase.Rollback()`, `rtStore.MarkArtifactsRolledBack()`, `dStore.CancelByRun()`, and `rtStore.FailAgentsByRun()` as four independent operations. A crash after the first leaves a partially-rolled-back database with stale artifact/agent statuses. The gate check (`CountArtifacts` now filters `status='active'`) will see incorrect data on restart.

### Significant

**F02 — `--layer=code` multiplexes a read-only query into a mutation subcommand.** This violates the CLI naming convention and creates a semantic boundary problem. The code rollback query is not a rollback operation.

**F03 — Duplicate validation and double `Get()` between `machine.Rollback()` and `store.RollbackPhase()`.** Both functions fetch the run, check terminal status, resolve the chain, and validate phase ordering. This creates a TOCTOU window and adds an unnecessary database round-trip per rollback.

**F04 — `CancelByRun` cancels all run dispatches, not just those in rolled-back phases.** This is semantically overbroad and will cause incorrect behavior when a run has dispatches spanning multiple phases.

### Informational

**F05 — Dispatch cancellation event recorded with empty `dispatch_id`.** The `evStore.AddDispatchEvent` call passes `""` as the dispatch_id, producing an audit row with no dispatch reference. Event bus consumers may mishandle this.

**F06 — AGENTS.md still references `lib-intercore.sh (v0.6.0)` after bump to `1.1.0`.** Stale documentation.

**F07 — `ChainPhasesBetween` doc comment is orientation-ambiguous for rollback callers.** The comment describes general usage but the rollback call passes arguments in reverse-semantic order, which could mislead future callers.

**F08 — `--format=text` in `cmdRunRollbackCode` is premature.** No existing consumer uses it; JSON is the project standard. It introduces a one-off output path with no test coverage that validates its content.

**F09 — Dry-run does not preview artifact/agent impact counts.** The dry-run output is less useful than the live output, which undermines its purpose.

---

## 5. Recommended Remediation

### Must-Fix

1. **Wrap the rollback compound operation in a transaction.** Open a single `*sql.Tx` in `cmdRunRollbackWorkflow` before calling `phase.Rollback()` and commit after `FailAgentsByRun`. Thread the tx through all four store calls, or add a `RollbackAll(ctx, tx, runID, ...)` function in a higher-level orchestration layer. The dispatch event notification can remain outside the transaction (fire-and-forget, as is the pattern for event bus callbacks).

2. **Split `--layer=code` into a distinct subcommand or query path.** Minimal change: rename to `ic run artifact list --with-dispatch` or `ic run code-changes <id>`. The routing fork in `cmdRunRollback` is the smell; moving it preserves the behavior with correct naming.

### Should-Fix

3. **Eliminate the double `Get()` in `store.RollbackPhase`.** The store method should trust the validated inputs from `machine.Rollback()` and perform only the SQL UPDATE with a conditional WHERE on status. If the terminal-run check is desired in the store for direct callers, use a WHERE clause rather than a pre-read.

4. **Scope `CancelByRun` to the rolled-back phases.** Rename to `CancelByRunAndPhases(ctx, runID string, phases []string)` and add a phase filter to the UPDATE query, matching how `MarkArtifactsRolledBack` scopes its operation.

5. **Record a valid dispatch_id or skip the dispatch event when no specific dispatch was cancelled.** Either pass a synthesized or aggregate ID, or emit a different event type (e.g., a phase event with cancellation detail) rather than a malformed dispatch event.

### Optional

6. Update AGENTS.md to reference `lib-intercore.sh (v1.1.0)`.
7. Clarify `ChainPhasesBetween` doc comment to note that `from < to` in chain order (lower index to higher index).
8. Remove `--format=text` from `cmdRunRollbackCode` until there is a concrete consumer.
9. Add read-only count queries to the dry-run path so it previews artifact and agent impact.
