# Architecture Review: Intercore Gate System

**Date:** 2026-02-18
**Diff:** /tmp/qg-diff-1771469961.txt
**Files reviewed:** cmd/ic/gate.go, internal/phase/gate.go, internal/phase/gate_test.go, internal/phase/machine.go, internal/runtrack/store.go, internal/dispatch/dispatch.go, cmd/ic/run.go, lib-intercore.sh, test-integration.sh

---

## Project Mode

Operating in codebase-aware mode. AGENTS.md and CLAUDE.md are present and authoritative. All recommendations are grounded in the intercore conventions (exit codes, sentinel error patterns, single-connection SQLite, package structure).

---

## Boundaries and Coupling Assessment

The gate system correctly handles the core coupling challenge: `internal/phase` needs to query runtrack and dispatch data without importing those packages (which would create an import cycle through `cmd/ic`). The solution — `RuntrackQuerier` and `VerdictQuerier` interfaces declared in `internal/phase/gate.go`, implemented by `runtrack.Store` and `dispatch.Store` respectively — is the right approach. The interface surface is minimal (two and one method respectively), and both interfaces are immediately satisfied by exactly one real implementation each, which is the appropriate threshold for introducing an interface in this codebase.

The data flow is clean: `cmd/ic/gate.go` and `cmd/ic/run.go` instantiate the concrete stores and pass them as interface values into `phase.Advance` and `phase.EvaluateGate`. The `internal/phase` package remains free of cross-package imports. This is consistent with the project's existing boundary pattern (the `phase` package only imports `internal/db` through the `*sql.DB` handle, not higher-level stores).

Gate evaluation is correctly placed in `internal/phase/gate.go` rather than in the CLI layer — the CLI files are thin dispatch handlers. The `gateRules` table is data-driven and co-located with the phase types and transition table it depends on. `evaluateGate` is package-private; `EvaluateGate` (dry-run) and `Advance` (stateful) are the two public entry points, which is a clean contract.

The `cmdGateOverride` function in `cmd/ic/gate.go` is the main coupling concern: it duplicates the phase-transition sequence from `Advance` in the CLI layer. This is detailed under Issues Found.

---

## Pattern Analysis

### Strengths

The pattern of storing structured `GateEvidence` JSON in `phase_events.reason` is a good fit for this codebase. The `reason` column is already a TEXT field used by other event types for free-form annotation; embedding the evidence as JSON in that field avoids a schema migration while making evidence machine-readable. The `GateEvidence.String()` method handles serialization at the boundary, and the tests in `gate_test.go` verify that the JSON round-trips correctly.

The `GateConfig` struct cleanly separates priority (tier computation), `DisableAll` (full bypass), and `SkipReason` (manual override annotation) into distinct fields. This avoids the combinatorial flag explosion that often appears in gate systems.

The `advanceToPhase` test helper is a well-isolated utility: it uses `Priority: 4` (TierNone) to bypass gates and navigate to a target phase for test setup. This pattern will scale as more gate tests are added.

### Concerns

**`strPtr` duplication.** The helper function `strPtr(s string) *string` exists in `internal/phase/phase.go:226` and is re-declared in `cmd/ic/gate.go:261`. Within `package phase`, the function is already available to `gate.go` and `machine.go` (same package). In `package main`, the function may or may not already exist in another `cmd/ic/*.go` file — if it does, the declaration in `gate.go` creates a compile error or silent redundancy. If it does not, it belongs in a shared helpers file for the `main` package.

**`GateWarn` is dead.** `GateWarn = "warn"` is declared in `internal/phase/phase.go:39` but is never referenced anywhere in the codebase. The soft-gate-fail-but-advance scenario currently records `GateFail` as the `GateResult` in the event (with tier `TierSoft` explaining why the run advanced despite the fail). This is defensible — the result is factual (the check failed), and the tier field carries the enforcement meaning. But `GateWarn` then has no purpose and should be removed, or the soft-fail event type should be changed to use it for consistency.

**`GateRulesInfo` anonymous structs.** The return type of `GateRulesInfo` uses an inline anonymous struct literal for both the outer slice element and the inner checks slice element. This works, but callers cannot name the type, and the function signature requires the reader to parse the full struct literal to understand the shape. Two small named types (`GateRuleInfo`, `GateCheckSpec`) would make this legible without adding behavioral complexity.

**Nil-querier contract is implicit.** The `Advance` function comment at `machine.go:37` documents that `rt` and `vq` may be nil when `Priority >= 4`. The implementation is safe because `evaluateGate` exits early for `Priority >= 4` (returning `GateNone`) before reaching any interface call. The same early exit applies for `DisableAll: true`. The concern is that the safety is structural (ordering of early returns) rather than defensive. If a future gate check type were added and its `case` block appeared before the priority check (which it would not, since the priority check is at the top), or if a rule is added to a currently-unguarded transition (e.g., `polish → done`), a nil dereference would occur at runtime with no compile-time signal. A guard comment or explicit nil check at entry to `evaluateGate` would make the contract self-documenting.

