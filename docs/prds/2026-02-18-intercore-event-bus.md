# PRD: Intercore Wave 2 — Event Bus
**Bead:** iv-egxf (sprint epic), iv-k1xt (feature)

## Problem

After Wave 1's policy engine, intercore can evaluate whether a phase *should* advance — but nothing reacts after it does. `Advance()` writes to the DB and returns silently. Hooks must poll temp files, agents aren't auto-spawned, and there's no way to observe run activity in real time. The system is inert.

## Solution

Add an event bus to intercore with two complementary layers: an in-process Notifier that fires handlers after phase and dispatch state changes, and a polling cursor CLI (`ic events tail`) for external consumers. Three built-in handlers ship with Wave 2: event logging, auto-agent-spawn, and shell hook triggers.

## Features

### F1: Unified Event Model + Dispatch Events Table
**What:** Define the `Event` struct as the canonical event type and add a `dispatch_events` table for dispatch lifecycle tracking (keeping `phase_events` unchanged).
**Acceptance criteria:**
- [ ] `Event` struct defined in `internal/event/event.go` with fields: ID, RunID, Source (phase/dispatch), Type, FromState, ToState, Reason, Timestamp
- [ ] `dispatch_events` table added in schema v5 migration with same structure as `phase_events` (autoincrement ID, run_id FK, event_type, from_status, to_status, reason, timestamp)
- [ ] `EventStore` interface with `AddEvent`, `ListEvents(runID, since, limit)`, `ListAllEvents(since, limit)` that merges both tables ordered by timestamp
- [ ] Dispatch status changes (spawn, complete, fail, timeout, cancel) write to `dispatch_events`
- [ ] Existing `phase_events` writes unchanged — backward compatible
- [ ] Schema migration from v4 to v5 with automatic backup

### F2: In-Process Notifier Interface + Wiring
**What:** A Go Notifier that fires registered handlers after DB commits in `Advance()` and dispatch status changes.
**Acceptance criteria:**
- [ ] `Notifier` interface in `internal/event/notifier.go`: `Subscribe(EventHandler)`, `Notify(ctx, Event) error`
- [ ] `InProcessNotifier` implementation with synchronous sequential dispatch
- [ ] `Advance()` in `internal/phase/machine.go` calls `notifier.Notify()` after successful phase update
- [ ] Dispatch status changes in `internal/dispatch/dispatch.go` call `notifier.Notify()` after status update
- [ ] Handler errors are logged but do not fail the parent operation (fire-and-forget semantics)
- [ ] Notifier initialized at CLI startup and passed to phase machine and dispatch via dependency injection

### F3: Event Logging Handler
**What:** First built-in handler that emits structured log lines for every event.
**Acceptance criteria:**
- [ ] Handler registered by default when Notifier is initialized
- [ ] Log format: `[event] source=phase type=advance run=<id> from=planned to=executing reason="Gate passed"`
- [ ] Respects `--quiet` flag (suppressed in quiet mode)
- [ ] Works for both phase and dispatch events

### F4: Auto-Agent-Spawn Handler
**What:** When phase advances to `executing`, automatically spawn agents registered for that run.
**Acceptance criteria:**
- [ ] Handler checks: event source=phase, type=advance, to_state=executing
- [ ] Queries `run_agents` for agents with status `pending` for the run
- [ ] Calls existing `dispatch.Spawn()` for each pending agent
- [ ] Logs spawn results (success/failure per agent)
- [ ] No-op if no pending agents exist for the run
- [ ] Configurable: can be disabled via `--no-auto-spawn` flag or `auto_spawn=false` state key

### F5: Shell Hook Trigger Handler
**What:** Executes convention-based shell hooks after phase and dispatch events.
**Acceptance criteria:**
- [ ] Phase events trigger `.clavain/hooks/on-phase-advance` (if exists and executable)
- [ ] Dispatch events trigger `.clavain/hooks/on-dispatch-change` (if exists and executable)
- [ ] Event data passed as JSON on stdin: `{"run_id", "source", "type", "from_state", "to_state", "reason", "timestamp"}`
- [ ] Hook exit code: 0 = success, non-zero = logged as warning, never blocks the parent operation
- [ ] Hook execution is synchronous but with a 5-second timeout (kill after timeout)
- [ ] Hook path resolved relative to project directory (from run's `project_dir`)

### F6: `ic events tail` CLI Command
**What:** A CLI command that streams events as JSON lines with cursor support.
**Acceptance criteria:**
- [ ] `ic events tail <run_id>` — events for a specific run
- [ ] `ic events tail --all` — events across all runs
- [ ] `--since-phase=<id>` / `--since-dispatch=<id>` — replay from specific per-table event IDs (dual cursors, never a single `--since` that conflates independent ID spaces)
- [ ] `--follow` / `-f` — continuous polling mode (default: exit after current events)
- [ ] `--consumer=<name>` — persist cursor in `state` table as `cursor:<name>` key
- [ ] `--poll-interval=<duration>` — configurable poll interval (default 500ms)
- [ ] Output: one JSON object per line (JSON Lines format)
- [ ] Consumer cursor auto-pruned via existing state TTL (24h default)
- [ ] `ic events cursor list` — show all active consumer cursors
- [ ] `ic events cursor reset <name>` — reset a consumer's cursor to 0

### F7: Bash Library Wrappers
**What:** `lib-intercore.sh` additions for event bus operations.
**Acceptance criteria:**
- [ ] `intercore_events_tail <run_id> [--since=N]` — one-shot event dump
- [ ] `intercore_events_follow <run_id>` — follow mode for hooks (background process)
- [ ] `intercore_events_cursor_set <name> <event_id>` — set cursor position
- [ ] `intercore_events_cursor_get <name>` — get current cursor position
- [ ] All wrappers check `intercore_available()` first (fail-open pattern)
- [ ] Version bump to `0.6.0`

## Non-goals

- Cross-process event streaming (Unix socket, gRPC, WebSocket)
- Event filtering/routing rules (consumers filter client-side)
- General-purpose event bus for non-phase/dispatch domains
- Durable subscriptions surviving schema migrations
- Event replay/compaction/archival
- GUI/TUI event viewer (consumers of `ic events tail` build their own)

## Dependencies

- Wave 1 (policy engine) — **complete** (`iv-tq6i` closed)
- `modernc.org/sqlite` — existing dependency, no new Go deps needed
- Existing `state` table — used for consumer cursors (no schema change needed for this)
- Existing `dispatch.Spawn()` — used by auto-agent-spawn handler

## Open Questions

1. **Hook async vs sync** — PRD specifies sync with 5s timeout. If real-world usage shows hooks are slow, we may revisit with goroutine+channel pattern in a future iteration.
2. **Unified events table vs dual tables** — PRD keeps `phase_events` unchanged and adds `dispatch_events`. The `EventStore.ListEvents` merges them with a UNION query. If performance is an issue with large event volumes, we may add a materialized `events` view later.
