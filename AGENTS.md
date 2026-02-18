# AGENTS.md — intercore

## Overview

intercore is a Go CLI (`ic`) backed by a single SQLite WAL database that provides atomic state operations, throttle guards, agent dispatch tracking, and phase lifecycle management callable from bash hooks. It replaces ~15 scattered temp files in `/tmp/` used by the Clavain hook infrastructure.

**Location:** `infra/intercore/` (infrastructure, not a plugin — hooks depend on it)
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)

## Architecture

```
cmd/ic/main.go          CLI entry point, argument parsing, shared helpers
cmd/ic/dispatch.go      Dispatch subcommands (spawn, status, list, poll, wait, kill, prune)
cmd/ic/run.go           Run subcommands (create, status, advance, phase, current, agent, artifact)
internal/db/db.go       SQLite connection, migration, health check
internal/db/schema.sql  Embedded DDL (tables: state, sentinels, dispatches, runs, phase_events, run_agents, run_artifacts)
internal/db/disk.go     Disk space check (Linux syscall)
internal/state/         State CRUD with JSON validation and TTL
internal/sentinel/      Atomic throttle guards with UPDATE+RETURNING
internal/dispatch/      Agent dispatch lifecycle: spawn, poll, collect, wait
  dispatch.go           Store (CRUD), Dispatch struct, ID generation
  spawn.go              Process spawning, dispatch.sh resolution, prompt hashing
  collect.go            Liveness polling, verdict/summary parsing, wait loop
internal/phase/         Phase state machine: run lifecycle with complexity-based skip
  phase.go              Types, constants, transition table, skip logic
  store.go              Run + PhaseEvent CRUD with optimistic concurrency, Current()
  machine.go            Advance() with gate evaluation, auto_advance pause
  errors.go             Error sentinels
internal/runtrack/      Agent and artifact tracking within runs
  runtrack.go           Agent/Artifact types, status constants
  store.go              CRUD for run_agents and run_artifacts
  errors.go             Error sentinels (ErrAgentNotFound, ErrArtifactNotFound)
lib-intercore.sh        Bash wrappers for hooks (v0.3.0)
test-integration.sh     End-to-end integration test (~70 tests)
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
ic dispatch prune --older-than=<dur>       Remove old terminal dispatches
ic run create --project=<dir> --goal=<text> [--complexity=N] [--scope-id=S]
ic run status <id>                         Show run details
ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]
ic run phase <id>                          Print current phase (scripting)
ic run list [--active] [--scope=S]         List runs
ic run events <id>                         Phase event audit trail
ic run cancel <id>                         Cancel a run
ic run current [--project=<dir>]            Print active run ID for project (exit 0=found, 1=none)
ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]
ic run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
ic run agent list <run>                    List agents for a run
ic run agent update <id> --status=<s>      Update agent status (active|completed|failed)
ic run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
ic run artifact list <run> [--phase=<p>]   List artifacts for a run
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
```

## Phase Module

The phase module implements a run lifecycle state machine ported from `lib-sprint.sh` + `lib-gates.sh`. intercore owns phase transitions instead of relying on LLM prompt instructions.

### Phase Chain

```
brainstorm → brainstorm-reviewed → strategized → planned → executing → review → polish → done
```

### Complexity-Based Skip

| Complexity | Phases | Skipped |
|-----------|--------|---------|
| 1 (trivial) | brainstorm → planned → executing → done | brainstorm-reviewed, strategized, review, polish |
| 2 (small) | brainstorm → brainstorm-reviewed → planned → executing → done | strategized, review, polish |
| 3-5 (full) | All 8 phases | None |

`--force-full` overrides complexity and walks every phase.

### Gate Tiers

| Priority | Tier | Behavior |
|----------|------|----------|
| 0-1 | Hard | Block advance if gate fails |
| 2-3 | Soft | Warn but allow advance |
| 4+ | None | Skip gate evaluation |

Gates always pass in v1 (stub). Real gate logic (artifact presence, review checks) is deferred to Phase 2 (iv-qfg8).

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
| dispatch update | No transaction | Single UPDATE |
| migrate | Transaction (default) | Schema DDL + version update |

### Migration Safety
- Pre-migration backup created automatically (`.backup-YYYYMMDD-HHMMSS`)
- Schema version read inside transaction to prevent TOCTOU
- `CREATE TABLE IF NOT EXISTS` makes migration idempotent

## Testing

```bash
go test ./...                    # Unit tests (~90 tests across 6 packages)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (~70 tests)
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
