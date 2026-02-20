# Correctness Review: Gate System

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-18
**Diff:** /tmp/qg-diff-1771469961.txt
**Files reviewed:**
- `internal/phase/gate.go` (NEW)
- `internal/phase/machine.go`
- `internal/runtrack/store.go`
- `internal/dispatch/dispatch.go`
- `cmd/ic/gate.go` (NEW)
- `internal/phase/gate_test.go` (NEW)

---

## Invariants Established Before Review

The following invariants must hold for the gate system to be correct:

1. **Phase monotonicity:** A run's phase field only moves forward (brainstorm → ... → done). No rollbacks.
2. **Optimistic concurrency:** `UpdatePhase(ctx, id, expectedPhase, newPhase)` must return `ErrStalePhase` if another writer already advanced the phase. Double-advance must be impossible.
3. **Gate atomicity:** If a gate blocks, no `UpdatePhase` executes and the run phase is unchanged.
4. **Audit completeness:** Every `UpdatePhase` is paired with an `AddEvent` (either before or after, depending on crash-safety design). Missing events are explicitly a degraded-but-acceptable failure mode (R3 decision for override).
5. **Evidence accuracy:** Gate evidence (conditions, counts, results) stored in `phase_events.reason` accurately reflects the DB state at evaluation time.
6. **Verdict gate correctness:** `verdict_exists` check requires a terminal, non-rejected dispatch verdict tied to the run's scope.
7. **Skip-transition bypass:** Complexity-based skip transitions have no intermediate gate requirements (explicit design decision).

---

## Architecture Context

**Single-process, single-writer SQLite with WAL mode and `SetMaxOpenConns(1)`.**

This is the central fact that governs most concurrency analysis. With a single connection pool slot, all SQLite operations are serialized through one connection. WAL mode allows concurrent reads from other processes but `SetMaxOpenConns(1)` prevents this process from issuing overlapping writes. `ic` is a short-lived CLI process — it opens the DB, does its work, and exits. Concurrent `ic` invocations from hooks are possible but the busy_timeout provides backpressure.

The gate evaluation sequence is:
```
Get(runID)                     → SELECT from runs
evaluateGate(...)              → SELECT COUNT from run_artifacts / run_agents / dispatches
UpdatePhase(id, from, to)      → UPDATE runs WHERE phase = expectedPhase
AddEvent(...)                  → INSERT into phase_events
[if done] UpdateStatus(...)    → UPDATE runs SET status = 'completed'
```

None of these are wrapped in an explicit transaction. Each is an independent auto-committed statement.

---

## Findings

### C-01 (LOW): Read-evaluate-write without transaction — TOCTOU window

**What it is:** The `Advance()` function reads run state, evaluates gate conditions, then writes the new phase as separate auto-committed statements. There is no `BEGIN IMMEDIATE` transaction holding a read lock across the evaluation window.

**Failure narrative:**

Two concurrent `ic run advance` invocations for the same run:

```
Process A: Get(run=X) → phase=brainstorm, AutoAdvance=true
Process B: AddArtifact(run=X, phase=brainstorm) → committed
Process A: CountArtifacts(run=X, phase=brainstorm) → returns 0 (sees pre-B state in snapshot)
Process B: exits
Process A: gate=FAIL → records EventBlock, does NOT call UpdatePhase
```

Result: Gate blocks spuriously because Process A's read snapshot predates Process B's artifact write. The run is stuck at brainstorm with a block event, despite the artifact being present in the DB.

The inverse race: artifact is removed between check and evaluation, causing a spurious pass. Less likely in this system since artifacts are not deleteable via CLI.

**Mitigating factors:**
- `SetMaxOpenConns(1)` serializes writes within one process, not across processes.
- WAL mode: readers get a consistent snapshot at statement start, not read-committed per-statement. Within a single `SELECT COUNT(*)`, the read is consistent. The vulnerability is between separate SELECTs.
- `ic` is CLI-invoked and hooks use sequential sentinel-guarded calls in most patterns.
- The consequence is a false block (not a false advance), which is the safer failure direction.
- `ErrStalePhase` from `UpdatePhase` prevents double-advance if two processes both pass the gate evaluation.

