# AGENTS.md -- Intercore

Kernel layer (Layer 1) of the Demarch autonomous software agency platform. Host-agnostic Go CLI (`ic`) backed by a single SQLite WAL database providing the durable system of record for runs, phases, gates, dispatches, events, token budgets, coordination locks, discovery pipelines, work lanes, scheduling, sessions, routing decisions, landed changes, and replay inputs.

## Canonical References
1. [`PHILOSOPHY.md`](../../PHILOSOPHY.md) -- direction for ideation and planning decisions.
2. `CLAUDE.md` -- implementation details, architecture, testing, and release workflow.

## Quick Reference

```bash
go build -o ic ./cmd/ic                    # Build
go test ./...                              # Unit tests
bash test-integration.sh                   # Integration tests
ic init                                    # Create/migrate DB
ic health                                  # Check DB + schema
ic version                                 # CLI + schema versions
```

**Module:** `github.com/mistakeknot/intercore`
**Location:** `core/intercore/`
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)
**Schema:** v30 (31 tables, `PRAGMA user_version` tracked)
**CLI version:** 0.3.2

## Directory Layout

```
cmd/ic/          CLI entry point + 21 subcommand files
internal/        28 packages (see Modules section)
pkg/             2 packages: contract/, redaction/
contracts/       JSON Schema contract registry + codegen (cli/, events/, overrides/)
config/          costs.yaml (model pricing)
scripts/         validate-gitleaks-waivers.sh
agents/          Topic guide markdown files
lib-intercore.sh Bash wrappers for hooks (44 functions)
```

## Topic Guides

| Topic | File | Covers |
|-------|------|--------|
| CLI Reference | [agents/cli-reference.md](agents/cli-reference.md) | All `ic` commands, flags, exit codes, publish pipeline |
| Modules | [agents/modules.md](agents/modules.md) | Dispatch, Phase, Gate, Event, Coordination, Scheduler, Lane, Discovery, Cost, Portfolio, Lock, supporting libraries |
| Architecture | [agents/architecture.md](agents/architecture.md) | Security model, SQLite patterns, schema upgrade |
| Bash Wrappers | [agents/bash-wrappers.md](agents/bash-wrappers.md) | lib-intercore.sh (44 functions) |
| Testing & Recovery | [agents/testing.md](agents/testing.md) | Test suites, DB corruption, stuck locks, schema mismatch |

## Modules

28 internal packages organized by domain:

### Core Infrastructure

**db** -- SQLite database management. Embeds `schema.sql` (31 tables), handles WAL mode, PRAGMAs, `SetMaxOpenConns(1)`, auto-backup before migration, `PRAGMA user_version` tracking. 15 incremental migration files (v16-v30) in `internal/db/migrations/`.

**state** -- Key-value state store with optional TTL. Used by event cursors, budget state, and coordination metadata.

**sentinel** -- Idempotency/rate-limiting via atomic claim-or-throttle. Prune support for old entries.

**observability** -- Distributed trace context propagation via `IC_TRACE_ID`, `IC_SPAN_ID`, `IC_PARENT_SPAN_ID` environment variables. Generates OTel-compatible 128-bit trace IDs and 64-bit span IDs. Provides `slog.JSONHandler` that auto-injects trace attributes.

### Run Lifecycle

**phase** -- Run lifecycle state machine with optimistic concurrency (`WHERE phase = ?`). Default chain: `brainstorm -> brainstorm-reviewed -> strategized -> planned -> executing -> review -> polish -> reflect -> done`. Custom chains via `--phases`. Phase skip, rollback, gate evaluation, and evidence tracking.

**runtrack** -- Run agent tracking, artifact tracking, and code rollback metadata. Tables: `run_agents`, `run_artifacts`.

**action** -- Phase action registry. CRUD for per-phase commands with template variable resolution and batch operations.

**lifecycle** -- Agent state machine: `waiting -> generating -> thinking -> idle -> stalled -> error -> completed`. Stall detection via configurable velocity thresholds (tokens-per-minute).

### Dispatch & Coordination

**dispatch** -- Agent dispatch lifecycle tracking in SQLite. Lifecycle: `spawned -> running -> completed | failed | timeout | cancelled`. Spawn policy (budget, concurrency, agent cap, spawn depth), write-set conflict detection, merge intents (transactional outbox pattern).

