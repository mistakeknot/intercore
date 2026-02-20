# Correctness Review: E2 Event Reactor Completion Plan

**Plan file:** `docs/plans/2026-02-19-intercore-e2-event-reactor.md`
**Reviewer:** Julik (Flux-drive Correctness)
**Date:** 2026-02-19
**Bead:** iv-9plh

---

## Invariants Being Verified

1. Test event shape must match exactly what `cmdRunAdvance`/`phaseCallback` produces at runtime.
2. Mock types in the proposed test must match the actual interface signatures declared in `handler_spawn.go`.
3. CLI command syntax in documentation examples must parse correctly against the actual flag-parsing code in `cmd/ic/events.go` and `cmd/ic/dispatch.go`.
4. Consumer cursor behavior descriptions must match the actual implementation in `events.go`.
5. Log message assertions in tests must match the literal strings written by `NewSpawnHandler`.

---

## Finding 1 — CRITICAL: Test mock uses wrong interface method name

**File:** `infra/intercore/internal/event/handler_spawn_test.go` (proposed new test)

**The plan writes:**

```go
spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
    spawned = append(spawned, agentID)
    return nil
})
```

This is correct — `AgentSpawnerFunc` is a real type and adapts a plain function. No issue here.

**BUT** the plan also introduces a `mockQuerier`:

```go
querier := &mockQuerier{ids: []string{"agent-1", "agent-2"}}
```

The existing `mockQuerier` in the file (line 11-18 of the current `handler_spawn_test.go`) uses field name `agents`, not `ids`:

```go
type mockQuerier struct {
    agents []string
    err    error
}

func (m *mockQuerier) ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error) {
    return m.agents, m.err
}
```

The plan's test instantiates `&mockQuerier{ids: []string{"agent-1", "agent-2"}}` — this will **not compile** because the existing struct has no `ids` field. The implementer will either get a compile error or, if they rename the existing mock, break the four existing passing tests that use `&mockQuerier{agents: ...}`.

**Fix:** Change the plan's mock instantiation to use the existing field name:

```go
querier := &mockQuerier{agents: []string{"agent-1", "agent-2"}}
```

The negative test has the same issue:

```go
querier := &mockQuerier{ids: []string{"agent-1"}}
```

Must be:

```go
querier := &mockQuerier{agents: []string{"agent-1"}}
```

---

## Finding 2 — CRITICAL: Log message assertion will never match

**File:** `infra/intercore/internal/event/handler_spawn_test.go` (proposed test, step 6)

The plan asserts:

```go
if !strings.Contains(logBuf.String(), "auto-spawn: agent agent-1 started") {
    t.Errorf("missing log for agent-1: %s", logBuf.String())
}
```

The actual log line written by `NewSpawnHandler` (line 52 of `handler_spawn.go`) is:

```go
fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
```

The log output is `[event] auto-spawn: agent agent-1 started`, not `auto-spawn: agent agent-1 started`. The assertion omits the `[event] ` prefix. This test will always fail at the log-message check, even if spawning worked correctly.

The existing test `TestSpawnHandler_PartialFailure` (line 111) correctly uses `"fail1 failed"` which is present in `"[event] auto-spawn: agent fail1 failed: ..."`. The new test is less careful.

**Fix:**

```go
if !strings.Contains(logBuf.String(), "[event] auto-spawn: agent agent-1 started") {
    t.Errorf("missing log for agent-1: %s", logBuf.String())
}
```

---

## Finding 3 — CRITICAL: Event type field mismatch between test and runtime

**File:** Proposed `TestSpawnWiringIntegration`

The plan fires:

```go
notifier.Notify(ctx, Event{
    RunID:     "test-run-1",
    Source:    SourcePhase,
    Type:      "phase_advance",
    FromState: "planned",
    ToState:   "executing",
    Timestamp: time.Now(),
})
```

But what does `cmdRunAdvance`'s `phaseCallback` actually produce? (lines 338-350 of `run.go`):

```go
phaseCallback := func(runID, eventType, fromPhase, toPhase, reason string) {
    e := event.Event{
        RunID:     runID,
        Source:    event.SourcePhase,
        Type:      eventType,       // comes from phase.Advance result
        FromState: fromPhase,
        ToState:   toPhase,
        Reason:    reason,
        Timestamp: time.Now(),
    }
    notifier.Notify(ctx, e)
}
```

