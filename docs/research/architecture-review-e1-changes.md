# Architecture Review — Intercore E1 Kernel Primitives

**Date:** 2026-02-19
**Scope:** Schema migration v5→v6, configurable phase chains, phase skip mechanism, artifact content hashing, token tracking, token aggregation, budget threshold events, legacy code removal
**Changed files:** 25 files, +1925/-438 lines

---

## Summary

The E1 changes are a well-scoped kernel primitive upgrade that replaces a hardcoded complexity-whitelist state machine with a general configurable-chain model, adds token observability at the dispatch level, and introduces a budget dedup mechanism. The structural direction is correct: the old `transitionTable`, `complexityWhitelist`, and `validTransitions` maps were encoding policy that belongs at the call site, and removing them reduces the state machine to a clean linear-chain navigator.

The change surface is proportional. There are no new cross-package circular dependencies. The `internal/budget` package uses narrow interfaces (`PhaseStoreQuerier`) and concrete types for the two stores it needs — this is consistent with the existing gate evaluation pattern (`RuntrackQuerier`, `VerdictQuerier`). The bash shell library additions are thin wrappers consistent with the existing wrapper style.

Two structural issues stand out and are worth addressing before the primitives are depended on by callers at scale. One is a silent-error swallow in the JSON unmarshal path for `phases` stored in the database; a corrupt value is silently ignored and the run falls back to the default chain, which changes semantics invisibly. The second is that `budget.Checker` takes two concrete store types (`*dispatch.Store`, `*state.Store`) and one narrow interface, which is an inconsistency that slightly tightens the coupling surface of the new package relative to the pattern already established by the gate system.

Additionally, `cmdDispatchTokens` constructs a budget checker directly in the CLI layer, pulling in three store dependencies and budget logic into a command handler. This mixes two concerns and creates a growing CLI layer that starts accumulating policy.

---

## Boundaries and Coupling

### Phase chain storage: silent unmarshal fallback

In `internal/phase/store.go`, the JSON unmarshal of `phases` from the database is silently swallowed when it fails:

```go
// store.go, Get(), Current(), queryRuns()
if phasesJSON.Valid {
    var chain []string
    if err := json.Unmarshal([]byte(phasesJSON.String), &chain); err == nil {
        r.Phases = chain
    }
}
```

If the stored JSON is corrupt or malformed — possible after a partial write, manual DB edit, or schema conflict — the run silently falls back to `DefaultPhaseChain` without any indication that the intended chain was not loaded. Callers of `store.Get()` have no way to detect this. A run that was created with a 3-phase chain would silently execute an 8-phase default chain.

The existing pattern in this codebase is to surface errors upward: `store.Get()` returns `(*Run, error)` and all other decode failures are wrapped and returned. This path is an exception with no documented justification.

Minimal fix: return an error when `phasesJSON.Valid && unmarshal fails`. This appears in three call sites within `store.go`: `Get`, `Current`, and `queryRuns`.

### budget.Checker: inconsistent use of concrete vs interface types

`internal/budget/budget.go`:

```go
type Checker struct {
    phaseStore    PhaseStoreQuerier    // narrow interface — correct
    dispatchStore *dispatch.Store      // concrete — inconsistent
    stateStore    *state.Store         // concrete — inconsistent
}
```

The gate system (same codebase, `internal/phase/gate.go`) uses `RuntrackQuerier` and `VerdictQuerier` interfaces to avoid direct coupling to concrete packages. The budget package takes two concrete store types directly while defining a narrow interface only for the phase store. This is inconsistent and prevents testing the budget checker in isolation from the dispatch and state packages.

The narrower fix is to define `DispatchTokenAggregator` and `StateStorer` interfaces in `internal/budget` using the same pattern already established by the gate system. This is a small change that maintains architectural consistency and makes test isolation clean.

### cmdDispatchTokens: policy in the CLI handler

`cmd/ic/dispatch.go`, `cmdDispatchTokens()`:

```go
// Budget check: if this dispatch belongs to a run, check budget thresholds
if disp, err := dStore.Get(ctx, id); err == nil && disp.ScopeID != nil {
    pStore := phase.New(d.SqlDB())
    sStore := state.New(d.SqlDB())
    checker := budget.New(pStore, dStore, sStore, nil)
    result, err := checker.Check(ctx, *disp.ScopeID)
    ...
}
```

The `cmdRunBudget` command already exists specifically for explicit budget checks. Wiring an automatic budget evaluation inside a token-update command creates two paths for the same check. The `cmdDispatchTokens` handler is now constructing three store objects and instantiating a `budget.Checker` inside a CLI handler — this is policy logic drifting into the CLI layer.

The outputs are written to stderr as side-effects of a write operation, which makes this command do two different things: update tokens, then report budget status. This violates the single-responsibility principle of the CLI commands as documented in this repo. The existing pattern is that commands have one outcome and budget queries are separate.

Minimal fix: remove the budget side-effect from `cmdDispatchTokens`. Any caller that needs a post-write budget check calls `ic run budget <id>` explicitly. This also removes the three extra store constructions from the handler.

### GateRulesInfo: not updated for custom chains

`internal/phase/gate.go`, `GateRulesInfo()`:

```go
for i := 0; i < len(DefaultPhaseChain)-1; i++ {
    from := DefaultPhaseChain[i]
    to := DefaultPhaseChain[i+1]
    ...
}
```

