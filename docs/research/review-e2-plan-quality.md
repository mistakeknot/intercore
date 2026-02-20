# Quality Review: Intercore E2 Event Reactor — Implementation Plan

**Reviewed:** `/root/projects/Interverse/docs/plans/2026-02-19-intercore-e2-event-reactor.md`
**Context files:**
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn_test.go`
- `/root/projects/Interverse/infra/intercore/internal/event/notifier.go`
- `/root/projects/Interverse/infra/intercore/internal/event/event.go`
- `/root/projects/Interverse/infra/intercore/internal/event/notifier_test.go`

---

## Summary

The plan is low-risk and the proposed tests are mostly sound, but they contain three concrete correctness problems and one redundancy that will produce confusion or test noise. These are enumerated below with precise fixes.

---

## Issue 1 — BLOCKING: Wrong mock type name (`mockQuerier.ids` vs `mockQuerier.agents`)

**Severity:** Blocking — the proposed tests will not compile against the existing mock.

**Plan code (Task 2, Step 1):**
```go
querier := &mockQuerier{ids: []string{"agent-1", "agent-2"}}
```

**Existing mock in `handler_spawn_test.go`:**
```go
type mockQuerier struct {
    agents []string
    err    error
}
```

The field is named `agents`, not `ids`. The plan introduces a struct literal with an undefined field. This is a compile-time error.

**Fix:** Use the existing field name.
```go
querier := &mockQuerier{agents: []string{"agent-1", "agent-2"}}
```

No new struct type or field should be introduced. The existing `mockQuerier` covers the need exactly.

---

## Issue 2 — BLOCKING: Plan introduces `AgentSpawnerFunc` but uses wrong interface for mock

**Severity:** Blocking — type mismatch at compile time.

**Plan code (Task 2, Step 1):**
```go
var spawned []string
spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
    spawned = append(spawned, agentID)
    return nil
})
```

`AgentSpawnerFunc` is a valid type defined in `handler_spawn.go` and satisfies `AgentSpawner`. This part is correct in isolation.

However, this test is in package `event` (same package as the source). The existing test suite uses the `mockSpawner` struct, not `AgentSpawnerFunc`. Using `AgentSpawnerFunc` here is not wrong, but it splits the test suite into two styles for no reason: some tests use `mockSpawner` (which supports `failIDs` for partial failure testing), others use a closure.

**Recommendation:** For this wiring test, use `mockSpawner` to stay consistent with the existing test style:
```go
s := &mockSpawner{failIDs: map[string]bool{}}
```

Then assert `s.spawned` directly. This is simpler, avoids a new local variable, and matches the file's established pattern.

If `AgentSpawnerFunc` is used (it is a valid functional option), that is acceptable — but the plan should not mix both in the same file without explanation.

---

## Issue 3 — CORRECTNESS: Wrong event `Type` value in proposed tests

**Severity:** Medium — tests pass today by accident but will fail if the handler ever adds type filtering.

**Plan code:**
```go
notifier.Notify(ctx, Event{
    ...
    Type:      "phase_advance",
    ...
})
```

**Actual `Event.Type` values used in the existing tests and handler:**
```go
// handler_spawn_test.go (existing, correct):
e := Event{Source: SourcePhase, Type: "advance", ...}

