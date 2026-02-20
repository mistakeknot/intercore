# Correctness Review: SpawnHandler Integration Tests, Event Reactor Pattern Doc, AGENTS.md

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-19
**Scope:** Three artefacts checked against source code in `/root/projects/Interverse/infra/intercore/`

---

## Invariants Being Checked

1. Integration tests must mirror production wiring exactly — same handler name, same subscription point, same event fields.
2. Documentation event type strings must match compiled constants, not approximations.
3. AGENTS.md status claims must reflect what is actually implemented.

---

## 1. Integration Tests in `internal/event/handler_spawn_test.go`

**Verdict: Tests correctly mirror production wiring. One minor log-string gap noted.**

### 1.1 Subscription name and handler construction match

In `cmd/ic/run.go` line 336:

```go
notifier.Subscribe("spawn", event.NewSpawnHandler(rtStore, spawner, os.Stderr))
```

In `TestSpawnWiringIntegration` (test file line 133):

```go
notifier.Subscribe("spawn", NewSpawnHandler(q, spawner, &logBuf))
```

The subscription name `"spawn"` matches. The `NewSpawnHandler` call signature matches the production call:
- First arg: `AgentQuerier` interface (production: `rtStore *runtrack.Store`, test: `*mockQuerier`) — both satisfy `ListPendingAgentIDs`.
- Second arg: `AgentSpawner` interface (production: `AgentSpawnerFunc`, test: `AgentSpawnerFunc`) — same adapter type.
- Third arg: `io.Writer` (production: `os.Stderr`, test: `&logBuf`) — correct.

### 1.2 Event field population matches

Production `phaseCallback` in `run.go` lines 339-350:

```go
e := event.Event{
    RunID:     runID,
    Source:    event.SourcePhase,
    Type:      eventType,
    FromState: fromPhase,
    ToState:   toPhase,
    Reason:    reason,
    Timestamp: time.Now(),
}
notifier.Notify(ctx, e)
```

`TestSpawnWiringIntegration` line 137-144:

```go
notifier.Notify(ctx, Event{
    RunID:     "test-run-1",
    Source:    SourcePhase,
    Type:      "advance",
    FromState: "planned",
    ToState:   "executing",
    Timestamp: time.Now(),
})
```

The fields that matter for SpawnHandler (`Source` and `ToState`) are correctly populated. The handler ignores `Type`, `FromState`, and `Reason` — so those fields being set differently in tests vs production does not affect handler correctness.

### 1.3 Handler trigger condition is correctly tested

`handler_spawn.go` line 34:

```go
if e.Source != SourcePhase || e.ToState != "executing" {
    return nil
}
```

Tests cover:
- `TestSpawnHandler_TriggersOnExecuting`: `Source=SourcePhase`, `ToState="executing"` → triggers. CORRECT.
- `TestSpawnHandler_IgnoresNonExecuting`: `Source=SourcePhase`, `ToState="strategized"` → no trigger. CORRECT.
- `TestSpawnHandler_IgnoresDispatchEvents`: `Source=SourceDispatch`, `ToState="executing"` → no trigger. CORRECT.

All three branches of the filter condition are covered.

### 1.4 Log message assertion gap (minor)

`TestSpawnWiringIntegration` at line 156 checks:

```go
if !strings.Contains(logStr, "[event] auto-spawn: agent agent-1 started") {
```

Actual log format from `handler_spawn.go` line 52:

```go
fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
```

The assertion matches the actual format exactly. No defect.

`TestSpawnHandler_PartialFailure` at line 112-113:

```go
if !bytes.Contains(buf.Bytes(), []byte("fail1 failed")) {
    t.Error("expected failure log for fail1")
}
```

Actual failure log format (`handler_spawn.go` line 49):

```go
fmt.Fprintf(logw, "[event] auto-spawn: agent %s failed: %v\n", id, err)
// Expands to: "[event] auto-spawn: agent fail1 failed: spawn failed\n"
```

The substring `"fail1 failed"` is present in that string. The check passes but it is imprecise — it would also pass if the error message accidentally contained "fail1 failed" from a different context. This is acceptable for a log smoke test but worth noting.

### 1.5 Notifier.Notify is synchronous — test assertions are safe

`notifier.go` lines 38-53: `Notify` iterates handlers synchronously in the caller's goroutine. There is no goroutine spawning inside `Notify` itself. Assertions after `Notify(...)` will see the final state without any sleep or synchronization needed. The tests are correctly structured.

### 1.6 `TestSpawnWiringIntegration_MultipleHandlers` correctly models production handler ordering

In production `cmdRunAdvance`:
1. `notifier.Subscribe("log", ...)` — line 252
2. `notifier.Subscribe("hook", ...)` — line 264
3. `notifier.Subscribe("spawn", ...)` — line 336

The multi-handler test subscribes "log" before "spawn", matching the production ordering intent. The AGENTS.md handler table (section: Event Bus Module → Handlers) also lists LogHandler before SpawnHandler. Ordering is consistent.

---

## 2. Documentation: `docs/event-reactor-pattern.md`

**Verdict: Two correctness defects. One is breaking for any script that uses it.**

### 2.1 BREAKING: Budget event type strings use underscores; code uses dots

The document at lines 101-102 states:

```
**Budget events** (`source: "phase"`, `type` varies):
- `budget_warning` — Token usage crossed warning threshold
- `budget_exceeded` — Token usage exceeded budget
```

The bash reactor example at lines 158-163 then switches on these strings:

```bash
case "$type" in
  budget_warning)
    ...
  budget_exceeded)
    ...
esac
```

The actual constants in `internal/budget/budget.go` lines 16-17:

