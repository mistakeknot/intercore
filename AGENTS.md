# AGENTS.md — Intercore

## Overview

Intercore is the kernel layer (Layer 1) of the Demarch autonomous software agency platform. It is a host-agnostic Go CLI (`ic`) backed by a single SQLite WAL database that provides the durable system of record for runs, phases, gates, dispatches, events, token budgets, coordination locks, discovery pipelines, work lanes, and scheduling. The kernel is mechanism, not policy — it does not know what "brainstorm" means, only that a phase transition happened and needs recording.

In the three-layer architecture, Intercore sits beneath the OS (Clavain, Layer 2) and plugins (Interverse, Layer 3). If the host platform changes, the kernel and all its data survive untouched. Bash hooks and the Clavain sprint pipeline call `ic` for all state operations.

**Module:** `github.com/mistakeknot/intercore`
**Location:** `core/intercore/`
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)
**Schema:** v20 (24 tables, `PRAGMA user_version` tracked)
**CLI version:** 0.3.0

## Architecture

```
cmd/ic/
  main.go             CLI entry point, global flag parsing, command routing
  run.go              Run subcommands (create, status, advance, skip, rollback, phase, list, events, cancel, set, current, agent, artifact, action, tokens, budget)
  dispatch.go         Dispatch subcommands (spawn, status, list, poll, wait, kill, tokens, prune)
  gate.go             Gate subcommands (check, override, rules)
  portfolio.go        Portfolio subcommands (dep add/list/remove, relay, order, status)
  lock.go             Lock subcommands (acquire, release, list, stale, clean)
  events.go           Event subcommands (tail, cursor list/reset)
  coordination.go     Coordination subcommands (reserve, release, check, list, sweep, transfer)
  scheduler_cmd.go    Scheduler subcommands (submit, status, stats, pause, resume, list, cancel, prune)
  lane.go             Lane subcommands (create, list, status, close, events, sync, members, velocity)
  discovery.go        Discovery subcommands (submit, status, list, score, promote, dismiss, feedback, profile, decay, rollback, search)
  cost.go             Cost subcommands (reconcile, list)
  interspect.go       Interspect subcommands (record, query)
  agency.go           Agency subcommands (load, validate, show, capabilities)
  action.go           Run action subcommands (add, list, update, delete)
  config.go           Config subcommands (set, get, list)

internal/
  db/                 SQLite connection, migration (embedded DDL), health, disk check
  state/              State CRUD with JSON validation and TTL
  sentinel/           Atomic throttle guards with UPDATE+RETURNING
  dispatch/           Agent dispatch lifecycle: spawn, poll, collect, wait, policy, conflict, intent, telemetry, outcome
  phase/              Phase state machine: run lifecycle with configurable chains, gate evaluation
  event/              Event bus: 5 sources (phase, dispatch, interspect, discovery, coordination), notifier, handlers
  runtrack/           Agent and artifact tracking within runs
  portfolio/          Cross-project coordination: deps (cycle detection), topo sort, relay, dbpool
  budget/             Token budget checking with dedup, cost reconciliation, composition
  lock/               Filesystem-based mutex using POSIX mkdir atomicity
  action/             Phase action store (event-driven advancement)
  agency/             Agency spec parser: YAML schema, validation, stage-to-phase mapping
  coordination/       Unified file reservations, named locks, write-sets (SQLite-backed, glob overlap detection)
  scheduler/          Fair spawn scheduler: priority queue, rate limiting, backoff, per-agent caps
  lane/               Thematic work lanes: standing/arc types, membership, velocity scoring, starvation detection
  discovery/          Research discovery pipeline: submit, score, promote, decay, semantic search, dedup
  scoring/            Multi-factor agent-task assignment scoring (type bonus, tag affinity, file overlap, context penalty)
  lifecycle/          Agent state machine: waiting/generating/thinking/idle/stalled/error/completed with stall detection
  handoff/            Structured session handoff format for context preservation across rotations
  audit/              Tamper-evident audit trail: SHA-256 hash chain, per-session sequence numbers, auto-redaction
  redaction/          Secret scanning: pattern matching with 4 modes (off/warn/redact/block), category allowlists

lib-intercore.sh      Bash wrappers for hooks (45 functions)
test-integration.sh   End-to-end CLI integration test (1320 lines)
```

