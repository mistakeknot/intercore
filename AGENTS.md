# AGENTS.md — intercore

## Overview

intercore is the kernel layer (Layer 1) of the Clavain autonomous software agency. It is a host-agnostic Go CLI (`ic`) backed by a single SQLite WAL database that provides the durable system of record for runs, phases, gates, dispatches, events, and token budgets. The kernel is mechanism, not policy — it doesn't know what "brainstorm" means, only that a phase transition happened and needs recording.

In the three-layer architecture, intercore sits beneath the OS (Clavain, Layer 2) and drivers (companion plugins, Layer 3). If the host platform changes, the kernel and all its data survive untouched. Bash hooks and the Clavain sprint pipeline call `ic` for all state operations instead of writing to temp files.

**Location:** `infra/intercore/` (infrastructure, not a plugin — hooks depend on it)
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)

## Architecture

```
cmd/ic/main.go          CLI entry point, argument parsing, shared helpers
cmd/ic/dispatch.go      Dispatch subcommands (spawn, status, list, poll, wait, kill, prune)
cmd/ic/run.go           Run subcommands (create, status, advance, phase, current, agent, artifact)
cmd/ic/gate.go          Gate subcommands (check, override, rules)
cmd/ic/portfolio.go     Portfolio subcommands (dep add/list/remove, relay)
cmd/ic/lock.go          Lock subcommands (acquire, release, list, stale, clean) — filesystem-only
internal/db/db.go       SQLite connection, migration, health check
internal/db/schema.sql  Embedded DDL (tables: state, sentinels, dispatches, runs, phase_events, dispatch_events, run_agents, run_artifacts, project_deps)
internal/db/disk.go     Disk space check (Linux syscall)
internal/state/         State CRUD with JSON validation and TTL
internal/sentinel/      Atomic throttle guards with UPDATE+RETURNING
internal/dispatch/      Agent dispatch lifecycle: spawn, poll, collect, wait
  dispatch.go           Store (CRUD), Dispatch struct, ID generation
  spawn.go              Process spawning, dispatch.sh resolution, prompt hashing
  collect.go            Liveness polling, verdict/summary parsing, wait loop
internal/phase/         Phase state machine: run lifecycle with configurable chains
  phase.go              Types, constants, chain operations, DefaultPhaseChain
  store.go              Run + PhaseEvent CRUD with optimistic concurrency, Current()
  machine.go            Advance() with gate evaluation, skip-walk, auto_advance pause
  gate.go               Gate types, interfaces, rules table, evaluateGate, EvaluateGate
  errors.go             Error sentinels
internal/portfolio/     Portfolio orchestration: cross-project coordination
  deps.go               DepStore: project dependency CRUD with self-loop prevention
  dbpool.go             DBPool: read-only cached handles to child project databases
  relay.go              Relay: polls child DBs, relays events, tracks dispatch counts
  deps_test.go          Unit tests (7 tests)
internal/budget/        Token budget threshold checking with dedup
  budget.go             Checker, narrow interfaces, state-based dedup
internal/lock/          Filesystem-based mutex using POSIX mkdir atomicity
  lock.go               Manager, Acquire (spin-wait + stale-break), Release, List, Clean
  lock_test.go          Unit tests (8 tests, race-detector safe)
internal/event/         Event bus: Event type, EventStore, Notifier, handlers
  event.go              Event struct, source constants (SourcePhase, SourceDispatch)
  store.go              EventStore: AddPhaseEvent, AddDispatchEvent, ListEvents, ListAllEvents (UNION + dual cursors)
  notifier.go           Notifier: Subscribe/Notify with named handlers
  handler_log.go        LogHandler: logs events to stderr (quiet mode suppresses)
  handler_spawn.go      SpawnHandler: auto-agent-spawn on phase transitions + AgentSpawnerFunc adapter
  handler_hook.go       HookHandler: executes .clavain/hooks/on-event.sh async (goroutine, 5s timeout)
cmd/ic/events.go        CLI: ic events tail (dual cursor), ic events cursor (list, reset)
internal/runtrack/      Agent and artifact tracking within runs
  runtrack.go           Agent/Artifact types, status constants
  store.go              CRUD for run_agents and run_artifacts
  errors.go             Error sentinels (ErrAgentNotFound, ErrArtifactNotFound)
lib-intercore.sh        Bash wrappers for hooks (v0.6.0)
test-integration.sh     End-to-end integration test (~93 tests)
```

