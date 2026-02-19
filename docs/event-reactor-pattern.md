# Event Reactor Pattern

An **event reactor** is a long-running process that consumes kernel events via `ic events tail -f` and dispatches actions in response. The kernel is stateless between CLI calls — it writes events to SQLite but never acts on them. Reactors are OS components (Clavain hooks, Interspect scripts, custom automation) that subscribe to events and make decisions.

This document explains how to build event reactors using the intercore event bus.

## Quick Start

Minimal bash reactor that reacts to phase transitions:

```bash
#!/usr/bin/env bash
set -euo pipefail

ic events tail --all -f --consumer=my-reactor --poll-interval=1s | while IFS= read -r line; do
  source=$(echo "$line" | jq -r '.source')
  type=$(echo "$line" | jq -r '.type')
  to_state=$(echo "$line" | jq -r '.to_state')
  run_id=$(echo "$line" | jq -r '.run_id')

  case "${source}:${type}" in
    phase:advance)
      echo "[reactor] Phase advance to ${to_state} for run ${run_id}"
      ;;
    dispatch:status_change)
      echo "[reactor] Dispatch status → ${to_state} for run ${run_id}"
      ;;
  esac
done
```

## Consumer Registration

The `--consumer=<name>` flag gives your reactor a durable cursor:

```bash
# Named consumer — cursor persisted, survives restarts
ic events tail --all -f --consumer=clavain-reactor --poll-interval=1s

# Anonymous consumer — replays from start every time (avoid for reactors)
ic events tail --all -f
```

### Cursor Behavior

- **Persistence:** After each batch of events, the cursor (high-water mark) is saved to the state store. On restart, events resume from where you left off.
- **TTL:** Cursors have a 24-hour TTL. If your consumer doesn't poll for 24 hours, the cursor expires and the next poll replays from the oldest available event.
- **Scope:** If you tail a specific run (`ic events tail <run-id> -f --consumer=X`), the cursor is scoped to that run. If you tail `--all`, the cursor covers all runs.

### Cursor Management

```bash
# List active consumers and their cursor positions
ic events cursor list

# Reset a consumer's cursor (replays from start)
ic events cursor reset my-reactor
```

## Event Schema

Events are emitted as JSON objects, one per line:

```json
{
  "id": 42,
  "run_id": "run-abc123",
  "source": "phase",
  "type": "advance",
  "from_state": "planned",
  "to_state": "executing",
  "reason": "",
  "timestamp": "2026-02-19T12:00:00Z"
}
```

### Fields

| Field | Type | Description |
|-------|------|-------------|
| `id` | int | Auto-increment ID (used for cursor tracking) |
| `run_id` | string | Run this event belongs to |
| `source` | string | `"phase"` or `"dispatch"` |
| `type` | string | Event type (see below) |
| `from_state` | string | Previous phase/status |
| `to_state` | string | New phase/status |
| `reason` | string | Optional reason (e.g., skip reason, gate block reason) |
| `timestamp` | string | ISO 8601 timestamp |

### Event Types by Source

**Phase events** (`source: "phase"`):
- `advance` — Phase transitioned (e.g., `planned` → `executing`)
- `skip` — Phase was skipped (via `ic run skip`)
- `block` — Gate blocked the advance

**Dispatch events** (`source: "dispatch"`):
- `status_change` — Dispatch status changed (e.g., `spawned` → `running` → `completed`)

**Budget events** (`source: "phase"`, `type` varies):
- `budget.warning` — Token usage crossed warning threshold
- `budget.exceeded` — Token usage exceeded budget

**Other phase events:**
- `pause` — Auto-advance was disabled; run is paused at this phase

## Common Patterns

### Phase-Triggered Agent Spawning

Spawn review agents when a run enters the review phase:

```bash
ic events tail --all -f --consumer=agent-spawner --poll-interval=1s | while IFS= read -r line; do
  source=$(echo "$line" | jq -r '.source')
  type=$(echo "$line" | jq -r '.type')
  to_state=$(echo "$line" | jq -r '.to_state')

  if [[ "$source" == "phase" && "$type" == "advance" && "$to_state" == "review" ]]; then
    run_id=$(echo "$line" | jq -r '.run_id')
    ic dispatch spawn --prompt-file=".ic/prompts/reviewer.md" \
      --project="." --name="review-agent"
  fi
done
```

### Dispatch Completion → Phase Advance

