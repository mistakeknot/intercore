# Quality Gate Synthesis: Rollback and Recovery Feature

**Date:** 2026-02-20
**Context:** 18 files changed across Go, SQL, and Shell. Risk domains: database migration (schema v8), state machine (phase rollback), SQL queries (artifact marking, dispatch cancellation), CLI argument handling.
**Agents:** fd-architecture, fd-correctness, fd-quality, fd-safety
**Output dir:** `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/`

---

## Validation

All 4 agent output files found and validated:
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-architecture.md` — Valid (Findings Index present, `Verdict: needs-changes`)
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md` — Valid (Findings Index present, `Verdict: needs-changes`)
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md` — Valid (Findings Index present, `Verdict: safe`)
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md` — Valid (Findings Index present, `Verdict: safe`)

**Validation: 4/4 agents valid, 0 failed**

Two agents returned `needs-changes` (fd-architecture, fd-correctness). Two returned `safe` (fd-quality, fd-safety).

---

## Verdict Summary

| Agent | Status | Summary |
|---|---|---|
| fd-architecture | NEEDS_ATTENTION | Missing transaction for compound rollback; overbroad dispatch cancellation; boundary violations |
| fd-correctness | NEEDS_ATTENTION | Partial failure exits 0; CodeRollbackEntry missing status field; no phase CAS guard |
| fd-quality | CLEAN | Architecturally sound; minor style debt and doc drift |
| fd-safety | CLEAN | No exploitable vulnerabilities; two operational gaps |

**Overall verdict: needs-changes**
**Gate: FAIL**

Three P1 findings converge across agents. No P0 critical findings. fd-quality rated overall "safe" but independently confirmed the transaction problem at LOW — severity conflict resolved in favor of fd-architecture which traces the concrete gate-pass failure mode.

---

## Findings Index

### P1 — Must fix (blocks merge)

**[P1-A] No transaction wrapping the rollback compound operation**
- Convergence: 4/4 agents (fd-architecture F01 CRITICAL, fd-correctness C-04 LOW, fd-quality Q3 LOW, fd-safety S1 LOW)
- File: `cmd/ic/run.go` lines 155-181 (`cmdRunRollbackWorkflow`)
- Root cause: `phase.Rollback()`, `MarkArtifactsRolledBack()`, `CancelByRun()`, and `FailAgentsByRun()` execute as four independent SQL writes with no enclosing transaction. A crash after step 1 leaves phase rewound but artifacts still `status='active'`. Gate checks on next advance see stale active artifacts from rolled-back phases and may pass on invalid state.
- Secondary issue (fd-correctness C-01): Cleanup errors are warning-only and return exit 0. Automation treating exit 0 as "clean rollback" cannot detect partial failure.
- Fix: Wrap all four operations in a `*sql.Tx`. Keep event-bus callbacks outside the transaction (matches existing Advance pattern). On any cleanup failure, return exit code 2 and emit `"warnings": [...]` in JSON output.

**[P1-B] `CancelByRun` cancels dispatches beyond rolled-back phases**
- Convergence: 3/4 agents (fd-architecture F04 SIGNIFICANT, fd-correctness C-09 INFO, fd-quality Q6 INFO)
- File: `internal/dispatch/dispatch.go` `CancelByRun` lines 491-506
- Root cause: `CancelByRun` filters only on `scope_id = runID` with no phase constraint. `MarkArtifactsRolledBack` correctly accepts a `phases []string` argument and scopes to only rolled-back phases. If a run has dispatches in phases that were NOT rolled back, those dispatches are incorrectly cancelled by the broad filter.
- Note: fd-correctness partially dismisses this as a "pre-existing design constraint" for custom scope_id cases. fd-architecture correctly identifies it as a regression introduced by the asymmetry with `MarkArtifactsRolledBack`. Adopted the fd-architecture framing.
- Fix: Rename to `CancelByRunAndPhases(ctx, runID string, phases []string)` and add `AND phase IN (...)` clause matching the precision of `MarkArtifactsRolledBack`.