## CLI Commands

### Core

```
ic init                                    Create/migrate the database
ic health                                  Check DB readable, schema current, disk space
ic version                                 Print CLI and schema versions
ic compat status                           Show legacy temp file vs DB coverage
ic compat check <key>                      Check if key has data in DB
```

### State & Sentinels

```
ic state set <key> <scope> [--ttl=<dur>]   Write JSON (stdin or @filepath)
ic state get <key> <scope>                 Read JSON (exit 0=found, 1=not found)
ic state delete <key> <scope>              Remove a state entry
ic state list <key>                        List scope_ids for a key
ic state prune                             Remove expired state entries
ic sentinel check <name> <scope> --interval=<sec>   Atomic claim (exit 0=allowed, 1=throttled)
ic sentinel reset <name> <scope>           Clear a sentinel
ic sentinel list                           List all sentinels
ic sentinel prune --older-than=<dur>       Remove old sentinels
```

### Dispatch

```
ic dispatch spawn [flags]                  Spawn an agent dispatch (prints ID)
ic dispatch status <id>                    Show dispatch details
ic dispatch list [--active] [--scope=<s>]  List dispatches
ic dispatch poll <id>                      Check liveness, update stats
ic dispatch wait <id> [--timeout=<dur>]    Block until terminal or timeout
ic dispatch kill <id>                      SIGTERM then SIGKILL a dispatch
ic dispatch tokens <id> --set --in=N --out=N [--cache=N]   Update token counts
ic dispatch prune --older-than=<dur>       Remove old terminal dispatches
```

### Run

```
ic run create --project=<dir> --goal=<text> [--complexity=N] [--scope-id=S] [--phases='[...]'] [--token-budget=N] [--budget-warn-pct=N] [--budget-enforce] [--max-agents=N] [--actions='{}']
ic run status <id>                         Show run details
ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]
ic run phase <id>                          Print current phase (scripting)
ic run list [--active] [--scope=S] [--portfolio]  List runs
ic run events <id>                         Phase event audit trail
ic run cancel <id>                         Cancel a run
ic run current [--project=<dir>]           Print active run ID for project
ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool] [--max-dispatches=N]
ic run skip <id> <phase> [--reason=<text>] [--actor=<name>]   Pre-skip a phase
ic run rollback <id> --to-phase=<phase> --reason=<text> [--dry-run]   Workflow rollback
ic run rollback <id> --layer=code [--phase=<p>] [--format=json|text]  Code rollback metadata
ic run tokens <id> [--project=<dir>] [--json]   Token aggregation across dispatches
ic run budget <id> [--json]                Check budget thresholds (exit 1=exceeded)
ic run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
ic run agent list <run>                    List agents for a run
ic run agent update <id> --status=<s>      Update agent status (active|completed|failed)
ic run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
ic run artifact list <run> [--phase=<p>]   List artifacts for a run
ic run action add <run> --phase=<p> --command=<cmd> [--args=<json>] [--mode=<m>] [--type=<t>] [--priority=N]
ic run action list <run> [--phase=<p>]     List actions for a run
ic run action update <run> --phase=<p> --command=<cmd> [--args=<json>]
ic run action delete <run> --phase=<p> --command=<cmd>
```

### Gate

```
ic gate check <run_id> [--priority=N]      Dry-run gate evaluation (exit 0=pass, 1=fail)
ic gate override <run_id> --reason=<text>  Force-advance past a failed gate
ic gate rules [--phase=<p>]                Display gate rules table
```

### Lock (filesystem-only, no SQLite)

```
ic lock acquire <name> <scope> [--timeout=<dur>] [--owner=<id>]
ic lock release <name> <scope> [--owner=<id>]
ic lock list                               List all held locks
ic lock stale [--older-than=<dur>]         List stale locks
ic lock clean [--older-than=<dur>]         Remove stale locks (PID-liveness check)
```