The `eventType` is passed from `phase.Advance`, which sets it from the phase event record. Looking at the existing unit tests that already pass (e.g., `TestSpawnHandler_TriggersOnExecuting` at line 42), the event type used there is `"advance"`, not `"phase_advance"`.

The `NewSpawnHandler` (line 34 of `handler_spawn.go`) only checks:

```go
if e.Source != SourcePhase || e.ToState != "executing" {
    return nil
}
```

The handler **does not inspect `e.Type` at all** — it only checks `Source` and `ToState`. So using `"phase_advance"` as the event type in the test won't cause the test to fail mechanically, but it will fail the stated goal: "mirrors how cmdRunAdvance wires the notifier."

The plan's comment says "simulates Advance callback" but uses a type string (`"phase_advance"`) that does not match what `phase.Advance` actually produces. This is a correctness gap in the documentation intent — the test is misleadingly labeled as an integration wire test but uses a fictional event type.

**Fix:** Use `"advance"` to match what the existing passing unit tests and the phase engine produce, or look up the exact constant from the phase package. Either way, add a comment explaining that the handler ignores Type, so the wire test is validating Source + ToState routing only.

---

## Finding 4 — MEDIUM: Documentation example uses non-existent `--run=` flag for `dispatch list`

**File:** `infra/intercore/docs/event-reactor-pattern.md` (to be created)

The plan's "Dispatch completion" example (Task 1, Pattern b) contains:

```bash
active=$(ic dispatch list --active --run="$run_id" --json | jq 'length')
```

The actual `cmdDispatchList` implementation (lines 157-207 of `dispatch.go`) accepts only two flags:

- `--active` (boolean)
- `--scope=<value>`

There is **no `--run=` flag** on `dispatch list`. Passing `--run="$run_id"` will be silently ignored (it hits the `default:` case in the switch, which does nothing — no error is returned). The query will return all active dispatches system-wide, not just those for the specified run. This will cause the reactor to advance the run phase prematurely when any other run has zero active dispatches.

This is a real behavioral bug in the documented pattern: the condition `active == 0` will fire incorrectly in multi-run environments.

The correct approach depends on what the actual API offers. Since `--scope=` exists (and likely accepts a project dir), the author should either:
- Use `--scope=` if that scopes by project, or
- Acknowledge that per-run dispatch filtering requires a different query path, or
- Note that this is a single-run simplification

**Fix:** Remove `--run="$run_id"` from the example, document the limitation, and if per-run filtering is genuinely needed, track it as a gap in `dispatch list`.

---

## Finding 5 — MEDIUM: Documentation claims cursor persists "after each batch" — verify against implementation

**File:** Task 1, Section 3 (Consumer Registration) and Task 3 (AGENTS.md)

The plan states: "Named consumers get cursor persistence (auto-saved after each batch)."

The actual implementation (lines 143-145 of `events.go`):

```go
if consumer != "" && len(events) > 0 && !encodeErr {
    saveCursor(ctx, stStore, consumer, runID, sincePhase, sinceDispatch)
}
```

The cursor is saved only when:
1. A consumer name is set
2. At least one event was retrieved in this poll cycle (`len(events) > 0`)
3. No encode error occurred (`!encodeErr`)

This is correctly described by the plan. However, the plan does not mention the `encodeErr` guard, which means an encode failure (e.g., stdout pipe closed) silently prevents cursor advancement. This is intentional (the code comment says "skip on encode error to avoid advancing past undelivered events") but the documentation should mention it as part of the at-least-once guarantee. The plan's idempotency section (Section 6) says events use at-least-once delivery but does not connect this to the encode-error behavior.

Not a correctness bug in the plan itself, but a documentation gap that will confuse operators debugging replayed events after pipe failures.

---

## Finding 6 — LOW: AGENTS.md quick reference uses `--follow` flag inconsistently

**File:** Task 3 (AGENTS.md section to be added)

The plan's quick reference shows:

```bash
ic events tail --all -f --consumer=my-reactor --poll-interval=1s
```

The implementation (line 44 of `events.go`) accepts both `--follow` and `-f`:

```go
case args[i] == "--follow" || args[i] == "-f":
```

The `-f` shorthand is correct. But the plan also mentions elsewhere (Task 1, Section 7):

> "Pipe breaks (SIGPIPE) require the consumer to restart the `ic events tail` process"

This is accurate — there is no reconnect logic; the consumer loop exits on encode error after the SIGPIPE. This is fine for documentation.

---

## Finding 7 — LOW: `"without --consumer, events replay from the beginning"` is partially wrong

**File:** Task 1, Section 3 (Consumer Registration)