This function now iterates only over `DefaultPhaseChain`. After the v6 change, the gate rule display (`ic gate rules`) only shows transitions for the default 8-phase lifecycle. A run with a custom phase chain will have no gate rules displayed, and the function has no way to show chain-specific gate rules. This is a known limitation but not documented in either the code comment or the CLI help.

The risk is low — `ic gate rules` is informational — but if gate rules are ever extended to apply to custom chains, this function will silently omit them. A comment noting this limitation is the minimum acceptable change.

---

## Pattern Analysis

### Skip-walk in Advance: loop termination when all remaining phases are skipped

`internal/phase/machine.go`:

```go
for skipped[toPhase] && !ChainIsTerminal(chain, toPhase) {
    next, err := ChainNextPhase(chain, toPhase)
    if err != nil {
        break
    }
    toPhase = next
}
```

The loop exits when it reaches a non-skipped phase or a terminal phase. However, it can also exit via `break` if `ChainNextPhase` returns an error — which happens when `toPhase` is not found in the chain. In that case, `toPhase` retains whatever value it had at the start of the failing iteration, not the last known-good value.

Because `ChainNextPhase` only returns an error if the current phase is terminal (index at end, returning `ErrNoTransition`) or unknown, and the loop already guards `!ChainIsTerminal`, the `break` is only reachable if a corrupt chain produces a phase not in the slice. The behavior in this edge case is to proceed with an unresolved phase, which would then fail at `UpdatePhase`. This is fragile but non-critical given current chain validation.

A clearer pattern would be to assign `toPhase = next` before breaking out of the iteration, or to return the error to the caller. The current implicit contract (break leaves `toPhase` at the last unskippable but-possibly-skipped value) is easy to misread.

### SkipPhase allows skipping the next phase the run would advance to

`store.SkipPhase` validates that `targetPhase` is ahead of `currentPhase` using `ChainIsValidTransition`. This includes the immediate next phase. When combined with the skip-walk loop in `Advance`, skipping the immediate next phase produces a skip directly to the phase after it without any record that a skip-walk occurred within `Advance` itself (the event type remains `EventAdvance` for the walk, only the pre-registered skip event records the skipped phase).

The audit trail is not misleading because the `EventSkip` was already recorded by `SkipPhase`. The `Advance` result also returns `ToPhase` which reflects the actual destination. But the `EventType` field in the `AdvanceResult` is always `EventAdvance` after the skip-walk, even when the advance effectively skipped phases. This may confuse observers reading the audit trail chronologically: they see a skip event followed by an advance event that jumped over the skipped phase, but the advance event itself does not indicate a skip occurred.

This is an observation, not a blocking issue. Documenting the intended event sequence would reduce future confusion.

### lib-intercore.sh: intercore_run_budget returns 0 when unavailable

```bash
intercore_run_budget() {
    local run_id="$1"
    if ! intercore_available; then return 0; fi
    ...
}
```

Compare with `intercore_run_skip`:

```bash
intercore_run_skip() {
    ...
    if ! intercore_available; then return 1; fi
```

Budget returns 0 (success/OK) when the binary is unavailable, while skip returns 1. For a budget guard in a hook, returning 0 when the check could not run means the hook will silently pass instead of failing or being skipped. This is intentional for graceful degradation but the asymmetry is not commented, and it differs from the `return 1` pattern used in other wrappers when availability fails.

If a hook calls `intercore_run_budget` to gate on budget exceeded (checking `$?`), it will always get exit 0 when the binary is missing. The comment says "Returns: 0=OK/warning, 1=exceeded" which is correct for the nominal path, but doesn't state what unavailability returns.

---

## Simplicity and YAGNI

### ForceFull and Complexity fields retained on Run struct with no operational effect

The `Run` struct retains `ForceFull bool` and `Complexity int`. The `--force-full` and `--complexity` CLI flags still exist in `cmdRunCreate`. However, the machine no longer consults these fields for phase navigation — `Advance` and `EvaluateGate` use `ResolveChain(run)` which only reads `run.Phases`. The fields are stored in the DB and returned in `ic run status --json` output, but they influence nothing.

This creates a misleading API surface: users can pass `--complexity=1` and expect shorter runs, but complexity now has no effect unless the caller explicitly maps it to `--phases`. The CLI help (`ic run create`) still documents `--complexity` without a note that it no longer affects phase selection.

The AGENTS.md documents that complexity-based skip is removed and refers callers to use `--phases` instead. But the CLI accepts `--complexity` without error or warning. A caller who doesn't read the migration notes will assume complexity still matters.

Minimum fix: add a deprecation note to the usage output for `--complexity` in `cmdRunCreate`, or remove the flag. The second option is smaller total surface.

### Duplicate JSON unmarshal for phases across three read paths

The pattern:

```go
if phasesJSON.Valid {
    var chain []string
    if err := json.Unmarshal([]byte(phasesJSON.String), &chain); err == nil {
        r.Phases = chain
    }
}
```

appears identically in `Get`, `Current`, and `queryRuns`. A private helper function `parsePhaseChain(ns sql.NullString) ([]string, error)` would consolidate this and, combined with the error-surfacing fix recommended above, ensure consistent behavior across all read paths. This is a modest cleanup that also makes the error-surface fix apply to all three locations with one change.

---

## Verdict

**needs-changes**

Two issues require fixes before this API surface is depended on by callers: the silent phase-chain unmarshal fallback (semantic correctness risk) and the budget side-effect in `cmdDispatchTokens` (policy in CLI, duplicate check path). The remaining items are improvements that reduce future confusion.
