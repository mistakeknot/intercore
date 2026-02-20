# Quality Review: Intercore Policy Engine Plan
# 2026-02-18-intercore-policy-engine.md

**Reviewer:** Flux-drive Quality & Style Reviewer
**Date:** 2026-02-18
**Plan file:** `docs/plans/2026-02-18-intercore-policy-engine.md`
**Verdict file:** `.clavain/verdicts/fd-quality-gate-plan.md`

---

## Summary Verdict

The plan is architecturally sound and well-aligned with the existing codebase
conventions. There are four concrete issues worth fixing before implementation,
ranging from a correctness gap in test coverage to a subtle design tension in
`GateCondition`. No blocking issues exist; the plan can proceed with the
amendments below applied.

---

## 1. Naming Conventions

**Status: Compliant**

The plan follows the codebase's established naming patterns faithfully.

- `GateCondition`, `GateEvidence`, `GateCheckResult` are exported with
  PascalCase and match the existing `GateConfig`, `GatePass`, `GateFail`
  vocabulary. The 5-second rule applies cleanly.
- `gateRule`, `gateRules` (unexported) follow Go conventions for package-private
  types.
- Method names `CountArtifacts`, `CountActiveAgents`, `HasVerdict` are
  consistent with the existing `ListActive`, `ListAgents`, `AddArtifact`
  pattern.
- `EvaluateGate` (public) and `evaluateGate` (private) follow the same
  exported-wrapper / unexported-implementation split already present in the
  codebase.
- `EventOverride = "override"` is consistent with `EventAdvance = "advance"`,
  `EventBlock = "block"`, etc.
- `cmdGate` follows `cmdRun`, `cmdDispatch`, `cmdLock`.
- `intercore_gate_check` / `intercore_gate_override` follow the
  `intercore_lock_acquire` / `intercore_dispatch_wait` naming pattern in
  `lib-intercore.sh`.

**One minor observation:** `GateCheckResult` lives in `machine.go` per the plan.
Given it is a data-only type with no behaviour, placing it in `gate.go`
alongside `GateCondition` and `GateEvidence` would keep `machine.go` focused on
the state-machine logic and `gate.go` as the complete gate type surface. This is
a preference, not a defect.

---

## 2. Error Handling Consistency

**Status: Mostly compliant — two issues**

### 2a. Bare error propagation in `EvaluateGate` (Task 2)

```go
run, err := s.Get(ctx, runID)
if err != nil {
    return nil, err   // <-- bare, no wrapping context
}
```

Every other Store method in the codebase wraps DB errors:
```go
return nil, fmt.Errorf("run get: %w", err)
return nil, fmt.Errorf("event add: %w", err)
```

`EvaluateGate` is a public method and is the entry point from the CLI. If `Get`
returns an error, the caller sees a raw sentinel (`ErrNotFound`) or a raw SQL
error with no hint of where in the call chain it originated. The fix is to
follow the established pattern and wrap:

```go
run, err := s.Get(ctx, runID)
if err != nil {
    return nil, fmt.Errorf("gate check: %w", err)
}
```

The same bare propagation exists in `HasVerdict` for the inner `s.Get` call:
```go
run, err := s.Get(ctx, runID)
if err != nil {
    return false, err  // <-- should be fmt.Errorf("has verdict: %w", err)
}
```

Both need the codebase-standard `"domain verb: %w"` wrapping.

### 2b. Query error handling inside `evaluateGate` (Task 2)

When a DB query fails inside the rule loop, the plan emits a bare string via
`fmt.Sprintf("query error: %v", err)` into `cond.Detail` and sets the condition
to `GateFail`. This silently converts an infrastructure error into a gate
failure, which could block a valid advance. The detail string also uses `%v`
(losing the error chain) rather than preserving the original error for
diagnostics.

Two options, both acceptable:

**Option A** — Return a wrapped error immediately on DB failure, rather than
converting it to a gate failure:
```go
count, err := s.CountArtifacts(ctx, runID, rule.phase)
if err != nil {
    return GateFail, tier, nil, fmt.Errorf("evaluate gate: artifact check: %w", err)
}
```
This requires adding `error` to the return signature of the private
`evaluateGate`, which the public `EvaluateGate` and `Advance` callers then
handle explicitly.

**Option B** — Keep the current design (DB errors as soft gate failures) but log
to stderr with `%w`-preserving context, and set `cond.Detail` to the full error
string. This is acceptable if the design intent is "query errors should not be
fatal to the caller"; document this explicitly with a comment.

