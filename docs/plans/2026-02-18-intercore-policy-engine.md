# Intercore Policy Engine Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Bead:** iv-xn01 (sprint), iv-tq6i (wave 1 task)
**Phase:** executing (as of 2026-02-19T02:45:53Z)

**Goal:** Replace the `evaluateGate` stub in `internal/phase/machine.go` with real artifact-presence, agent-completion, and verdict-status checks. Add `ic gate check/override/rules` CLI commands and bash wrappers.

**Architecture:** Gate conditions are defined in a lookup table keyed by `(from, to)` phase pair. Each condition queries existing tables (`run_artifacts`, `run_agents`, `dispatches`) — no schema changes. Gate evidence is serialized as JSON in `phase_events.reason`. Cross-package queries use interfaces (`RuntrackQuerier`, `VerdictQuerier`) defined in `gate.go` to avoid hidden coupling between `phase`, `runtrack`, and `dispatch` packages.

**Tech Stack:** Go 1.22, modernc.org/sqlite, bash (lib-intercore.sh wrappers)

**Review Amendments (flux-drive 2026-02-18):**
- R1: `CountArtifacts`/`CountActiveAgents` belong on `runtrack.Store`, `HasVerdict` on `dispatch.Store` — phase package uses interfaces
- R2: Remove `HasVerdict` NULL scope_id fallback — return `false, nil` when scope_id is nil
- R3: Gate override wraps `UpdatePhase` + `AddEvent` in a transaction (or calls UpdatePhase first)
- R4: Add `default` branch to evaluateGate switch + define string constants for check types
- R5: Wrap errors with `"domain verb: %w"` in `EvaluateGate` and `HasVerdict`
- R6: `advanceToPhase` test helper checks `result.Advanced` and adds overshoot guard
- R7: Add test for DB errors inside gate evaluation

**Key Conventions (from existing codebase):**
- 8-char alphanumeric IDs via `crypto/rand`
- Exit codes: 0=success, 1=expected negative (gate fail), 2=error, 3=usage
- `--flag=value` manual arg parsing in CLI
- `sql.NullString`/`sql.NullInt64` for nullable columns
- `SetMaxOpenConns(1)`, PRAGMAs set explicitly after `sql.Open`
- Integration tests in `test-integration.sh`, unit tests in `*_test.go`

---

## What Already Exists

- `evaluateGate(cfg GateConfig, from, to string) (result, tier string)` — stub that always returns `GatePass`
- `Advance(ctx, store, runID, cfg)` — calls `evaluateGate` and handles result (block on hard fail, warn+advance on soft fail)
- `GateConfig{Priority, DisableAll, SkipReason}` — caller-supplied config
- `AdvanceResult{FromPhase, ToPhase, EventType, GateResult, GateTier, Reason, Advanced}` — result struct
- Gate constants: `GatePass`, `GateFail`, `GateWarn`, `GateNone`, `TierHard`, `TierSoft`, `TierNone`
- Event types: `EventAdvance`, `EventSkip`, `EventPause`, `EventBlock`, `EventCancel`, `EventSet`
- `run_artifacts` table with `(run_id, phase, path, type)` and index on `(run_id, phase)`
- `run_agents` table with `(run_id, status)` and partial index on `status = 'active'`
- `dispatches` table with `(scope_id, verdict_status, verdict_summary)`

## What This Plan Delivers

1. **Real gate evaluation** — transition-specific checks against DB state
2. **Gate evidence** — structured JSON in `phase_events.reason` showing per-condition results
3. **`ic gate check`** — dry-run gate evaluation without advancing
4. **`ic gate override`** — force advance past a failing gate with audit trail
5. **`ic gate rules`** — show configured gate conditions
6. **Bash wrappers** — `intercore_gate_check`/`intercore_gate_override`
7. **Updated tests** — existing tests adapted + new gate-specific tests

---

## Task 0: Define gate types and condition table

**File:** `internal/phase/gate.go` (new file)