---

## Simplicity and YAGNI Assessment

The `gateRules` map approach is appropriately minimal: adding a new gate condition requires only a new `case` in the `evaluateGate` switch and a new entry in the `gateRules` map. No factory, registry, or plugin system is needed for the current three check types.

The decision to store evidence JSON in the existing `reason` column is a good YAGNI call — it avoids a schema migration for structured evidence while keeping the data accessible.

The `EvaluateGate` dry-run path correctly performs no writes, which is important for `ic gate check` to be safely callable from hooks without side effects.

The `ic gate rules` command is appropriately thin: it reads from a static in-memory table and formats it. No database round-trip, no abstraction. The `GateRulesInfo()` function exists to decouple the display logic from the internal `gateRules` map type — this is justified since `gateRule` is unexported.

---

## Issues Found (Detailed)

### A1. MEDIUM: `cmdGateCheck` uses `==` to compare a wrapped sentinel error

`EvaluateGate` in `internal/phase/gate.go:192-194` calls `store.Get(ctx, runID)` and, on error, returns `fmt.Errorf("evaluate gate: %w", err)`. When the run does not exist, `store.Get` returns the bare `ErrNotFound` sentinel. `EvaluateGate` wraps it: the caller receives `&fmt.wrapError{msg: "evaluate gate: run not found", err: ErrNotFound}`.

In `cmd/ic/gate.go:75`, the check is:
```go
if err == phase.ErrNotFound {
```

This comparison uses pointer equality against the wrapped error value, which evaluates to `false`. The not-found run falls through to the generic handler at line 82 (`return 2`), which is the "unexpected error" exit code — the wrong code for a semantically expected result.

The `ErrTerminalRun` and `ErrTerminalPhase` checks at line 79 happen to work because `EvaluateGate` returns those sentinels unwrapped (lines 197 and 200). This inconsistency — some sentinels wrapped, others not — is fragile and will break if `EvaluateGate` is refactored to wrap all errors.

**Fix:** Replace all three sentinel comparisons with `errors.Is`:
```go
if errors.Is(err, phase.ErrNotFound) {
    fmt.Fprintf(os.Stderr, "ic: gate check: not found: %s\n", runID)
    return 1
}
if errors.Is(err, phase.ErrTerminalRun) || errors.Is(err, phase.ErrTerminalPhase) {
    fmt.Fprintf(os.Stderr, "ic: gate check: %v\n", err)
    return 1
}
```

This is a one-line-per-sentinel change. It also aligns `cmdGateCheck` with Go's standard error unwrapping idiom and protects against future wrapping changes in `EvaluateGate`.

Evidence: `cmd/ic/gate.go:74-84`, `internal/phase/gate.go:192-201`.

---

### A2. MEDIUM: `cmdGateOverride` duplicates phase-transition logic in the CLI layer

`cmdGateOverride` (`cmd/ic/gate.go:126-212`) manually implements the full phase-transition sequence:

1. `store.Get` (line 158)
2. Terminal status check (line 168)
3. Terminal phase check (line 172)
4. `NextRequiredPhase` computation (line 178)
5. `store.UpdatePhase` (line 182)
6. `store.AddEvent` (line 187)
7. `store.UpdateStatus` on done (line 198-200)

This is identical in structure to `Advance` in `internal/phase/machine.go:38-170`, which already performs steps 1-7. The only behavioral difference is the event type: `cmdGateOverride` records `EventOverride` whereas `Advance` records `EventAdvance` or `EventSkip`.

This duplication creates two risks:
- If the transition logic changes (e.g., a new terminal status, a new event field, or the `UpdateStatus` on-done path changes), `cmdGateOverride` will not automatically track it.
- The `UpdatePhase` call in `cmdGateOverride` uses the same optimistic concurrency mechanism (`WHERE phase = ?`) as `Advance`, but this is easy to miss when reading `cmdGateOverride` in isolation — there is no explicit comment linking them.

The AGENTS.md documents the override ordering rationale: "UpdatePhase first, then record event — if a crash occurs between, the advance happened without audit (safer than audit without advance)." This rationale is important enough that it should be in the authoritative implementation, not in a CLI function.

