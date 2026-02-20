# Intercore E2: Event Reactor Completion — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Goal:** Complete E2 acceptance criteria: document the OS-level event reactor pattern, add integration tests for spawn wiring, and update AGENTS.md.

**Architecture:** No kernel code changes. This is documentation + tests. The event bus, spawn handler, consumer cursors, and tail follow mode are all already implemented and passing.

**Tech Stack:** Go 1.22, existing intercore test patterns.

**Bead:** iv-9plh
**Phase:** executing (as of 2026-02-19T18:37:51Z)

---

## Plan Review Amendments (flux-drive 2026-02-19)

The following amendments incorporate P1 findings from the 3-agent flux-drive review:

1. **Task 1 — Phantom CLI flag**: The dispatch completion example uses `ic dispatch list --active --run="$run_id"` but `--run` does not exist. `ic dispatch list` only supports `--active`. Fix: filter dispatches by run_id using `ic dispatch list --active --json | jq '[.[] | select(.run_id == "'"$run_id"'")]'` instead.

2. **Task 2 — Mock field name**: Plan uses `&mockQuerier{ids: [...]}` but actual mock struct has field `agents`. Fix: use `&mockQuerier{agents: [...]}`.

3. **Task 2 — Event Type mismatch**: Plan uses `Type: "phase_advance"` but existing tests and store use `Type: "advance"`. Fix: use `Type: "advance"`.

4. **Task 2 — Log assertion prefix**: Plan checks for `"auto-spawn: agent agent-1 started"` but handler emits `"[event] auto-spawn: agent agent-1 started"`. Fix: include `[event] ` prefix in assertion.

5. **Task 2 — Duplicate negative test**: `TestSpawnWiringIntegration_NonExecutingPhase` duplicates existing `TestSpawnHandler_IgnoresNonExecuting`. Drop the duplicate — the integration test's value is testing the Notifier→Handler chain, not re-testing handler filtering.

6. **Task 3 — Stale AGENTS.md entry**: The existing handlers table says `SpawnHandler | Not wired | Scaffolded`. Must update to `SpawnHandler | Always | Auto-spawns agents when phase transitions to "executing"` (wired at run.go:336).

7. **Task 4 — Race detector**: Project convention (CLAUDE.md) is `go test -race ./...`. Add `-race` flag.

---

### Task 1: Event Reactor Pattern Documentation

**Files:**
- Create: `infra/intercore/docs/event-reactor-pattern.md`

**What to write:**

The document should cover these sections:

1. **Overview** — What is an event reactor? A long-running process that consumes kernel events via `ic events tail -f` and dispatches actions. The kernel is stateless between calls; the reactor is an OS component.

2. **Quick Start** — Minimal bash example:
   ```bash
   ic events tail --all -f --consumer=my-reactor --poll-interval=1s | while IFS= read -r line; do
     type=$(echo "$line" | jq -r '.type')
     to_state=$(echo "$line" | jq -r '.to_state')
     run_id=$(echo "$line" | jq -r '.run_id')
     # ... dispatch to handler
   done
   ```

3. **Consumer Registration** — Explain `--consumer=<name>`:
   - Named consumers get cursor persistence (auto-saved after each batch)
   - Default cursor TTL is 24h (auto-cleaned)
   - Without `--consumer`, events replay from the beginning each time
   - `ic events cursor list` to see active consumers
   - `ic events cursor reset <name>` to replay from start

4. **Event Schema** — JSON fields: `run_id`, `source` (phase|dispatch), `type` (phase_advance|status_change|skip|budget_warning|budget_exceeded), `from_state`, `to_state`, `reason`, `timestamp`, `phase_event_id`/`dispatch_event_id`

5. **Common Patterns:**

   a. **Phase-triggered actions:** React to `to_state == "executing"` to spawn agents
   ```bash
   if [[ "$type" == "phase_advance" && "$to_state" == "review" ]]; then
     ic dispatch spawn --prompt-file=".ic/prompts/reviewer.md" --project="$project" --name="review-agent"
   fi
   ```

   b. **Dispatch completion:** React to `to_state == "completed"` + check if all dispatches done → advance phase
   ```bash
   if [[ "$type" == "status_change" && "$to_state" == "completed" ]]; then
     active=$(ic dispatch list --active --run="$run_id" --json | jq 'length')
     if [[ "$active" == "0" ]]; then
       ic run advance "$run_id"
     fi
   fi
   ```

   c. **Budget alerts:** React to `budget.warning` and `budget.exceeded` events

