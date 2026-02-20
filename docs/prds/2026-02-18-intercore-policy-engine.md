# PRD: Intercore Policy Engine (Wave 1)

**Bead:** iv-xn01
**Date:** 2026-02-18
**Status:** PRD
**Parent:** iv-qfg8 (Phase 2 umbrella)

## Problem Statement

Intercore's phase lifecycle runs with zero enforcement. The `evaluateGate` function in `internal/phase/machine.go` is a stub that always returns `GatePass`. This means:

1. A run can advance from `planned` to `executing` without a plan artifact existing
2. A run can advance from `executing` to `review` while agents are still active
3. A run can advance from `review` to `polish` without any review verdict
4. Gate override has no audit trail (the `override` event_type exists but is never written)

The sprint hooks (`lib-sprint.sh`, `lib-gates.sh`) implement their own ad-hoc checks outside intercore, creating two sources of truth for "is this transition allowed?"

## Solution

Replace the `evaluateGate` stub with a real implementation that checks artifact presence, agent completion, and verdict status using data already in the intercore database. No new tables needed — all gate inputs exist in `run_artifacts`, `run_agents`, and `dispatches`.

## Feature Requirements

### F1: Gate Evaluation (core)

Replace `evaluateGate(cfg GateConfig, from, to string) (result, tier string)` with real transition-specific checks. The function signature stays the same — callers are unaffected.

**Gate conditions per transition:**

| From → To | Check | Table | Query |
|-----------|-------|-------|-------|
| brainstorm → brainstorm-reviewed | Brainstorm artifact exists | run_artifacts | `WHERE run_id=? AND phase='brainstorm'` |
| brainstorm-reviewed → strategized | Strategy/PRD artifact exists | run_artifacts | `WHERE run_id=? AND phase='brainstorm-reviewed'` |
| strategized → planned | Plan artifact exists | run_artifacts | `WHERE run_id=? AND phase IN ('strategized','planned')` |
| planned → executing | Plan reviewed (artifact or verdict) | run_artifacts + dispatches | artifact with type='review' OR dispatch with verdict_status set |
| executing → review | No active agents | run_agents | `WHERE run_id=? AND status='active'` count = 0 |
| review → polish | Review verdict exists and not 'reject' | dispatches | `WHERE scope_id=? AND verdict_status IS NOT NULL AND verdict_status != 'reject'` |
| polish → done | No hard requirement | — | Always passes |

**Soft vs Hard enforcement:**
- Transitions with artifact checks use the caller's `GateConfig.Priority` to determine tier (existing logic)
- `planned → executing` is always hard (priority 0-1) regardless of caller config — execution without review is the highest-risk transition
- `polish → done` is always `TierNone` — human judgment

**Gate function needs database access:** The current signature `evaluateGate(cfg GateConfig, from, to string)` has no DB handle. The function must be refactored to accept a `*Store` parameter (or become a Store method) so it can query `run_artifacts` and `run_agents`. This is an internal change — `Advance()` already has the store and can pass it.

### F2: Gate Evidence (structured results)

When `evaluateGate` runs, it produces a list of checked conditions. This evidence is stored in `phase_events.reason` as a JSON structure:

```json
{
  "conditions": [
    {"check": "artifact_exists", "phase": "brainstorm", "result": "pass", "count": 1},
    {"check": "agents_complete", "result": "fail", "active_count": 2}
  ]
}
```

This enables:
- TUI (Wave 3) rendering gate results as a checklist
- CLI (`ic gate check`) showing human-readable gate status
- Debugging: "why did this gate fail?" is answered by reading the event

### F3: Gate CLI Commands

**`ic gate check <run_id>`** — Dry-run gate evaluation for the current transition.
- Shows what would pass/fail without actually advancing
- Displays per-condition results
- Exit 0 if all pass, exit 1 if any fail, exit 2 on error
- `--json` flag for structured output

**`ic gate override <run_id> --reason="<reason>"`** — Force advance past a failing gate.
- Writes a phase_event with `event_type='override'` and `gate_result='override'`
- Requires `--reason` (audit trail is mandatory)
- Exit 0 on success, exit 1 if run is terminal, exit 2 on error

**`ic gate rules [--phase=<from>]`** — Show configured gate rules.
- Lists all transition rules with their check types
- Filterable by source phase
- Informational only

### F4: Bash Wrappers

Add to `lib-intercore.sh`:

```bash
intercore_gate_check <run_id>           # Exit 0=pass, 1=fail
intercore_gate_override <run_id> <reason>  # Force advance
```

These replace ad-hoc checks in `lib-gates.sh` with intercore-backed evaluation.

## Non-Requirements (explicitly excluded)

1. **Policy profiles** — No named configs ("strict", "relaxed"). Hard-code sensible defaults. Profiles add abstraction before we know what configurations are needed.
2. **External gate checks** — No CI status polling, no GitHub review status. Gates evaluate local DB state only. External integrations are Wave 2+.
3. **Configurable rules per project** — All projects use the same gate conditions. Per-project customization is future work.
4. **Async gate evaluation** — Gates are synchronous DB queries (sub-ms). Async is only needed for external checks (not in scope).
5. **Event bus integration** — Gate results are written to phase_events (already happens). Event bus (Wave 2) will read these events; no special integration needed in Wave 1.

## Technical Design Notes

### Refactoring evaluateGate

Current:
```go
func evaluateGate(cfg GateConfig, from, to string) (result, tier string) {
    // stub — always passes
}
```

New:
```go
func (s *Store) evaluateGate(ctx context.Context, runID string, cfg GateConfig, from, to string) (result, tier string, evidence []GateCondition, err error)
```

Changes:
1. Becomes a Store method (needs DB access)
2. Takes `ctx` and `runID` (needs to query run-specific data)
3. Returns `evidence []GateCondition` (structured results)
4. Returns `err` (DB queries can fail)

`Advance()` already has `store`, `ctx`, and `runID` — threading them through is mechanical.

### GateCondition type

```go
type GateCondition struct {
    Check   string `json:"check"`
    Phase   string `json:"phase,omitempty"`
    Result  string `json:"result"`        // pass, fail
    Count   *int   `json:"count,omitempty"`
    Detail  string `json:"detail,omitempty"`
}
```

### Schema impact

**No new tables.** All gate queries use existing tables:
- `run_artifacts` (artifact presence)
- `run_agents` (agent completion)
- `dispatches` (verdict presence)
- `phase_events` (override audit)

**No schema version bump.** The only change is Go code.

## Success Criteria

1. `evaluateGate` returns real results based on DB state
2. A run cannot advance from `planned` to `executing` without a plan artifact (hard gate)
3. `ic gate check` shows per-condition results in text and JSON
4. `ic gate override` creates an audit trail
5. All existing unit tests pass (backward compatible — gate stubs become real checks, but test fixtures need to create appropriate artifacts)
6. Integration tests validate gate enforcement end-to-end
7. `lib-gates.sh` wrappers work with the new gate commands

## Effort Estimate

- Task 0: Refactor evaluateGate signature — 30 min
- Task 1: Implement gate conditions — 1 hour
- Task 2: Gate evidence + JSON serialization — 30 min
- Task 3: ic gate check/override/rules CLI — 1 hour
- Task 4: Bash wrappers — 30 min
- Task 5: Unit tests — 1 hour
- Task 6: Integration tests — 30 min
- Task 7: Update AGENTS.md — 15 min

**Total: ~5 hours (1-2 sessions)**
