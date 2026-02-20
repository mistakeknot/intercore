# Architecture Review: Intercore E2 Event Reactor Completion Plan

**Plan file:** `docs/plans/2026-02-19-intercore-e2-event-reactor.md`
**Reviewer role:** Architecture & Design
**Date:** 2026-02-19

---

## Summary

The plan is low-risk and structurally sound. It correctly limits scope to documentation
and integration tests with no kernel code changes. Two defects need fixing before
execution: a struct field name mismatch in the proposed test code, and a log message
mismatch in the test assertion. One documentation gap needs addressing: the plan omits
noting that `SpawnHandler` is currently listed as "Not wired" in AGENTS.md, which is
directly contradicted by the already-merged wiring at `cmd/ic/run.go:336`. The AGENTS.md
update in Task 3 must correct this stale description as part of the work. Everything
else is appropriate in scope and consistent with the existing codebase conventions.

---

## 1. Boundaries and Coupling

### No boundary violations

The plan stays entirely within the declared scope:
- Task 1 creates a new doc under `infra/intercore/docs/`
- Task 2 adds tests to `internal/event/` using types already in scope for that package
- Task 3 updates `infra/intercore/AGENTS.md`
- Task 4 runs the existing test command

There are no new cross-package dependencies introduced. The proposed integration test
correctly uses `event.NewNotifier()`, `event.NewSpawnHandler()`, and mocks defined
within the `event` package (the existing mock types are already in the package-level
test file). This is consistent with how the existing unit tests are structured.

### Wiring already in production

The plan describes SpawnHandler as an integration point to test. The actual wiring at
`cmd/ic/run.go:336` is already live:

```go
notifier.Subscribe("spawn", event.NewSpawnHandler(rtStore, spawner, os.Stderr))
```

The integration test in Task 2 correctly mirrors this wiring path. The test validates
the Notifier → SpawnHandler → AgentSpawner chain, which is the right seam to test.

### AGENTS.md has a stale description (must-fix)

`infra/intercore/AGENTS.md`, line 44 under the Event Bus handlers table, currently reads:

```
| SpawnHandler | Not wired | Scaffolded for auto-agent-spawn on phase transitions |
```

This is incorrect. SpawnHandler is wired at `cmd/ic/run.go:336`. Task 3 adds a new
"Event Reactor Pattern" section to AGENTS.md but does not correct this stale row. The
handler table entry must be updated as part of Task 3 to avoid future confusion. The
correction is:

```
| SpawnHandler | cmdRunAdvance | Auto-spawns pending agents on phase→executing transitions |
```

---

## 2. Pattern Analysis

### Test code has a field name mismatch (must-fix)

The plan proposes this mock construction in `TestSpawnWiringIntegration`:

```go
querier := &mockQuerier{ids: []string{"agent-1", "agent-2"}}
```

The actual `mockQuerier` struct defined in
`internal/event/handler_spawn_test.go` uses the field name `agents`, not `ids`:

```go
type mockQuerier struct {
    agents []string
    err    error
}
```

This will produce a compile error: `unknown field 'ids' in struct literal of type mockQuerier`.
The fix is straightforward — replace `ids:` with `agents:`.

### Test assertion uses wrong log message format (must-fix)

The plan's assertion checks:

```go
if !strings.Contains(logBuf.String(), "auto-spawn: agent agent-1 started") {
```

The actual log line emitted by `NewSpawnHandler` in `handler_spawn.go:51` is:

```go
fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
```

The prefix `[event] ` is present in the real output but absent from the assertion string.
The assertion will always pass trivially if the check is wrong, defeating its purpose.
The corrected assertion is:

```go
if !strings.Contains(logBuf.String(), "[event] auto-spawn: agent agent-1 started") {
```

### Duplicate test coverage (low severity, acceptable)