### Events

```
ic events tail <run_id> [--consumer=<name>] [--follow] [--since-phase=N] [--since-dispatch=N] [--limit=N] [--poll-interval=<dur>]
ic events tail --all [flags]               Tail events across all runs
ic events cursor list                      List consumer cursors
ic events cursor reset <consumer>          Reset a consumer cursor
```

### Coordination (SQLite-backed, v20)

```
ic coordination reserve --owner=<o> --scope=<s> --pattern=<p> [--type=file_reservation|named_lock|write_set] [--ttl=<sec>] [--exclusive] [--reason=<text>] [--dispatch=<id>] [--run=<id>]
ic coordination release <id>               Release by lock ID
ic coordination release --owner=<o> --scope=<s>   Release by owner+scope
ic coordination check --scope=<s> --pattern=<p> [--exclude-owner=<o>]   Check for conflicts (exit 0=clear, 1=conflict)
ic coordination list [--scope=<s>] [--owner=<o>] [--type=<t>] [--active]
ic coordination sweep                      Expire TTL-based locks
ic coordination transfer <id> --to=<new-owner>   Transfer lock ownership
```

### Scheduler (v19)

```
ic scheduler submit --prompt-file=<f> --project=<dir> [--type=codex] [--session=<name>] [--name=<label>] [--priority=N]
ic scheduler status <job-id>               Check job status
ic scheduler stats                         Queue stats by status
ic scheduler list [--status=pending]       List jobs
ic scheduler cancel <job-id>               Cancel a job
ic scheduler pause                         Pause processing
ic scheduler resume                        Resume processing
ic scheduler prune --older-than=<dur>      Clean completed jobs
```

### Lane (v13)

```
ic lane create --name=<n> [--type=standing|arc] [--description=<d>]
ic lane list [--active] [--status=<s>]     List lanes
ic lane status <id-or-name>                Show lane details + members
ic lane close <id-or-name>                 Close a lane
ic lane events <id-or-name>                Lane event history
ic lane sync <id-or-name>                  Sync lane membership
ic lane members <id-or-name>               List bead members
ic lane velocity [--window=<days>]         Compute starvation/throughput scores
```

### Discovery (v9)

```
ic discovery submit --source=<s> --source-id=<id> --title=<t> [--score=<0-1>] [--summary=<s>] [--url=<u>] [--metadata=@<file>] [--embedding=@<file>]
ic discovery status <id> [--json]
ic discovery list [--source=<s>] [--status=<s>] [--tier=<t>] [--limit=N]
ic discovery score <id> --score=<0.0-1.0>
ic discovery promote <id> --bead-id=<bid> [--force]
ic discovery dismiss <id>
ic discovery feedback <id> --signal=<type> [--data=@<file>] [--actor=<name>]
ic discovery profile [--json]
ic discovery profile update --keyword-weights=<json> --source-weights=<json>
ic discovery decay --rate=<0.0-1.0> [--min-age=<sec>]
ic discovery rollback --source=<s> --since=<unix-ts>
ic discovery search --embedding=@<file> [--source=<s>] [--min-score=<f>] [--limit=N]
```

### Cost

```
ic cost reconcile <run_id> --billed-in=N --billed-out=N [--dispatch=<id>] [--source=<s>]
ic cost list <run_id> [--limit=N]
```

### Interspect

```
ic interspect record --agent=<name> --type=<type> [--run=<id>] [--reason=<text>] [--context=<json>] [--session=<id>] [--project=<dir>]
ic interspect query [--agent=<name>] [--since=<id>] [--limit=N]
```

### Portfolio

```
ic run create --projects=<p1>,<p2> --goal=<text> [--max-dispatches=N]
ic portfolio dep add <id> --upstream=<path> --downstream=<path>
ic portfolio dep list <id>
ic portfolio dep remove <id> --upstream=<path> --downstream=<path>
ic portfolio relay <id> [--interval=2s]
ic portfolio order <id>                    Topological build order (deterministic)
ic portfolio status <id>                   Per-child readiness with blocked-by details
```

### Config & Agency