**coordination** -- Unified SQLite-backed coordination locks replacing filesystem locks for multi-agent file coordination. Three types: `file_reservation`, `named_lock`, `write_set`. NFA-based glob overlap detection with DoS guards (50-token, 10-wildcard limits). TTL-based expiration, event emission.

**lock** -- Process-level mutual exclusion using POSIX `mkdir` atomicity. Entirely filesystem-based, no SQLite. Layout: `/tmp/intercore/locks/<name>/<scope>/owner.json`. PID-liveness stale detection.

**scheduler** -- Fair spawn scheduler with priority queue (0=critical, 4=backlog), per-agent rate limiting, per-agent concurrency caps, exponential backoff, session-based fair scheduling, pause/resume. Table: `scheduler_jobs` (v19).

### Events & Audit

**event** -- Reactive event bus with 5 source types (`phase`, `dispatch`, `interspect`, `discovery`, `coordination`). In-process `Notifier` with callback-based wiring. Handlers: LogHandler (stderr), HookHandler (async shell hooks, 5s timeout), SpawnHandler (auto-spawn on executing phase). Dual cursors for independent phase/dispatch sequences. Additional tables: `dispatch_events`, `interspect_events`, `review_events`, `intent_events`.

**audit** -- Tamper-evident audit trail with SHA-256 hash chain. Per-session sequence numbers detect deletion gaps. Auto-redacts sensitive values before persistence. Table: `audit_log` (v15).

### Budget & Cost

**budget** -- Token budget enforcement. Warning thresholds, exceeded checks, composition/meet semantics, reconciliation against billing API data.

**cost** -- Cost reconciliation: compares billing API data against self-reported dispatch token counts. Discrepancies emit events. Table: `cost_reconciliations` (v17).

### Discovery & Scoring

**discovery** -- Research discovery tracking. Lifecycle: `new -> scored -> promoted | proposed | dismissed`. Confidence tiers (high>=0.8, medium>=0.5, low>=0.3, discard<0.3). Source dedup, relevance decay, semantic search via embeddings (BLOB), feedback signals, interest profiles. Tables: `discoveries` (v9), `discovery_events`, `feedback_signals`, `interest_profile`.

**scoring** -- Multi-factor assignment scoring for (agent, task) pairs. Factors: base priority, agent type bonus, profile tag affinity, file focus overlap, context exhaustion penalty, file reservation conflict penalty.

### Portfolio & Lanes