The plan proposes `TestSpawnWiringIntegration_NonExecutingPhase` to verify that a
non-"executing" `ToState` does not trigger spawning. This behavior is already covered
by `TestSpawnHandler_IgnoresNonExecuting` in the existing file. The new test adds
"wiring via Notifier" as the additional dimension, which is the stated goal of Task 2.
Keeping both is acceptable because the existing test calls the handler directly while
the new test validates the Notifier dispatch path. The distinction is worth preserving
but should be documented in a test comment so future readers do not merge the two.

### Documentation structure is consistent

The plan places the new doc at `infra/intercore/docs/event-reactor-pattern.md`. The
`docs/` directory already exists with subdirectories for brainstorms, plans, prds,
product, and research. A pattern guide belongs at the root of `docs/`, not in a
subdirectory. This is correct placement — no change needed.

### Bash examples in Task 1 have a minor semantic error

Section 5a in the documentation spec reads:

```bash
if [[ "$type" == "phase_advance" && "$to_state" == "review" ]]; then
  ic dispatch spawn --prompt-file=".ic/prompts/reviewer.md" --project="$project" --name="review-agent"
fi
```

The comment above it says "React to `to_state == "executing"` to spawn agents" but the
bash example uses `"review"` as the target state. This contradiction will confuse readers.
Either the comment should say "executing or review" or the example should use
`"executing"` to match the comment. The comment should be revised to match the example
or a separate example for each trigger state should be shown. This is a documentation
correctness issue, not a code issue.

---

## 3. Simplicity and YAGNI

### The plan is appropriately scoped

No premature abstractions are introduced. The integration test reuses existing mock
types already in the package. The documentation added is directly linked to observable
behavior in the running binary. The AGENTS.md addition provides quick-reference content
that is genuinely useful at the `ic events tail` usage level.

### The "Lifecycle" section in Task 1 includes Clavain-specific operational detail

Section 8 of the event-reactor-pattern.md spec mentions:

> Clavain session hook (runs per-session, not global)

This is accurate operational guidance but is a Clavain-specific integration note mixed
into the general intercore pattern doc. It does not cause structural harm, but if the
doc is later published or shared outside Clavain context, this note becomes noise.
Consider adding a brief "Clavain-specific" label to that bullet point rather than
moving it, since the overhead of restructuring outweighs the clarity benefit here.

### Task 4 is correct as a gate

Running `go test ./...` after adding tests is the right completion gate. No additional
CI concerns arise from this plan since no new packages, interfaces, or external
dependencies are introduced.

---

## Ordered Findings

### Must fix before execution

1. **Field name mismatch in test:** Change `mockQuerier{ids: ...}` to
   `mockQuerier{agents: ...}` in `TestSpawnWiringIntegration`. The current spelling
   will not compile.

2. **Log assertion missing `[event]` prefix:** Change the assertion string from
   `"auto-spawn: agent agent-1 started"` to `"[event] auto-spawn: agent agent-1 started"`
   to match the actual output from `handler_spawn.go`.

3. **Stale handler table in AGENTS.md:** Task 3 must also update the "SpawnHandler" row
   in the Event Bus handlers table from `Not wired` to reflect the actual wiring in
   `cmdRunAdvance`. This is a correctness issue, not a style issue.

### Should fix before execution

4. **Contradicting comment in doc Section 5a:** Reconcile the inline comment ("React to
   `to_state == "executing"`") with the bash example that checks `"review"`. Use
   separate examples for each trigger state or pick one.

### Optional cleanup

5. **Add test comment distinguishing Notifier path from direct-call path:** In the new
   integration tests, a one-line comment explaining why these tests coexist with
   `TestSpawnHandler_IgnoresNonExecuting` will prevent future consolidation pressure.

---

## Task Order Assessment

The plan's declared ordering is correct:

```
Task 1 → Task 3
Task 2 → Task 4
```

Tasks 1 and 2 are independent and can run in parallel. Task 3 must follow Task 1 (it
links to the new doc). Task 4 must follow Task 2 (validates the new tests compile and
pass). The dependency graph is sound and the risk rating of "very low" is appropriate.