```go
package phase

import (
	"context"
	"encoding/json"
	"fmt"
)

// EventOverride is the event type for manual gate overrides.
const EventOverride = "override"

// Check type constants for gateRules (R4: named constants prevent silent typo bugs).
const (
	CheckArtifactExists = "artifact_exists"
	CheckAgentsComplete = "agents_complete"
	CheckVerdictExists  = "verdict_exists"
)

// RuntrackQuerier abstracts runtrack.Store queries needed by gate evaluation (R1).
type RuntrackQuerier interface {
	CountArtifacts(ctx context.Context, runID, phase string) (int, error)
	CountActiveAgents(ctx context.Context, runID string) (int, error)
}

// VerdictQuerier abstracts dispatch.Store queries needed by gate evaluation (R1).
type VerdictQuerier interface {
	HasVerdict(ctx context.Context, scopeID string) (bool, error)
}

// GateCondition represents a single check within a gate evaluation.
type GateCondition struct {
	Check   string `json:"check"`
	Phase   string `json:"phase,omitempty"`
	Result  string `json:"result"`             // "pass" or "fail"
	Count   *int   `json:"count,omitempty"`     // number of matching records
	Detail  string `json:"detail,omitempty"`    // human-readable explanation
}

// GateEvidence is the structured result of a gate evaluation.
type GateEvidence struct {
	Conditions []GateCondition `json:"conditions"`
}

// String serializes evidence to JSON for storage in phase_events.reason.
func (e *GateEvidence) String() string {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Sprintf(`{"error":%q}`, err.Error())
	}
	return string(b)
}

// gateRule defines a single gate check to perform for a phase transition.
type gateRule struct {
	check string // CheckArtifactExists, CheckAgentsComplete, CheckVerdictExists
	phase string // which phase's artifacts to check (empty = use fromPhase)
}

// gateRules maps (from, to) phase pairs to their required checks.
// Transitions not in this table have no gate requirements.
// Skip-transitions (complexity-based phase skipping) bypass this table entirely.
var gateRules = map[[2]string][]gateRule{
	{PhaseBrainstorm, PhaseBrainstormReviewed}: {
		{check: CheckArtifactExists, phase: PhaseBrainstorm},
	},
	{PhaseBrainstormReviewed, PhaseStrategized}: {
		{check: CheckArtifactExists, phase: PhaseBrainstormReviewed},
	},
	{PhaseStrategized, PhasePlanned}: {
		{check: CheckArtifactExists, phase: PhaseStrategized},
	},
	{PhasePlanned, PhaseExecuting}: {
		{check: CheckArtifactExists, phase: PhasePlanned},
	},
	{PhaseExecuting, PhaseReview}: {
		{check: CheckAgentsComplete},
	},
	{PhaseReview, PhasePolish}: {
		{check: CheckVerdictExists},
	},
	// polish → done: no gate requirements (human judgment)
}
```

The `gateRules` table is the single source of truth for what each transition requires. Adding a new check is one line in this table.

**Acceptance:** File compiles. Types are exported for use by CLI and tests.

---

## Task 1: Add gate query methods to runtrack.Store and dispatch.Store

**R1 amendment:** Queries belong on the packages that own the tables, not on `phase.Store`.

**File:** `internal/runtrack/store.go` — add two methods:

```go
// CountArtifacts returns the number of artifacts for a run in the given phase.
func (s *Store) CountArtifacts(ctx context.Context, runID, phase string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_artifacts WHERE run_id = ? AND phase = ?`,
		runID, phase).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count artifacts: %w", err)
	}
	return count, nil
}