**Risk level:** Low for declared single-process deployment. Becomes medium if hooks ever invoke `ic run advance` and `ic run artifact add` truly concurrently.

**Fix:** Wrap `Get → evaluateGate → UpdatePhase → AddEvent` in `BEGIN IMMEDIATE` if concurrent invocations become a requirement. For now, document the single-invocation assumption in `machine.go`.

Evidence: `internal/phase/machine.go` lines 39–169.

---

### C-02 (LOW): AddEvent and UpdateStatus errors silently discarded in cmdGateOverride

**Location:** `cmd/ic/gate.go` lines 187–199.

**The code:**

```go
// R3: UpdatePhase first, then record event.
if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
    fmt.Fprintf(os.Stderr, "ic: gate override: %v\n", err)
    return 2
}

store.AddEvent(ctx, &phase.PhaseEvent{   // error return discarded
    RunID:      runID,
    ...
})

if toPhase == phase.PhaseDone {
    store.UpdateStatus(ctx, runID, phase.StatusCompleted)   // error return discarded
}
```

**What it is:** After the critical `UpdatePhase` succeeds, `AddEvent` and `UpdateStatus` are called without capturing their error returns. The R3 rationale (advance-before-audit) is correct: if the process crashes between `UpdatePhase` and `AddEvent`, the phase advanced without an event record — that is the explicit design choice, and it is sound. The issue is that if `AddEvent` fails for a recoverable reason (context deadline from parent shell timeout, momentary disk full), the caller receives:
- exit code 0
- success output to stdout
- no indication that the audit event was dropped

A run advanced to `done` but with `status=active` (failed `UpdateStatus`) will confuse `ic run list --active` forever.

**Contrast with `Advance()` in `machine.go`:** Both `AddEvent` (line 150) and `UpdateStatus` (line 156) return errors that are propagated to the caller. The CLI override path is inconsistent.

**Failure scenario:** Shell context timeout kills the DB write for `AddEvent`. The override appears to succeed. The audit trail has no override event. Operators reviewing `ic run events` see an unexplained phase jump with no reason recorded.

**Fix:**

```go
if err := store.AddEvent(ctx, &phase.PhaseEvent{...}); err != nil {
    fmt.Fprintf(os.Stderr, "ic: gate override: warning: audit event not recorded: %v\n", err)
    // Do NOT return non-zero — the advance happened and is correct.
}
if toPhase == phase.PhaseDone {
    if err := store.UpdateStatus(ctx, runID, phase.StatusCompleted); err != nil {
        fmt.Fprintf(os.Stderr, "ic: gate override: warning: run status not updated: %v\n", err)
    }
}
```

Evidence: `cmd/ic/gate.go` lines 187–199; compare `internal/phase/machine.go` lines 142–158.

---

### C-03 (LOW): HasVerdict matches non-terminal dispatches

**Location:** `internal/dispatch/dispatch.go` lines 247–260.

**The query:**

```sql
SELECT COUNT(*) FROM dispatches
WHERE scope_id = ? AND verdict_status IS NOT NULL AND verdict_status != 'reject'
```

**What it is:** This query does not restrict to terminal dispatch statuses (`completed`, `failed`). In the normal lifecycle, `verdict_status` is written at collection time (terminal state). However:

1. `UpdateStatus` accepts arbitrary `UpdateFields` including `verdict_status`. A caller could write `verdict_status` to a running dispatch.
2. Dispatches from a prior sprint/run in the same `scope_id` that had a passing verdict and were never pruned will satisfy the gate for a new run in the same scope.
3. A failed dispatch (`status=failed`) may have `verdict_status` set to a non-reject value if it partially completed before failure. This would incorrectly satisfy the gate.