## CLI Commands

```
ic init                                    Create/migrate the database
ic health                                  Check DB readable, schema current, disk space
ic version                                 Print CLI and schema versions
ic sentinel check <name> <scope> --interval=<sec>   Atomic claim (exit 0=allowed, 1=throttled)
ic sentinel reset <name> <scope>           Clear a sentinel
ic sentinel list                           List all sentinels
ic sentinel prune --older-than=<dur>       Remove old sentinels
ic state set <key> <scope> [--ttl=<dur>]   Write JSON (stdin or @filepath)
ic state get <key> <scope>                 Read JSON (exit 0=found, 1=not found)
ic state delete <key> <scope>              Remove a state entry
ic state list <key>                        List scope_ids for a key
ic state prune                             Remove expired state entries
ic dispatch spawn [flags]                  Spawn an agent dispatch (prints ID)
ic dispatch status <id>                    Show dispatch details
ic dispatch list [--active] [--scope=<s>]  List dispatches
ic dispatch poll <id>                      Check liveness, update stats
ic dispatch wait <id> [--timeout=<dur>]    Block until terminal or timeout
ic dispatch kill <id>                      SIGTERM → SIGKILL a dispatch
ic dispatch tokens <id> --set --in=N --out=N [--cache=N]   Update token counts
ic dispatch prune --older-than=<dur>       Remove old terminal dispatches
ic run create --project=<dir> --goal=<text> [--complexity=N] [--scope-id=S] [--phases='[...]'] [--token-budget=N] [--budget-warn-pct=N]
ic run status <id>                         Show run details
ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]
ic run phase <id>                          Print current phase (scripting)
ic run list [--active] [--scope=S]         List runs
ic run events <id>                         Phase event audit trail
ic run cancel <id>                         Cancel a run
ic run current [--project=<dir>]            Print active run ID for project (exit 0=found, 1=none)
ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]
ic run skip <id> <phase> [--reason=<text>] [--actor=<name>]   Pre-skip a phase (audit trail)
ic run tokens <id> [--project=<dir>] [--json]                 Token aggregation across dispatches
ic run budget <id> [--json]                                   Check budget thresholds (exit 1=exceeded)
ic run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
ic run agent list <run>                    List agents for a run
ic run agent update <id> --status=<s>      Update agent status (active|completed|failed)
ic run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
ic run artifact list <run> [--phase=<p>]   List artifacts for a run
ic events tail <run_id> [flags]             Tail events for a run (JSON lines)
ic events tail --all [flags]               Tail events across all runs
ic events cursor list                      List consumer cursors
ic events cursor reset <consumer>          Reset a consumer cursor
ic gate check <run_id> [--priority=N]      Dry-run gate evaluation (exit 0=pass, 1=fail)
ic gate override <run_id> --reason=<text>  Force-advance past a failed gate
ic gate rules [--phase=<p>]                Display gate rules table
ic lock acquire <name> <scope> [--timeout=<dur>] [--owner=<id>]  Acquire lock (exit 0=acquired, 1=contention)
ic lock release <name> <scope> [--owner=<id>]                   Release lock (verifies owner)
ic lock list                               List all held locks
ic lock stale [--older-than=<dur>]         List stale locks
ic lock clean [--older-than=<dur>]         Remove stale locks (PID-liveness check)
ic run create --projects=<p1>,<p2> --goal=<text> [--max-dispatches=N]   Create portfolio run with children
ic run set <id> --max-dispatches=N         Set max concurrent dispatches (portfolio only)
ic run list --portfolio                    List portfolio runs only
ic portfolio dep add <id> --upstream=<path> --downstream=<path>   Add project dependency
ic portfolio dep list <id>                 List dependencies for a portfolio
ic portfolio dep remove <id> --upstream=<path> --downstream=<path>   Remove dependency
ic portfolio relay <id> [--interval=2s]    Start event relay (blocks, SIGINT to stop)
ic compat status                           Show legacy temp file vs DB coverage
ic compat check <key>                      Check if key has data in DB
```

### Exit Codes

| Code | Meaning | Example |
|------|---------|---------|
| 0 | Success / allowed / found | `ic state get` returns payload |
| 1 | Expected negative result | `ic sentinel check` → throttled |
| 2 | Unexpected error | Invalid JSON, DB corruption |
| 3 | Usage error | Missing required argument |

### Global Flags