// CountActiveAgents returns the number of agents with status='active' for a run.
func (s *Store) CountActiveAgents(ctx context.Context, runID string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM run_agents WHERE run_id = ? AND status = 'active'`,
		runID).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count active agents: %w", err)
	}
	return count, nil
}
```

These satisfy `phase.RuntrackQuerier`.

**File:** `internal/dispatch/store.go` — add one method:

```go
// HasVerdict returns true if any dispatch for the given scope has a non-null, non-reject verdict.
// R2: When scopeID is empty, returns false (gate fails explicitly — use override for unusual configs).
func (s *Store) HasVerdict(ctx context.Context, scopeID string) (bool, error) {
	if scopeID == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM dispatches
			WHERE scope_id = ? AND verdict_status IS NOT NULL AND verdict_status != 'reject'`,
		scopeID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("has verdict: %w", err)
	}
	return count > 0, nil
}
```

This satisfies `phase.VerdictQuerier`. No NULL fallback (R2): empty scope_id = gate fails explicitly.

**CLI wiring (cmd/ic/run.go and cmd/ic/gate.go):** Instantiate `runtrack.New(db)` and `dispatch.New(db)` alongside `phase.New(db)`, pass them to gate evaluation methods.

**Acceptance:** Methods compile. `runtrack.Store` satisfies `RuntrackQuerier`. `dispatch.Store` satisfies `VerdictQuerier`.

---

## Task 2: Implement real evaluateGate

**File:** `internal/phase/machine.go`

Replace the stub `evaluateGate` with a real implementation. The function signature changes to accept interface parameters (R1) instead of being a Store method:

**Before:**
```go
func evaluateGate(cfg GateConfig, from, to string) (result, tier string) {
	if cfg.DisableAll {
		return GateNone, TierNone
	}
	// tier derivation...
	return GatePass, tier
}
```

**After:**
```go
// evaluateGate checks whether a phase transition should be allowed.
// Returns gate result, tier, and structured evidence.
// R1: Uses interfaces instead of direct Store access to avoid cross-package coupling.
func evaluateGate(ctx context.Context, run *Run, cfg GateConfig, from, to string, rt RuntrackQuerier, vq VerdictQuerier) (result, tier string, evidence *GateEvidence, err error) {
	if cfg.DisableAll {
		return GateNone, TierNone, nil, nil
	}

	// Determine tier from priority
	switch {
	case cfg.Priority <= 1:
		tier = TierHard
	case cfg.Priority <= 3:
		tier = TierSoft
	default:
		return GateNone, TierNone, nil, nil
	}

	// Look up rules for this transition
	rules, ok := gateRules[[2]string{from, to}]
	if !ok {
		return GatePass, tier, nil, nil
	}

	evidence = &GateEvidence{}
	allPass := true

	for _, rule := range rules {
		cond := GateCondition{
			Check: rule.check,
			Phase: rule.phase,
		}

		switch rule.check {
		case CheckArtifactExists:
			count, qerr := rt.CountArtifacts(ctx, run.ID, rule.phase)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			cond.Count = &count
			if count > 0 {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = fmt.Sprintf("no artifacts found for phase %q", rule.phase)
				allPass = false
			}

		case CheckAgentsComplete:
			count, qerr := rt.CountActiveAgents(ctx, run.ID)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			cond.Count = &count
			if count == 0 {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = fmt.Sprintf("%d agents still active", count)
				allPass = false
			}

		case CheckVerdictExists:
			scopeID := ""
			if run.ScopeID != nil {
				scopeID = *run.ScopeID
			}
			has, qerr := vq.HasVerdict(ctx, scopeID)
			if qerr != nil {
				return "", "", nil, fmt.Errorf("gate check: %w", qerr)
			}
			if has {
				cond.Result = GatePass
			} else {
				cond.Result = GateFail
				cond.Detail = "no passing verdict found"
				allPass = false
			}

		default:
			// R4: Unknown check type fails explicitly instead of silently passing.
			cond.Result = GateFail
			cond.Detail = fmt.Sprintf("unknown check type: %q", rule.check)
			allPass = false
		}

		evidence.Conditions = append(evidence.Conditions, cond)
	}

	if allPass {
		return GatePass, tier, evidence, nil
	}
	return GateFail, tier, evidence, nil
}
```

**Update `Advance()` signature** to accept interface parameters:

```go
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier) (*AdvanceResult, error) {
```

**Update `Advance()` call site** (was line 88):

Before: `gateResult, gateTier := evaluateGate(cfg, fromPhase, toPhase)`
After: `gateResult, gateTier, evidence, err := evaluateGate(ctx, run, cfg, fromPhase, toPhase, rt, vq)`
Handle the new `err` return (R5: wrap errors).

**Update the reason field** in `Advance()` to include evidence when present:

In the block event and advance event, if evidence is non-nil, set the reason to `evidence.String()` (JSON).

**Also add** `EvaluateGate` as a public function for the CLI's dry-run check:

```go
// EvaluateGate performs a dry-run gate check for the next transition.
// R5: Wraps all errors with domain context.
func EvaluateGate(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier) (*GateCheckResult, error) {
	run, err := store.Get(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("evaluate gate: %w", err)
	}
	if IsTerminalStatus(run.Status) {
		return nil, ErrTerminalRun
	}
	if IsTerminalPhase(run.Phase) {
		return nil, ErrTerminalPhase
	}

	toPhase := NextRequiredPhase(run.Phase, run.Complexity, run.ForceFull)
	result, tier, evidence, err := evaluateGate(ctx, run, cfg, run.Phase, toPhase, rt, vq)
	if err != nil {
		return nil, fmt.Errorf("evaluate gate: %w", err)
	}

	return &GateCheckResult{
		RunID:     runID,
		FromPhase: run.Phase,
		ToPhase:   toPhase,
		Result:    result,
		Tier:      tier,
		Evidence:  evidence,
	}, nil
}

// GateCheckResult is the public result of a dry-run gate evaluation.
type GateCheckResult struct {
	RunID     string
	FromPhase string
	ToPhase   string
	Result    string
	Tier      string
	Evidence  *GateEvidence
}
```

**Acceptance:** `evaluateGate` returns real results + error. `Advance()` accepts interface params. Existing tests that use `GateConfig{Priority: 4}` (TierNone) are unaffected since Priority 4+ skips gate evaluation entirely. Tests pass `nil` for `rt`/`vq` when using TierNone.

---

## Task 3: Update existing tests for gate evaluation

**File:** `internal/phase/machine_test.go`

The existing tests mostly use `GateConfig{Priority: 4}` which means `TierNone` (no gate evaluation). These continue to pass without changes.

The tests that use Priority 0-2 need artifacts/agents in the DB for gates to pass:

**`TestAdvance_GateTiers`** — The Priority 2 and Priority 0 cases now need a brainstorm artifact for the brainstorm→brainstorm-reviewed gate to pass. Add artifact creation before the advance:

```go
// Add artifact so gate passes
rtStore := runtrack.New(d.SqlDB())
rtStore.AddArtifact(ctx, &runtrack.Artifact{
	RunID: id2, Phase: "brainstorm", Path: "test.md", Type: "file",
})
```

Same for the Priority 0 test case (id3).

**Update `setupMachineTest`** to return `(*Store, *runtrack.Store, *sql.DB, context.Context)` so tests can create artifacts and pass `RuntrackQuerier`/`VerdictQuerier` to `Advance()`.

**Update all `Advance()` call sites** in tests: pass `rtStore` as `RuntrackQuerier` and a stub `VerdictQuerier` (or `nil` for TierNone tests where Priority >= 4).

**Acceptance:** All existing tests pass with the new gate evaluation. Tests that use Priority ≤3 have appropriate artifacts.

---

## Task 4: Add gate-specific unit tests

**File:** `internal/phase/gate_test.go` (new file)

Tests:

- `TestGate_ArtifactExists_Pass` — Create run, add brainstorm artifact, advance with Priority 0. Gate passes.
- `TestGate_ArtifactExists_Fail` — Create run, no artifact, advance with Priority 0. Gate blocks (hard tier).
- `TestGate_ArtifactExists_SoftFail` — Create run, no artifact, advance with Priority 2. Gate warns but advances.
- `TestGate_AgentsComplete_Pass` — Create run at `executing` phase, no active agents, advance. Gate passes.
- `TestGate_AgentsComplete_Fail` — Create run at `executing`, add active agent, advance. Gate blocks.
- `TestGate_VerdictExists_Pass` — Create run at `review`, add dispatch with verdict. Gate passes.
- `TestGate_VerdictExists_Fail` — Create run at `review`, no verdict. Gate blocks.
- `TestGate_DisableAll` — Gate disabled, advance regardless of missing artifacts.
- `TestGate_NoRulesForTransition` — polish→done has no rules, always passes.
- `TestGate_Evidence_Serialized` — Check that evidence JSON appears in phase_events.reason.
- `TestEvaluateGate_DryRun` — Public `EvaluateGate()` returns result without advancing.

Each test creates the required DB fixtures (runs, artifacts, agents, dispatches) using the existing store methods.

**Test helper for advancing a run to a specific phase (R6: overshoot guard + Advanced check):**

```go
func advanceToPhase(t *testing.T, store *Store, runID string, target string, rt RuntrackQuerier) {
	t.Helper()
	cfg := GateConfig{Priority: 4} // TierNone — bypass gates
	for {
		run, err := store.Get(context.Background(), runID)
		if err != nil {
			t.Fatalf("advanceToPhase(%s): get: %v", target, err)
		}
		if run.Phase == target {
			return
		}
		if IsTerminalPhase(run.Phase) || IsTerminalStatus(run.Status) {
			t.Fatalf("advanceToPhase(%s): overshot (currently at %s)", target, run.Phase)
		}
		result, err := Advance(context.Background(), store, runID, cfg, rt, nil)
		if err != nil {
			t.Fatalf("advanceToPhase(%s): %v", target, err)
		}
		if !result.Advanced {
			t.Fatalf("advanceToPhase(%s): advance returned Advanced=false at %s", target, run.Phase)
		}
	}
}
```

**Additional test (R7): `TestGate_DBError`** — test that a cancelled context (or closed DB) inside gate evaluation returns an error, not a silent pass:

```go
func TestGate_DBError(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)
	// Create run, advance to brainstorm
	id, _ := store.Create(ctx, &Run{...})
	// Cancel context
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	// Attempt advance with cancelled context — should error, not pass
	_, err := Advance(cancelCtx, store, id, GateConfig{Priority: 0}, rtStore, nil)
	if err == nil {
		t.Fatal("expected error from gate check with cancelled context")
	}
}
```

**Acceptance:** All new gate tests pass. Coverage of gate evaluation logic is comprehensive. DB error behavior is pinned.

---

## Task 5: Add `ic gate` CLI commands

**New file:** `cmd/ic/gate.go`

Add case to main switch in `cmd/ic/main.go`:
```go
case "gate":
	exitCode = cmdGate(ctx, subArgs)