**[P1-C] Partial cleanup failure exits 0 — automation cannot detect degraded rollback**
- Convergence: 3/4 agents (fd-correctness C-01 MEDIUM, fd-quality Q3 LOW, fd-safety S1 LOW)
- File: `cmd/ic/run.go` lines 166-180
- Root cause: `MarkArtifactsRolledBack`, `CancelByRun`, `FailAgentsByRun` are each wrapped in `warning: ... failed` handlers that continue and return exit 0. A caller relying on exit code to determine rollback success has no signal that cleanup was incomplete.
- Note: Partially subsumed by P1-A if the transaction approach is adopted. Kept separate because the exit code behavior requires its own CLI change regardless of the transaction wrapping.
- Fix: Return exit code 2 when any cleanup step fails; include `"warnings": [...]` key in JSON output.

---

### P2 — Should fix

**[P2-A] `CodeRollbackEntry` lacks `status` field — cannot distinguish active vs rolled-back artifacts**
- Convergence: 2/4 agents (fd-correctness C-02 MEDIUM, fd-quality Q4 LOW)
- File: `internal/runtrack/store.go` `ListArtifactsForCodeRollback`; `runtrack.go` `CodeRollbackEntry`
- Root cause: SELECT returns all artifacts including `status='rolled_back'` but does not project `a.status`. `CodeRollbackEntry` struct has no `Status` field. Operator using `ic run rollback --layer=code` after a workflow rollback cannot tell which artifacts are still active vs already invalidated, risking redundant or incorrect file reversions.
- Fix: Add `Status string` to `CodeRollbackEntry` and include `a.status` in the SELECT projection.

**[P2-B] `--layer=code` flag routes a query into a mutation subcommand**
- Convergence: 1/4 agents (fd-architecture F02 SIGNIFICANT)
- File: `cmd/ic/run.go` lines 79-91
- Root cause: The routing fork at line 79-90 sends `--layer=code` to `cmdRunRollbackCode`, a function that only queries artifacts and performs no rollback. No other `ic run` subcommand merges query and mutation behind a flag. Creates a semantic boundary violation; callers reading `ic run rollback --layer=code` interpret it as performing a code rollback.
- Fix: Split into a dedicated subcommand or query path (e.g., `ic run artifact list --with-dispatch` or `ic run code-changes <id>`).

**[P2-C] Duplicate validation and double `Get()` between machine and store creates TOCTOU window**
- Convergence: 3/4 agents (fd-architecture F03 SIGNIFICANT, fd-quality Q1 LOW, fd-correctness C-08 INFO)
- File: `internal/phase/machine.go` lines 551-568; `internal/phase/store.go` lines 814-855
- Root cause: Both `phase.Rollback()` and `phase.RollbackPhase()` independently fetch the run, check terminal status, resolve the phase chain, and validate ordering. Creates a TOCTOU window between the two reads. Also creates maintenance burden: changing rollback acceptance criteria requires editing two sites.
- Fix: Remove pre-read from `store.RollbackPhase`; use `WHERE id = ? AND status NOT IN ('cancelled','failed')` predicate on the UPDATE instead of a pre-fetch, and return `ErrTerminalRun` if 0 rows affected.

**[P2-D] No CAS guard on `RollbackPhase` UPDATE — concurrent rollbacks produce duplicate audit events**
- Convergence: 1/4 agents (fd-correctness C-03 LOW)
- File: `internal/phase/store.go` lines 257-261
- Root cause: `UPDATE runs SET phase = ? WHERE id = ?` has no `AND phase = currentPhase` predicate. Two concurrent `ic run rollback` processes see the same current phase, both validate OK, both write the same target phase, both record a rollback event. Run state is idempotently correct but the audit trail contains a duplicate rollback event. Contrast with `UpdatePhase` at `store.go:140` which uses CAS.
- Fix: Add `AND phase = currentPhase` to UPDATE; return `ErrAlreadyRolledBack` when 0 rows affected.