- `--db=<path>` — Database path (default: `.clavain/intercore.db`, auto-discovered)
- `--timeout=<dur>` — SQLite busy timeout (default: 100ms)
- `--verbose` — Verbose output
- `--json` — JSON output

## Dispatch Module

The dispatch module tracks Codex agent lifecycle in the SQLite DB. Go owns the lifecycle tracking; `dispatch.sh` remains the execution engine.

### Lifecycle

```
ic dispatch spawn  → INSERT (status=spawned) → fork dispatch.sh → UPDATE (status=running, pid)
ic dispatch poll   → kill(pid,0) liveness → read state file → UPDATE stats
ic dispatch wait   → poll loop → on timeout: SIGTERM/SIGKILL → status=timeout
ic dispatch collect → read .verdict + .summary sidecars → UPDATE final results
```

### State Flow

```
spawned → running → completed | failed | timeout | cancelled
```

### dispatch.sh Resolution (spawn)

1. `--dispatch-sh=<path>` flag
2. `CLAVAIN_DISPATCH_SH` env var
3. Walk up from CWD for `hub/clavain/scripts/dispatch.sh`
4. Fallback: bare `codex exec` (no JSONL, no verdict)

### Spawn Flags

```
--type=codex          Agent type (default: codex)
--prompt-file=<path>  Required: prompt file path
--project=<dir>       Required: working directory (default: CWD)
--output=<path>       Output file path (auto-generated if omitted)
--name=<label>        Human-readable label
--model=<model>       Codex model
--sandbox=<mode>      Sandbox mode (default: workspace-write)
--timeout=<dur>       Agent timeout
--scope-id=<id>       Grouping scope
--parent-id=<id>      Parent dispatch ID (fan-out tracking)
--dispatch-sh=<path>  Explicit dispatch.sh path
```

### Reparented Process Handling

When `ic dispatch spawn` exits after forking, dispatch.sh gets reparented to init. Later `ic dispatch poll` can't `waitpid()` it, so liveness uses three convergent signals:
- `kill(pid, 0)` returning ESRCH (process gone)
- State file (`/tmp/clavain-dispatch-{pid}.json`) disappearing
- `.verdict` and `.summary` sidecars appearing

### Bash Wrappers (lib-intercore.sh)

```bash
intercore_dispatch_spawn <type> <project> <prompt_file> [output] [name]
intercore_dispatch_status <id>
intercore_dispatch_wait <id> [timeout]
intercore_dispatch_list_active
intercore_dispatch_kill <id>
```

## Run Tracking Module

The run tracking module (schema v4) adds agent and artifact tracking within runs. Agents represent individual AI agents working on a run phase; artifacts represent files produced during a run.

### Tables

- `run_agents` — tracks agents within a run (FK: `run_id → runs.id`)
- `run_artifacts` — tracks files produced during a run (FK: `run_id → runs.id`)

Foreign keys are enforced (`PRAGMA foreign_keys = ON` set in `db.Open()`).

### Agent Status Flow

```
active → completed | failed
```

### ic run current

Returns the most recent active run for a project directory. If multiple active runs exist for the same project, returns the one with the latest `created_at`. JSON mode returns `{"found": true/false, ...}`.

### Bash Wrappers (lib-intercore.sh)

```bash
intercore_run_current [project_dir]                        # Print active run ID (exit 0=found, 1=none)
intercore_run_phase <run_id>                               # Print current phase
intercore_run_agent_add <run_id> <type> [name] [dispatch_id]  # Add agent, print ID
intercore_run_artifact_add <run_id> <phase> <path> [type]  # Add artifact, print ID
intercore_run_skip <run_id> <phase> [reason] [actor]       # Pre-skip a phase
intercore_run_tokens <run_id>                              # Token aggregation (JSON)
intercore_run_budget <run_id>                              # Budget check (JSON, exit 1=exceeded)
intercore_dispatch_tokens <id> <in> <out> [cache]          # Update dispatch token counts
intercore_gate_check <run_id>                              # Gate check (0=pass, 1=fail, 2+=error)
intercore_gate_override <run_id> <reason>                  # Force-advance past failed gate
```

## Lock Module

The lock module provides process-level mutual exclusion using POSIX `mkdir` atomicity — entirely filesystem-based, no SQLite involved. This separates concerns: SQLite sentinels handle time-based throttle guards; filesystem locks handle read-modify-write serialization.

### Lock Directory Layout

```
/tmp/intercore/locks/<name>/<scope>/owner.json
```