```

Add to `printUsage()`:
```
  gate check <run_id> [--priority=N]     Dry-run gate evaluation
  gate override <run_id> --reason=<s>    Force advance past failing gate
  gate rules [--phase=<from>]            Show gate conditions
```

**`cmdGate(ctx, args)`** dispatches to subcommands:

**`ic gate check <run_id>`:**
- Opens DB, creates phase.Store
- Calls `store.EvaluateGate(ctx, runID, cfg)`
- Text output: per-condition results with pass/fail indicators
- JSON output: `GateCheckResult` serialized
- Exit 0 if all pass, exit 1 if any fail

**`ic gate override <run_id> --reason="..."`:**
- Opens DB, creates phase.Store, runtrack.Store, dispatch.Store
- Verifies run exists and is not terminal
- Gets current phase and next required phase
- R3: Calls `store.UpdatePhase` FIRST, then records the `EventOverride` event (crash between = advance happened without event, safer than event without advance)
- Exit 0 on success, exit 1 if terminal, exit 2 on error

**`ic gate rules [--phase=<from>]`:**
- Pure data dump: iterates `gateRules` table
- Text output: table of transitions and their checks
- JSON output: array of `{from, to, checks: [{check, phase}]}`
- Exit 0 always

**Acceptance:** `ic gate check`, `ic gate override`, and `ic gate rules` work end-to-end.

---

## Task 6: Add bash wrappers to lib-intercore.sh

**File:** `infra/intercore/lib-intercore.sh`

Add after the lock wrappers section:

```bash
# --- Gate wrappers ---

