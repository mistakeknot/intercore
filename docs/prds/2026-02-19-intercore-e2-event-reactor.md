# PRD: Intercore E2 — Level 2 React Completion

**Bead:** iv-9plh
**Date:** 2026-02-19
**Brainstorm:** docs/brainstorms/2026-02-19-intercore-e2-event-reactor-brainstorm.md

## Problem

E2 is the critical-path epic blocking 4 downstream epics (E3, E4, E6, E7). Most of the kernel code is already built — the spawn handler, event bus, consumer cursors, and tail follow mode all work. But E2 can't be closed because two acceptance criteria remain unfulfilled:

1. **No documented pattern** for how OS components should consume kernel events. Without this, E3 (Hook Cutover) will lack a reference architecture for migrating Clavain hooks to event-driven patterns.

2. **No integration test** proving the full spawn flow works end-to-end through the notifier. The 5 unit tests validate the handler in isolation, but nothing tests the wiring in cmdRunAdvance.

## Solution

### F1: Event Reactor Pattern Documentation

Create `infra/intercore/docs/event-reactor-pattern.md` documenting:

- **What is an event reactor?** — Long-running consumer that tails kernel events and dispatches actions
- **Consumer registration** — How to use `ic events tail -f --consumer=<name> --poll-interval=500ms`
- **Cursor management** — How cursors auto-save, TTL expiry, reset
- **Reference patterns** — Bash and conceptual Go examples for:
  - Phase-triggered actions (e.g., spawn review agents on `phase_advance` to `review`)
  - Dispatch completion handling (e.g., advance phase when all dispatches complete)
  - Budget threshold reactions (e.g., alert on `budget.warning`)
- **Idempotency requirements** — Why consumers must be idempotent (at-least-once delivery)
- **Error handling** — What happens when the consumer crashes, restarts, or falls behind

**Acceptance:** A developer reading the doc can build a working event consumer without reading kernel source code.

### F2: Integration Test for Spawn Flow

Create a test in `internal/event/` that exercises the full wiring pattern:

1. Create a Notifier
2. Subscribe a SpawnHandler with a mock `AgentQuerier` (returns pending agents) and mock `AgentSpawner` (records spawn calls)
3. Fire a phase event: `{Source: SourcePhase, ToState: "executing", RunID: "test-run"}`
4. Assert: mock spawner received `SpawnByAgentID` calls for each pending agent

This tests the Notifier → Handler → Interface chain without shelling out to real processes.

**Acceptance:** `go test ./internal/event/...` includes a test named `TestSpawnWiringIntegration` that passes.

### F3: Update AGENTS.md

Add an "Event Reactor" section to `infra/intercore/AGENTS.md` with:
- Quick reference for `ic events tail` flags
- Consumer naming conventions
- Link to the full pattern doc

**Acceptance:** AGENTS.md has an "Event Reactor" section.

## Non-Goals

- Building a Go reactor binary (E3+ scope)
- Auto-advancing phases on dispatch completion (OS policy, not kernel mechanism)
- New event types (E1 already emits what's needed)
- Modifying SpawnHandler logic (already correct)

## Effort

Complexity 2/5. Deliverables are documentation-heavy with one focused test. No schema changes, no new CLI commands.

## Dependencies

- **Depends on:** E1 (DONE) — kernel primitives, event bus, consumer cursors
- **Blocks:** E3 (Hook Cutover), E4 (Interspect), E6 (Rollback), E7 (Autarch P1)

## Success Metric

Close E2 epic (`iv-9plh`), unblocking 4 downstream epics.