```
ic config set <key> <value>                Set kernel config (global_max_dispatches, max_spawn_depth)
ic config get <key>                        Get kernel config value
ic config list [--verbose]                 List all kernel config values
ic agency load <stage|all> --run=<id> --spec-dir=<path>
ic agency validate <file> | --all --spec-dir=<path>
ic agency show <stage> --spec-dir=<path>
ic agency capabilities <run-id>
```

### Exit Codes

| Code | Meaning | Example |
|------|---------|---------|
| 0 | Success / allowed / found | `ic state get` returns payload |
| 1 | Expected negative result | `ic sentinel check` throttled, `ic coordination check` conflict |
| 2 | Unexpected error | Invalid JSON, DB corruption |
| 3 | Usage error | Missing required argument |

### Global Flags

- `--db=<path>` — Database path (default: `.clavain/intercore.db`, auto-discovered)
- `--timeout=<dur>` — SQLite busy timeout (default: 100ms)
- `--verbose` — Verbose output
- `--json` — JSON output (must appear before subcommand)

## Dispatch Module

Tracks agent dispatch lifecycle in SQLite. Go owns lifecycle tracking; `dispatch.sh` remains the execution engine.

**Lifecycle:** `spawned -> running -> completed | failed | timeout | cancelled`

**Spawn policy** (`dispatch/policy.go`) — checked before every `dispatch spawn`:
- Budget enforcement (if `budget_enforce=true`)
- Per-run concurrency (active dispatches vs `max_dispatches`)
- Global concurrency (all active dispatches vs `kernel.global_max_dispatches`)
- Agent cap (lifetime dispatches vs `max_agents`)
- Spawn depth (nested depth vs `kernel.max_spawn_depth`)

**Write-set conflict detection** (`dispatch/conflict.go`) — at merge time, detects file-level conflicts between concurrent dispatches.

**Merge intents** (`dispatch/intent.go`) — transactional outbox pattern for SQLite+git coordination.

## Phase Module

Implements a run lifecycle state machine. Intercore owns phase transitions instead of relying on LLM prompt instructions.

**Default chain:** `brainstorm -> brainstorm-reviewed -> strategized -> planned -> executing -> review -> polish -> reflect -> done`

Custom chains via `--phases='["a","b","c"]'` (min 2 phases, no duplicates, last is terminal).

**Phase skip:** `ic run skip` pre-marks phases. `Advance()` automatically walks past skipped phases.

**Optimistic concurrency:** `UpdatePhase` uses `WHERE phase = ?`. Concurrent `ic run advance` calls get `ErrStalePhase` instead of double-advancing.

## Gate System

Gates enforce conditions before phase transitions. Defined in `gateRules` (`internal/phase/gate.go`).

**Checks:** `artifact_exists`, `agents_complete`, `verdict_exists`, `children_at_phase` (portfolio), `upstreams_at_phase` (child runs with deps), `budget_not_exceeded` (when `budget_enforce=true`).

**Gate tiers** (set by `--priority`): 0-1 = Hard (block), 2-3 = Soft (warn), 4+ = None (skip).

**Evidence:** Every evaluation produces `GateEvidence` with per-condition results (JSON in event `reason` field).

**Interfaces:** `RuntrackQuerier`, `VerdictQuerier`, `PortfolioQuerier`, `DepQuerier` prevent cross-package coupling.

## Event Bus Module

Reactive notification of state changes across 5 source types. In-process `Notifier` with callback-based wiring.

**Sources:** `phase`, `dispatch`, `interspect`, `discovery`, `coordination`

**Flow:** State change -> callback (after DB commit) -> `Notifier.Notify()` -> handlers. Handler errors logged but never fail the parent operation.

**Handlers:**

| Handler | Behavior |
|---------|----------|
| LogHandler | Logs events to stderr (quiet mode suppresses) |
| HookHandler | Executes `.clavain/hooks/on-event.sh` async (goroutine, 5s timeout) |
| SpawnHandler | Auto-spawns agents on phase transition to "executing" |

**Dual cursors:** `ic events tail` tracks separate high-water marks for phase and dispatch events (independent AUTOINCREMENT sequences). Cursors stored in `state` table with 24h TTL.

