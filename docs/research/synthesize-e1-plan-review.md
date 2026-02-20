# Synthesis: Intercore E1 Kernel Primitives Plan Review

**Date:** 2026-02-19
**Plan:** `docs/plans/2026-02-19-intercore-e1-kernel-primitives.md`
**Mode:** quality-gates
**Agents:** fd-architecture, fd-correctness, fd-quality (3/3 valid, 0 failed)
**Overall Verdict:** needs-changes
**Gate:** FAIL

---

## Validation

All three agent files passed structural validation:
- Each starts with `### Findings Index`
- Each has a `Verdict:` line
- All three verdicts: `needs-changes`

No malformed or empty files. No agent errors.

---

## Agent Verdict Summary

| Agent | Status | Finding Count | Key Concern |
|-------|--------|---------------|-------------|
| fd-architecture | NEEDS_ATTENTION | 8 findings (3 MEDIUM, 4 LOW, 1 INFO) | IsTerminalPhase hardcoding; skip-scan O(n); BudgetChecker concrete types |
| fd-correctness | NEEDS_ATTENTION | 10 findings (3 HIGH, 4 MEDIUM, 3 LOW) | Migration not idempotent; all-skipped teleport breaks audit + completion; budget dedup fragility |
| fd-quality | NEEDS_ATTENTION | 14 findings (5 MEDIUM, 5 LOW, 4 INFO) | Chain* naming convention; CacheHits type; skip-walk O(n^2); BudgetChecker concrete types |

---

## P1 Findings (HIGH — must fix before implementation)

### P1-A: Migration ALTER TABLE not idempotent on re-run
**ID:** C-01 | **Convergence:** 2/3 agents (fd-correctness HIGH, fd-architecture LOW A6)

The v5->v6 migration applies multiple `ALTER TABLE` statements inside a transaction. If any statement fails mid-sequence (e.g., transient disk error), the transaction rolls back — but on retry, the first ALTER succeeds again, then the second fails with "duplicate column name." SQLite ≥ 3.37.0 supports `ALTER TABLE ADD COLUMN IF NOT EXISTS`. The bundled `modernc.org/sqlite` uses 3.43+, so every ALTER statement in the migration must use `IF NOT EXISTS`.

**Fix required:** `ALTER TABLE runs ADD COLUMN IF NOT EXISTS phases TEXT;` (and all other columns).

Conflict note: fd-architecture (A6) considered the migration ordering merely needing a comment; fd-correctness (C-01) correctly identifies the idempotency failure as HIGH. C-01 supersedes A6.

---

### P1-B: All-phases-skipped fallthrough teleports to terminal without firing completion hook or recording intermediate events
**ID:** C-02 | **Convergence:** 3/3 agents (fd-correctness C-02/C-05, fd-architecture A3/A7, fd-quality Q3)

When `Advance` walks forward and all remaining phases are marked as skipped, the plan falls back to `toPhase = chain[len(chain)-1]`. Two defects:

1. **Audit trail gap:** Only one event is recorded (`fromPhase` -> terminal). Intermediate skipped phases have no `EventAdvance` entry. Skip events recorded by `SkipPhase` cover the explicitly-skipped phases, but the advance event shows a multi-hop jump without explanation.

2. **Completion hook does not fire for custom chains:** `machine.go:167` checks `if toPhase == PhaseDone` to call `UpdateStatus(completed)`. For custom chains where the terminal phase is not literally `"done"`, this check never fires. The run stays at `status=active` permanently even after reaching its terminal phase.

**Fix required:** Replace `if toPhase == PhaseDone` with `if ChainIsTerminal(chain, toPhase)` at every call site in `machine.go`. Audit all uses of `IsTerminalPhase` in `phase.go`, `gate.go`, `machine.go` — all must be migrated to chain-aware checks.

---

### P1-C: Budget dedup via state.Store re-emits events after DB restore or manual cleanup
**ID:** C-03 | **Convergence:** 2/3 agents (fd-correctness HIGH, fd-architecture MEDIUM A1)

`BudgetChecker.CheckBudget` uses `stateStore.Exists(ctx, "budget.warning", runID)` to prevent double-emission of `budget.warning` and `budget.exceeded` events. This dedup is erased by:

- DB restore from backup (state entries pre-date the event)
- `ic state delete budget.warning.<run_id>` for cleanup
- Any TTL expiry if the implementation uses one

Second emission fires Slack alerts, gate overrides, and `budget.exceeded` blocks again.