# intercore_gate_check — Dry-run gate evaluation for the next transition.
# Args: $1=run_id
# Returns: 0=pass, 1=fail, 2+=error/fallthrough
intercore_gate_check() {
    local run_id="$1"
    if intercore_available; then
        local rc=0
        "$INTERCORE_BIN" gate check "$run_id" ${INTERCORE_DB:+--db="$INTERCORE_DB"} >/dev/null || rc=$?
        return $rc
    fi
    return 0  # fail-open: no intercore = gates disabled
}

# intercore_gate_override — Force advance past a failing gate.
# Args: $1=run_id, $2=reason
# Returns: 0=success, 1=terminal, 2+=error
intercore_gate_override() {
    local run_id="$1" reason="$2"
    if intercore_available; then
        "$INTERCORE_BIN" gate override "$run_id" --reason="$reason" \
            ${INTERCORE_DB:+--db="$INTERCORE_DB"} || return $?
        return 0
    fi
    return 0  # fail-open
}
```

Bump `INTERCORE_WRAPPER_VERSION` to `"0.5.0"`.

**Acceptance:** Wrappers work when `ic` is available, fail-open when it's not.

---

## Task 7: Integration tests

**File:** `infra/intercore/test-integration.sh`

Add a new section `# === Gates ===`:

