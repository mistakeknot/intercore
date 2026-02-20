# Intercore Wave 2: Event Bus — Reactive Phase Transitions
**Bead:** iv-k1xt
**Phase:** brainstorm (as of 2026-02-19T03:22:26Z)

## What We're Building

An event bus for intercore that makes phase transitions reactive instead of inert. Today, `Advance()` writes to the DB and returns — nothing happens next. Wave 2 adds three layers:

1. **In-process Notifier** — A Go pub/sub within `ic` that fires registered handlers after phase advances and dispatch status changes. Handlers include logging, auto-agent-spawn, and shell hook triggers.

2. **Polling cursor for external consumers** — `ic events tail` command that polls `phase_events` and dispatch events with at-least-once delivery. Consumers store their cursor position in the existing `state` table.

3. **`ic events tail` CLI** — A `tail -f`-style command that streams events to stdout as JSON lines. Supports `--since=<event_id>` for replay and `--follow` for continuous polling.

## Why This Approach

**Problem:** After Wave 1's policy engine made gates real, the system can now evaluate whether a phase *should* advance — but nothing reacts *after* it does. Hooks must poll, agents aren't auto-spawned, and there's no way to tail activity.

**Approach:** Two complementary patterns serve different consumers:
- **In-process Notifier** for reactions that should happen immediately within the `ic` process (logging, auto-spawn, hook exec)
- **Polling cursor** for external consumers that run in separate processes (TUI dashboards, monitoring scripts, other agents)

This avoids the complexity of IPC or a message broker while covering all consumer needs.

## Key Decisions

### Event Scope: Phase + Dispatch Events
Events cover two domains:
- **Phase events** (existing `phase_events` table): advance, skip, block, pause, complete, cancel, gate override
- **Dispatch events** (from `dispatches` table): spawned, running, completed, failed, timeout, cancelled

This gives a rich picture of run activity without going full general-purpose. A unified event stream merges both sources ordered by timestamp.

### Delivery: At-Least-Once with Cursor
- Consumer cursors stored in the `state` table as `cursor:<consumer-name>` keys with the project as scope
- On restart, consumers replay from their last-seen event ID
- Idempotent consumers handle duplicates (event IDs are monotonic)
- No new tables needed — reuses existing `state` infrastructure with TTL for abandoned cursors

### In-Process Notifier: Three Built-In Reactions
1. **Event logging** — Structured log output for every event (debug observability)
2. **Auto-agent-spawn** — When phase advances to `executing`, spawn agents registered for that run. Uses existing `dispatch spawn` machinery.
3. **Shell hook trigger** — Executes `.clavain/hooks/on-phase-advance` (if it exists) with event data as JSON on stdin. Convention-based, no registration needed. Mirrors git hook patterns.

### Hook Integration: Convention Over Configuration
- Hook path: `.clavain/hooks/on-phase-advance` (executable)
- Event data: JSON object on stdin with `run_id`, `from_phase`, `to_phase`, `event_type`, `reason`, `timestamp`
- Exit code: 0 = success, non-zero = logged but doesn't block the advance (fire-and-forget)
- Optional: `.clavain/hooks/on-dispatch-change` for dispatch events

### CLI: `ic events tail`
- `ic events tail <run_id>` — Stream events for a specific run
- `ic events tail --all` — Stream events across all runs
- `--since=<event_id>` — Replay from a specific event
- `--follow` / `-f` — Keep polling (default: exit after current events)
- `--json` — Raw JSON lines output (default)
- `--consumer=<name>` — Persist cursor position for named consumer
- Poll interval: configurable, default 500ms

### Architecture: Notifier as Interface
```
type Notifier interface {
    Subscribe(handler EventHandler)
    Notify(ctx context.Context, event Event)
}

type EventHandler func(ctx context.Context, event Event) error
```
- Synchronous dispatch (handlers run in-process, sequentially)
- Advance() calls `notifier.Notify()` after DB commit succeeds
- Handlers registered at ic startup based on configuration
- No goroutine pool — keep it simple, handlers should be fast

### Unified Event Type
```
type Event struct {
    ID        int64
    RunID     string
    Source    string   // "phase" or "dispatch"
    Type     string   // "advance", "skip", "block", "spawned", "completed", etc.
    FromState string   // from_phase or from_status
    ToState   string   // to_phase or to_status
    Reason    string   // JSON evidence or human reason
    Timestamp time.Time
}
```

## Open Questions

1. **Should dispatch events get their own `dispatch_events` table** or should we unify into a single `events` table? The single-table approach is simpler for `ic events tail` but mixes concerns. The dual-table approach (keep `phase_events`, add `dispatch_events`) preserves existing schema stability.

2. **Should hook execution be async (goroutine) or sync?** Sync is simpler and predictable but slow hooks would delay the advance return. Async risks losing context if `ic` exits quickly.

3. **Should `ic events tail --follow` use polling or filesystem notification (inotify on the SQLite WAL)?** Polling is portable and simple. Inotify would give sub-millisecond latency but adds platform-specific complexity.

4. **Consumer cursor cleanup** — Should abandoned cursors be auto-pruned via TTL, or require explicit `ic events unsubscribe`? TTL via the existing `state` table TTL mechanism seems natural.

## Non-Goals (Wave 3+)

- Cross-process event streaming (Unix socket, gRPC)
- Event filtering/routing rules
- Durable subscriptions surviving schema migrations
- General-purpose event bus for non-phase/dispatch domains
- Event replay/compaction