`owner.json` contains `{"pid": N, "host": "hostname", "owner": "PID:hostname", "created": <unix_epoch>}`. The `<name>/<scope>` subdirectory structure avoids ambiguity with hyphenated names.

### Acquire Behavior

1. `os.Mkdir` atomic attempt (fail = lock held)
2. Write `owner.json` with caller identity
3. On failure: spin-wait with 100ms sleep (`DefaultRetryWait`), check for stale locks
4. Stale detection: compare `owner.json` created timestamp against `StaleAge` (default 5s)
5. Stale-break: `os.Remove(owner.json)` + `os.Remove(lockDir)` (no `os.RemoveAll` — prevents destroying concurrently re-acquired locks)

### PID-Liveness Check (Clean)

`ic lock clean` uses `syscall.Kill(pid, 0)` before evicting:
- `nil` or `EPERM` → process alive, skip
- `ESRCH` → process gone, safe to remove

### Bash Wrappers (lib-intercore.sh)

```bash
intercore_lock_available                   # Binary-only check (no DB health — locks are filesystem-only)
intercore_lock <name> <scope> [timeout]    # 3-way exit: 0=acquired, 1=contention, 2+=fallback to mkdir
intercore_unlock <name> <scope>            # Fail-safe: always returns 0
intercore_lock_clean [max_age]             # Remove stale locks (fallback: find -not -newermt)
```

### Exit Codes (lock commands)

| Code | Meaning |
|------|---------|
| 0 | Lock acquired / released / cleaned |
| 1 | Expected negative: timeout (acquire), not-found or not-owner (release) |
| 2 | Unexpected error |
| 3 | Usage error |

### Input Validation