Option A is more consistent with how the rest of the codebase treats DB errors
(every other DB method returns an `error`; none silently swallow them).

---

## 3. GateCondition Type Design

**Status: Functional, one extensibility concern**

### 3a. String-typed `Check` field is fragile

```go
type GateCondition struct {
    Check   string `json:"check"`
    ...
}
```

The `Check` field is compared with string literals in the switch statement:
```go
case "artifact_exists":
case "agents_complete":
case "verdict_exists":
```

And the `gateRule` struct also uses `check string`. This means:
- Typos in `gateRules` definitions produce silent no-ops (the default case of
  the switch does nothing, `allPass` is not set to false, and the condition is
  appended with empty Result).
- Adding a new check requires updating both the `gateRules` map and the switch
  in `evaluateGate` — there is no compile-time enforcement of completeness.

**Recommendation:** Define typed constants for the check names and add a
`default` case to the switch that either panics (during init) or returns an
error. The simplest safe approach:

```go
const (
    CheckArtifactExists = "artifact_exists"
    CheckAgentsComplete = "agents_complete"
    CheckVerdictExists  = "verdict_exists"
)
```

Then in the switch, add:
```go
default:
    cond.Result = GateFail
    cond.Detail = fmt.Sprintf("unknown check type: %q", rule.check)
    allPass = false
```

This prevents silent pass-through when an unknown check name is introduced.

### 3b. Missing `Result` on the default (no-op) path

As currently written, if `rule.check` does not match any known case, the
condition is appended with an empty `Result` field (`""`), which is neither
`"pass"` nor `"fail"`. The evidence JSON will be technically malformed relative
to what consumers expect. The `default` case fix above resolves this.

### 3c. The `Count *int` field has acceptable design

The pointer-to-int for `Count` is the right choice here: it signals "count was
not measured" (nil) vs. "count was 0" (pointer to 0). This is consistent with
the codebase's use of `sql.NullInt64` for optional integers and `*string` for
optional string fields.

---

## 4. Test Coverage Assessment

**Status: Gaps present — three missing cases**

The 11 proposed tests in `gate_test.go` cover the main paths well. The following
cases are missing and represent meaningful risk:

### 4a. DB error inside gate evaluation (critical gap)

There is no test for what happens when `CountArtifacts`, `CountActiveAgents`, or
`HasVerdict` returns a DB error. The current plan converts DB errors into gate
failures, so the observable behavior is: advance is blocked when the DB is
unavailable. This may or may not be intended. A test like:

```go
// TestGate_DBError_TreatedAs...
// Use a closed DB or cancelled context to force a query error.
// Assert the observed behavior (blocked or error returned).
```

This test is critical because it pins down the contract. Whether the decision is
"DB error = block" or "DB error = propagated error", it should be asserted, not
left implicit.

### 4b. Soft gate fail: evidence still populated (missing)

`TestGate_ArtifactExists_SoftFail` advances past a failing soft gate, which is
correct. But the plan does not include a test that verifies the evidence JSON is
still stored in `phase_events.reason` even when the soft gate fails and the
advance proceeds. The advance *succeeds* but the audit trail should still show
the evidence of what failed.

### 4c. `advanceToPhase` infinite loop risk (implementation concern, not test)

```go
func advanceToPhase(t *testing.T, store *Store, runID string, target string) {
    ...
    for {
        run, _ := store.Get(context.Background(), runID)
        if run.Phase == target {
            return
        }
        _, err := Advance(context.Background(), store, runID, cfg)
        if err != nil {
            t.Fatalf("advanceToPhase(%s): %v", target, err)
        }
    }
}
```