6. **Idempotency** — Events use at-least-once delivery. Consumers MUST handle duplicates. Pattern: check current state before acting (e.g., don't spawn if agent already exists).

7. **Error Handling** — If the consumer crashes:
   - Named consumers resume from last cursor position on restart
   - Unnamed consumers replay everything (usually not what you want)
   - Use `--poll-interval` to control CPU usage (default: instant poll)
   - Pipe breaks (`SIGPIPE`) require the consumer to restart the `ic events tail` process

8. **Lifecycle** — Process management options:
   - Systemd unit (recommended for production)
   - Bash background process (ok for dev)
   - Clavain session hook (runs per-session, not global)

**Acceptance:** File exists, covers all 8 sections, examples are syntactically correct.

### Task 2: Integration Test — SpawnHandler Wiring via Notifier

**Files:**
- Modify: `infra/intercore/internal/event/handler_spawn_test.go`

**Step 1: Write the test**

Add `TestSpawnWiringIntegration` that mirrors the cmdRunAdvance wiring:

```go
func TestSpawnWiringIntegration(t *testing.T) {
	// 1. Create a notifier (same as cmdRunAdvance)
	notifier := NewNotifier()

	// 2. Create mocks
	querier := &mockQuerier{ids: []string{"agent-1", "agent-2"}}
	var spawned []string
	spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
		spawned = append(spawned, agentID)
		return nil
	})

	// 3. Subscribe (same pattern as cmdRunAdvance line 336)
	var logBuf bytes.Buffer
	notifier.Subscribe("spawn", NewSpawnHandler(querier, spawner, &logBuf))

	// 4. Fire a phase event to "executing" (simulates Advance callback)
	ctx := context.Background()
	notifier.Notify(ctx, Event{
		RunID:     "test-run-1",
		Source:    SourcePhase,
		Type:      "phase_advance",
		FromState: "planned",
		ToState:   "executing",
		Timestamp: time.Now(),
	})

	// 5. Assert spawns happened
	if len(spawned) != 2 {
		t.Fatalf("spawned %d agents, want 2", len(spawned))
	}
	if spawned[0] != "agent-1" || spawned[1] != "agent-2" {
		t.Errorf("spawned = %v, want [agent-1, agent-2]", spawned)
	}

	// 6. Assert log output
	if !strings.Contains(logBuf.String(), "auto-spawn: agent agent-1 started") {
		t.Errorf("missing log for agent-1: %s", logBuf.String())
	}
}
```

Also add a negative test:

```go
func TestSpawnWiringIntegration_NonExecutingPhase(t *testing.T) {
	notifier := NewNotifier()
	querier := &mockQuerier{ids: []string{"agent-1"}}
	var spawned []string
	spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
		spawned = append(spawned, agentID)
		return nil
	})
	notifier.Subscribe("spawn", NewSpawnHandler(querier, spawner, nil))

	ctx := context.Background()
	notifier.Notify(ctx, Event{
		RunID:     "test-run-1",
		Source:    SourcePhase,
		Type:      "phase_advance",
		FromState: "brainstorm",
		ToState:   "planned",  // NOT "executing"
		Timestamp: time.Now(),
	})

	if len(spawned) != 0 {
		t.Fatalf("spawned %d agents for non-executing phase, want 0", len(spawned))
	}
}
```

**Acceptance:** `go test ./internal/event/...` passes with both new tests.

### Task 3: Update AGENTS.md — Event Reactor Section

**Files:**
- Modify: `infra/intercore/AGENTS.md`

**What to add:** After the existing "Event Bus" section (or at the end of the architecture section), add:

```markdown
## Event Reactor Pattern

The kernel emits events but does not react to them. OS components (Clavain, Interspect, custom scripts) subscribe as event consumers.

### Quick Reference

```bash
# Start a consumer (long-running, cursor-persisted)
ic events tail --all -f --consumer=my-reactor --poll-interval=1s

# One-shot read (no cursor, all events)
ic events tail --all

# Filter by run
ic events tail <run-id> -f --consumer=my-reactor

# Manage cursors
ic events cursor list
ic events cursor reset <consumer-name>
```

### Consumer Guidelines

- Always use `--consumer=<name>` for durability (cursor survives restarts)
- Consumers MUST be idempotent — events are at-least-once
- Use `--poll-interval` to control CPU (500ms-2s recommended)
- Check `docs/event-reactor-pattern.md` for full patterns and examples
```

**Acceptance:** AGENTS.md has an "Event Reactor Pattern" section with the quick reference.

### Task 4: Run Full Test Suite

```bash
cd infra/intercore && go test ./...
```

Verify all existing tests pass plus new integration tests.

**Acceptance:** Exit code 0, all packages pass.

## Ordering

Task 1 (docs) and Task 2 (tests) are independent — can run in parallel.
Task 3 (AGENTS.md) depends on Task 1 (needs to link to the doc).
Task 4 (test suite) depends on Task 2.

```
Task 1 ──→ Task 3
Task 2 ──→ Task 4
```

## Risk

- **Very low.** No kernel code changes. Documentation is self-contained. Tests use existing mock patterns.