**portfolio** -- Cross-project coordination through parent/child run hierarchies. Dependency graph with cycle detection (DFS). Deterministic topological build order (Kahn's algorithm, lexicographic tie-breaking). Event relay from child project databases (read-only `DBPool`). Gates: `children_at_phase`, `upstreams_at_phase`.

**lane** -- Thematic work lanes for organizing beads by theme. Types: `standing` (permanent) and `arc` (temporary). Velocity scoring: starvation and throughput per lane based on bead membership within a sliding window. Tables: `lanes` (v13), `lane_events`, `lane_members`.

### Sessions & Routing

**session** -- Agent session registration and attribution tracking. Tracks session lifecycle (start/end), token accumulation (additive updates per turn), and point-in-time attribution changes (bead, run, phase). Tables: `sessions`, `session_attributions` (v26, v30).

**routing** -- Cost-aware capability matching for model/agent selection. Unified routing logic replacing `lib-routing.sh` + `agent-roles.yaml` + `interserve classify`. Hierarchical resolution: per-agent override > phase-category > phase-model > default-category > default-model > "sonnet" fallback. Safety floor clamping from `agent-roles.yaml`. Dispatch tier resolution with 3-hop fallback chain. Batch resolution with category inference from agent name patterns. Routing decisions persisted for audit. Cost table: effective cost formula `input_per_mtok + 15 * output_per_mtok`. Tables: `routing_decisions` (v27).

### Landed Changes & Replay

**landed** -- Tracks commits that reached the trunk branch. Links commits to dispatches, runs, beads, sessions, and merge intents. Revert tracking. Summary aggregation by bead and run. Table: `landed_changes` (v25).

**replay** -- Deterministic replay infrastructure. Records nondeterministic inputs (e.g., LLM responses, external API results) associated with runs. `BuildTimeline` reconstructs phase/dispatch decisions and links them to recorded inputs for reproducibility analysis. Table: `run_replay_inputs` (v22).

### Agency & Handoff

**agency** -- YAML-based agency spec loading and validation. Validates stage names, phases, duplicate agents. Multi-stage loading for run initialization.

**handoff** -- Structured session handoff format (~400 tokens). Required fields: `Goal` (what was accomplished), `Now` (what to do next). Statuses: complete, partial, blocked.

### Supporting

**observation** -- Unified system observation via `Collector`. Aggregates state from phase, dispatch, event, and scheduler stores into a `Snapshot` (runs, dispatches, events, queue depth, budget). Used by `ic situation snapshot`.

**publish** -- Plugin publish pipeline. Phases: discovery -> validation -> bump -> commit plugin -> push plugin -> update marketplace -> sync local -> sync agent-rig.json -> done. Crash recovery via SQLite-tracked phase progress.

## Contracts

The `contracts/` package provides a JSON Schema contract registry for `ic` CLI output types. Schemas are generated via `go generate ./contracts/...` and written to `contracts/cli/` (24 schemas) and `contracts/events/` (4 schemas). This enables downstream consumers to validate `ic` JSON output without importing Go types.

Registered contract types cover: coordination, dispatch, phase/run, runtrack, scheduler, lane, discovery, and events.

## CLI Commands (Summary)

21 subcommand files covering:

| Domain | Commands |
|--------|----------|
| Core | `init`, `health`, `version`, `compat` |
| State/Sentinel | `state set/get/delete/list/prune`, `sentinel check/reset/list/prune` |
| Run | `run create/status/advance/phase/list/events/cancel/current/set/skip/rollback/tokens/budget/agent/artifact/action` |
| Dispatch | `dispatch spawn/status/list/poll/wait/kill/tokens/prune` |
| Gate | `gate check/override/rules` |
| Lock | `lock acquire/release/list/stale/clean` |
| Events | `events tail/record/cursor` |
| Coordination | `coordination reserve/release/check/list/sweep/transfer` |
| Scheduler | `scheduler submit/status/stats/list/cancel/pause/resume/prune` |
| Lane | `lane create/list/status/close/events/sync/members/velocity` |
| Discovery | `discovery submit/status/list/score/promote/dismiss/feedback/profile/decay/rollback/search` |
| Cost | `cost reconcile/list` |
| Interspect | `interspect record/query` |
| Portfolio | `portfolio dep/relay/order/status` |
| Situation | `situation snapshot` |
| Config/Agency | `config set/get/list`, `agency load/validate/show/capabilities` |
| Publish | `publish <version>/--patch/--minor/--auto/--dry-run/init/status/doctor/clean` |
| Route | `route model/batch/dispatch/table/record/list` |
| Landed | `landed record/list/revert/summary` |
| Session | `session start/attribute/end/current/list/tokens` |

## Exit Codes

| Code | Meaning | Example |
|------|---------|---------|
| 0 | Success / allowed / found | `ic state get` returns payload |
| 1 | Expected negative result | `ic sentinel check` throttled, `ic coordination check` conflict |
| 2 | Unexpected error | Invalid JSON, DB corruption |
| 3 | Usage error | Missing required argument |

## Global Flags

- `--db=<path>` -- Database path (default: `.clavain/intercore.db`, auto-discovered)
- `--timeout=<dur>` -- SQLite busy timeout (default: 5s)
- `--verbose` -- Verbose output (slog info level)
- `--vv` -- Debug-level verbose output
- `--json` -- JSON output (must appear before subcommand)

## Testing

```bash
go test ./...                    # Unit tests (~687 test functions across 28 packages)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (1320-line bash suite)
```

## Decay Policy

Operational state (C1) follows intermem's decay model adapted for kernel data:

| Data type | Grace period | TTL | Hysteresis | Action |
|-----------|-------------|-----|------------|--------|
| Completed runs | 30 days | 30d from completion | N/A | Pruned from active queries (retained in DB for audit) |
| Coordination locks | Per-lock TTL | Lock-specific (default 60s) | N/A | Auto-released at expiry |
| Dispatch records | 30 days | 30d from completion | N/A | Excluded from cost aggregation |
| Event stream | 90 days | 90d retention | N/A | Old events excluded from reactor processing |

**Standard pattern:** Grace period -> TTL expiry -> no hysteresis (kernel state is operational, not knowledge). Intercore uses TTL-based cleanup rather than confidence decay because C1 data has a clear "done" state -- completed runs don't gradually lose relevance, they become irrelevant after their monitoring window closes. Sentinel auto-prune runs synchronously in the same transaction as new writes.