**Fix options (in preference order):**
1. Store budget events in `dispatch_events` (durable, append-only, survives backup restores). Query `SELECT 1 FROM dispatch_events WHERE run_id=? AND event_type='budget.warning'` before emitting.
2. Accept at-least-once semantics and document that all downstream consumers of budget events must be idempotent.

The plan acknowledges none of these trade-offs. At minimum, document the behavior before implementation.

---

## P2 Findings (MEDIUM — should fix)

### IsTerminalPhase hardcodes "done" — custom chain runs never complete
**ID:** A3/C-05 | **Convergence:** 3/3 agents

`phase.go:182` defines `IsTerminalPhase(p string) bool { return p == PhaseDone }`. After E1, the terminal phase of a custom chain is `chain[len(chain)-1]` — any string. Every call site of `IsTerminalPhase` (including `EvaluateGate` in `gate.go:217` and `machine.go:56`) will incorrectly treat non-"done" terminals as non-terminal. Covered by P1-B fix above but requires auditing all call sites, not just `machine.go`.

---

### Advance skip-walk reads full event log (unbounded) and uses O(n^2) chain scan
**ID:** A2/Q3 | **Convergence:** 2/3 agents

`skippedPhases` helper calls `store.Events(ctx, runID)` — returns all events for the run, with no filter by type. For long Clavain runs, this is an unbounded sequential scan on every `advance` call. Additionally, the walk loop calls `ChainIsValidTransition(chain, fromPhase, next)` inside the range — each call is O(n) — making the full loop O(n^2) over chain length.

**Fix:**
1. Add `SkippedPhases(ctx, runID) (map[string]bool, error)` to the store with `SELECT to_phase FROM phase_events WHERE run_id=? AND event_type='skip'`.
2. Replace the walk loop with a suffix-flag pattern (see fd-quality Q3 for the corrected code).

---

### BudgetChecker holds concrete store pointers — untestable, violates project interface pattern
**ID:** A1/Q5 | **Convergence:** 2/3 agents

`internal/budget/budget.go` holds `*phase.Store`, `*dispatch.Store`, `*state.Store`. Project convention uses narrow interfaces (`RuntrackQuerier`, `VerdictQuerier` in `gate.go`). Concrete pointers:
- Create circular import risk if `phase` ever needs `budget`
- Make `CheckBudget` untestable without real SQLite instances
- Violate "accept interfaces, return structs" convention

**Fix:** Define `RunBudgetReader`, `TokenAggregator`, `StateReader` interfaces in `budget.go` following the `gate.go` pattern.

---

### Skip-walk Advance records all transitions as EventAdvance — EventSkip becomes dead code
**ID:** C-04 | **Convergence:** 1/3 agents

The refactored `Advance` in Task 3/Step 4 does not set `eventType = EventSkip` when walking past pre-skipped phases. All multi-hop skips appear as `EventAdvance` in the audit trail. `EventSkip` is still emitted by `SkipPhase` but not by `Advance` when it auto-walks through skipped phases.

**Fix:** After computing `toPhase` via skip-walk, set `eventType = EventSkip` when `toPhase != chain[fromIdx+1]`.

---

### COALESCE(SUM(cache_hits), 0) conflates no-data with zero
**ID:** C-06/Q4 | **Convergence:** 2/3 agents

`AggregateTokens` query collapses three distinct states into `TotalCache = 0`: no dispatches at all; all dispatches with `cache_hits = NULL` (not reported); all dispatches with `cache_hits = 0` (reported, genuinely zero). The `ic run tokens` output shows `Cache ratio: 0.0%` for both unreported and confirmed-zero cases.

**Fix:** Return `*int64` for `TotalCache` (nil if all rows were NULL) by adding `COUNT(CASE WHEN cache_hits IS NOT NULL THEN 1 END)` to the query. Suppress the cache ratio line in output when count is 0.

---

### Chain* function prefix inverts existing naming convention
**ID:** Q2 | **Convergence:** 1/3 agents

Existing functions follow `IsTerminalPhase`, `IsValidTransition`, `NextPhase` (verb/adjective + subject). New functions: `ChainIsTerminal`, `ChainIsValidTransition`, `ChainNextPhase` (noun prefix). This creates inconsistency in IDE completions.

**Recommended rename:**
- `ChainIsTerminal` -> `IsChainTerminal`
- `ChainIsValidTransition` -> `IsValidChainTransition`
- `ChainNextPhase` -> `NextInChain`
- `ParsePhaseChain` — keep as-is (constructor-like, acceptable)

