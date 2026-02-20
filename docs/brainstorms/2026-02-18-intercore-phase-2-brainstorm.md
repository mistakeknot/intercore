# Intercore Phase 2: Event Bus, Policy Engine, and Control Room

**Bead:** iv-xn01 (sprint), iv-qfg8 (parent feature)
**Phase:** brainstorm (as of 2026-02-19T02:30:24Z)
**Date:** 2026-02-18
**Status:** Brainstorm

## The Problem

Phase 1 gave intercore the **mechanical foundation**: a state machine that tracks runs through 8 lifecycle phases, agent dispatch tracking, artifact recording, and filesystem locks. But the system is inert — gates always pass (stubs), nothing reacts to phase transitions, and the only interface is the CLI.

The result:
- **No enforcement.** A run can advance from `planned` to `executing` even if no plan artifact exists. The `evaluateGate` function is a stub that returns `GatePass` for everything.
- **No visibility.** Operators can't see what's happening across runs without running `ic run list` and `ic run events` manually. There's no dashboard, no notifications, no portfolio view.
- **No reaction.** When a run advances phases, nothing happens automatically — no agents are spawned, no reviews are triggered, no artifacts are verified. The sprint hooks (`lib-sprint.sh`) do this work, but they're disconnected from intercore's run model.

Phase 2 is about making intercore **alive**: gates that enforce quality, events that trigger reactions, and a control room where operators can see and steer the fleet.

## What Phase 2 Delivers (from iv-byh3 vision)

The original iv-byh3 decision document identified six capabilities:

1. **Event bus** — at-least-once delivery, ULID event IDs, per-run ordering
2. **Policy engine** — allow/deny/require_human gate decisions with evidence
3. **interhub TUI control room** — portfolio view, run timeline, agent fleet, exceptions
4. **Multi-project support** — project_id → initiative_id → run_id identity model
5. **API surface** — HTTP+JSON commands/queries, SSE events
6. **Read model projections** — materialized views for interhub queries

This is 6 months of work if built all at once. The question is: **what delivers value first, and what can wait?**

## Decomposition: Three Waves

### Wave 1: Policy Engine (the gates come alive)

**Why first:** This is the highest-impact, lowest-risk change. The extension point already exists (`evaluateGate` stub). The data already exists (artifacts in `run_artifacts`, verdicts in `dispatches`). No new tables needed. No new packages beyond extending `internal/phase`. The bash hooks already call `advance_phase` through lib-gates.sh — they'll automatically get gate enforcement once `evaluateGate` is real.

**What it does:**
- Replace `evaluateGate` with real artifact-presence and verdict checks
- Each phase transition has a specific gate condition:
  - `brainstorm → brainstorm-reviewed`: brainstorm artifact exists in `run_artifacts`
  - `brainstorm-reviewed → strategized`: strategy/PRD artifact exists
  - `strategized → planned`: plan artifact exists
  - `planned → executing`: plan has been reviewed (verdict exists in dispatches or run_artifacts with type "review")
  - `executing → review`: all agents completed (query `run_agents WHERE status = 'active'` returns 0)
  - `review → polish`: review verdict exists and is not "reject"
  - `polish → done`: no hard requirement (human judgment)
- Persist gate rules as data, not code: `policy_rules` in the `state` table (key=`gate:<from>:<to>`, scope=project, payload=JSON rule definition)
- `ic gate check <run_id>` — dry-run gate evaluation without advancing (shows what would pass/fail)
- `ic gate override <run_id> --reason="..."` — skip a gate with audit trail (writes a phase_event with event_type=`override`)
- Gate evaluation results are stored in phase_events (already happening — just need real results instead of stub values)

**Estimated effort:** 1-2 sessions. Schema: no migration needed (reuse `state` table for policy config, phase_events for results). New CLI: `ic gate check/override`. New code: real `evaluateGate` implementation + policy rule parser.

**Extension point:** `evaluateGate(cfg GateConfig, from, to string) (result, tier string)` — signature stays the same, internals change from stub to real evaluation.

### Wave 2: Event Bus (reactions to transitions)

**Why second:** The event bus makes intercore reactive. Currently, phase transitions are fire-and-forget writes to the database. With an event bus, transitions can trigger downstream actions: spawn review agents when entering `review` phase, notify Slack when a run is blocked, send metrics to interstat.

**Architecture decision: polling vs. push**

The `phase_events` table is already an append-only event log. Two approaches:

**Option A: Polling readers (simplest)**
- Each consumer maintains a cursor (last seen event ID) in the `state` table
- Consumer polls `SELECT * FROM phase_events WHERE id > ? ORDER BY id ASC LIMIT 100` on interval
- Pro: No new infra, SQLite-native, works with single-writer constraint
- Con: Latency (poll interval), consumers must be long-running processes