## Coordination Module

Unified SQLite-backed coordination locks replacing the filesystem lock module for multi-agent file coordination. Three lock types: `file_reservation`, `named_lock`, `write_set`.

**Glob overlap detection:** NFA-based pattern matching (copied from Intermute to avoid L1-to-L1 coupling). `ValidateComplexity` rejects patterns with >50 tokens or >10 wildcards (DoS guard).

**Reserve behavior:** `BEGIN IMMEDIATE` transaction, conflict check against active locks in same scope via glob overlap, TTL-based expiration. Returns `ReserveResult` with either the lock or `ConflictInfo`.

**Event emission:** Fires `coordination.acquired`, `.released`, `.conflict`, `.expired`, `.transferred` events after DB commit. Events stored in `coordination_events` table.

## Scheduler Module

Fair spawn scheduler that serializes and paces all agent dispatches to prevent resource exhaustion and rate limit errors.

**Features:** Priority queue (0=critical, 4=backlog), per-agent rate limiting, per-agent concurrency caps, exponential backoff for resource errors, session-based fair scheduling, pause/resume.

**Integration:** `ic dispatch spawn --scheduled` creates a scheduler job instead of direct execution. The scheduler dequeues and executes spawns at a controlled pace.

**Tables:** `scheduler_jobs` (v19) — status lifecycle: `pending -> running -> completed | failed | cancelled`.

## Lane Module

Thematic work lanes for organizing beads (work items) by theme. Two types: `standing` (permanent, e.g., "tech-debt") and `arc` (temporary, e.g., "q1-launch").

**Velocity scoring:** `VelocityCalculator` computes starvation and throughput per lane based on bead membership and closure events within a sliding window. Higher starvation score = lane needs attention.

**Tables:** `lanes` (v13), `lane_events`, `lane_members`.

## Discovery Pipeline

Research discovery tracking from submission through scoring, promotion, and decay.

**Lifecycle:** `new -> scored -> promoted | proposed | dismissed`

**Confidence tiers:** high (>=0.8), medium (>=0.5), low (>=0.3), discard (<0.3).

**Features:** Source dedup (UNIQUE on source+source_id), relevance decay over time, semantic search via embeddings (BLOB), feedback signals (boost/penalize/promote/dismiss), interest profile for personalized scoring.

**Tables:** `discoveries` (v9), `discovery_events`, `feedback_signals`, `interest_profile`.

## Cost Reconciliation

Compares billing API data against self-reported dispatch token counts. Exit 0 = tokens match, exit 1 = discrepancy found. Discrepancies emit `cost.reconciliation_discrepancy` events.

**Table:** `cost_reconciliations` (v17).

## Supporting Libraries

**audit** — Tamper-evident audit trail with SHA-256 hash chain. Each entry includes `prev_hash` of previous entry. Per-session sequence numbers detect deletion gaps. Auto-redacts sensitive values before persistence. Table: `audit_log` (v15).

**redaction** — Scans strings for sensitive patterns (API keys, tokens, secrets). Four modes: `off`, `warn` (report only), `redact` (replace with placeholders), `block` (set Blocked flag). Category-based allowlists.

**scoring** — Multi-factor assignment scoring for (agent, task) pairs. Factors: base priority, agent type bonus, profile tag affinity, file focus overlap, context exhaustion penalty, file reservation conflict penalty.

**lifecycle** — Agent state machine: `waiting -> generating -> thinking -> idle -> stalled -> error -> completed`. Stall detection via configurable velocity thresholds (tokens-per-minute).

**handoff** — Structured session handoff format (~400 tokens). Required fields: `Goal` (what was accomplished), `Now` (what to do next). Statuses: complete, partial, blocked.

## Portfolio Orchestration

Cross-project coordination through parent/child run hierarchies.

**Portfolio runs:** Created with `ic run create --projects=p1,p2`. Children linked via `parent_run_id`. Cancellation cascades to all active children.