---

### Skip-walk loop silently teleports to terminal when fromPhase not in chain
**ID:** C-07/Q3b | **Convergence:** 2/3 agents

If `fromPhase` is not found in the chain (DB corruption or schema misuse), `ChainIsValidTransition` returns false for all candidates, `toPhase` stays `""`, and the all-skipped fallthrough fires — silently teleporting to terminal. Caller sees `ErrTerminalPhase` on next advance instead of a descriptive error.

**Fix:** Validate that `fromPhase` is in the chain before entering the walk loop. Return a descriptive error if not found.

---

## P3 / IMP Findings (Low / Nice-to-have)

| ID | Agent | Title |
|----|-------|-------|
| A4 | fd-architecture | Document gateRules silent-pass for custom chains; add comment |
| A5 | fd-architecture | Use sql.NullInt64 scan for nullable cache_hits; note 32-bit truncation risk |
| A6 | fd-architecture | Add code comment distinguishing fresh DB vs existing v5 migration paths |
| A7 | fd-architecture | Guard SkipPhase against skipping terminal phase — advance never fires completion |
| A8/Q9/C-09 | all three | Move hashFile I/O to CLI layer; distinguish ErrNotExist from permission errors |
| C-08 | fd-correctness | Add FK REFERENCES dispatches(id) ON DELETE SET NULL to run_artifacts.dispatch_id |
| C-10 | fd-correctness | Make migration early-return explicit with tx.Rollback() comment |
| Q1 | fd-quality | Use slices.Equal/slices.Contains from stdlib (Go 1.22) |
| Q6 | fd-quality | Fix migration test insert ordering to respect FK parent-before-child |
| Q7 | fd-quality | Add empty-string guard to ParsePhaseChain |
| Q8 | fd-quality | Guard SkipPhase against duplicate skip events for same phase |
| Q10 | fd-quality | Document that runCols + all three Scan sites must be updated atomically |
| Q13 | fd-quality | Update cmdRun usage string to include skip and tokens subcommands |
| I2 | fd-architecture | Consolidate generateID into internal/idgen package |
| I3 | fd-architecture | Document phases=NULL sentinel as PhasesLegacy constant |
| I4 | fd-architecture | Add composite index phase_events(run_id, event_type) |

---

## Conflicts

**A6 vs C-01 (migration ordering):**
- fd-architecture (A6) rated the migration block placement as LOW, needing only a comment.
- fd-correctness (C-01) rated it HIGH: if any ALTER fails mid-sequence, the migration becomes permanently broken on retry due to "duplicate column name."
- Resolution: C-01 is correct and supersedes A6. `IF NOT EXISTS` on all ALTER statements is mandatory, not optional.

No other conflicts detected.

---

## Output Files

- Synthesis report: `/root/projects/Interverse/.clavain/quality-gates/synthesis.md`
- Findings JSON: `/root/projects/Interverse/.clavain/quality-gates/findings.json`
- Agent reports:
  - `/root/projects/Interverse/.clavain/quality-gates/fd-architecture.md`
  - `/root/projects/Interverse/.clavain/quality-gates/fd-correctness.md`
  - `/root/projects/Interverse/.clavain/quality-gates/fd-quality.md`
- Verdict JSON:
  - `/root/projects/Interverse/.clavain/verdicts/fd-architecture.json`
  - `/root/projects/Interverse/.clavain/verdicts/fd-correctness.json`
  - `/root/projects/Interverse/.clavain/verdicts/fd-quality.json`

---

## Compact Return Summary

```
Validation: 3/3 agents valid
Verdict: needs-changes
Gate: FAIL
P0: 0 | P1: 3 | P2: 7 | IMP: 16
Conflicts: 1 (A6 vs C-01 — migration idempotency severity disagreement; C-01 wins)
Top findings:
- P1 C-01: Migration ALTER TABLE not idempotent on re-run — fd-correctness (2/3)
- P1 C-02: All-phases-skipped teleport breaks audit trail and completion hook — fd-correctness (3/3)
- P1 C-03: Budget dedup via state.Store re-emits events after DB restore — fd-correctness (2/3)
- P2 A3/C-05: IsTerminalPhase hardcodes "done" — custom chains never complete — all agents (3/3)
- P2 A2/Q3: Advance skip-walk unbounded event scan + O(n^2) chain loop — arch+quality (2/3)
```