**Option B: In-process event dispatch (internal pub/sub)**
- A `Notifier` type in a new `internal/events` package
- `Advance()` calls `notifier.Publish(event)` after committing
- Subscribers register handlers: `notifier.Subscribe("phase_advance", handler)`
- Pro: Zero latency, clean API
- Con: Only works in-process (not for external consumers like TUI or hooks)

**Recommendation: Option A for external consumers, Option B for internal reactions.**

The polling approach is perfect for the TUI (which runs as a separate process). The in-process approach is perfect for post-advance reactions within `ic` itself (e.g., auto-spawning review agents). Both can coexist.

**What it does:**
- `internal/events` package with `Notifier` type (in-process fan-out)
- Event types: `phase.advance`, `phase.skip`, `phase.block`, `gate.fail`, `gate.warn`, `agent.start`, `agent.complete`, `agent.fail`, `artifact.add`
- `Advance()` publishes to Notifier after successful DB commit
- Polling cursor support: `ic events tail <run_id> [--since=<event_id>]` (like `tail -f` for events)
- Hook integration: `intercore_events_tail` bash wrapper for hooks to consume events
- Subscriber registrations persisted in `state` table (key=`sub:<consumer>`, payload=filter config)

**What it does NOT do (yet):**
- HTTP/SSE streaming (Wave 3)
- Webhook delivery (Wave 3)
- Cross-process notification (TUI polls instead)

**Estimated effort:** 2-3 sessions. New package `internal/events`. New CLI `ic events tail`. Hooks integration.

### Wave 3: Control Room TUI + API surface

**Why third:** The TUI and API are presentation layers. They need the policy engine (Wave 1) and event bus (Wave 2) to have something to display and react to. Building them first would mean building against stubs.

**Architecture: interhub as a separate binary**

The TUI should be a separate binary (`interhub`) that reads from intercore's SQLite database. It does NOT modify the database directly — all mutations go through `ic` CLI commands (maintaining the single-writer principle). The TUI is a read-heavy dashboard with occasional write actions (manual advance, gate override) that shell out to `ic`.

**Technology: Bubble Tea (Go TUI framework)**

The Clavain ecosystem already uses Bubble Tea for TUI components (tuivision plugin). Bubble Tea's Elm architecture (Model → Update → View) maps cleanly to:
- Model: run list, selected run's phase/events/agents/artifacts
- Update: poll database on timer tick, handle key events
- View: render panels (run list, phase stepper, event timeline, agent fleet)

**Panels:**

1. **Portfolio view** (left panel): List of active runs across all projects. Columns: project, phase, agent count, last activity. Filterable by project/phase/status. Sorted by last activity.

2. **Run detail** (main panel): Selected run's phase stepper (8-phase timeline with current phase highlighted, skipped phases dimmed). Below: event timeline showing recent phase_events with gate results.

3. **Agent fleet** (bottom panel): Active agents for the selected run. Columns: name, type, status, duration. Links to dispatch IDs. Shows dispatch tree (parent→child fan-out).

4. **Exception queue** (overlay): Runs with `gate.block` or `gate.fail` events that need human attention. Gate override dialog with reason input.

**API surface (optional, deferred):**
- HTTP+JSON would enable web dashboards, Slack bots, CI integrations
- SSE for real-time event streaming to web clients
- Can be a separate binary (`intercore-api`) or a flag on `interhub`
- Not needed for v1 of Phase 2 — TUI is sufficient for single-operator use

**Estimated effort:** 3-5 sessions for basic TUI. API surface is a separate effort.

## What NOT to Build in Phase 2

These items from the iv-byh3 vision are deliberately deferred:

1. **Multi-project identity model** (project_id → initiative_id → run_id). Current `runs.project_dir` is sufficient. Identity hierarchy adds complexity without immediate value. Revisit when managing 10+ concurrent projects.

2. **Read model projections.** The TUI can query SQLite directly — it's a single-file database, not a distributed system. Materialized views add write amplification for no benefit at current scale.

3. **Webhook delivery.** External integrations (Slack, GitHub Actions) can poll `ic events tail` or read phase_events directly. Push delivery is a reliability problem (retries, dead letter queues) that's premature.

4. **Policy profiles (named configs).** The first implementation should hard-code sensible defaults. Named profiles ("strict", "relaxed") add an abstraction layer before we know what configurations are actually needed.

## Open Questions

### Q1: Should gate evaluation be synchronous or async?

**Synchronous (current design):** `evaluateGate` runs inline during `Advance()`. Gate checks query the DB (fast — indexed queries on `run_artifacts` and `dispatches`). The caller blocks until the gate decision is made. Simple. Deterministic.