**Dependencies:** `project_deps` table with cycle detection (DFS + transactional add). `ic portfolio order` computes deterministic topological build order (Kahn's algorithm, lexicographic tie-breaking).

**Event relay:** `ic portfolio relay` polls child project databases (read-only via `DBPool`), relays phase events as `child_advanced`/`child_blocked`/`child_cancelled`/`child_rolledback`/`upstream_changed`. Dispatch count written to state table.

**Gates:** `children_at_phase` blocks portfolio advancement until all active children have reached the target phase. `upstreams_at_phase` blocks child advancement until upstream deps have reached the phase. Terminal runs (completed/cancelled/failed) do not block.

## Lock Module

Process-level mutual exclusion using POSIX `mkdir` atomicity — entirely filesystem-based, no SQLite.

**Layout:** `/tmp/intercore/locks/<name>/<scope>/owner.json`

**Acquire:** Atomic `os.Mkdir` -> spin-wait with 100ms sleep -> stale detection (5s default) -> stale-break (sequential remove, not `os.RemoveAll`).

**Clean:** `syscall.Kill(pid, 0)` liveness check. `nil`/`EPERM` = alive (skip), `ESRCH` = gone (remove).

## Security

### Path Traversal Protection

The `--db` flag is validated: must end in `.db`, no `..` components, must resolve under CWD, parent directory must not be a symlink.

### JSON Payload Validation

Max 1MB size, 20 levels nesting, 1000-char keys, 100KB string values, 10000-element arrays.

### Lock Input Validation

Name and scope components reject `/`, `\`, `..`, empty, `.`. Resolved path must remain under `BaseDir`.

## SQLite Patterns

- `SetMaxOpenConns(1)` — single writer for WAL mode correctness
- PRAGMAs set explicitly after `sql.Open` (DSN `_pragma` unreliable with modernc driver)
- `busy_timeout` set to prevent immediate `SQLITE_BUSY`
- `modernc.org/sqlite` does NOT support CTE + UPDATE RETURNING — use direct `UPDATE ... RETURNING`
- Transaction isolation varies by operation (see `internal/` package docs for specifics)
- Pre-migration backup created automatically (`.backup-YYYYMMDD-HHMMSS`)
- Schema version inside transaction prevents TOCTOU
- `CREATE TABLE IF NOT EXISTS` makes migration idempotent

### Deployment: Schema Upgrade

```bash
go build -o /home/mk/go/bin/ic ./cmd/ic   # Rebuild (schema is //go:embed'd)
ic init                                     # Migrate live DB (creates backup)
ic version                                  # Verify schema version
```

## Bash Wrappers (lib-intercore.sh)

45 wrapper functions for use from bash hooks and scripts. Key groups:

**State/Sentinel:** `intercore_available`, `intercore_state_set/get`, `intercore_sentinel_check/reset`, `intercore_check_or_die`, `intercore_cleanup_stale`

**Dispatch:** `intercore_dispatch_spawn/status/wait/list_active/kill/tokens`

**Run:** `intercore_run_current/phase/advance/skip/tokens/budget`, `intercore_run_agent_add`, `intercore_run_artifact_add`, `intercore_run_rollback/rollback_dry/code_rollback`

**Actions:** `intercore_run_action_add/list/update/delete`

**Gates:** `intercore_gate_check/override`

**Locks:** `intercore_lock_available/lock/unlock/lock_clean`

**Events:** `intercore_events_tail/tail_all/cursor_get/cursor_set/cursor_reset`

**Agency:** `intercore_agency_load/validate`

## Testing

```bash
go test ./...                    # Unit tests (~529 test functions across 23 packages)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (1320-line bash suite)
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
# If binary too old: upgrade intercore binary
# If DB too old: ic init (auto-migrates)
```

### Sentinel Stuck After Crash
```bash
ic sentinel reset <name> <scope_id>
```

### Lock Stuck After Crash
```bash
ic lock stale --older-than=5s
ic lock clean --older-than=5s    # Checks PID liveness before removal
```

### Coordination Lock Stuck
```bash
ic coordination list --active
ic coordination sweep            # Expire TTL-based locks
ic coordination release <id>     # Manual release
```