The plan states: "Without `--consumer`, events replay from the beginning each time."

This is true only if `--since-phase` and `--since-dispatch` are also absent. If those flags are set explicitly, they override the cursor (and there is no cursor to replay from anyway). The statement is technically correct for the described scenario but elides an important nuance: `--since-phase` and `--since-dispatch` are the mechanism for stateless consumers that want to start from a specific position without persisting state.

This is a documentation precision issue, not a runtime correctness bug. Worth tightening to: "Without `--consumer`, each invocation starts from the beginning unless `--since-phase`/`--since-dispatch` are specified."

---

## Finding 8 — LOW: `ic events cursor reset <name>` resets only the global cursor, not per-run cursors

**File:** Task 1, Section 3, and Task 3 quick reference

The cursor key is `consumer` when `runID == ""`, and `consumer + ":" + runID` when a run ID is present (lines 235-238 of `events.go`):

```go
func loadCursor(ctx context.Context, store *state.Store, consumer, scope string) (int64, int64) {
    key := consumer
    if scope != "" {
        key = consumer + ":" + scope
    }
```

`ic events cursor reset <name>` (line 218 of `events.go`) deletes the key `args[0]` directly, which is the consumer name. If the consumer was used with a specific run ID (`--consumer=my-reactor <run-id>`), the key stored is `my-reactor:<run-id>`, not `my-reactor`. The reset command as documented (`ic events cursor reset my-reactor`) will delete the global cursor but **not** any per-run cursors, leaving stale state.

The documentation does not mention this. Users who run per-run-scoped consumers and try to reset them by name will silently fail to reset. The correct invocation for a per-run cursor reset is not surfaced anywhere in the plan.

This is a documentation gap that will cause operator confusion during incident response.

---

## Summary Table

| # | Severity | Area | Issue |
|---|----------|------|-------|
| 1 | CRITICAL | Test (Task 2) | `mockQuerier{ids: ...}` — field name is `agents`, not `ids`. Will not compile. |
| 2 | CRITICAL | Test (Task 2) | Log assertion omits `[event] ` prefix. Will always fail. |
| 3 | CRITICAL | Test (Task 2) | Event type `"phase_advance"` does not match what `phaseCallback` produces (`"advance"`). Misleadingly labeled as a wire test. |
| 4 | MEDIUM | Docs (Task 1) | `dispatch list --run=` flag does not exist. Silent wrong behavior in multi-run environments. |
| 5 | MEDIUM | Docs (Task 1) | Encode-error cursor-skip guard not documented; at-least-once guarantee explanation is incomplete. |
| 6 | LOW | Docs (Task 3) | Minor: `-f` is correct but inconsistent with some inline prose. |
| 7 | LOW | Docs (Task 1) | "Replay from beginning" ignores `--since-phase`/`--since-dispatch` override. |
| 8 | LOW | Docs (Task 1/3) | `cursor reset <name>` only resets global cursors, not per-run-scoped ones. |

---

## Required Fixes Before Implementation

**Task 2 — tests — three mandatory fixes:**

1. In both `TestSpawnWiringIntegration` and `TestSpawnWiringIntegration_NonExecutingPhase`, change `&mockQuerier{ids: ...}` to `&mockQuerier{agents: ...}`.

2. In `TestSpawnWiringIntegration`, change the log assertion from:
   ```go
   strings.Contains(logBuf.String(), "auto-spawn: agent agent-1 started")
   ```
   to:
   ```go
   strings.Contains(logBuf.String(), "[event] auto-spawn: agent agent-1 started")
   ```

3. In `TestSpawnWiringIntegration`, change the event Type from `"phase_advance"` to `"advance"` (matching what existing unit tests and the phase engine use), and add a comment that the handler only inspects `Source` and `ToState`.

**Task 1 — docs — one mandatory fix:**

4. Remove `--run="$run_id"` from the dispatch list example in Pattern b. Replace with a comment noting that `dispatch list --active` is global; per-run filtering is not yet available via this command. Alternatively, check whether `--scope=` scopes by run project and use that if applicable.

---

## Verdict

The test code in Task 2 has three bugs that will cause immediate compile or test failures. None of the logic is unsafe in terms of data corruption — the plan makes no kernel changes — but the tests as written will not pass. Fix the three mandatory items before implementation begins.

The documentation bugs in Task 1 are functional correctness issues: the `--run=` flag phantom is the most dangerous because a reactor following the pattern exactly will silently check the wrong scope and advance runs prematurely.
