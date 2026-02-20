# Quality Review: Go Code — SpawnHandler Integration Tests, Event Reactor Doc, AGENTS.md

**Scope:** Go 1.22, modernc.org/sqlite, package `event`

**Files reviewed:**
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn_test.go`
- `/root/projects/Interverse/infra/intercore/docs/event-reactor-pattern.md`
- `/root/projects/Interverse/infra/intercore/AGENTS.md` (Event Reactor Pattern + SpawnHandler sections)

**Reference files examined:**
- `internal/event/handler_spawn.go` — production source
- `internal/event/notifier.go` — production source
- `internal/lock/lock_test.go` — existing test style baseline

---

## Summary Verdict

The changes are well-considered and mostly solid. The integration tests add genuine coverage at the right layer. The documentation is accurate and production-grade. There are three concrete correctness concerns worth fixing before treating this as done, plus two style observations that bring the tests into closer alignment with project conventions.

---

## 1. Test File — `handler_spawn_test.go`

### 1.1 Correctness: Unchecked error return in `TestSpawnHandler_IgnoresNonExecuting` and `TestSpawnHandler_IgnoresDispatchEvents`

**Lines 65, 78:**
```go
h(context.Background(), e)
```

The return value of `h(...)` is silently discarded. In the non-executing and dispatch-ignore cases the handler should return `nil`, but discarding the return means a regression that starts returning an error here would go undetected. The existing unit-test baseline (`lock_test.go`, every other test in the package) always captures errors from operations under test.

**Fix:**
```go
if err := h(context.Background(), e); err != nil {
    t.Fatalf("unexpected error: %v", err)
}
```

This is consistent with the style used in `TestSpawnHandler_NoAgents` (line 91) and `TestSpawnHandler_TriggersOnExecuting` (line 49–51) in the same file.

The same pattern appears in `TestSpawnHandler_PartialFailure` (line 107), where the call returns an error but the result is discarded. The partial-failure contract (continue on error, log, return nil from the handler) is a design decision, but if that contract ever changes, the silent discard is the wrong safety net. Capture and assert `nil`:
```go
if err := h(context.Background(), e); err != nil {
    t.Fatalf("unexpected error from partial failure path: %v", err)
}
```

### 1.2 Correctness: Integration tests do not assert against `Notify` error

**Lines 137–144 (`TestSpawnWiringIntegration`) and 186–193 (`TestSpawnWiringIntegration_MultipleHandlers`):**
```go
notifier.Notify(ctx, Event{ ... })
```

`Notify` returns the first handler error (`error`). The integration tests discard it silently. Because `Notifier.Notify` propagates handler errors purely for logging purposes (as documented in `notifier.go` line 37: "Returns the first error encountered (if any) for logging purposes only"), a test asserting the full success path should confirm `Notify` returned `nil`. Discarding the return leaves the test blind to unexpected handler errors during the test itself.

**Fix:**
```go
if err := notifier.Notify(ctx, Event{...}); err != nil {
    t.Fatalf("Notify: %v", err)
}
```

### 1.3 Style: Mixed mock initialization style for `failIDs`

Three tests initialize `mockSpawner` with an explicit but empty `failIDs` map:
```go
s := &mockSpawner{failIDs: map[string]bool{}}
```

But `mockSpawner.SpawnByAgentID` guards the map access with `m.failIDs[agentID]` — in Go, reading a nil map returns the zero value (`false`) without panicking, so the explicit initialization is unnecessary in tests where no failures are expected. The inconsistency creates reader confusion: tests that pass `nil` look different from tests that pass `map[string]bool{}`, with no behavioral difference.

Choose one convention:
- If the concern is clarity (reader can see "no failures intended"), keep the explicit empty map and add it to `TestSpawnHandler_PartialFailure` too (line 102 already has it correctly).
- If the concern is brevity, rely on the zero-value nil-map read for the "no failure" cases and only initialize in `TestSpawnHandler_PartialFailure`.

The partial failure test already demonstrates the idiomatic initialization with `failIDs: map[string]bool{"fail1": true}`, which is correct and readable.

### 1.4 Style: Integration test comment style is verbose for Go

The step-numbered comments ("`// 1. Create notifier`", "`// 2. Create mocks`", etc.) are useful for long setup sequences, but the narrative style is heavier than project conventions in the same package. `lock_test.go` uses terse single-line comments (`// Verify lock dir exists.`). This is a minor style observation, not a blocking issue.

### 1.5 Missing: No table-driven test covering interaction between `SourcePhase` and `SourceDispatch` filter

The existing unit tests correctly cover the filter conditions individually. However, there is no table-driven test that exercises the full filter matrix: (source, to_state) → (expect spawn, expect no spawn). The integration tests add notifier wiring coverage, but a table-driven unit test would make the filter logic exhaustive and regression-proof as new sources or states are added. Consider:

```go
var filterTests = []struct {
    name    string
    source  string
    toState string
    wantSpawn bool
}{
    {"phase executing", SourcePhase, "executing", true},
    {"phase non-executing", SourcePhase, "planned", false},
    {"dispatch executing", SourceDispatch, "executing", false},
    {"dispatch non-executing", SourceDispatch, "planned", false},
}
```

This would consolidate `TestSpawnHandler_IgnoresNonExecuting` and `TestSpawnHandler_IgnoresDispatchEvents` into a single, extensible test.

### 1.6 Note: `AgentSpawnerFunc` adapter used in integration tests but not in existing unit tests