// handler_spawn.go — handler only checks Source and ToState, NOT Type:
if e.Source != SourcePhase || e.ToState != "executing" {
    return nil
}
```

The handler does not filter on `Type` today, so `"phase_advance"` passes. But the rest of the test suite uses `"advance"` consistently. The documentation in Task 1 uses `"phase_advance"` as the JSON schema field name for an event kind — that is the correct external schema label. Internally the `Type` field is stored as `"advance"`, `"skip"`, `"status_change"`.

**Fix:** Use `Type: "advance"` in the new tests to match the existing test suite and the actual values written to the store. Leaving `"phase_advance"` is not a compile error and not a test failure today, but it is misleading and inconsistent.

---

## Issue 4 — REDUNDANCY: `TestSpawnWiringIntegration_NonExecutingPhase` duplicates existing coverage

**Severity:** Low — not harmful, but adds noise without adding value.

The existing `handler_spawn_test.go` already has `TestSpawnHandler_IgnoresNonExecuting`:
```go
func TestSpawnHandler_IgnoresNonExecuting(t *testing.T) {
    q := &mockQuerier{agents: []string{"agent1"}}
    s := &mockSpawner{failIDs: map[string]bool{}}
    h := NewSpawnHandler(q, s, nil)
    e := Event{Source: SourcePhase, Type: "advance", ToState: "strategized"}
    h(context.Background(), e)
    if len(s.spawned) != 0 {
        t.Errorf("should not spawn for non-executing phase, spawned %d", len(s.spawned))
    }
}
```

The proposed `TestSpawnWiringIntegration_NonExecutingPhase` exercises the same logic path, just with the `Notifier` wrapper. The plan's rationale is testing the wiring, but since `TestSpawnWiringIntegration` already validates that the happy path wiring works, the negative wiring test is redundant.

**Recommendation:** Drop `TestSpawnWiringIntegration_NonExecutingPhase`. If wiring for non-executing phases is explicitly a risk worth testing (it is not — the handler is a closure and the filter is unconditional), one sentence in a comment on the positive test is sufficient.

---

## Issue 5 — STYLE: Log assertion checks for log message that does not match actual output format

**Plan assertion:**
```go
if !strings.Contains(logBuf.String(), "auto-spawn: agent agent-1 started") {
    t.Errorf("missing log for agent-1: %s", logBuf.String())
}
```

**Actual log line in `handler_spawn.go`:**
```go
fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
```

The actual log output is `[event] auto-spawn: agent agent-1 started`. The plan checks for `"auto-spawn: agent agent-1 started"` which is a substring of the actual string, so the assertion passes. However, the `[event]` prefix is omitted from the expected string, which hides it.

**Recommendation:** Use the full prefix in the assertion to be explicit:
```go
if !strings.Contains(logBuf.String(), "[event] auto-spawn: agent agent-1 started") {
```

This is a minor style point but matters for test readability and robustness against future prefix changes.

---

## Issue 6 — STYLE: Agent IDs use hyphens in plan but underscores in existing tests

**Plan:**
```go
querier := &mockQuerier{agents: []string{"agent-1", "agent-2"}}
```

**Existing tests:**
```go
q := &mockQuerier{agents: []string{"agent1", "agent2"}}
```

This is not a bug — both are valid strings. But the inconsistency in test fixtures makes the files harder to scan. The existing convention uses no separator. The new tests should follow suit: `"agent1"`, `"agent2"`.

---

## What the Plan Does Well

- The overall test strategy is correct: test the notifier wiring end-to-end rather than repeating handler unit tests. This is the right level of integration for Task 2.
- Using `AgentSpawnerFunc` is a valid demonstration of the functional adapter pattern even if `mockSpawner` is also available.
- The plan correctly avoids touching kernel code and scopes the change to documentation + tests.
- The existing mock types (`mockQuerier`, `mockSpawner`) are well-structured with explicit `failIDs` support — the plan is right to reuse them rather than introducing new abstractions.
- `go test -race ./internal/event/...` is implied by the project's test conventions (`CLAUDE.md` calls out `-race` explicitly) — the plan should add this flag to the Task 4 command.

---

## Corrected Test Code for Task 2

```go
// TestSpawnWiringIntegration verifies that NewSpawnHandler registered via
// Notifier.Subscribe triggers spawns when a phase advances to "executing".
func TestSpawnWiringIntegration(t *testing.T) {
    notifier := NewNotifier()

    q := &mockQuerier{agents: []string{"agent1", "agent2"}}
    s := &mockSpawner{failIDs: map[string]bool{}}
    var logBuf bytes.Buffer
    notifier.Subscribe("spawn", NewSpawnHandler(q, s, &logBuf))

    err := notifier.Notify(context.Background(), Event{
        RunID:     "test-run-1",
        Source:    SourcePhase,
        Type:      "advance",
        FromState: "planned",
        ToState:   "executing",
        Timestamp: time.Now(),
    })
    if err != nil {
        t.Fatalf("Notify returned unexpected error: %v", err)
    }
    if len(s.spawned) != 2 {
        t.Fatalf("spawned %d agents, want 2", len(s.spawned))
    }
    if s.spawned[0] != "agent1" || s.spawned[1] != "agent2" {
        t.Errorf("spawned = %v, want [agent1, agent2]", s.spawned)
    }
    if !strings.Contains(logBuf.String(), "[event] auto-spawn: agent agent1 started") {
        t.Errorf("missing log for agent1; got: %s", logBuf.String())
    }
}
```

Drop `TestSpawnWiringIntegration_NonExecutingPhase` entirely.

Add `"strings"` to the import block (it is not currently imported in `handler_spawn_test.go`).

---

## Task 4 Command Fix

Change:
```bash
cd infra/intercore && go test ./...
```

To:
```bash
cd infra/intercore && go test -race ./...
```

The `-race` flag is called out in `CLAUDE.md` as the expected invocation for concurrent code. The `Notifier` uses `sync.RWMutex` — race detection is relevant.

---

## Summary Table

| Issue | Severity | Blocks Compilation | Blocks Test Pass |
|---|---|---|---|
| `ids` vs `agents` field name | BLOCKING | Yes | Yes |
| Type value `"phase_advance"` vs `"advance"` | Medium | No | No (today) |
| Redundant negative wiring test | Low | No | No |
| Log assertion missing `[event]` prefix | Low | No | No |
| Agent ID format inconsistency | Low | No | No |
| Missing `-race` in Task 4 | Low | No | No |