---

### P3/IMP — Nice to have

| ID | Convergence | Title |
|----|-------------|-------|
| F06/Q5 | 2/4 | AGENTS.md still references `lib-intercore.sh` at `v0.6.0` — bump to `v1.1.0` at lines 52, 358 |
| F05/C-05 | 2/4 | Dispatch cancellation event recorded with `dispatch_id=""` — use synthetic `"rollback-bulk"` or omit |
| C-06 | 1/4 | Migration guard comment says "v7->v8" but fires for v4-v7 — update comment |
| F07 | 1/4 | `ChainPhasesBetween` doc comment orientation-ambiguous for rollback callers |
| Q2 | 1/4 | New rollback CLI code uses `err == sentinel` not `errors.Is` — 3 new sites at lines 671, 719, 781 |
| S2 | 1/4 | Schema v8 downgrade procedure (binary revert + DB restore) not documented in rollback output or CLAUDE.md |
| S3 | 1/4 | Unknown `--format` values silently treated as JSON — should reject with exit 3 |
| S4 | 1/4 | `--reason` stored permanently in audit trail without documentation — add note to help text |
| Q8 | 1/4 | `CancelByRun` has no unit test; `FailAgentsByRun` does — add parallel test in `internal/dispatch/` |
| F09 | 1/4 | Dry-run output omits artifact and agent impact counts — less useful for pre-flight assessment |
| C-07 | 1/4 | `isDuplicateColumnError` uses driver error string match — fragile if driver updates message |
| F08 | 1/4 | `--format=text` in `cmdRunRollbackCode` premature — no consumer, not covered by integration test content |

---

## Conflicts

**Transaction severity conflict (fd-architecture vs fd-quality):** fd-architecture rates the missing transaction CRITICAL (F01). fd-quality rates the same issue LOW (Q3) and returns overall verdict "safe." fd-correctness rates it MEDIUM (C-01). fd-safety rates it LOW (S1).

Resolution: fd-architecture is most complete — it traces the concrete gate-pass failure mode (stale `status='active'` artifacts satisfying `artifact_exists` checks), independently confirmed by fd-correctness (C-01). The fd-quality "safe" verdict appears to underweight this finding. Adopted severity: **P1 (must fix)**.

**CancelByRun scope conflict (fd-architecture vs fd-correctness):** fd-correctness (C-09) calls the scope issue a "pre-existing design constraint, not a regression." fd-architecture (F04 SIGNIFICANT) correctly identifies it as a regression because `MarkArtifactsRolledBack` uses `phases []string` but `CancelByRun` does not, creating a new asymmetry. Adopted: fd-architecture framing. Severity: **P1 (must fix)**.

---

## Compact Summary (host agent return value)

```
Validation: 4/4 agents valid
Verdict: needs-changes
Gate: FAIL
P0: 0 | P1: 3 | P2: 4 | IMP: 12
Conflicts: 2 (resolved — see Conflicts section)
Top findings:
- P1 No transaction wrapping rollback compound operation — all 4 agents (4/4)
- P1 Partial cleanup failure exits 0, automation cannot detect degraded rollback — fd-correctness/fd-quality/fd-safety (3/4)
- P1 CancelByRun cancels dispatches beyond rolled-back phases — fd-architecture/fd-correctness/fd-quality (3/4)
- P2 CodeRollbackEntry lacks status field — fd-correctness/fd-quality (2/4)
- P2 Duplicate validation + TOCTOU window between machine and store — fd-architecture/fd-correctness/fd-quality (3/4)
```

---

## Output Files Written

- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/synthesis.md` — full human-readable synthesis (this document mirrors that)
- `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/findings.json` — structured findings data
- `/root/projects/Interverse/infra/intercore/docs/research/synthesize-quality-gate-results.md` — this file