If `target` is a phase that is skipped for the run's complexity (e.g., asking
a complexity-1 run to advance to `PhaseStrategized`), `Advance` will jump over
`target` and keep going until the run completes, at which point `Advance` will
return `ErrTerminalRun`. The `t.Fatalf` catches this, so it does not loop
forever, but the error message ("advanceToPhase(strategized): run is in a
terminal status") will be confusing. Add a guard:

```go
if IsTerminalPhase(run.Phase) || IsTerminalStatus(run.Status) {
    t.Fatalf("advanceToPhase(%s): overshot (currently at %s)", target, run.Phase)
}
```

### 4d. `setupMachineTest` return value change (Task 3)

The plan proposes changing `setupMachineTest` to return `(*Store, *sql.DB,
context.Context)` so tests can create runtrack artifacts. Note that
`setupTestStore` in `store_test.go` returns only `*Store` (no context, no raw
DB). The two helpers already diverge (one includes `ctx`, the other does not).

The proposed change to return `*sql.DB` from `setupMachineTest` bypasses the
`runtrack.Store` abstraction. Callers would then do direct SQL inserts or
construct a `runtrack.Store` separately. A cleaner approach is to expose a
`setupMachineTestWithRuntrack` helper that returns
`(*Store, *runtrack.Store, context.Context)` — keeping the raw `*sql.DB` hidden
from test code, consistent with how the production code interacts through Store
interfaces.

---

## 5. Task 2: `Advance()` Call Site Update

**Status: One ambiguity**

The plan says (Task 2):

> In the block event (line ~91), and in the advance event (line ~129), if
> evidence is non-nil, set the reason to `evidence.String()` (JSON). If
> `cfg.SkipReason` is also non-empty, combine them:
> `cfg.SkipReason + " | " + evidence.String()`.

The `reason` field in `phase_events` is `TEXT`. JSON evidence can be large
(multi-condition gate results). The current codebase stores plain text reasons
like `"auto_advance disabled"` or `"manual override"`. Combining a human-readable
reason with a JSON blob via `" | "` is awkward to parse from the CLI's
`ic run events` output.

**Recommendation:** Store the evidence JSON as the entire reason when evidence
is available, and store `cfg.SkipReason` separately in the existing `Reason`
field of `AdvanceResult` (not in the DB). If the intent is to have both, use a
small wrapper struct:

```go
type reasonPayload struct {
    SkipReason string       `json:"skip_reason,omitempty"`
    Evidence   *GateEvidence `json:"evidence,omitempty"`
}
```

This makes `ic run events --json` easier to consume programmatically.
Alternatively, store only the raw `evidence.String()` in the DB and surface
`SkipReason` only in `AdvanceResult`. Either is better than the `" | "` concat.

---

## 6. Task 5: CLI Design

**Status: Minor gap**

`ic gate override` as described writes a `phase_event` with `EventOverride` type
and then calls `store.UpdatePhase`. This correctly separates override from
`advance`. However, the plan does not specify what the `from_phase` and
`to_phase` fields of the override event should be. The pattern from `Advance` is
to record the actual `fromPhase` and `toPhase`. The gate.go file should specify
this explicitly, since "override" events have a different semantic (skipping
rather than transitioning) and the CLI tests should assert both phases are
recorded correctly.

---

## 7. Task 6: Bash Wrappers

**Status: Compliant**

The proposed wrappers match the existing patterns in `lib-intercore.sh`
(fail-open, `${INTERCORE_DB:+--db="$INTERCORE_DB"}` expansion, `local rc=0`
pattern). Version bump from 0.4.0 to 0.5.0 is appropriate for a new command
group.

One note: the wrapper comment says:

```bash
# Returns: 0=pass, 1=fail, 2+=error/fallthrough
```

But the fail-open path `return 0` when `ic` is unavailable is not an error or a
fallthrough — it's a deliberate policy decision. The comment should be
`# Returns: 0=pass or ic-unavailable, 1=fail` to avoid confusion for future
maintainers who wonder why a missing binary returns 0.

---

## 8. File Organization

**Status: Compliant**

- `internal/phase/gate.go` (new) is the right home for gate types and the rule
  table. The phase package already owns `machine.go`, `store.go`, `phase.go`,
  `errors.go` — adding `gate.go` fits the existing "one concern per file" style.
- `cmd/ic/gate.go` (new) matches `cmd/ic/run.go`, `cmd/ic/lock.go`.
- `internal/phase/gate_test.go` (new) matches `internal/phase/machine_test.go`.

No concerns here.

---

## Summary Table

| Area | Status | Severity |
|------|--------|----------|
| Naming conventions | Compliant | — |
| Error wrapping in `EvaluateGate`/`HasVerdict` | Fix required | Medium |
| DB errors swallowed in gate evaluation loop | Design decision needed | Medium |
| `GateCondition.Check` string-typed with no default branch | Fix required | Low |
| Test: DB error path unspecified | Add test | Medium |
| Test: soft fail evidence in audit trail | Add test | Low |
| `advanceToPhase` overshoot guard | Fix required | Low |
| `setupMachineTest` returning raw `*sql.DB` | Use `runtrack.Store` instead | Low |
| Reason field format (JSON + skip reason concat) | Reconsider | Low |
| CLI override event field spec | Clarify in plan | Low |
| Bash wrapper comment | Cosmetic | Cosmetic |
