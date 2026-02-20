# Brainstorm: Intercore E2 — Level 2 React Completion

**Bead:** iv-9plh
**Phase:** brainstorm (as of 2026-02-19T18:32:36Z)
**Date:** 2026-02-19
**Context:** E1 (kernel primitives) shipped today. E2 (Level 2 React) is the next milestone on the critical path — it blocks E3 (Hook Cutover), E4 (Interspect Integration), E6 (Rollback), and E7 (Autarch P1).

## What's Already Done

Significant E2 work was completed in earlier sprints:

1. **SpawnHandler** (`handler_spawn.go`) — fully implemented, 5 unit tests pass
2. **AgentSpawnerFunc adapter** — closure-based pattern matching `dispatchRecorder`
3. **SpawnHandler wiring in cmdRunAdvance** — subscribed at line 336 of `run.go`, with CAS dispatch linking and orphan process cleanup
4. **Event bus** — in-process Notifier with Subscribe/Notify, log handler, hook handler, spawn handler
5. **Event store** — phase events and dispatch events persisted to SQLite
6. **Consumer cursors** — `ic events tail -f --consumer=<name> --poll-interval=500ms` with cursor persistence and auto-cleanup
7. **runtrack store** — `GetAgent`, `ListPendingAgentIDs`, `UpdateAgentDispatch` (with conflict detection)

## What Remains (E2 Acceptance Criteria Gap)

From the roadmap PRD, E2 requires:

| Criterion | Status | Remaining Work |
|-----------|--------|----------------|
| `SpawnByAgentID` implemented | DONE | — |
| SpawnHandler registered in Notifier | DONE | — |
| `ic events tail -f --consumer --poll-interval` | DONE | — |
| Documented pattern for OS-level event reactor | **NOT DONE** | Document how Clavain or other OS components consume events |
| Integration test: agent → event → spawn | **PARTIAL** | Unit tests exist, but no end-to-end test through cmdRunAdvance |

## Problem Space

### 1. OS-Level Event Reactor Pattern

The vision doc describes the reactor as: "An OS-level event reactor runs as a long-lived `ic events tail -f --consumer=clavain-reactor` process with `--poll-interval`. This is an OS component, not a kernel daemon."

The question is: what does this look like concretely? Options:

**Option A: Bash script reactor (simple)**
A long-running bash script that tails events and dispatches to handlers:
```bash
ic events tail --all -f --consumer=clavain-reactor --poll-interval=500ms | while read line; do
  event_type=$(echo "$line" | jq -r '.type')
  case "$event_type" in
    "phase_advance") handle_phase_advance "$line" ;;
    "dispatch_completed") handle_dispatch_complete "$line" ;;
  esac
done
```
Pro: Simple, debuggable, zero infrastructure. Con: Fragile (pipe breaks, jq overhead, no concurrency).

**Option B: Go binary reactor (robust)**
A standalone Go binary (or subcommand of `ic`) that consumes events and dispatches to handler functions. Could be `ic reactor start` or a separate `ic-reactor` binary.
Pro: Type-safe, concurrent, testable. Con: More code, needs process management.

**Option C: Documented pattern only (minimal)**
Document the pattern without building a reference implementation. OS components (Clavain hooks, Interspect scripts) each implement their own tailing loops.
Pro: Zero code, each consumer is independent. Con: Duplicated boilerplate, no reference to copy from.

**Recommendation:** Option C for now — document the pattern, provide a reference bash snippet, and let OS components implement their own consumers. A Go reactor is E3/E4 scope. E2's acceptance criterion says "documented pattern," not "built reactor."

### 2. Integration Test Coverage

Current spawn handler tests use mock `AgentQuerier` and `AgentSpawner`. What's missing is a test that exercises the full path through cmdRunAdvance: create a run, add an agent, advance to executing, verify the spawn handler fires.

Challenge: `dispatch.Spawn` shells out to `codex` or `claude`, which doesn't work in tests. Options:

**Option A: Test the wiring compiles + handler is subscribed**
Verify that `notifier.Subscribe("spawn", ...)` exists in cmdRunAdvance by checking the code pattern. Not a runtime test, but confirms the wiring won't regress.

**Option B: Refactor to inject spawner**
Make cmdRunAdvance accept a `SpawnFunc` parameter (or env var to override the spawn binary), allowing tests to substitute a no-op spawner. This is a bigger refactor.

**Option C: Test via Notifier directly**
Create a test that builds the notifier the same way cmdRunAdvance does (minus the actual dispatch.Spawn call), fires a phase event, and asserts the spawn handler received it. This tests the event flow without shelling out.

**Recommendation:** Option C — it tests the actual wiring pattern (Notifier → SpawnHandler → AgentQuerier → AgentSpawner) without needing process spawning. Plus a smoke test (Option A) for regression confidence.

### 3. Scope Boundary: What's E2 vs E3?

E2 = the kernel has reactive event infrastructure. OS components CAN subscribe.
E3 = Clavain's hooks actually DO subscribe — the big-bang migration from temp files to `ic`.

The reactor *document* is E2. The reactor *implementation in Clavain* is E3. The boundary is clean.

## Deliverables (Scoped for This Sprint)

1. **Event reactor pattern documentation** — a doc in `infra/intercore/docs/` explaining how to build OS-level event consumers using `ic events tail -f --consumer`, with examples for phase-triggered actions, dispatch completion handling, and cursor management
2. **Integration test** — test that creates a Notifier, subscribes the SpawnHandler with a mock spawner, fires a phase event to "executing", and asserts the spawn was attempted
3. **Wiring regression test** — compile-time or pattern-based check that cmdRunAdvance subscribes "spawn"
4. **Update AGENTS.md** — add event reactor section with consumer examples

## What We're NOT Doing

- Building a Go reactor binary (that's E3+ scope)
- Auto-advancing phases on dispatch completion (that's OS-layer policy, not kernel mechanism)
- Adding new event types beyond what E1 already emits
- Modifying the SpawnHandler logic (it's already correct and tested)

## Risks

- **Low:** All kernel code is done. This sprint is documentation + testing + minor wiring verification.
- **Only risk:** Integration test might reveal a subtle ordering issue in the Notifier dispatch, but the 5 existing unit tests make this unlikely.

## Estimated Effort

Complexity 2/5. Half-day session. Mostly writing docs and tests, minimal new Go code.