Name and scope components are validated: no `/`, `\`, `..`, empty, or `.` allowed. The resolved path must remain under `BaseDir` (containment check via `strings.HasPrefix`).

## Phase Module

The phase module implements a run lifecycle state machine ported from `lib-sprint.sh` + `lib-gates.sh`. intercore owns phase transitions instead of relying on LLM prompt instructions.

### Configurable Phase Chains (v6)

Runs can specify a custom phase chain via `--phases='["a","b","c"]'` at creation. If no chain is specified, the default 9-phase Clavain lifecycle is used:

```
brainstorm → brainstorm-reviewed → strategized → planned → executing → review → polish → reflect → done
```

Custom chains must have at least 2 phases and no duplicates. The last phase in the chain is terminal — reaching it sets `status=completed`.

### Phase Skip

Individual phases can be pre-skipped via `ic run skip <id> <phase>`. When `Advance()` encounters a pre-skipped phase, it automatically walks past it to the next non-skipped phase. Skip events are recorded in the audit trail.

### Legacy: Complexity-Based Skip (removed in v6)

The old `transitionTable`, `complexityWhitelist`, `NextRequiredPhase`, and `ShouldSkip` functions have been removed. The same effect is achieved by:
- **Short chains:** `--phases='["brainstorm","planned","executing","done"]'` (equivalent to complexity=1)
- **Explicit skip:** `ic run skip <id> <phase>` for selective skipping

### Gate System

Gates enforce conditions before phase transitions. Each transition has a set of checks defined in `gateRules` (see `internal/phase/gate.go`).

**Checks:**
- `artifact_exists` — requires at least one artifact recorded for the source phase
- `agents_complete` — requires no active agents (all completed or failed)
- `verdict_exists` — requires a non-rejected dispatch verdict (needs `scope_id` on the run)
- `children_at_phase` — (portfolio only) requires all active children at or past target phase

**Interfaces:** Gate evaluation uses `RuntrackQuerier`, `VerdictQuerier`, and `PortfolioQuerier` interfaces to avoid cross-package coupling. The actual implementations live in `runtrack.Store`, `dispatch.Store`, and `phase.Store`.

**Gate Tiers** (set by `--priority` flag):

| Priority | Tier | Behavior |
|----------|------|----------|
| 0-1 | Hard | Block advance if gate fails |
| 2-3 | Soft | Warn but allow advance |
| 4+ | None | Skip gate evaluation |

**Evidence:** Every gate evaluation produces a `GateEvidence` struct with per-condition results, serialized as JSON in the event's `reason` field. Both pass and block events include evidence.

**EvaluateGate vs Advance:** `EvaluateGate()` is a dry-run (read-only) used by `ic gate check`. `Advance()` evaluates the gate and applies the result (advance or block). Both record events.

**Override:** `ic gate override` force-advances past a failed gate. It calls `UpdatePhase` first, then records the event — if a crash occurs between, the advance happened without audit (safer than audit without advance).

## Event Bus Module

The event bus (schema v5) provides reactive notification of phase transitions and dispatch status changes. It uses an in-process `Notifier` with callback-based wiring — no cross-package interfaces.

### Architecture

```
phase.Advance() → PhaseEventCallback → Notifier.Notify() → handlers
dispatch.UpdateStatus() → DispatchEventRecorder → Notifier.Notify() → handlers
```

Callbacks fire **after DB commit** (fire-and-forget). Handler errors are logged but never fail the parent operation. The hook handler runs in a detached goroutine with `context.Background()` and a 5s timeout to avoid blocking the single DB connection.

### Tables

- `phase_events` — phase transitions (pre-existing, read by event bus)
- `dispatch_events` — dispatch status changes (new in v5). No FK on `dispatch_id` — dispatches may be pruned while events are retained.

### Dual Cursors

`ic events tail` tracks separate high-water marks for phase and dispatch events because they use independent AUTOINCREMENT sequences. Cursors are persisted in the `state` table with a 24h TTL for auto-cleanup.

### Handlers

| Handler | Registered | Behavior |
|---------|-----------|----------|
| LogHandler | Always | Logs events to stderr; quiet mode (default) suppresses output |
| HookHandler | Always | Executes `.clavain/hooks/on-event.sh` with event JSON on stdin; async goroutine |
| SpawnHandler | Always | Auto-spawns pending agents when phase transitions to "executing"; wired in `cmdRunAdvance` |

### PhaseEventCallback

The callback fires on **all** advance attempts: advance, skip, block, and pause. Consumers see the full lifecycle, not just successful transitions.

### Column Allowlist (dispatch.UpdateStatus)

`UpdateFields` map keys are validated against `allowedUpdateCols` before SQL interpolation. This prevents column injection via the exported `UpdateFields` type.

### Bash Wrappers (lib-intercore.sh v0.6.0)

```bash
intercore_events_tail <run_id> [flags]        # One-shot event dump (JSON lines)
intercore_events_tail_all [flags]             # Events across all runs
intercore_events_cursor_get <consumer>        # Get cursor JSON
intercore_events_cursor_set <consumer> <phase_id> <dispatch_id>  # Manual cursor set
intercore_events_cursor_reset <consumer>      # Reset cursor
```

### Event Reactor Pattern

The kernel emits events but does not react to them. OS components (Clavain, Interspect, custom scripts) subscribe as event consumers using `ic events tail -f`.

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

**Consumer guidelines:**
- Always use `--consumer=<name>` for durability (cursor survives restarts)
- Consumers MUST be idempotent — events are at-least-once
- Use `--poll-interval` to control CPU (500ms–2s recommended)
- See `docs/event-reactor-pattern.md` for full patterns, examples, and lifecycle management

### Optimistic Concurrency

`UpdatePhase` uses `WHERE phase = ?` with the expected current phase. If another process already advanced the run, 0 rows are affected → `ErrStalePhase`. This prevents two concurrent `ic run advance` invocations from double-advancing.

### Deployment: Schema Upgrade

When rebuilding after schema changes, follow the 3-step sequence:
```bash
go build -o /home/mk/go/bin/ic ./cmd/ic   # Rebuild (schema is //go:embed'd)
ic init                                     # Migrate live DB (creates backup)
ic version                                  # Verify schema version
```
See `docs/solutions/patterns/intercore-schema-upgrade-deployment-20260218.md` for details.

## Portfolio Orchestration Module

The portfolio module (schema v10) enables cross-project coordination through parent/child run hierarchies, project dependencies, and an event relay.

### Portfolio Runs

A portfolio run is identified by `project_dir = ""`. It has children linked via `parent_run_id`. Created with `ic run create --projects=p1,p2`.

```
ic run create --projects=/path/a,/path/b --goal="Multi-project feature" --max-dispatches=5
```

Paths are resolved to absolute. The portfolio run and all children are created atomically in a single transaction. Cancelling a portfolio cascades to all active children.

### Project Dependencies

The `project_deps` table stores directed dependency edges between child projects within a portfolio. Used by the relay to emit `upstream_changed` events.

```
ic portfolio dep add <portfolio-id> --upstream=/path/a --downstream=/path/b
```

Self-loop prevention: upstream == downstream is rejected. Paths are normalized to absolute via `filepath.Abs`.

### Event Relay

`ic portfolio relay <id>` starts a blocking polling loop that:
1. Opens child project databases in read-only mode (`?mode=ro`) via `DBPool`
2. Queries `phase_events` from each child since the last cursor
3. Inserts relay events into the portfolio's `phase_events` table
4. Persists cursors atomically in the same transaction as relay events
5. Counts active dispatches across all children, writes to state table

Event type mapping from child to portfolio:
- `advance`, `skip`, `set` → `child_advanced`
- `cancel` → `child_cancelled`
- `block`, `pause` → `child_blocked`
- `rollback` → `child_rolledback`
- Upstream dependency match → `upstream_changed`

The relay is the only writer to the `active-dispatch-count` state entry. If the relay isn't running, the dispatch limit check degrades gracefully (allows spawn with a warning).

### Portfolio Gates

Portfolio runs automatically receive a `children_at_phase` gate check. This gate blocks the portfolio from advancing to phase P unless all active children have reached phase P (or later) in their own chains.

- Completed and cancelled children don't block
- Failed children DO block (intentional — portfolio should not advance past a failed child)
- Children whose chain doesn't contain the target phase are treated as "past it"
- Gate freshness is bounded by the relay poll interval (default 2s)

The gate uses the `PortfolioQuerier` interface (`GetChildren(ctx, runID)`) to avoid cross-package coupling.

### Dispatch Budget

Portfolio runs can set `max_dispatches` to limit concurrent agent dispatches across all children. The relay maintains `active-dispatch-count` in the state table. The CLI checks this at spawn time.

**Important:** The dispatch limit is advisory (TOCTOU-vulnerable). Concurrent spawns may exceed the limit. Document this for operators.

### DBPool

`DBPool` caches read-only `*sql.DB` handles keyed by absolute project directory path. Handles are opened with `?mode=ro` and `SetMaxOpenConns(1)`. Absolute path enforcement prevents relative path ambiguity.

## Security

### Path Traversal Protection

The `--db` flag is validated by `validateDBPath()`:
- Must end in `.db` extension
- No `..` path components
- Must resolve to a path under CWD
- Parent directory must not be a symlink

### JSON Payload Validation

All payloads are validated before storage:
- Size: max 1MB
- Nesting depth: max 20 levels
- Key length: max 1000 chars
- String values: max 100KB each
- Array length: max 10,000 elements

## SQLite Patterns

### Connection Management
- `SetMaxOpenConns(1)` — single writer for WAL mode correctness
- PRAGMAs set explicitly after `sql.Open` (DSN `_pragma` unreliable with modernc driver)
- `busy_timeout` set to prevent immediate SQLITE_BUSY on contention

### Important: No CTE + UPDATE RETURNING
`modernc.org/sqlite` does NOT support `WITH claim AS (UPDATE ... RETURNING) SELECT ...`. Use direct `UPDATE ... RETURNING` with row counting instead. This is a known limitation.

### Transaction Isolation
| Operation | Isolation | Rationale |
|-----------|-----------|-----------|
| state set | Transaction (default) | Write with REPLACE |
| state get | No transaction | Read-only |
| sentinel check | Transaction (default) | Atomic claim + auto-prune |
| dispatch create | No transaction | Single INSERT |
| dispatch update | Transaction | Capture prev status + UPDATE + commit before event recorder |
| migrate | Transaction (default) | Schema DDL + version update |

### Migration Safety
- Pre-migration backup created automatically (`.backup-YYYYMMDD-HHMMSS`)
- Schema version read inside transaction to prevent TOCTOU
- `CREATE TABLE IF NOT EXISTS` makes migration idempotent

## Testing

```bash
go test ./...                    # Unit tests (~140 tests across 12 packages)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (~100+ tests)
```

## Recovery Procedures

### DB Corruption
```bash
ic health                        # Diagnose
cp .clavain/intercore.db.backup-* .clavain/intercore.db  # Restore latest backup
ic health                        # Verify
```

### Schema Mismatch
```bash
ic version                       # Shows "schema: v<N>"
# If binary is too old: upgrade intercore binary
# If DB is too old: ic init (auto-migrates)
```

### Sentinel Stuck After Crash
```bash
ic sentinel reset <name> <scope_id>  # Clear the sentinel
```

### Lock Stuck After Crash
```bash
ic lock stale --older-than=5s        # Find stale locks
ic lock clean --older-than=5s        # Remove stale locks (checks PID liveness)
```
