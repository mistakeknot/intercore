# intercore Philosophy

## Purpose
intercore is a Go CLI (`ic`) backed by a single SQLite WAL database that provides atomic state operations, throttle guards, agent dispatch tracking, and phase lifecycle management callable from bash hooks. It replaces ~15 scattered temp files in `/tmp/` used by the Clavain hook infrastructure.

## North Star
Advance intercore through small, testable changes aligned to its core mission: intercore is a Go CLI (`ic`) backed by a single SQLite WAL database that provides atomic state operations, throttle guards, agent dispatch tracking, and phase lifecycle management callable from bash hooks. It replaces ~15 scattered temp files in `/tmp/` used by the Clavain hook infrastructure.

## Working Priorities
- Tracking
- Throttle
- State

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
- Brainstorms analyzed: 0
- Plans analyzed: 0
- Source confidence: inferred (no local brainstorm/plan corpus found)
- Representative artifacts: none yet. Build this corpus over time under `docs/brainstorms/` and `docs/plans/`.