**Minimal fix:** Extract an `OverridePhase` function in `internal/phase/machine.go`:
```go
// OverridePhase force-advances a run past a failed gate, recording an override event.
// UpdatePhase is called before AddEvent per the crash-safety ordering: advance happens
// even if audit is lost, never the reverse.
func OverridePhase(ctx context.Context, store *Store, runID, reason string) (*AdvanceResult, error) {
    run, err := store.Get(ctx, runID)
    if err != nil { return nil, err }
    if IsTerminalStatus(run.Status) { return nil, ErrTerminalRun }
    if IsTerminalPhase(run.Phase) { return nil, ErrTerminalPhase }
    fromPhase := run.Phase
    toPhase := NextRequiredPhase(fromPhase, run.Complexity, run.ForceFull)
    if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
        return nil, fmt.Errorf("override phase: %w", err)
    }
    store.AddEvent(ctx, &PhaseEvent{
        RunID: runID, FromPhase: fromPhase, ToPhase: toPhase,
        EventType: EventOverride, GateResult: strPtr(GateFail),
        GateTier: strPtr(TierHard), Reason: strPtr(reason),
    })
    if toPhase == PhaseDone {
        store.UpdateStatus(ctx, runID, StatusCompleted)
    }
    return &AdvanceResult{FromPhase: fromPhase, ToPhase: toPhase,
        EventType: EventOverride, GateResult: GateFail, GateTier: TierHard,
        Reason: reason, Advanced: true}, nil
}
```

`cmdGateOverride` would then be reduced to argument parsing, DB open, and a call to `phase.OverridePhase`. This also resolves A1 naturally because error sentinel comparison moves to the CLI layer where `errors.Is` is applied uniformly.

Evidence: `cmd/ic/gate.go:126-212`, `internal/phase/machine.go:38-170`.

---

## Improvements

### A3. `strPtr` helper is declared twice — check for existing `main`-package declaration

`strPtr(s string) *string` is declared in `internal/phase/phase.go:226` (package `phase`) and again at `cmd/ic/gate.go:261` (package `main`). Within the `phase` package both `gate.go` and `machine.go` can use the `phase.strPtr` declaration directly. Whether the `main` package declaration in `gate.go` conflicts with an existing declaration in another `cmd/ic/*.go` file should be verified by checking all files in `cmd/ic/`. If a duplicate exists, it is a compile error; if not, the helper belongs in a shared `cmd/ic/helpers.go` file rather than in the gate-specific file.

Evidence: `internal/phase/phase.go:226`, `cmd/ic/gate.go:261`.

### A4. Remove `GateWarn` or use it for soft-gate-fail events

`GateWarn = "warn"` at `internal/phase/phase.go:39` is declared alongside the other gate result constants but has no callers. The soft-gate-fail-but-advance scenario currently records `GateResult: GateFail` with `GateTier: TierSoft`. If that encoding is intentional (the check genuinely failed; the tier explains leniency), `GateWarn` serves no purpose and should be removed to avoid confusion. If `GateWarn` was intended as the result string for soft-failed-but-advanced events, the event recording at `machine.go:146` should use it, and a constant comment should explain the distinction.

Evidence: `internal/phase/phase.go:39`.

### A5. Give `GateRulesInfo` a named return type

`GateRulesInfo()` at `internal/phase/gate.go:220-262` returns a slice of an anonymous struct that itself contains a slice of another anonymous struct. The function body declares a local variable of the same anonymous type, builds entries, and returns them. Declaring two small named types:
```go
type GateCheckSpec struct { Check, Phase string }
type GateRuleInfo struct { From, To string; Checks []GateCheckSpec }
```
would eliminate the repeated anonymous struct literal syntax (currently appearing four times in 40 lines), make the function signature readable in godoc, and allow test code to declare variables of the named type without a cast. No behavioral change.

Evidence: `internal/phase/gate.go:220-262`.

### A6. Strengthen nil-querier contract in `evaluateGate`

The `Advance` function comment documents that `rt` and `vq` may be nil when `Priority >= 4`. The implementation is safe because `evaluateGate` returns `GateNone` before reaching any interface call when `Priority >= 4` or `DisableAll` is true. The risk is that a new rule added to `gateRules` for a transition that previously had no rules (e.g., `polish → done`) could require a querier call, and a caller passing `nil` would panic at runtime. Adding an explicit guard at the top of `evaluateGate`:
```go
// Guard: if rules exist for this transition and we have a nil querier, fail closed.
// Callers may pass nil when Priority >= 4 or DisableAll, both of which return before here.
```
or adding a nil check before each interface call with a descriptive error message would make the constraint explicit and produce a clear failure mode rather than a nil dereference.

Evidence: `internal/phase/machine.go:37`, `internal/phase/gate.go:98-117`.

---

## Summary of Required vs. Optional Work

**Must fix before relying on `ic gate check` in hooks:**
- A1: `errors.Is` for sentinel comparison in `cmdGateCheck`

**Should fix to prevent drift:**
- A2: Extract `OverridePhase` into `internal/phase/machine.go`

**Optional cleanup (low risk, do when convenient):**
- A3: Verify/consolidate `strPtr` in `cmd/ic`
- A4: Remove or activate `GateWarn`
- A5: Named types for `GateRulesInfo`
- A6: Explicit nil-querier guard or comment in `evaluateGate`
