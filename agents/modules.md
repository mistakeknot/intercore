# Modules

Internal module descriptions covering dispatch, phase, gate, events, coordination, scheduler, lane, discovery, cost, portfolio, lock, and supporting libraries.

## Dispatch Module

Tracks agent dispatch lifecycle in SQLite. Go owns lifecycle tracking; `dispatch.sh` remains the execution engine.

**Lifecycle:** `spawned -> running -> completed | failed | timeout | cancelled`

**Spawn policy** (`dispatch/policy.go`) -- checked before every `dispatch spawn`:
- Budget enforcement (if `budget_enforce=true`)
- Per-run concurrency (active dispatches vs `max_dispatches`)
- Global concurrency (all active dispatches vs `kernel.global_max_dispatches`)
- Agent cap (lifetime dispatches vs `max_agents`)
- Spawn depth (nested depth vs `kernel.max_spawn_depth`)

**Write-set conflict detection** (`dispatch/conflict.go`) -- at merge time, detects file-level conflicts between concurrent dispatches.

**Merge intents** (`dispatch/intent.go`) -- transactional outbox pattern for SQLite+git coordination.

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

**Tables:** `scheduler_jobs` (v19) -- status lifecycle: `pending -> running -> completed | failed | cancelled`.

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

**audit** -- Tamper-evident audit trail with SHA-256 hash chain. Each entry includes `prev_hash` of previous entry. Per-session sequence numbers detect deletion gaps. Auto-redacts sensitive values before persistence. Table: `audit_log` (v15).

**redaction** -- Scans strings for sensitive patterns (API keys, tokens, secrets). Four modes: `off`, `warn` (report only), `redact` (replace with placeholders), `block` (set Blocked flag). Category-based allowlists.

**scoring** -- Multi-factor assignment scoring for (agent, task) pairs. Factors: base priority, agent type bonus, profile tag affinity, file focus overlap, context exhaustion penalty, file reservation conflict penalty.

**lifecycle** -- Agent state machine: `waiting -> generating -> thinking -> idle -> stalled -> error -> completed`. Stall detection via configurable velocity thresholds (tokens-per-minute).

**handoff** -- Structured session handoff format (~400 tokens). Required fields: `Goal` (what was accomplished), `Now` (what to do next). Statuses: complete, partial, blocked.

## Portfolio Orchestration

Cross-project coordination through parent/child run hierarchies.

**Portfolio runs:** Created with `ic run create --projects=p1,p2`. Children linked via `parent_run_id`. Cancellation cascades to all active children.

**Dependencies:** `project_deps` table with cycle detection (DFS + transactional add). `ic portfolio order` computes deterministic topological build order (Kahn's algorithm, lexicographic tie-breaking).

**Event relay:** `ic portfolio relay` polls child project databases (read-only via `DBPool`), relays phase events as `child_advanced`/`child_blocked`/`child_cancelled`/`child_rolledback`/`upstream_changed`. Dispatch count written to state table.

**Gates:** `children_at_phase` blocks portfolio advancement until all active children have reached the target phase. `upstreams_at_phase` blocks child advancement until upstream deps have reached the phase. Terminal runs (completed/cancelled/failed) do not block.

## Lock Module

Process-level mutual exclusion using POSIX `mkdir` atomicity -- entirely filesystem-based, no SQLite.

**Layout:** `/tmp/intercore/locks/<name>/<scope>/owner.json`

**Acquire:** Atomic `os.Mkdir` -> spin-wait with 100ms sleep -> stale detection (5s default) -> stale-break (sequential remove, not `os.RemoveAll`).

**Clean:** `syscall.Kill(pid, 0)` liveness check. `nil`/`EPERM` = alive (skip), `ESRCH` = gone (remove).