**Failure scenario:** Scope "sprint-42" ran two weeks ago, had an approved verdict, was never pruned. A new run is created with `scope_id=sprint-42`. When the new run reaches `review → polish`, the gate passes immediately because the old dispatch's verdict is still in the DB. The operator never knows the review gate was satisfied by stale data.

**Fix:** Add status restriction:

```sql
SELECT COUNT(*) FROM dispatches
WHERE scope_id = ?
  AND status IN ('completed', 'failed')
  AND verdict_status IS NOT NULL
  AND verdict_status != 'reject'
```

Whether to include `failed` dispatches is a product decision (some failed dispatches may have partial verdicts worth honoring). A conservative default would restrict to `status = 'completed'` only.

Note also: the existing `idx_dispatches_scope` index is a partial index on `scope_id IS NOT NULL`, which will be used for this query. Adding `status` to the index filter would improve performance for large deployment scopes.

Evidence: `internal/dispatch/dispatch.go` lines 247–260; `internal/db/schema.sql` line 53.

---

### C-04 (INFO): Nil rt or vq with Priority <= 3 causes nil pointer dereference panic

**Location:** `internal/phase/gate.go` lines 129–172.

**The doc comment:** `// rt and vq may be nil when Priority >= 4 (TierNone skips gate evaluation).`

This is accurate but only half the contract. At `Priority <= 3`, evaluateGate will dereference both `rt` and `vq` if the transition has rules requiring them. No nil guards exist:

```go
case CheckArtifactExists:
    count, qerr := rt.CountArtifacts(ctx, run.ID, rule.phase)   // PANIC if rt == nil
```

**When can this happen?**

Current callers:
- `cmd/ic/run.go`: passes real stores unconditionally.
- `cmd/ic/gate.go`: passes real stores unconditionally.
- Tests with `Priority=4` bypass.
- `TestGate_NoRulesForTransition`: passes `rtStore` non-nil and `nil` for vq. This is safe *only* because `polish → done` has no rules. If the test were run for `review → polish` with `vq=nil`, it would panic.

The nil-vq case is the most likely future mistake: a caller wants to check artifact conditions for most transitions and assumes `vq=nil` is safe if they never have a `verdict_exists` rule in the path. For transitions without a `verdict_exists` rule, `vq` is never called — this is currently safe by coincidence of the rule table. If a future rule is added that requires `vq` for a transition a caller thought was safe, the panic surfaces.

**Fix:**

```go
case CheckArtifactExists:
    if rt == nil {
        return "", "", nil, fmt.Errorf("gate check: RuntrackQuerier is nil (required for %s)", rule.check)
    }
    count, qerr := rt.CountArtifacts(ctx, run.ID, rule.phase)
    // ...

case CheckVerdictExists:
    if vq == nil {
        return "", "", nil, fmt.Errorf("gate check: VerdictQuerier is nil (required for %s)", rule.check)
    }
    // ...
```

Evidence: `internal/phase/gate.go` lines 128–172.

---

### C-05 (INFO): advanceToPhase test helper has unbounded loop

**Location:** `internal/phase/machine_test.go` lines 335–357.

The helper loops until `run.Phase == target`, with checks for terminal overshot. If `Advance` returns `Advanced=true` but the DB state does not reflect it (hypothetically: the `UpdatePhase` succeeded but `store.Get` returns stale data), the loop would spin forever, hanging `go test` with no timeout.

In practice with a real in-process SQLite, `Get` always sees the committed state. The risk is negligible but the pattern is bad hygiene for a test helper.

**Fix:** Add `if i >= len(allPhases)+1 { t.Fatalf(...) }` guard.

Evidence: `internal/phase/machine_test.go` lines 335–357.

---

### C-06 (INFO): EvaluateGate double-wraps ErrNotFound — equality check fails in cmdGateCheck

**Location:** `cmd/ic/gate.go` lines 74–85; `internal/phase/gate.go` line 194.

`EvaluateGate` wraps the store error:

```go
return nil, fmt.Errorf("evaluate gate: %w", err)  // wraps ErrNotFound
```

`cmdGateCheck` checks with `==`:

```go
if err == phase.ErrNotFound {
    // returns exit code 1 (expected-negative)
}
```

Because the wrapped error is `"evaluate gate: run not found"` and not `phase.ErrNotFound` itself, the equality test fails. The code falls through to the generic `return 2` (unexpected error), producing the wrong exit code and a confusing error message.

The same bug applies to the `ErrTerminalRun` and `ErrTerminalPhase` checks on the same line.

**Fix:** Use `errors.Is`:

```go
if errors.Is(err, phase.ErrNotFound) {
```

Evidence: `cmd/ic/gate.go` lines 74–85.

---

### C-07 (INFO, by design): Complexity-skip transitions bypass all gate checks

**Location:** `internal/phase/gate.go` lines 73–94.

This is documented as intentional. When `NextRequiredPhase` returns a phase two or more steps ahead (e.g., brainstorm→planned for complexity=1), `evaluateGate` looks up `gateRules[[2]string{"brainstorm", "planned"}]` which does not exist in the table, so it returns `GatePass` with no conditions checked.

Consequence: A complexity-1 run has zero artifact gate enforcement. Any operator relying on `ic gate check` to confirm brainstorm readiness on a complexity-1 run will see `GatePass` with no evidence, which is technically correct but potentially misleading.

No code fix required. **Documentation action:** Add a note in gate.go or AGENTS.md that skip transitions bypass intermediate gates, and that operators on complexity-1/2 runs should verify artifacts manually if quality assurance is required.

Evidence: `internal/phase/gate.go` lines 73–94; `internal/phase/phase.go` `NextRequiredPhase` lines 150–170.

---

## Positive Findings

These aspects are correctly implemented and provide meaningful correctness guarantees:

1. **Optimistic concurrency via `WHERE phase = ?` is correct.** Two concurrent advances will produce exactly one `ErrStalePhase` — the second writer loses cleanly with no data corruption.

2. **`nil` `vq` is safe for all current non-verdict transitions.** The interface nil issue (C-04) only manifests if a `verdict_exists` rule exists for the transition being evaluated. Current test coverage of `TestGate_NoRulesForTransition` demonstrates this is empirically safe for `polish → done`.

3. **`HasVerdict` empty-scopeID guard is correct.** Returning `false, nil` when `scopeID == ""` means runs without scope linkage cannot accidentally pass the verdict gate, forcing an explicit override.

4. **Override crash-safety ordering (UpdatePhase before AddEvent) is correct per R3.** An advance without an audit event is recoverable (the event is just missing). An audit event without the corresponding advance would be actively misleading (the event says the run advanced but it did not).

5. **`ErrStalePhase` detection in `UpdatePhase` uses a secondary Get to distinguish not-found from stale.** This is the correct approach — checking rows-affected=0 alone cannot tell you whether the run doesn't exist or has been concurrently advanced.

6. **Gate evidence is always recorded for both pass and block events.** The structured JSON in `phase_events.reason` provides full auditability for post-hoc analysis.

7. **Tests cover the key failure modes.** `TestGate_ArtifactExists_Fail` verifies the phase is NOT changed on a hard gate block. `TestGate_ArtifactExists_SoftFail` verifies soft gates advance despite condition failure. `TestGate_VerdictExists_NoScopeID_Fails` verifies the empty-scope-id guard.

---

## Summary

The gate system is structurally correct for its declared deployment model (single-process CLI, `SetMaxOpenConns(1)`, sequential hook invocations). The optimistic concurrency mechanism is solid. Three items need attention before production: (C-02) AddEvent/UpdateStatus errors must emit warnings in cmdGateOverride rather than being swallowed; (C-03) HasVerdict should restrict to terminal-status dispatches to prevent stale scope data from satisfying the verdict gate; (C-06) errors.Is must replace == for sentinel error checks in cmdGateCheck. C-04 (nil interface panic) is a latent API contract trap that should be closed with nil guards before the interface gets more callers.

---

*Full findings also written to: /root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md*