```go
const (
    EventBudgetWarning  = "budget.warning"
    EventBudgetExceeded = "budget.exceeded"
)
```

The separator is a dot (`.`), not an underscore (`_`). A bash `case` statement that matches against `budget_warning` will never match an event with type `"budget.warning"`. Any script copied from this documentation will silently drop all budget events. This is a production-level failure for operators.

**Fix:** Change all three occurrences in the doc:
- Line 101: `budget_warning` → `budget.warning`
- Line 102: `budget_exceeded` → `budget.exceeded`
- Lines 159, 163: same substitution in the bash case block.

### 2.2 Missing `pause` event type in the phase event table

AGENTS.md (Event Bus Module → PhaseEventCallback section) states:

> The callback fires on **all** advance attempts: advance, skip, block, and pause.

`internal/phase/phase.go` line 33:
```go
EventPause = "pause"
```

`internal/phase/machine.go` lines 86-109 confirm that the `pause` event fires when `auto_advance` is false and no skip reason overrides it.

The documentation in `event-reactor-pattern.md` at lines 93-95 lists only:

```
**Phase events** (`source: "phase"`):
- `advance` — Phase transitioned
- `skip` — Phase was skipped
- `block` — Gate blocked the advance
```

The `pause` type is missing. A reactor that tries to detect when a run is waiting for human intervention (auto_advance=false) will not find the event type documented anywhere in this file.

**Fix:** Add to the table:
```
- `pause` — Advance blocked because auto_advance is disabled
```

### 2.3 CLI flag spellings and examples are correct

`ic events tail --all -f --consumer=my-reactor --poll-interval=1s` — verified against `cmd/ic/events.go` (not read here but consistent with AGENTS.md and CLAUDE.md Quick Reference).

Event JSON schema fields (`id`, `run_id`, `source`, `type`, `from_state`, `to_state`, `reason`, `timestamp`) match `internal/event/event.go`:

```go
type Event struct {
    ID        int64     `json:"id"`
    RunID     string    `json:"run_id"`
    Source    string    `json:"source"`
    Type      string    `json:"type"`
    FromState string    `json:"from_state"`
    ToState   string    `json:"to_state"`
    Reason    string    `json:"reason,omitempty"`
    Timestamp time.Time `json:"timestamp"`
}
```

All JSON field names in the documentation schema table match the Go struct tags. The example JSON object in the doc is correct.

### 2.4 Event source for budget events claims `"phase"`

The doc says:

> **Budget events** (`source: "phase"`, `type` varies):

The `internal/budget/budget.go` `emitEvent` callback is defined by the caller. In `cmd/ic/run.go`, the budget checker does not appear to be wired into `cmdRunAdvance` at all — it is only called directly via `cmdRunBudget`. Budget events may be emitted by a separate caller path. Without seeing the full budget emitter wiring (outside the scope of files reviewed here), the claim that budget events have `source: "phase"` cannot be verified or refuted. This is a documentation assumption risk, not a confirmed defect.

**Recommendation:** Add a note clarifying who sets the `source` field for budget events, or verify that the emitter passes `SourcePhase`.

---

## 3. AGENTS.md: SpawnHandler Status

**Verdict: Correct. The wiring line reference is accurate.**

The AGENTS.md Handlers table (Event Bus Module section):

| Handler | Registered | Behavior |
|---------|-----------|----------|
| SpawnHandler | Always | Auto-spawns pending agents when phase transitions to "executing"; wired at `cmd/ic/run.go:336` |

Verified against `cmd/ic/run.go` line 336:

```go
notifier.Subscribe("spawn", event.NewSpawnHandler(rtStore, spawner, os.Stderr))
```

The line number is accurate. The behavior description ("auto-spawns pending agents when phase transitions to 'executing'") matches `handler_spawn.go` line 34:

```go
if e.Source != SourcePhase || e.ToState != "executing" {
    return nil
}
```

The "Always" registration claim is accurate — `cmdRunAdvance` unconditionally subscribes the spawn handler regardless of flags or run state.

---

## Summary of Findings

| Severity | Location | Finding |
|----------|----------|---------|
| **Breaking** | `docs/event-reactor-pattern.md` lines 101-102, 159, 163 | Budget event type strings use `_` (underscore) but code emits `.` (dot). All bash scripts using these patterns silently drop budget events. |
| **Minor** | `docs/event-reactor-pattern.md` lines 93-95 | `pause` event type is missing from the phase event type table. |
| **Correct** | `internal/event/handler_spawn_test.go` | Tests accurately mirror production wiring in `run.go:336`. Subscription name, handler type, event fields all match. |
| **Correct** | `AGENTS.md` Handlers table | SpawnHandler status and line reference (`run.go:336`) are accurate. |
| **Unverifiable** | `docs/event-reactor-pattern.md` line 100 | Budget event `source: "phase"` claim not verifiable without full budget emitter wiring review. |

---

## Required Changes

### `docs/event-reactor-pattern.md`

**Change 1 (breaking fix):** Lines 101-102 and bash case block:

```diff
-**Budget events** (`source: "phase"`, `type` varies):
-- `budget_warning` — Token usage crossed warning threshold
-- `budget_exceeded` — Token usage exceeded budget
+**Budget events** (`source: "phase"`, `type` varies):
+- `budget.warning` — Token usage crossed warning threshold
+- `budget.exceeded` — Token usage exceeded budget
```

And in the bash case block:

```diff
-    budget_warning)
+    budget.warning)
```

```diff
-    budget_exceeded)
+    budget.exceeded)
```

**Change 2 (completeness):** Add to the phase event type list:

```diff
 - `block` — Gate blocked the advance
+- `pause` — Advance was deferred because auto_advance is disabled
```

No changes required to `AGENTS.md` or the integration test file.