The integration tests correctly use `AgentSpawnerFunc` (the functional adapter exported from `handler_spawn.go`) instead of the `mockSpawner` struct. This is good — it validates that the adapter itself works end-to-end. The unit tests use `mockSpawner` (interface implementation) for isolation, and the integration tests use `AgentSpawnerFunc` for wiring validation. This is a sound split of concerns and matches the project's stated "accept interfaces" pattern.

---

## 2. Documentation — `docs/event-reactor-pattern.md`

The document is comprehensive, accurate to the implementation, and correctly addresses the operational concerns that matter in production:
- Cursor behavior and TTL
- At-least-once delivery and idempotency requirement
- Pipe-break restart pattern
- Systemd lifecycle

### 2.1 Shell strictness in examples

**Line 15 (Quick Start example):**
```bash
#!/usr/bin/env bash
set -euo pipefail

ic events tail ... | while IFS= read -r line; do
  source=$(echo "$line" | jq -r '.source')
```

The script correctly sets `set -euo pipefail` at the top, but `set -e` behavior inside a pipeline is non-trivial: `set -e` does not apply to commands in a pipeline's non-last position. The `while read` loop body is fine, but if `jq` fails (e.g., malformed JSON line), the script exits abruptly without logging the offending line or continuing. For a reactor that is expected to be resilient, the pattern should guard jq calls or use a null-safe fallback:

```bash
source=$(echo "$line" | jq -r '.source // empty' 2>/dev/null) || continue
```

This is a documentation quality issue, not a correctness issue in the Go code itself, but these examples are copy-pasted into production reactors.

### 2.2 `Dispatch Completion → Phase Advance` example uses unfiltered `ic dispatch list`

**Lines 139–141:**
```bash
active=$(ic dispatch list --active --json 2>/dev/null \
  | jq --arg rid "$run_id" '[.[] | select(.run_id == $rid)] | length')
```

The inline comment already documents that `ic dispatch list --active` returns all dispatches and requires client-side filtering. This is accurate. However, the example suppresses `stderr` with `2>/dev/null` which will silently mask `ic` errors. A more defensive pattern would be to capture stderr to a variable and only suppress expected "no rows" cases, or remove the stderr suppression and let the error propagate.

### 2.3 Budget events documented under `source: "phase"` but `type` is not `advance`

The schema table is correct that budget events have `source: "phase"`, but the introductory "Event Types by Source" section lists them under "Phase events" with `type`: `advance`, `skip`, `block` — and then separately describes budget events as a separate category also under `source: "phase"`. The grouping is clear in the current layout (separate header), but a reader scanning the table might not realize `budget_warning` and `budget_exceeded` also come from `source: "phase"`. A note in the table would eliminate ambiguity:

```
**Budget events** (`source: "phase"`, not triggered by phase transitions):
```

This is a documentation clarity issue, not incorrect.

---

## 3. AGENTS.md Updates

### 3.1 SpawnHandler registration point is pinned to a line number

**AGENTS.md, Event Bus Module section:**
```
SpawnHandler | Always | Auto-spawns pending agents when phase transitions to "executing"; wired at cmd/ic/run.go:336
```

Line-number references in documentation rot immediately when surrounding code is modified. The file location (`cmd/ic/run.go`) is useful; the line number (`:336`) is not and will mislead readers after any future edit. Recommend removing the line number:

```
SpawnHandler | Always | Auto-spawns pending agents when phase transitions to "executing"; wired in cmdRunAdvance (cmd/ic/run.go)
```

### 3.2 Integration test count in AGENTS.md is understated

AGENTS.md states:
```
go test ./...   # Unit tests (~130 tests across 9 packages)
```

The two new integration tests (`TestSpawnWiringIntegration`, `TestSpawnWiringIntegration_MultipleHandlers`) plus the existing unit tests added in the same batch increase this count. While "~130" is approximate, it should be bumped if the actual count is materially higher. This is a documentation maintenance note.

### 3.3 Event Reactor Pattern section is well-integrated

The AGENTS.md summary for the Event Reactor Pattern is appropriately terse and correctly defers to the full doc. The consumer guidelines are accurate and match the implementation. No issues found here.

---

## Findings Summary

| # | File | Severity | Finding |
|---|------|----------|---------|
| 1.1 | handler_spawn_test.go | Medium | Unchecked error returns on lines 65, 78, 107 — regressions in these paths would be silent |
| 1.2 | handler_spawn_test.go | Medium | `Notify` return values discarded in both integration tests — unexpected handler errors undetected |
| 1.3 | handler_spawn_test.go | Low | Inconsistent `failIDs` initialization (`nil` vs `map[string]bool{}`) across tests |
| 1.4 | handler_spawn_test.go | Low | Integration test comment style heavier than package convention (informational) |
| 1.5 | handler_spawn_test.go | Low | No table-driven test for filter matrix; would make filter coverage exhaustive |
| 2.1 | event-reactor-pattern.md | Low | `jq` failures in examples exit silently; reactor examples should show guard pattern |
| 2.2 | event-reactor-pattern.md | Low | `2>/dev/null` in dispatch completion example silently masks `ic` errors |
| 2.3 | event-reactor-pattern.md | Informational | Budget events grouping under phase events is accurate but could be clearer in schema table |
| 3.1 | AGENTS.md | Low | Line-number reference `:336` in SpawnHandler table will rot — remove the line number |
| 3.2 | AGENTS.md | Informational | Unit test count `~130` may need update after new tests added |

**Priority fixes before merging:** Items 1.1 and 1.2 (unchecked errors in tests). These are medium severity because they would silently mask handler regressions — which is exactly what tests exist to catch.

**Lower priority but recommended:** Items 1.3, 1.5, 3.1 (style alignment and AGENTS.md line-number rot). Item 2.1 for documentation quality.
