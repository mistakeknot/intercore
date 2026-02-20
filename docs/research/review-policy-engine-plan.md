# Correctness Review: Intercore Policy Engine Plan

**Plan file:** `docs/plans/2026-02-18-intercore-policy-engine.md`
**Full verdict:** `.clavain/verdicts/fd-correctness-gate-plan.md`
**Reviewed:** 2026-02-18
**Reviewer:** Julik (Flux-drive Correctness Reviewer)

---

## Scope

Review of the gate evaluation implementation plan for data consistency, transaction safety, and race conditions. The plan replaces the `evaluateGate` stub in `internal/phase/machine.go` with real artifact-presence, agent-completion, and verdict-status checks. It adds `ic gate check/override/rules` CLI commands.

Key context confirmed from codebase:
- `SetMaxOpenConns(1)` — single writer per process, WAL mode, `busy_timeout` set
- `UpdatePhase` uses `WHERE phase = ?` optimistic concurrency; returns `ErrStalePhase` on double-advance
- `Advance()` calls `UpdatePhase` then `AddEvent` as two separate `ExecContext` calls (no transaction)
- Multiple `ic` processes can call `Advance()` concurrently via bash hooks from different sessions

---

## Invariants Established

1. Phase monotonicity: `phase` column only moves forward; `UpdatePhase` with `WHERE phase = ?` guards this.
2. Gate-advance atomicity: phase update and audit event must appear together or not at all.
3. HasVerdict correctness: verdicts must reflect dispatch outcomes, not artifact presence.
4. Override audit integrity: override event must be atomic with the phase update.
5. Concurrent advance safety: exactly one advance succeeds; the other gets `ErrStalePhase`.

---

## Key Findings

### Finding 2 — HIGH: UpdatePhase + AddEvent Are Two Separate Writes (No Transaction)

The existing `Advance()` makes two sequential `ExecContext` calls with no `BEGIN`/`COMMIT` wrapper. A process kill (SIGKILL, container termination) between `UpdatePhase` and `AddEvent` leaves the phase column updated but no audit event recorded. This is a pre-existing issue, but the plan introduces `ic gate override` which makes it materially worse: an unaudited override is a compliance failure. The override path in Task 5 is described as writing the event first, then calling `UpdatePhase` — which inverts the order and risks orphan override events in the audit trail for phase advances that never happened.

**Fix:** Wrap `UpdatePhase` + `AddEvent` in a single `BeginTx`/`Commit` for all advance and override paths. Given `SetMaxOpenConns(1)` and WAL mode, this is safe and will not deadlock.

### Finding 3 — HIGH: HasVerdict NULL scope_id Fallback Is Semantically Incorrect

When `run.ScopeID == nil`, the plan falls back to checking `run_artifacts WHERE type = 'review'`. A human-uploaded review document with `type = "review"` would pass the verdict gate even though no dispatch verdict exists. This produces false gate passes in any run that lacks a scope_id.

Additionally, the verdict semantics ("at least one non-reject dispatch verdict") means a run with one passing and one rejecting dispatch will advance past review. If any rejection should block the gate, the query is wrong.

**Fix:** Return `false, nil` when `scope_id` is NULL (fail-closed). Explicitly document the "at least one non-reject" policy as the intended business rule.

### Finding 4 — MEDIUM: Skip-Transition (from, to) Pairs Are Missing From gateRules

At complexity 1, `NextRequiredPhase("brainstorm", 1, false)` returns `"planned"`. The `gateRules` map has `{brainstorm, brainstorm-reviewed}` but not `{brainstorm, planned}`. The lookup miss is treated as "no rules — gate passes." A complexity-1 run at Priority 0 advances from brainstorm to planned with zero artifacts and no gate block.

**Fix:** Either add rules for all skip-transition pairs, or document explicitly that skip transitions intentionally bypass intermediate artifact gates.

### Finding 6 — MEDIUM: Gate Override Has No Atomicity (Orphan Events or Unaudited Advances)

The plan describes `ic gate override` as: (1) write a phase_event with type "override", then (2) call `store.UpdatePhase`. Without a transaction, a kill between steps produces either an orphan override event or an unaudited phase advance. The latter violates override audit integrity.

**Fix:** Same transaction fix as Finding 2. Specifically required for the override path before it ships.

### Finding 7 — MEDIUM: advanceToPhase Test Helper Infinite Loops on AutoAdvance=false

The `advanceToPhase` helper loops calling `Advance()` until `run.Phase == target`. If `Advance()` returns `Advanced=false` (pause event) without error, the helper loops forever. A test that creates a run with `AutoAdvance: false` will hang for the full test timeout.

**Fix:** Add `if !result.Advanced { t.Fatalf(...) }` after each `Advance()` call in the helper.

---

## Full Findings Table

| # | Severity | Finding |
|---|----------|---------|
| 1 | High | Gate eval and phase update are not in the same transaction |
| 2 | High | UpdatePhase + AddEvent are two separate writes — pre-existing, but plan adds override which makes it worse |
| 3 | High | HasVerdict NULL scope_id fallback checks artifacts, not verdicts |
| 4 | Medium | gateRules has no entries for complexity-skip (from, to) pairs |
| 5 | Medium | Gate conditions are multiple uncoordinated reads across time |
| 6 | Medium | ic gate override has no transaction wrapping AddEvent + UpdatePhase |
| 7 | Medium | advanceToPhase test helper loops forever on AutoAdvance=false |
| 8 | Medium | TestAdvance_GateTiers will fail for Priority 0 cases without the complete test update |
| 9 | Low | GateEvidence.String() silently drops json.Marshal errors |
| 10 | Low | ic gate override reason is unbounded; could bloat audit trail |
| 11 | Low | EvaluateGate dry-run reads phase and conditions without a snapshot |

---

## Required Before Shipping

1. **Fix HasVerdict NULL fallback** (Finding 3) — will produce incorrect gate passes in production.
2. **Wrap override AddEvent + UpdatePhase in a transaction** (Finding 6) — prevents unaudited overrides.
3. **Define and document skip-transition gate policy** (Finding 4) — currently a silent gap.
4. **Fix advanceToPhase test helper** (Finding 7) — will produce hanging tests immediately on implementation.
5. **Complete TestAdvance_GateTiers update** (Finding 8) — incomplete update will cause CI failures.

The full correctness analysis with concrete interleaving narratives is in:
`/root/projects/Interverse/.clavain/verdicts/fd-correctness-gate-plan.md`
