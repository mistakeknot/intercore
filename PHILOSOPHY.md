# intercore Philosophy

## Purpose
intercore is the kernel layer of the Clavain autonomous software agency. It is a host-agnostic Go CLI (`ic`) backed by a single SQLite WAL database that provides the durable system of record for runs, phases, gates, dispatches, events, and token budgets. The kernel is mechanism, not policy — it doesn't know what "brainstorm" means, only that a phase transition happened and needs recording.

In the three-layer architecture (Kernel → OS → Drivers), intercore is Layer 1. Clavain (the OS, Layer 2) encodes policy — which phases exist, what gates enforce, how agents are routed. Companion plugins (drivers, Layer 3) wrap individual capabilities. If the OS layer changes or the host platform disappears, the kernel and all its data survive untouched.

## North Star
Advance intercore through small, testable changes aligned to its core mission: provide durable, crash-safe orchestration primitives that any OS layer can build on. The kernel's event bus is the backbone of observability — every state change produces a typed, durable event. You can't refine what you can't see.

## Working Priorities
- Durability — every operation is crash-safe, every state change is auditable
- Mechanism — provide primitives, not opinions; policy belongs in the OS layer
- Observability — emit typed events for every transition so profilers and reactors can act

## Brainstorming Doctrine
1. Start from outcomes and failure modes, not implementation details.
2. Generate at least three options: conservative, balanced, and aggressive.
3. Explicitly call out assumptions, unknowns, and dependency risk across modules.
4. Prefer ideas that improve clarity, reversibility, and operational visibility.

## Planning Doctrine
1. Convert selected direction into small, testable, reversible slices.
2. Define acceptance criteria, verification steps, and rollback path for each slice.
3. Sequence dependencies explicitly and keep integration contracts narrow.
4. Reserve optimization work until correctness and reliability are proven.

## Decision Filters
- Does this reduce ambiguity for future sessions?
- Does this improve reliability without inflating cognitive load?
- Is the change observable, measurable, and easy to verify?
- Can we revert safely if assumptions fail?

## Evidence Base
- Brainstorms analyzed: 3 (E1 kernel primitives, E2 event reactor, event bus)
- Plans analyzed: 3 (E1 kernel primitives, E2 event reactor, event bus)
- Source confidence: high (multiple design docs, integration tests, shipped CLI)
- Representative artifacts: `docs/event-reactor-pattern.md`, `MIGRATION.md`