```bash
echo "=== Gates ==="

# Create a run for gate testing
GATE_RUN=$(ic run create --project=. --goal="gate test" --complexity=3 --json | jq -r .id)

# Gate check should fail (no brainstorm artifact)
ic gate check "$GATE_RUN" --priority=0
assert_exit 1 "gate check fails without artifact"

# Gate check should show rules
ic gate rules
assert_exit 0 "gate rules"

# Add a brainstorm artifact
ic run artifact add "$GATE_RUN" --phase=brainstorm --path=test.md

# Gate check should pass now
ic gate check "$GATE_RUN" --priority=0
assert_exit 0 "gate check passes with artifact"

# Advance (should succeed with artifact present)
ic run advance "$GATE_RUN" --priority=0
assert_exit 0 "advance with passing gate"

# Verify phase advanced
PHASE=$(ic run phase "$GATE_RUN")
[[ "$PHASE" == "brainstorm-reviewed" ]]
assert_exit 0 "phase is brainstorm-reviewed after gate pass"

# Gate override test — advance without required artifact
# (no brainstorm-reviewed artifact exists)
ic gate check "$GATE_RUN" --priority=0
assert_exit 1 "gate check fails without brainstorm-reviewed artifact"

ic gate override "$GATE_RUN" --reason="test override"
assert_exit 0 "gate override advances past failing gate"

PHASE2=$(ic run phase "$GATE_RUN")
[[ "$PHASE2" == "strategized" ]]
assert_exit 0 "phase is strategized after override"

# Verify override event in audit trail
ic run events "$GATE_RUN" --json | jq -e '.[] | select(.event_type=="override")' >/dev/null
assert_exit 0 "override event recorded"

# Gate check JSON output
GATE_RUN2=$(ic run create --project=. --goal="json test" --complexity=3 --json | jq -r .id)
ic gate check "$GATE_RUN2" --priority=0 --json | jq -e '.result' >/dev/null
assert_exit 0 "gate check --json"

# Wrapper test
source lib-intercore.sh
intercore_gate_check "$GATE_RUN2"
assert_exit 1 "wrapper: gate check fails"
```

**Acceptance:** `bash test-integration.sh` passes with all gate tests.

---

## Task 8: Update AGENTS.md and CLAUDE.md

**File:** `infra/intercore/AGENTS.md`

- Add `internal/phase/gate.go` to the architecture diagram
- Add gate CLI commands to the CLI Commands section
- Document the `gateRules` table in a new "Gate Evaluation" section
- Update "Gate stubs" references to reflect real implementation

**File:** `infra/intercore/CLAUDE.md`

Add gate quick reference:
```bash
# Gate evaluation (checks transition prerequisites)
ic gate check <run_id> [--priority=N]   # Dry-run: 0=pass, 1=fail
ic gate override <run_id> --reason=<s>  # Force advance past gate
ic gate rules [--phase=<from>]          # Show gate conditions
```

**Acceptance:** Documentation reflects the new gate subsystem.

---

## Dependency Graph

```
Task 0 (types + rules table)
  → Task 1 (store query methods)
    → Task 2 (real evaluateGate + EvaluateGate)
      → Task 3 (update existing tests)
      → Task 4 (new gate tests)
      → Task 5 (CLI commands)
        → Task 6 (bash wrappers)
        → Task 7 (integration tests)
          → Task 8 (docs)
```

**Parallelizable:** Tasks 3, 4, 5 are independent after Task 2.

## Design Decisions

- **No new tables.** All gate data comes from `run_artifacts`, `run_agents`, and `dispatches`. Policy config is a Go map, not a DB table.
- **Interface-based queries (R1).** `evaluateGate` takes `RuntrackQuerier` and `VerdictQuerier` interfaces instead of directly accessing `runtrack.Store` or `dispatch.Store`. Queries live on the stores that own the tables. Phase package has no import of runtrack or dispatch.
- **Evidence in `phase_events.reason`.** The `reason TEXT` column already exists. Storing structured JSON there requires no schema change and keeps the audit trail self-contained.
- **Gate override as a separate CLI command.** Override is intentionally separate from `ic run advance --skip-reason` because overrides should be explicit and auditable. The event_type is `override`, not `advance`.
- **Fail-open wrappers.** Bash wrappers return 0 (pass) when `ic` is unavailable, matching the existing fail-open pattern. This ensures hooks don't break if intercore isn't installed.
- **No HasVerdict fallback (R2).** When `scope_id` is nil, verdict gate returns false. Override is the correct path for unusual configurations.
- **DB errors fail the gate (R4, R5, R7).** Query errors return `error` from `evaluateGate` instead of being swallowed into condition details. Caller decides how to handle.

## Estimated Effort

- Tasks 0-2: ~1.5 hours (types + queries + evaluateGate)
- Tasks 3-4: ~1 hour (test updates + new tests)
- Tasks 5-6: ~1 hour (CLI + wrappers)
- Tasks 7-8: ~30 min (integration tests + docs)
- **Total: ~4 hours (1 session)**