Automatically advance a run when all its dispatches complete:

```bash
ic events tail --all -f --consumer=auto-advancer --poll-interval=1s | while IFS= read -r line; do
  source=$(echo "$line" | jq -r '.source')
  to_state=$(echo "$line" | jq -r '.to_state')

  if [[ "$source" == "dispatch" && "$to_state" == "completed" ]]; then
    run_id=$(echo "$line" | jq -r '.run_id')

    # Check if ALL dispatches for this run are complete
    # Note: ic dispatch list --active returns all active dispatches (no --run filter)
    # Filter by run_id client-side
    active=$(ic dispatch list --active --json 2>/dev/null \
      | jq --arg rid "$run_id" '[.[] | select(.run_id == $rid)] | length')

    if [[ "$active" == "0" ]]; then
      echo "[reactor] All dispatches complete for $run_id — advancing"
      ic run advance "$run_id"
    fi
  fi
done
```

### Budget Alerting

React to budget threshold events:

```bash
ic events tail --all -f --consumer=budget-monitor --poll-interval=2s | while IFS= read -r line; do
  type=$(echo "$line" | jq -r '.type')

  case "$type" in
    budget.warning)
      run_id=$(echo "$line" | jq -r '.run_id')
      echo "[WARN] Token budget warning for run $run_id"
      ;;
    budget.exceeded)
      run_id=$(echo "$line" | jq -r '.run_id')
      echo "[ALERT] Token budget EXCEEDED for run $run_id"
      # Could: pause dispatches, notify user, etc.
      ;;
  esac
done
```

## Idempotency

Events use **at-least-once delivery**. If your reactor crashes and restarts, it may re-process events that were already handled (between the last cursor save and the crash).

**Your reactor MUST be idempotent.** Before acting on an event, check current state:

```bash
# Before spawning: check if agent already exists
agent_count=$(ic run agent list "$run_id" --json | jq '[.[] | select(.name == "reviewer")] | length')
if [[ "$agent_count" == "0" ]]; then
  ic dispatch spawn ...
fi

# Before advancing: check current phase
current=$(ic run phase "$run_id")
if [[ "$current" != "done" ]]; then
  ic run advance "$run_id"
fi
```

## Error Handling

### Consumer Crashes

Named consumers resume from their last cursor position. Events between the last save and the crash are replayed (this is why idempotency matters).

### Pipe Breaks

If the `ic events tail` process dies (killed, OOM, etc.), the `while read` loop exits. Your wrapper should restart it:

```bash
while true; do
  ic events tail --all -f --consumer=my-reactor --poll-interval=1s | while IFS= read -r line; do
    # ... handle events
  done
  echo "[reactor] Pipe broke, restarting in 5s..."
  sleep 5
done
```

### Database Locked

If another process holds the SQLite write lock, `ic events tail` waits (busy timeout is configured at DB open). This is transparent — the consumer just sees a slower poll cycle.

## Lifecycle Management

### Systemd (Recommended for Production)

```ini
[Unit]
Description=Intercore Event Reactor
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/my-reactor.sh
Restart=always
RestartSec=5
Environment=IC_DB=/path/to/intercore.db

[Install]
WantedBy=default.target
```

### Bash Background Process (Development)

```bash
# Start in background
./my-reactor.sh &
REACTOR_PID=$!

# Stop
kill "$REACTOR_PID"
```

### Clavain Session Hook (Per-Session)

A session-scoped reactor can start in a `SessionStart` hook and die with the session. This is appropriate for UI-coupled reactions (e.g., updating a status display) but not for durable automation (the reactor dies when the session ends).

## Design Guidelines

1. **One reactor per concern.** Don't build a monolith reactor that handles everything. Separate concerns: `agent-spawner`, `auto-advancer`, `budget-monitor`.

2. **Prefer polling over tight loops.** `--poll-interval=1s` is fine for most reactors. Sub-second polling adds CPU cost with minimal latency benefit.

3. **The kernel is the source of truth.** Don't cache state in your reactor — always query `ic run phase`, `ic dispatch list`, etc. for current state before acting.

4. **Log actions.** Every action your reactor takes should be logged. When debugging, you need to trace: event received → state checked → action taken.

5. **Fail open, not closed.** If your reactor can't determine state (DB error, command failure), skip the event and log a warning. The next poll cycle will replay it (cursor hasn't advanced past a failed event).