**Async:** `Advance()` returns immediately with a "pending" status. A background evaluator checks gates and either advances or blocks. Enables long-running gate checks (e.g., "wait for CI to finish"). But adds complexity (state machine for gate evaluation itself, race conditions between advance requests).

**Recommendation:** Synchronous for artifact-presence gates (fast DB queries). Async only if we add external gate checks (CI status, code review approval) — and that's a future wave.

### Q2: Should the TUI shell out to `ic` or use Go library API?

The Phase 1 design explicitly chose "CLI-only, no Go library API." But the TUI is a Go binary that needs to read the database frequently. Shelling out to `ic` for every poll is wasteful (process spawn overhead).

**Options:**
- **A: Import internal packages directly.** The TUI imports `internal/phase`, `internal/runtrack`, etc. for reads. Writes still go through `ic` CLI. This is the pragmatic choice.
- **B: Shell out for everything.** Pure CLI integration. Slower but consistent with Phase 1 philosophy.
- **C: Read DB directly with raw SQL.** TUI opens a read-only connection. Avoids importing internal packages. Coupling is at the schema level instead.

**Recommendation:** Option A. The TUI is part of the same Go module — importing internal packages for reads is natural. Writes (advance, override) still go through `ic` to maintain single-writer and audit trail.

### Q3: Event bus scope — just phase events, or all mutations?

Phase 1 has multiple write paths:
- Phase transitions → `phase_events` (already logged)
- Agent status changes → `run_agents.status` (no event log)
- Artifact additions → `run_artifacts` (no event log)
- Dispatch lifecycle → `dispatches.status` (no event log)

Should the event bus only cover phase transitions (cheapest, already have the data), or expand to all mutations (enables richer reactions but requires adding event logging to every write path)?

**Recommendation:** Start with phase events only (Wave 2a). Add agent/artifact events in Wave 2b if the TUI or hook system needs them. Don't add event logging to `dispatches` — it already has `verdict_status` and `verdict_summary` that the policy engine can query directly.

### Q4: Gate evidence — what format?

When `evaluateGate` checks "does a brainstorm artifact exist?", it queries `run_artifacts`. But what does it return as evidence? Options:
- Just pass/fail (current `GateResult` constants)
- Pass/fail + reason string (current `AdvanceResult.Reason`)
- Pass/fail + structured evidence (list of checked conditions with individual results)

**Recommendation:** Structured evidence. The `Reason` field in `AdvanceResult` becomes a JSON blob with per-condition results. The TUI can render this as a checklist. Example:
```json
{
  "conditions": [
    {"check": "artifact_exists", "phase": "brainstorm", "result": "pass", "path": "docs/brainstorms/foo.md"},
    {"check": "verdict_present", "phase": "review", "result": "fail", "detail": "no review dispatch found"}
  ]
}
```

## Dependency Graph

```
Wave 1: Policy Engine
  ├── Real evaluateGate implementation
  ├── ic gate check/override CLI
  └── Gate evidence in phase_events.reason

Wave 2: Event Bus (depends on Wave 1)
  ├── internal/events Notifier
  ├── Post-advance publish
  ├── ic events tail CLI
  └── Hook integration

Wave 3: Control Room TUI (depends on Wave 1 + Wave 2)
  ├── interhub binary
  ├── Portfolio view
  ├── Run detail + phase stepper
  ├── Agent fleet panel
  └── Exception queue
```

Wave 1 unblocks Wave 2 and Wave 3 independently. Wave 2 and Wave 3 can proceed in parallel once Wave 1 is done — the TUI can poll the database directly without the event bus, but the event bus makes it reactive.

## Risk Assessment

| Risk | Mitigation |
|------|------------|
| SQLite write contention under TUI polling | TUI does read-only queries; writes go through `ic` CLI (single-writer preserved) |
| Gate evaluation slowing down `Advance()` | All gate checks are indexed DB queries (sub-ms). Async path only if adding external checks |
| TUI scope creep (feature-rich dashboard) | Wave 3 v1 is 4 panels only. No web UI, no API, no interactive editing |
| Event bus reliability (at-least-once delivery) | Polling cursor pattern with checkpoint — consumers re-read on restart. No ACK protocol needed |
| Bubble Tea complexity (layout, focus management) | Use lipgloss zones, not nested containers. Single-viewport layout. |

## Recommended Sprint Plan

**This sprint (current session):** Wave 1 — Policy Engine. This is 1-2 sessions of focused work with clear scope and an existing extension point.

**Next sprint:** Wave 2 — Event Bus. Builds on Wave 1's real gate evaluation to publish meaningful events.

**Future sprint:** Wave 3 — TUI. Builds on both Wave 1 and Wave 2 to present a reactive dashboard.

Each wave should be its own bead with its own plan document, reviewed through flux-drive before execution.
