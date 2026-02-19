# Intercore — Vision Document

**Version:** 1.5
**Date:** 2026-02-19
**Status:** Draft

---

## The Core Idea

Intercore is the orchestration kernel for autonomous software development. It provides the primitives — runs, phases, gates, dispatches, events, state, locks, scheduling — that a workflow system uses to manage the lifecycle of software from idea to shipped code.

Intercore is infrastructure, not product. It doesn't know what a "brainstorm" is — it knows what a "phase" is. It doesn't know what a "flux-drive review" is — it knows what a "dispatch" is. It doesn't care about Claude Code plugins — it cares about processes, state, and events.

The operating system built on this kernel is Clavain. The self-improvement profiler is Interspect. The companion plugins are drivers. The dispatched agents are processes. Intercore is the layer beneath all of them that makes everything durable, enforceable, and observable.

## Why a Kernel

LLMs forget. Context windows compress. Sessions end. Networks drop. Processes crash.

Every agent orchestration system must answer: **what survives when the agent dies?** Most systems answer this with ephemeral state — temp files, in-memory queues, prompt instructions that evaporate at session boundaries. Clavain started this way: ~15 temp files in `/tmp/`, bash variables that lived and died with the shell, gate logic embedded in LLM prompts that the model could simply ignore.

Intercore exists because the answer to "what survives" must be: **everything that matters**. Run state, phase transitions, gate evidence, dispatch outcomes, event history — all persisted in a SQLite WAL database that outlives any individual agent session.

This is the kernel's fundamental contract: **the system of record for what happened, what's happening, and what's allowed to happen next.**

## The Kernel / OS / Profiler Model

```
Clavain (Operating System)
├── Skills, prompts, routing tables, workflow definitions
├── Configures intercore: phase chains, gate rules, dispatch policies
├── Reacts to intercore events (agent completed → advance phase)
└── Provides the developer experience (slash commands, session hooks)

Intercore (Kernel)
├── Lifecycle: runs phases through configurable chains with gate enforcement
├── Dispatch: spawns agents, tracks liveness, collects results
├── Events: typed event bus for state changes
├── Discovery: scored discoveries with confidence-tiered autonomy gates
├── State: scoped key-value store for kernel coordination
├── Coordination: locks, sentinels, lane-based scheduling
├── Rollback: phase rewind, dispatch cancellation, discovery revert with full audit trail
├── Portfolio: cross-project runs, dependency graph, composite gate evaluation
├── Sandbox specs: stores requested/effective isolation contracts (enforcement by drivers)
└── Resource management: concurrency limits, token tracking, cost reconciliation, agent caps

Interspect (Profiler)
├── Reads kernel events (phase results, gate evidence, dispatch outcomes)
├── Correlates with human corrections
├── Proposes changes to OS configuration (routing rules, agent prompts)
└── Never modifies the kernel — only the OS layer

Interject (Research Engine)
├── Source adapters: arXiv, HN, GitHub, Exa, RSS, Anthropic docs
├── Embedding-based scoring against learned interest profile
├── Emits discovery events through kernel event bus
├── Consumes kernel events as targeted scan triggers
└── Backlog refinement: dedup, priority, dependencies, decay

Autarch (Apps / TUI Layer)
├── Bigend: multi-project mission control (agent monitoring, run dashboards)
├── Gurgeh: PRD generation with confidence scoring and spec evolution
├── Coldwine: task orchestration with agent coordination
├── Pollard: research intelligence with multi-domain hunters
├── pkg/tui: shared Bubble Tea components (ShellLayout, ChatPanel, Tokyo Night)
└── Each app migrating from own backend → intercore kernel as shared state

Companion Plugins (Drivers)
├── interflux: multi-agent review dispatch
├── interlock: file-level coordination
├── intermux: agent visibility
├── tldr-swinton: token-efficient code context
└── ... each extends the OS, all route through kernel primitives
```

### Three-Layer Architecture

The ecosystem has three distinct layers, each with clear ownership:

```
Layer 3: Drivers (Plugins)
├── Each plugin wraps one capability (review dispatch, file coordination, code mapping)
├── Plugins call `ic` directly for shared state — no Clavain bottleneck
├── Plugins are Claude Code-native but the capabilities they wrap are not
└── Examples: interflux (review), interlock (coordination), intermux (visibility)

Layer 2: OS (Clavain)
├── The opinionated workflow — sprint phases, quality gates, brainstorm→plan→ship
├── Skills orchestrate by calling both `ic` (state/gates/events) and plugins (capabilities)
├── Clavain is the developer experience: slash commands, session hooks, routing tables
└── If the host platform changes, Clavain's opinions survive; the UX wrappers are rewritten

Layer 1: Kernel (Intercore)
├── Host-agnostic Go CLI + SQLite — works from Claude Code, Codex, bare shell, or any future platform
├── State, gates, events, dispatch, discovery — the durable system of record
├── If Claude Code disappears, the kernel and all its data survive untouched
└── The "real magic" lives here: everything that matters is in `ic`
```

**The guiding principle:** Plugins work in Claude Code, but the real magic is in Clavain + Intercore. If the host platform changes, you lose UX convenience (slash commands, hooks) but not capability (state, gates, events, evidence, workflow logic). Drivers are swappable. The OS is portable. The kernel is permanent.

**What this means for plugin design:** Every plugin currently doing state management through temp files, bead metadata, or its own SQLite database should instead call `ic`. The big-bang hook cutover (see Migration Strategy below) is the forcing function — when Clavain hooks switch from temp files to `ic`, plugins that share state with those hooks must follow.

### What the Kernel Owns

The kernel provides **mechanism, not policy**. It says "a gate can block a transition" — it doesn't say "brainstorm requires an artifact." That's policy, which lives in the OS layer.

| Subsystem | Primitive | What It Provides |
|---|---|---|
| **Lifecycle** | Runs + Phases | Configurable phase chains with transition validation |
| **Gates** | Conditions at transitions | Pluggable checks evaluated before phase advancement |
| **Dispatch** | Agent processes | Spawn, liveness, collection, timeout, fan-out |
| **Events** | Typed event bus | Append-only log with consumer cursors |
| **Discovery** | Scored discoveries | Confidence-tiered autonomy gates for research intake and backlog generation |
| **State** | Scoped key-value | TTL-based storage for coordination data |
| **Coordination** | Locks + Sentinels | Mutual exclusion and time-based throttling |
| **Run Tracking** | Agents + Artifacts | What agents are active, what files were produced |
| **Rollback** | State reset + audit trail | Phase rewind, dispatch cancellation, discovery revert — preserving history |
| **Portfolio** | Cross-project runs | Multi-project run grouping with composite gate evaluation |

### What the Kernel Does Not Own

- **Phase names and semantics** — "brainstorm", "review", "polish" are Clavain vocabulary. The kernel accepts arbitrary phase chains.
- **Routing** — which agent to dispatch for which task. That's Clavain's routing table.
- **Prompt content** — skills, agent definitions, review templates. Pure OS.
- **Gate policies** — "brainstorm needs an artifact" is Clavain policy configured into kernel gates.
- **Session lifecycle** — hooks, context injection, handoff files. The OS integrating with Claude Code.
- **Self-improvement decisions** — Interspect reads kernel events and proposes OS changes.

## Process Model

Intercore is a **CLI binary**, not a daemon. Every `ic` invocation opens the SQLite database, performs its operation, and exits. There is no long-running server process.

This has important implications:

**Who calls the kernel?** The OS layer (Clavain's bash hooks) shells out to `ic` at workflow boundaries — session start, phase transitions, dispatch spawn. Claude Code plugins call `ic` from hook scripts. Interspect calls `ic` from analysis scripts. Every caller is a short-lived process.

**No background event loop.** The kernel does not poll, watch, or react on its own. Event consumption is **pull-based**: consumers call `ic events tail --consumer=<name>` to retrieve events since their last cursor position. The kernel writes events; consumers decide when to read them.

**Consumer patterns:** An OS-level event reactor (e.g., Clavain reacting to `dispatch.completed` by advancing the phase) runs as a long-lived `ic events tail -f --consumer=clavain-reactor` process with `--poll-interval`. This is an OS component, not a kernel daemon — the kernel is still stateless between calls. A TUI tails events for display. A one-off script reads events for analysis. All use the same cursor-based API.

**Why not a daemon?** Daemons add operational complexity — process management, health monitoring, restart policies, port conflicts. A CLI binary is zero-ops: it works when called, requires no lifecycle management, and can't crash between calls because it doesn't exist between calls. The SQLite database is the persistent state; the binary is stateless.

**Future consideration:** If event-driven reactions (Level 2 on the autonomy ladder) require sub-second latency, a lightweight daemon or socket-activated service could be introduced. The kernel's API surface (CLI commands) would remain the same — the daemon would be an optimization, not a new architecture. This is explicitly deferred until pull-based polling proves insufficient. When this becomes necessary, Autarch's signal broker provides a proven pattern: an in-process pub/sub fan-out with typed subscriptions, WebSocket streaming to TUI/web consumers, and backpressure handling (evict-oldest-on-full for non-durable consumers). The durable event log remains the source of truth; the broker is a real-time projection of it for latency-sensitive consumers.

## Design Principles

### 1. Mechanism, Not Policy

The kernel provides primitives. The OS provides opinions. A phase chain is a mechanism — an ordered sequence with transition rules. The decision that software development should flow through 8 specific phases is a policy that Clavain configures at run creation time.

This separation means intercore can orchestrate workflows that haven't been invented yet. A documentation project might use `draft → review → publish`. A hotfix might use `triage → fix → verify`. A research spike might use `explore → synthesize`. The kernel doesn't care — it walks a chain, evaluates gates, and records events.

### 2. Durable Over Ephemeral

If it matters, it's in the database. Phase transitions, gate evidence, dispatch outcomes, sentinel state — all persisted atomically in SQLite. The cost is write latency (transactions). The benefit is that any session, any agent, any process can query the true state of the system at any time.

One deliberate exception: **coordination locks** use filesystem `mkdir` operations, not SQLite. This is intentional — locks must work even when the database is unavailable (corruption, locked by another writer), providing a recovery path. Locks are transient coordination primitives, not persistent state. The database is the system of record; locks protect mutations to it.

Temp files, environment variables, and in-memory state are acceptable for non-critical signals. They are never acceptable for the system of record.

### 3. Observable by Default

Every state change produces an event. Phase transitions, gate evaluations (pass and fail), dispatch status changes — all written to the event log. Consumers maintain cursors for at-least-once delivery.

This means the OS doesn't need to poll for changes. It subscribes to kernel events and reacts. The TUI doesn't need to query every table — it tails the event stream. Interspect doesn't need custom instrumentation — it reads the same events everyone else reads.

### 4. Token Efficiency as Infrastructure

Tokens are the primary cost driver for autonomous agent operation. The kernel doesn't decide how to be efficient — that's OS policy. But the kernel provides the infrastructure that makes efficiency possible:

- **Token tracking per dispatch** — the kernel records how many tokens each agent consumed.
- **Token aggregation per run** — total cost per phase, per run, per project.
- **Budget events** — the kernel emits events when thresholds are crossed. The OS decides the response.
- **Phase skip primitive** — the kernel supports skipping phases with audit trails, so the OS can trim unnecessary phases for simple tasks based on its complexity heuristics.
- **Dispatch metadata** — model selection, sandbox mode, and timeout are dispatch-level data the kernel preserves, enabling the OS to analyze cost/quality tradeoffs.

### 5. Fail Safe, Not Fail Silent

When a gate blocks advancement, the evidence is recorded. When a dispatch times out, the event includes why. When a lock is broken due to staleness, the audit trail shows who held it and for how long.

The kernel never silently swallows failures. It records them, emits events about them, and lets the OS decide how to respond. This is the difference between "the system crashed and we don't know why" and "the system blocked this transition because condition X was not met, here is the evidence."

## The Autonomy Ladder

Intercore enables increasing levels of autonomous operation. Each level builds on the one below.

### Level 0: Record

The kernel records what happened. Runs, phases, dispatches, artifacts — all tracked. A human drives everything. The kernel is a logbook.

*This is where intercore started: replacing temp files with a proper database.*

### Level 1: Enforce

Gates evaluate real conditions. A run cannot advance from `planned` to `executing` without a plan artifact. The kernel enforces discipline that humans and LLMs might skip under pressure.

*This is the gates milestone — the system says "no" when preconditions aren't met.*

### Level 2: React

Events trigger automatic reactions. When a run advances to `review`, the kernel emits an event. The OS subscribes and spawns review agents. When all agents complete, the OS advances the phase. The human observes and intervenes only on exceptions.

*This is the event bus milestone — the system does the next obvious thing.*

### Level 3: Adapt

Interspect reads kernel events and correlates them with outcomes. Agents that consistently produce false positives get downweighted. Phases that never produce useful artifacts get skipped by default. Gate rules tighten or relax based on evidence.

The kernel supports this by recording structured evidence with enough dimensionality for meaningful analysis. Gate evaluations include not just pass/fail but the specific conditions checked and the artifacts examined. Dispatch outcomes include verdict quality, token cost, and wall-clock time. Over many runs, this evidence enables weighted confidence scoring across multiple dimensions — completeness, consistency, cost-effectiveness — following the pattern of Autarch's `ConfidenceScore`, which weights quality metrics (completeness 20%, consistency 25%, specificity 20%, research 20%, assumptions 15%) to produce an actionable composite score rather than a binary judgment.

The profiler proposes changes. The OS applies them as overlays. The kernel enforces the updated rules. The human reviews proposals and maintains veto power.

*This is evidence-based self-improvement — the system learns from its own history.*

### Level 4: Orchestrate

The kernel manages a portfolio of concurrent runs across multiple projects. Resource scheduling allocates agents, tokens, and compute across competing priorities. The OS defines priority rules. The kernel enforces them.

An urgent hotfix preempts a routine refactor. A high-complexity feature gets more review agents than a documentation update. Token budgets prevent runaway costs. A change in one project automatically triggers verification in downstream dependents.

*This is fleet management — the system balances competing demands across projects.*

## Kernel Subsystems

### Lifecycle (Runs + Phases)

A **run** is a unit of work with a goal, a phase chain, and a complexity level. A **phase** is a step in the chain. The kernel walks the chain, enforcing gates at each transition.

Phase chains are configurable — supplied as an ordered array at `ic run create` time. The kernel walks the array sequentially. This is linear-chain-with-skip, not a DAG.

The kernel provides a `SkipPhase(run_id, phase_id, reason, actor)` primitive that records a phase as skipped with an audit trail. This is a pure mechanism: the kernel validates the skip (phase must exist in the chain, must not already be completed), records the skip event with the caller-provided reason, and advances the phase pointer. The kernel does not decide _which_ phases to skip or _why_. All skip-decision logic — complexity scoring, heuristics, user preferences — lives entirely in the OS layer. Clavain maps its complexity model to skip sets and calls `SkipPhase` for each; the kernel records the audit trail.

**Why not DAGs:** Parallel phases introduce hard problems — convergence gates, partial failure, progress visualization. Linear chains with skip logic cover the workflow patterns we've encountered so far. The schema is designed to not preclude DAGs later, but the v1 kernel is deliberately linear — complexity is added when real workflows demand it, not speculatively.

**Optimistic concurrency:** Phase transitions use `WHERE phase = ?` to prevent double-advancement. If two processes race to advance the same run, one wins and one gets `ErrStalePhase`.

### Gates

Gates are conditions evaluated at phase transitions. The kernel provides the evaluation mechanism. The OS provides the rules.

Gate rules are data, not code — stored as configuration that maps transitions to check types. Check types are kernel-provided primitives:

- `artifact_exists` — does an artifact exist for a given phase?
- `agents_complete` — are all active agents finished?
- `verdict_exists` — does a non-rejected dispatch verdict exist?

Gate tiers control enforcement: hard gates block advancement, soft gates warn, none-tier gates skip evaluation entirely.

**Artifact identity:** An artifact is a kernel-tracked record of a file produced during a run. Each artifact has: a filesystem path, a content hash (SHA256), the dispatch that produced it, a type label (plan, brainstorm, review, diff, test-report), and a timestamp. The `artifact_exists` gate check verifies that at least one artifact record exists for the specified phase — it checks the database, not the filesystem directly. Artifact content lives on disk; the kernel tracks metadata. This follows Autarch's `RunArtifact` model (type, label, path, mime type, run ID) adapted for kernel use.

Every gate evaluation — pass or fail — produces structured evidence recorded in the event log. This evidence is the foundation for Interspect's analysis.

### Dispatch

The dispatch subsystem manages agent process lifecycle:

```
spawn → running → completed | failed | timeout | cancelled
```

Each dispatch carries a **dispatch config** — a structured record of how the agent should be executed: preferred agent backend (Claude CLI, Codex CLI), timeout, sandbox mode, model override, working directory, environment variables, and extra CLI flags. This config is captured at spawn time and stored with the dispatch record, giving provenance for every agent invocation. The pattern follows Autarch's `DispatchConfig` model, which separates billing path (subscription-cli vs api), execution constraints (sandbox, timeout), and runtime parameters (model, working directory) into a single declarative struct.

Key capabilities:
- **Fan-out tracking** — parent-child dispatch relationships for parallel agent patterns.
- **Liveness detection** — convergent signals (kill(pid,0), state file presence, sidecar appearance) handle reparented processes.
- **Verdict collection** — structured results (verdict status, summary, artifact paths) collected from completed dispatches. Follows Autarch's `Outcome` pattern: success/failure, human-readable summary, and a list of produced artifacts.
- **Spawn limits** — maximum concurrent dispatches per run, per project, globally. Prevents runaway agent proliferation (informed by OpenClaw's `maxSpawnDepth` and `maxChildrenPerAgent` patterns).
- **Backend detection** — the kernel validates that the requested agent backend (claude, codex) is available on `$PATH` before dispatching. A dispatch with an unavailable backend fails immediately with a clear error, not after timeout.

### Events

An append-only, typed event log with consumer cursors. Every state change in the kernel produces an event.

Event sources:
- Phase transitions (advance, skip, block, pause)
- Gate evaluations (pass, fail, override)
- Dispatch status changes (spawned, running, completed, failed, timeout)
- Discovery lifecycle (scanned, scored, promoted, proposed, dismissed)
- Backlog changes (refined, merged, submitted, prioritized)
- Rollback operations (run.rolled_back, phase.rolled_back, discovery.rolled_back, backlog.rolled_back)
- Cross-project signals (dependency.upstream_changed, portfolio.child_completed, portfolio.gate_evaluated)
- Cost/billing (budget.warning, budget.exceeded, cost.reconciliation_discrepancy)

**Idempotency:** Events carry a deduplication key (`source_type:source_id:action` — e.g., `dispatch:42:completed`). Producers that retry after a crash can re-emit the same event; the kernel ignores duplicates by dedup key. This makes at-least-once production safe without requiring exactly-once semantics in producers.

**Consumer cursors:** Each consumer tracks its high-water mark (the `rowid` of the last processed event). On restart, it replays from the cursor position. Cursors are stored in the state table. Two consumer classes exist:

- **Durable consumers** (e.g., Interspect, Clavain's event reactor) register with `ic events cursor register --durable`. Their cursors never expire. The kernel guarantees no event loss for durable consumers as long as events haven't been pruned by the retention policy.
- **Ephemeral consumers** (e.g., TUI sessions, one-off tails) have a TTL on their cursor (default: 7 days). After TTL expiry, the cursor is garbage-collected. On next poll, an expired ephemeral consumer receives events from the oldest retained event, not from the beginning of time.

**Event retention:** Events are pruned by a configurable retention policy (default: 30 days). The kernel guarantees that no event is pruned while any durable consumer's cursor still points before it. This means a durable consumer that falls behind can block event pruning — the OS should monitor consumer lag and alert on stale durable consumers.

**Delivery guarantee:** At-least-once from the consumer's perspective. The consumer is responsible for idempotent processing. The kernel does not track acknowledgments — cursor advancement is the consumer's responsibility (call `ic events cursor set`).

**Go API pattern:** For programmatic consumers (Interspect, TUI, future daemon), the kernel exposes a `Replay(sinceID, filter, handler)` function that iterates events matching a filter since a given cursor position, calling the handler for each. Filters are fluent builders — `NewEventFilter().WithEventTypes("phase.advanced", "dispatch.completed").WithSince(t).WithLimit(100)` — enabling consumers to express complex queries without string manipulation. This follows the pattern proven in Autarch's event spine, where the same `EventFilter` serves both CLI queries and programmatic replay.

### State

A scoped key-value store with TTL. Used **exclusively** for kernel-internal coordination data — consumer cursors, lease tokens, transient signals between kernel operations. This is not a general-purpose config store.

Keys are scoped by `scope_id` (typically a run ID or project path). Values are validated JSON with size and depth limits.

**What does not go here:** OS-level policy configuration. Gate rules, routing tables, phase chain definitions, and complexity heuristics are OS policy that lives in Clavain's config files. The kernel never stores, interprets, or manages OS policy.

**Run config snapshots:** At `ic run create` time, the kernel captures an immutable snapshot of the OS-provided configuration (phase chain, gate rules, dispatch policies) that was active when the run started. This gives provenance ("what policy was applied when this run executed?") and determinism (the run's behavior doesn't change if the OS updates its config mid-run). The snapshot is kernel-owned data, but the policy it captures is OS-authored. The kernel treats it as opaque structure — it evaluates gate rules from the snapshot but does not understand or modify the policy semantics.

### Coordination

The kernel uses two coordination mechanisms at different layers, each chosen for its failure properties:

**Locks (filesystem):** POSIX `mkdir`-based mutual exclusion. Deliberately filesystem-only — no SQLite dependency. This means locks work even when the database is corrupted or locked by another process, providing a recovery path.

**Scope invariant:** Filesystem locks protect exactly one thing: serializing read-modify-write operations on the SQLite database itself (e.g., phase advancement, dispatch creation). They are **not** a general-purpose coordination primitive. They are never used to protect non-DB resources. They are not part of the "system of record" — if the lock directory disappears, the worst case is a brief race on the next DB mutation, not data loss. The database remains the source of truth; the lock prevents concurrent mutations to that source of truth.

**Stale-breaking:** Locks include the holder's PID and hostname. Stale detection checks PID liveness (`kill(pid, 0)`). This is best-effort on single-machine deployments — PID reuse is theoretically possible but vanishingly rare for the short-lived operations locks protect (typically < 100ms). Container and multi-machine deployments would require a different coordination strategy (e.g., SQLite-native leases), which is explicitly deferred to when the deployment model demands it.

**Sentinels (SQLite):** Time-based throttle guards stored in the database. "Don't run this action more than once per N seconds." Used for rate-limiting hooks and periodic operations. Unlike locks, sentinel state is durable and survives process crashes.

**Lane-based scheduling (future):** Named concurrency lanes with configurable parallelism. A "review" lane might allow 2 concurrent agents. A "dispatch" lane might allow 4. A "critical" lane might serialize to 1. Inspired by OpenClaw's `CommandQueue` pattern.

## Contracts and Invariants

The kernel's value proposition is durability and enforcement. These are the guarantees the kernel provides to callers.

### Transactional Dual-Write

State table mutations and their corresponding events are written in the same SQLite transaction. A phase advancement writes the new phase to the runs table and appends a `phase.advanced` event atomically. There is no window where a table reflects a new state but the event log doesn't (or vice versa).

**State tables** are the canonical queryable state ("what is the current phase of run R42?"). **Events** are the canonical reaction/observation stream ("what happened since my last poll?"). Neither is derived from the other — both are primary, written together in one transaction. Consumers that want current state query tables. Consumers that want change notification tail events.

### Idempotency

Events carry a deduplication key (`source_type:source_id:action` — e.g., `dispatch:42:completed`). Producers that retry after a crash can re-emit the same event; the kernel ignores duplicates by dedup key. This makes at-least-once production safe without requiring exactly-once semantics in producers.

Phase transitions use optimistic concurrency (`WHERE phase = ?`) to prevent double-advancement. If two processes race to advance the same run, one succeeds and one receives `ErrStalePhase`. The loser should re-read state and decide whether the transition still applies.

### What the Kernel Enforces vs Records

| Category | Enforced | Recorded |
|---|---|---|
| Gate conditions | Hard gates block advancement | All evaluations (pass/fail/override) with evidence |
| Spawn limits | Max concurrent dispatches, max depth | All spawn attempts (including rejected) |
| Phase transitions | Optimistic concurrency, gate checks | Every transition with timestamp, actor, evidence |
| Coordination locks | Mutual exclusion via filesystem | Lock acquire/release/break events |
| Token usage | — | Reported counts per dispatch (self-reported by agents) |
| Sandbox contracts | — | Requested vs effective policy per dispatch |
| Discovery autonomy | Confidence tier gates (auto/propose/log/discard) | All discoveries with scores, sources, feedback signals |
| Backlog changes | Dedup threshold (blocks duplicate bead creation) | All refinements with evidence (merge, priority shift, decay) |
| Rollback | Phase reset validation, dispatch cancellation | All rollbacks with initiator, scope, reason, and affected records |
| Cross-project deps | Portfolio gate rollup (all children must pass) | Dependency change events, upstream verification triggers |
| Cost/billing | Budget threshold events | Self-reported tokens, reconciliation discrepancies |

The kernel enforces **structural invariants** (can't skip a nonexistent phase, can't exceed spawn limits, can't advance without gate passage). It records **operational metadata** (token usage, sandbox compliance) without enforcing it — enforcement of operational concerns is the OS's responsibility.

### Recovery Semantics

The kernel is crash-safe at the transaction boundary. If `ic` crashes mid-operation:

- **Before commit:** No state change occurred. The caller retries the operation. Idempotent operations (event emission, phase advancement) are safe to retry.
- **After commit:** The state change and its event were written atomically. The caller may not know the operation succeeded, but a subsequent `ic` call will see the new state.

The kernel does **not** provide automatic reconciliation between "process reality" (is the agent actually running?) and "DB state" (does the dispatch record say running?). The OS is responsible for periodic reconciliation — polling dispatch liveness and updating records for agents that died without reporting completion. The dispatch liveness subsystem (`ic dispatch poll`) provides the tools; the OS decides when and how often to run them.

**Reconciliation pattern:** The kernel provides `ic dispatch reconcile` as a fingerprint-based reconciliation primitive. Each dispatch record carries a fingerprint of its last-known state. On reconciliation, the kernel checks process liveness, compares against the stored fingerprint, and detects anomalies: status regressions (dispatch marked completed but process still running), stale dispatches (process dead but dispatch still marked running), and conflicts (dispatch state changed externally). Anomalies are logged as reconciliation events — the kernel records them but does not auto-resolve. This follows Autarch's reconciliation engine pattern, which uses SHA256 fingerprints and cursor-based change detection to emit derived events only when real state changes occur, avoiding redundant event noise.

## Sandboxing (Future)

Agent sandboxing controls **where** agents execute and **what** they can access. Sandboxing is a **dispatch driver capability**, not a kernel subsystem. The kernel's role is limited to storing contracts and recording compliance — it never enforces isolation itself.

The ownership model (informed by OpenClaw's three-layer security and NullClaw's multi-layer sandbox auto-detection):

1. **The kernel stores sandbox specs** — each dispatch record includes a `SandboxSpec` (requested tool allowlist, working directory, filesystem access mode, resource limits). These are data attached to the dispatch, not enforcement.
2. **The dispatch driver enforces the spec** — Claude Code, Codex, or a container runtime reads the spec and applies it using their native isolation mechanisms. The kernel cannot guarantee enforcement — a misconfigured or compromised driver may not honor the spec. The kernel can **refuse to dispatch** without a compliant driver registered, but cannot verify enforcement at runtime.
3. **The kernel records compliance** — dispatch completion events include the **effective** sandbox policy (what the driver actually applied) alongside the **requested** policy. Divergence between requested and effective is recorded as evidence for Interspect analysis. The kernel does not block on divergence — it records it.

### Tier 1: Tool Allowlists + Working Directory Isolation (Near-Term)

When the kernel spawns a dispatch, the sandbox contract specifies:
- Which tools the agent may use (allowlist)
- The agent's working directory (constrained to project scope)
- Filesystem access mode (read-only, read-write, none for paths outside working directory)

Claude Code and Codex already support these constraints via flags. The kernel configures and records the contract; the runner applies it.

### Tier 2: Container-Based Isolation (Future)

For fully autonomous operation — agents running for hours without human oversight — the sandbox contract expands to include container lifecycle:
- Container image and resource limits
- Mount points with controlled access
- Network isolation policy
- Result collection paths

The vision is not to build a container orchestrator — it's to provide the kernel primitives (sandbox contracts, lifecycle events, compliance auditing) that a container runtime integrates with. NullClaw's approach of auto-detecting the best available isolation mechanism (Landlock → Firejail → Bubblewrap → Docker → noop) is instructive: the kernel should support multiple enforcement backends without coupling to any one.

## Resource Management (Future)

### Token Tracking

The kernel records token usage per dispatch. This data feeds up to run-level and project-level aggregates, enabling the OS to make informed decisions about model selection, agent spawning, and phase skipping.

The kernel **tracks and reports**. The OS **decides and acts**. When a dispatch exceeds a configured threshold, the kernel emits a `budget.warning` event. The OS decides whether to kill the agent, downgrade the model, or let it continue.

### Concurrency Control

Lane-based scheduling (described under Coordination) provides the mechanism. The OS configures lane definitions and concurrency limits. The kernel enforces them.

Global limits prevent resource exhaustion:
- Maximum concurrent dispatches (across all runs)
- Maximum concurrent dispatches per run
- Maximum total active runs

### Agent Caps

Hard limits on agent proliferation:
- Maximum spawn depth (no recursive sub-agent spawning beyond configured depth)
- Maximum children per dispatch (fan-out limit)
- Maximum total agents per run

These are kernel-enforced invariants, not suggestions. An agent cannot bypass them regardless of what the LLM requests.

## Multi-Project Coordination

The Interverse monorepo contains 25+ modules, each with its own git repository. A change in intercore can break Clavain hooks. A plugin update may need testing across multiple modules. Today, there is no cross-project visibility — each project's `ic` database is an island.

Multi-project coordination extends the kernel with three complementary mechanisms:

### Cross-Project Event Bus

Kernel events from one project are visible to consumers in other projects. When intercore ships a new feature, the event is readable by Clavain's event consumer. When a plugin publishes a new version, downstream projects see the event.

**Mechanism:** A relay process (OS-layer, not kernel) tails events from multiple project databases and writes them to a shared relay database. Consumers subscribe to the relay with the same cursor-based API. The kernel provides the event format and cursor primitives; the OS provides the relay topology.

**Why relay, not shared database:** Each project's SQLite database is its transactional boundary. A shared database would serialize writes across all projects, creating contention. A relay provides eventual consistency (sub-second latency) while preserving per-project write independence.

### Portfolio-Level Runs

A run can span multiple projects. `ic run create --projects=intercore,clavain --goal="Migrate hooks"` creates a portfolio run with per-project scoping for artifacts, gates, and dispatches, but a unified run ID for tracking and reporting.

**Kernel mechanism:** The `runs` table gains an optional `portfolio_id` column. A portfolio run is a parent record that groups per-project child runs. Phase advancement in the portfolio requires gate passage in all child runs. The kernel enforces the rollup — a portfolio can't advance to "shipping" while any child is still in "executing."

**OS policy:** Which projects participate in a portfolio run, how gates compose across projects (all-pass vs majority-pass), and how dispatches are allocated across projects are OS-level decisions configured at portfolio creation time.

### Dependency Graph Awareness

The kernel knows that Clavain depends on intercore. When intercore ships a change, the kernel auto-creates a verification event for dependent projects. The OS consumes this event and creates a "verify downstream" bead in each dependent project.

**Mechanism:** A `project_deps` table maps project → dependency relationships. When a `run.completed` event fires for a project, the kernel checks for dependents and emits `dependency.upstream_changed` events for each. The OS decides what to do — auto-create a test run, create a bead, send a notification, or ignore.

**Why reactive, not pre-planned:** Dependency verification is triggered by actual changes, not by anticipated changes. This avoids the overhead of pre-scheduling verification runs for changes that might not happen.

## Cost and Billing Awareness

Token tracking (described under Resource Management) records what agents self-report. But self-reporting is insufficient for accurate cost management. The kernel needs a verification layer.

### Kernel Records, OS Verifies

The kernel records token counts per dispatch as reported by agents. The OS periodically cross-references these counts against API billing data (Claude usage exports, Anthropic billing API). Discrepancies are recorded as reconciliation events.

**What the kernel provides:**
- Per-dispatch token counts (input, output, cache hits) — self-reported by agents
- Per-run token aggregates — computed from dispatch records
- Per-project token aggregates — computed from run records
- Budget threshold events — emitted when configurable per-run or per-project thresholds are crossed

**What the OS provides:**
- Billing API integration — polling Anthropic's usage API for actual costs
- Reconciliation logic — comparing self-reported counts against billed counts
- Cost alerting — notifications when spending exceeds budgets
- Model selection policy — choosing cheaper models for low-complexity tasks based on cost data

**Future consideration:** If reconciliation consistently shows significant discrepancies, the runner layer could inject token tracking at the API call level — wrapping the LLM API client to capture actual token counts before the agent sees them. This is Tier 2 mitigation, deferred until self-reporting proves insufficient.

## Rollback and Recovery

When a sprint goes wrong — bad code, skipped gates, or erroneous discovery-created beads — there is no structured way to revert. Git handles code rollback. Nothing handles workflow state rollback or backlog rollback. This is a gap.

### Three Rollback Layers

**Code rollback.** Git revert handles code. The kernel records which commits were produced by which dispatches (artifact metadata includes git SHA). `ic run rollback <id> --layer=code` identifies all commits associated with a run's dispatches and generates a `git revert` sequence. The kernel doesn't execute the revert — it produces the plan. The OS or human executes it.

**Workflow state rollback.** When a sprint advances too fast or skips a gate incorrectly, the run's phase needs to reset. `ic run rollback <id> --to-phase=plan-review` resets the run's current phase, marks intervening phase transitions as `rolled_back` (not deleted — audit trail is preserved), and re-evaluates gates for the target phase. Dispatches spawned in rolled-back phases are cancelled if still running.

**Backlog rollback.** When the discovery pipeline auto-creates beads from a bad signal (noisy source, miscalibrated profile), the backlog needs cleanup. `ic discovery rollback --source=<source> --since=<timestamp>` identifies all beads created from discoveries by that source since the given time. It proposes closing them (with reason `rolled_back:discovery`) — the human confirms. Priority shifts and dependency suggestions triggered by rolled-back discoveries are also reverted.

### Rollback Audit Trail

Every rollback is recorded as a typed event: `run.rolled_back`, `phase.rolled_back`, `discovery.rolled_back`, `backlog.rolled_back`. The event includes who initiated the rollback, what was reverted, and why. This evidence feeds Interspect — patterns of rollbacks indicate systemic issues (e.g., a phase that's frequently rolled back should have stronger gates).

### Rollback Is Not Undo

Rollback resets state to enable re-execution. It does not erase history. All events, dispatches, and artifacts from the rolled-back period are preserved with `rolled_back` status. This means:
- Interspect can analyze what went wrong (the evidence exists)
- Billing data is accurate (the tokens were consumed)
- The audit trail is complete (no gaps in the event log)

## Migration Strategy

### Hook Cutover: Big-Bang

Clavain currently maintains ~20 temp-file state mechanisms in `/tmp/` (see gap analysis). The migration to intercore is a **big-bang cutover** — all hooks switch to `ic` in one release. `ic` becomes a hard dependency.

**Why not gradual?** Dual-path state management (try `ic`, fall back to temp files) creates a consistency nightmare. Two sources of truth for sentinel state, dispatch tracking, or phase position means every consumer must handle both. The complexity tax of maintaining fallback paths exceeds the risk of a clean cutover.

**What the cutover involves:**

| Current Pattern | `ic` Replacement |
|---|---|
| `/tmp/clavain-compound-last-${SID}` (throttle) | `ic sentinel check compound --ttl=300` |
| `/tmp/clavain-drift-last-${SID}` (throttle) | `ic sentinel check drift --ttl=600` |
| `/tmp/clavain-autopub.lock` (lock) | `ic lock acquire autopub session` |
| `/tmp/clavain-dispatch-$$.json` (dispatch state) | `ic dispatch status <id> --json` |
| `/tmp/intercheck-${SID}.json` (accumulator) | `ic state set intercheck.count <n> --scope=session` |
| `.clavain/scratch/handoff-*.md` (session state) | `ic run` + `ic state` (run state outlives sessions) |
| `.clavain/scratch/inflight-agents.json` | `ic dispatch list --active` |
| `/tmp/clavain-bead-${SID}.json` (phase sideband) | `ic run phase <id>` |

**Prerequisite:** The `ic` binary must be built and available on `$PATH` before the cutover release ships. The launcher script pattern (already used for MCP servers) handles first-run compilation.

### Sprint Migration: Hybrid to Kernel-Driven

The sprint skill currently orchestrates the full brainstorm→ship workflow through slash commands, with its own phase state in beads metadata and checkpoint JSON. Migration to kernel-driven sprints is staged:

**Phase 1 (Hybrid):** The sprint skill calls `ic run create` at sprint start and `ic run advance` alongside its existing logic. Both systems track phase state. The skill still drives the workflow. This phase provides kernel tracking and event emission without requiring a skill rewrite.

**Phase 2 (Handover):** `ic run advance` becomes the authoritative advancement call. The skill stops maintaining its own phase state. Gate enforcement is real — `ic run advance` returns exit code 1 and the phase doesn't change. The skill becomes a UX wrapper that translates user intent into kernel calls.

**Phase 3 (Kernel-driven):** The sprint skill is a thin experience layer. It prompts the user, invokes slash commands, and displays results. All state, gates, events, and dispatch tracking flow through the kernel. If the sprint skill breaks, the run state is intact and recoverable via `ic` CLI.

**Key milestone:** Phase 2 completion means the gap analysis item #2 ("gate enforcement is just prompting") is fully resolved. Gates become kernel-enforced invariants, not prompt suggestions.

### Interspect Migration: Staged to Kernel Events

Interspect currently operates with its own SQLite database and hook-based evidence collection. Migration to kernel events preserves the mechanism/policy separation — Interspect reads kernel data but doesn't own it.

**Phase 1:** Interspect becomes a consumer of existing kernel events (phase transitions, gate evaluations, dispatch outcomes) via `ic events tail --consumer=interspect --durable`. Its own DB supplements with Interspect-specific data (agent quality scores, false-positive patterns, routing proposals).

**Phase 2:** Add correction and override event types to the kernel. When a human overrides an agent finding or marks a false positive, the kernel records it as a typed event. Interspect reads these events instead of collecting them via its own hooks.

**Phase 3:** Retire Interspect's own SQLite database. Interspect's state becomes a materialized view derived entirely from kernel events. Single source of truth.

## Apps and TUI Layer (Autarch)

Autarch is merging into the Interverse monorepo as the apps/TUI layer — the visual, interactive surface for intercore's kernel state. Where Clavain provides the developer experience via CLI skills and hooks, Autarch provides the developer experience via rich terminal UIs.

### The Four Tools

**Bigend** — Multi-project mission control. A read-only aggregator that monitors agent activity, displays run progress, and provides a dashboard view across projects. Currently discovers projects via filesystem scanning and monitors agents via tmux session heuristics. Has both a web interface (htmx + Tailwind) and an in-progress TUI.

**Gurgeh** — PRD generation and validation. The most mature tool. Drives an 8-phase spec sprint with per-phase AI generation, confidence scoring (0.0–1.0 across completeness, consistency, specificity, and research axes), cross-section consistency checking, assumption confidence decay, and spec evolution versioning. Specs persist as YAML.

**Coldwine** — Task orchestration. Reads Gurgeh PRDs, generates epics/stories/tasks, manages git worktrees, coordinates agent execution, and integrates with Intermute for messaging. Has a full Bubble Tea TUI (the largest single view at 2200+ lines).

**Pollard** — Research intelligence. Multi-domain hunters (tech, academic, medical, legal, GitHub), continuous watch mode, and insight synthesis. CLI-first with integration into Gurgeh and Coldwine.

### Shared Component Library: `pkg/tui`

Autarch's shared TUI component library is fully portable and immediately reusable:

- `ShellLayout` — split-pane layout with resizable panels
- `ChatPanel` — streaming chat interface with message history
- `Composer` — text input with command completion
- `CommandPicker` — fuzzy-searchable command palette
- `AgentSelector` — agent selection with status indicators
- `View` interface — clean abstraction for pluggable view implementations
- Tokyo Night color scheme — consistent theming across all views

These components depend only on Bubble Tea and lipgloss. They have no Autarch domain coupling.

### Migration to Intercore Backend

Each tool migrates from its own storage backend (YAML files, tool-specific SQLite) to intercore's kernel as the shared state layer. The migration follows coupling depth — least coupled tools migrate first:

**1. Bigend (read-only — migrate first).** Bigend is a pure observer. Today it discovers projects via filesystem scanning and monitors agents by scraping tmux panes. Migration swaps these data sources:
- Project discovery → `ic run list` across project databases
- Agent monitoring → `ic dispatch list --active`
- Run progress → `ic events tail --all --consumer=bigend`
- Dashboard metrics → kernel aggregates (runs per state, dispatches per status, token totals)

Bigend never writes to the kernel — it only reads. This makes it the lowest-risk migration and the first validation that the kernel provides sufficient observability data.

**2. Pollard (research → discovery pipeline).** Pollard's multi-domain hunters map directly to intercore's discovery subsystem. Migration connects Pollard's research output to the kernel:
- Hunter results → `ic discovery` events through the kernel event bus
- Insight scoring → kernel confidence scoring with Pollard's domain-specific weights
- Research queries → `ic discovery search` for semantic retrieval
- Watch mode → kernel event consumer that triggers targeted scans

Pollard becomes the scanner component that feeds the discovery → backlog pipeline described in the Autonomous Research section. Its hunters become intercore source adapters.

**3. Gurgeh (PRD generation → run lifecycle).** Gurgeh's 8-phase spec sprint maps to intercore's run lifecycle with a custom phase chain. Migration creates runs for PRD generation:
- Spec sprint → `ic run create --phases='["vision","problem","users","features","cujs","requirements","scope","acceptance"]'`
- Phase confidence scores → kernel gate evidence (Gurgeh's confidence thresholds become gate rules)
- Spec artifacts → `ic run artifact add` for each generated section
- Spec evolution → run versioning (new run per spec revision, linked via portfolio)

Gurgeh's arbiter (the sprint orchestration engine) remains as tool-specific logic — it drives the LLM conversation that generates each spec section. The kernel tracks the lifecycle; Gurgeh provides the intelligence.

**4. Coldwine (task orchestration — migrate last).** Coldwine has the deepest coupling to Autarch's domain model (`Initiative → Epic → Story → Task`). Its migration is the most complex:
- Task hierarchy → beads (Coldwine's planning hierarchy maps to bead types and dependencies)
- Agent coordination → `ic dispatch` for agent lifecycle
- Git worktree management → remains in Coldwine (kernel doesn't manage git)
- Intermute integration → remains in Coldwine (kernel doesn't manage messaging)

Coldwine's migration overlaps with Clavain's sprint skill — both orchestrate task execution with agent dispatch. The resolution is that Coldwine provides TUI-driven orchestration while Clavain provides CLI-driven orchestration, both calling the same kernel primitives.

### Relationship to the Three-Layer Architecture

Autarch sits alongside Clavain at Layer 2 (OS), providing an alternative interaction surface:

```
User Interaction
├── Clavain (CLI: slash commands, hooks, skills) → calls ic
├── Autarch (TUI: Bigend, Gurgeh, Coldwine, Pollard) → calls ic
└── Direct CLI (ic run, ic dispatch, ic events) → for power users and scripts

All three surfaces share the same kernel state. A run created via Clavain's /sprint
is visible in Bigend's dashboard. A discovery from Pollard's hunters triggers the same
kernel events that Clavain's hooks consume. The kernel is the single source of truth.
```

### What `pkg/tui` Enables

Beyond the four Autarch tools, the shared component library enables a lightweight `ic tui` subcommand — a kernel-native TUI that provides basic observability without requiring the full Autarch tool suite:

- Run list with phase progress bars
- Event stream tail (live-updating)
- Dispatch status dashboard
- Discovery inbox for confidence-tiered review

This minimal TUI would be built on `pkg/tui` components and call `ic` directly. It's the kernel's own status display — simpler than Bigend but always available wherever `ic` is installed.

## Autonomous Research and Backlog Intelligence

The kernel's first three levels — Record, Enforce, React — focus on work that's already been defined. A human creates a run, the kernel tracks it. But where does the work come from? How does the system discover that a new arXiv paper invalidates an assumption, that an upstream dependency shipped a breaking change, or that three separate session transcripts reveal the same untracked pain point?

Autonomous research and backlog intelligence close the loop between **what the world knows** and **what the system is working on**. This is where the autonomy ladder extends beyond execution into discovery.

### The Discovery → Backlog Pipeline

```
Sources                     Scoring & Triage           Backlog Actions
─────────────────          ──────────────────         ──────────────────
arXiv (Atom feeds)    ┐
Hacker News (API)     │     Embedding-based           High confidence:
GitHub (releases,     │     relevance scoring    ──→    auto-create bead
  issues, READMEs)    ├──→  against learned             + briefing doc
Exa (semantic web     │     interest profile
  search)             │                               Medium confidence:
Anthropic docs        │     Confidence tiers:    ──→    propose to human
  (change detection)  │     high / medium /              (inbox review)
RSS/Atom feeds        │     low / discard
  (general)           │                               Low confidence:
User submissions      ┘     Adaptive thresholds  ──→    log only
                            (shift with feedback)

Internal signals                                      Backlog refinement:
─────────────────                                     ──────────────────
Beads history              Feedback loop:              merge duplicates
Solution docs        ──→   promotions strengthen ──→   update priorities
Error patterns             dismissals weaken           suggest dependencies
Session telemetry          source trust adapts          decay stale items
Kernel events              thresholds shift             link related work
```

### Kernel vs OS Responsibilities

This subsystem follows the same mechanism/policy separation as the rest of the kernel. The kernel provides primitives for tracking discoveries, scoring confidence, and emitting events. The OS decides what sources to scan, what confidence thresholds to use, and how aggressively to act on discoveries.

**What the kernel provides (mechanism):**

| Primitive | What It Does |
|---|---|
| **Discovery records** | Durable storage for scored discoveries with embedding vectors, source metadata, and lifecycle state |
| **Confidence scoring** | Embedding-based similarity against a learned profile vector, with configurable weight multipliers for source trust, keyword matches, recency, and capability gaps |
| **Confidence-tiered action gates** | Kernel-enforced thresholds that control which autonomy tier a discovery reaches — auto-execute, propose-to-human, log-only, or discard |
| **Discovery events** | Typed events (`discovery.scanned`, `discovery.scored`, `discovery.promoted`, `discovery.proposed`, `discovery.dismissed`) that flow through the same event bus as phase and dispatch events |
| **Backlog events** | Typed events (`backlog.refined`, `backlog.merged`, `backlog.submitted`, `backlog.prioritized`) for tracking how the backlog evolves from research signals |
| **Feedback ingestion** | Structured recording of human actions (promote, dismiss, adjust priority) that update the interest profile and source trust weights |

**What the OS provides (policy):**

- **Source configuration** — which RSS feeds, arXiv categories, GitHub repos, and search queries to monitor
- **Scan scheduling** — how often to scan (4x daily via systemd timer, event-driven after run completion, user-initiated via slash command)
- **Confidence thresholds** — what scores map to high/medium/low/discard tiers
- **Autonomy policy** — what the system does at each tier (create bead? write briefing? propose only?)
- **Backlog refinement rules** — dedup similarity threshold, staleness decay rate, priority escalation criteria
- **Interest profile management** — topic weights, keyword lists, source trust overrides

### Three Trigger Modes

The discovery pipeline can be triggered three ways, all producing the same event stream:

**Scheduled (background).** A systemd timer runs the scanner at configurable intervals (default: 4x daily with randomized jitter). Each scan queries all configured sources, scores discoveries against the interest profile, routes through the confidence gate, and emits kernel events. This is the "always watching" mode — the system continuously monitors the landscape without human initiation.

**Event-driven (reactive).** The scanner registers as an intercore event bus consumer. When specific kernel events occur, the scanner runs a targeted search:

- `run.completed` → search for literature related to the run's goal (did someone else solve this differently?)
- `bead.created` with `source: user` → check for existing research on the topic
- `dispatch.completed` with verdict containing novel technique → search for prior art
- `discovery.promoted` → search for related discoveries that strengthen or contradict the promoted one

This is not redundant with scheduled scans. Scheduled scans cast a wide net. Event-driven scans are targeted — they use the specific context of the triggering event to formulate precise queries. A scheduled scan might find "new MCP frameworks" generally; an event-driven scan after a dispatch timeout might find "MCP connection pooling best practices" specifically.

**User-initiated (on-demand).** Three entry points:

- `ic discovery scan` — trigger a full scan now (CLI equivalent of the scheduled timer)
- `ic discovery submit --text="..." --source=user` — submit a topic, URL, or idea for triage through the same scoring pipeline
- `ic discovery search --query="..."` — semantic search across stored discoveries using embedding similarity

User submissions flow through the same confidence scoring as automated discoveries, with one key difference: user-submitted items receive a source trust bonus (configurable, default 0.2) that reflects the signal value of a human choosing to submit something. A user submission that also scores high against the interest profile is very likely to be actionable.

### Confidence-Tiered Autonomy

The kernel enforces a confidence-gated autonomy model. Each discovery is scored and assigned to a tier. The tier determines what the system can do without human approval.

| Tier | Score Range | Autonomous Action | Human Action Required |
|---|---|---|---|
| **High** | ≥ 0.8 | Create bead (P3 default), write briefing doc, emit `discovery.promoted` | Notification in session inbox; human can adjust priority or dismiss |
| **Medium** | 0.5 – 0.8 | Write briefing draft, emit `discovery.proposed` | Appears in inbox; human promotes, dismisses, or adjusts |
| **Low** | 0.3 – 0.5 | Record in discoveries database, emit `discovery.scored` | Searchable via `ic discovery search`; not actively surfaced |
| **Discard** | < 0.3 | Record with `discarded` status | Not surfaced; contributes to negative signal for profile tuning |

This is a **kernel-enforced gate**, not a prompt suggestion. The scoring model produces a number; the tier boundaries are configuration; the action constraints at each tier are invariants. An OS-layer component cannot auto-create a bead for a discovery scored at 0.4 — the kernel will reject the promotion. The human can always override (promote a low-scoring discovery manually), and that override is recorded as a feedback signal that adjusts the profile.

**Adaptive thresholds:** Tier boundaries shift based on the promotion-to-discovery ratio. If humans consistently promote items the system scored as Medium (>30% promotion rate), the High threshold lowers by 0.02 per feedback cycle. If humans consistently dismiss items scored as High (<10% promotion rate), the threshold rises. The thresholds converge toward the human's actual decision boundary over time. The kernel records the threshold history — Interspect can analyze whether the thresholds are converging, oscillating, or drifting.

### Backlog Refinement

Raw discovery is necessary but not sufficient. A stream of unprocessed research findings creates its own kind of noise. The backlog refinement subsystem transforms raw discoveries into actionable, well-connected work items.

**Deduplication.** When a new discovery arrives, its embedding is compared against all open beads (cosine similarity). If similarity exceeds 0.85, the discovery is linked as additional evidence to the existing bead rather than creating a new one. This prevents the "100 beads about the same MCP framework" failure mode. The dedup threshold is configurable and its effectiveness is tracked — if the dedup rate is very high, the scanner may be over-covering a topic.

**Priority refinement.** When multiple independent sources converge on the same topic — an arXiv paper, a Hacker News discussion, and a GitHub release all about the same capability — the kernel bumps the associated bead's priority. The escalation rule is configurable: default requires 3+ independent sources within 7 days. This is evidence-based prioritization — not "this seems important" but "three independent signals confirm this matters."

**Dependency suggestion.** If a discovery about capability A references capability B, and B is tracked as a separate bead, the refinement engine suggests a dependency link. These suggestions are proposed, not auto-applied — dependency structure affects execution order and should have human review.

**Staleness decay.** Beads created from discoveries that are never promoted, never worked on, and receive no additional evidence decay in priority over time (configurable rate, default: one priority level per 30 days without activity). A P2 research bead that sits untouched for 60 days becomes P4. This prevents the backlog from growing without bound. Decayed beads that receive new evidence are re-evaluated — fresh signal reverses decay.

**Weekly digest.** A periodic rollup of research activity: what was discovered, what was promoted, what's trending across sources, what decayed, and what the interest profile learned. This is the human's checkpoint — a summary that lets them validate the system's autonomous decisions and course-correct the profile.

### The Feedback Loop

The discovery → backlog pipeline is not a one-way funnel. Human actions on discoveries feed back into the scoring model:

```
Discovery scored → Human promotes → Profile vector shifts toward discovery embedding
                                     Source trust for that source increases
                                     Adaptive threshold adjusts

Discovery scored → Human dismisses → Profile vector shifts away from discovery embedding
                                      Source trust for that source decreases
                                      If pattern: source deprioritized

Bead shipped     → Feedback signal → Discovery that created the bead marked "validated"
                                      Source that produced it gets trust bonus
                                      Similar future discoveries score higher
```

This is the same evidence-based improvement pattern that Interspect uses for agent routing — but applied to research intake. Over time, the system learns what the developer cares about, which sources produce actionable discoveries, and what confidence thresholds align with human judgment.

### Relationship to the Autonomy Ladder

Autonomous research extends the autonomy ladder with a capability that precedes Level 0:

**Level -1: Discover.** Before the system can record, enforce, or react to work, it must know what work exists. Autonomous research is the input funnel — the system scans the landscape, identifies relevant signals, and proposes them as work items. This is the difference between "the system executes what you tell it" and "the system finds things worth doing."

At Level 2 (React), the discovery pipeline becomes event-driven — kernel events trigger targeted research. At Level 3 (Adapt), the profile evolves from feedback. At Level 4 (Orchestrate), the discovery pipeline feeds the portfolio manager — competing research signals are weighed against resource constraints and strategic priorities across multiple projects.

### What Already Exists

The interject plugin already implements the core discovery engine: source adapters (arXiv, Hacker News, GitHub, Anthropic docs, Exa semantic search), embedding-based scoring (all-MiniLM-L6-v2, 384 dims), adaptive thresholds, and an output pipeline that creates beads and writes briefing docs. The intersearch library provides shared embedding and Exa search infrastructure. Systemd timer configs exist but are not installed.

What's missing is the kernel integration. Today, interject operates as a standalone silo with its own SQLite database, its own scheduling (not running), and no connection to the event bus. Discoveries don't produce kernel events. The scanner can't react to kernel events. The confidence gate auto-creates beads at medium tier without human review. There's no backlog refinement — no dedup, no priority updating, no dependency suggestion, no staleness decay.

The path forward is connecting interject to intercore: emit discovery events through the kernel event bus, consume kernel events as scan triggers, enforce confidence tiers as kernel gates, and add the backlog refinement engine as an event consumer that reads both discovery events and bead lifecycle events.

## What Makes This Different

### Landscape

Existing agent orchestration systems address parts of this problem. Understanding where they overlap and diverge with Intercore clarifies what the kernel contributes.

**Single-agent toolkits** (pi-mono, Claude Code, Cursor) manage one agent's tool loop, context window, and session state. pi-mono's EventStream primitive and two-phase context transform are best-in-class for single-agent operation. These systems focus on the agent-level problem and don't attempt multi-agent coordination or workflow lifecycle management.

**Multi-agent frameworks** (LangGraph, CrewAI, AutoGen) orchestrate multiple agents working on a task. LangGraph provides checkpoint-based persistence, durable execution, and human-in-the-loop interrupts with time-travel replay. CrewAI Flows offers persistence with a SQLite backend and state recovery. These are real, production-grade persistence capabilities — not in-memory toys. However, they are general-purpose graph/pipeline engines. Their primitives (nodes, edges, state channels) are domain-agnostic. Building software development lifecycle concepts (artifact-gated advancement, review verdict collection, phase-specific dispatch policies) on top of them requires custom application code.

**Personal AI gateways** (OpenClaw, NullClaw) route messages to agents across channels. OpenClaw has lane-based scheduling with concurrency caps, three-layer security (sandbox + tool policy + elevated), and sub-agent spawn limits with depth tracking. NullClaw achieves similar goals with a minimal Zig implementation and multi-layer sandbox auto-detection (Landlock → Firejail → Bubblewrap → Docker → noop). These systems excel at message routing and agent containment. They don't model task decomposition, workflow phases, or quality enforcement.

**Durable execution engines** (Temporal, Restate) provide crash-proof workflows with state captured at each step, retry policies, and saga patterns. Temporal is the closest conceptual relative to Intercore's durability story. The key differences are deployment model and domain specialization (see "Why not Temporal?" below).

**Autarch** (merging into Interverse) is a tool-first approach to the same problem space — four Go TUI tools (PRD generation, task orchestration, research intelligence, mission control) sharing a `pkg/contract` entity model, `pkg/events` event spine, and `pkg/db` SQLite helper. Autarch and Intercore share the same SQLite driver (`modernc.org/sqlite`), the same WAL/NORMAL/MaxOpenConns(1) configuration, and overlapping domain concepts (runs, artifacts, dispatches). Intercore adopts several Autarch patterns directly: the fluent `EventFilter` and `Replay()` API for event consumption, the fingerprint-based reconciliation engine for detecting state drift, the `DispatchConfig` struct for agent spawn parameters, and the `ConfidenceScore` model for weighted evidence quality analysis. Autarch is merging into the Interverse monorepo as the apps/TUI layer, with its tools progressively migrating from their own YAML/SQLite backends to intercore's kernel as the shared state backend (see Apps and TUI Layer below).

### Where Intercore Contributes

Rather than claiming unique capabilities in absolute terms, Intercore's contribution is making specific things **first-class and kernel-enforced** that other systems treat as application-layer concerns:

1. **Artifact-backed workflow enforcement** — Gates check real artifacts (plans, diffs, test reports, review verdicts) that live in the software delivery substrate. LangGraph can implement similar checks in application code; Intercore makes `artifact_exists` and `verdict_exists` kernel primitives with structured evidence recording.

2. **Durable run ledger across heterogeneous runtimes** — The kernel persists orchestration state independently of whether the agent is Claude Code, Codex, or a container runner. The run ledger is a single SQLite WAL database that any session — from any runtime — can query. LangGraph checkpoints are tied to the LangGraph runtime; Intercore's ledger is runtime-agnostic.

3. **Kernel-enforced invariants below the agent** — Spawn depth limits, concurrency lanes, and gate enforcement are kernel-level invariants that cannot be bypassed by prompts, agent code, or OS configuration. The kernel says "no" regardless of what the LLM requests. Most frameworks enforce limits at the application layer, where they can be circumvented.

4. **Evidence-based self-improvement infrastructure** — Every gate evaluation, dispatch outcome, and human override is recorded as structured evidence. Interspect reads this evidence stream and proposes OS configuration changes. Other frameworks can persist state; Intercore persists the _meta-state_ of how well the system is performing, providing the data foundation for closed-loop improvement.

5. **Mechanism/policy separation** — The kernel provides primitives. The OS provides opinions. The same kernel runs an 8-phase software sprint, a 3-phase documentation workflow, or a 2-phase hotfix — configured by the OS at run creation time, not embedded in kernel code.

### Why Not Temporal?

Intercore occupies similar conceptual territory as durable execution engines like Temporal and Restate — crash-proof workflows with state captured at each step. A contributor familiar with Temporal will reasonably ask why not just build on it. The differences are deployment model and domain specialization:

- **Local-first.** Intercore is a single Go binary with a SQLite file. No cluster, no Docker Compose, no Kubernetes operator. It runs where the developer works. Temporal requires a server, a database (Cassandra/MySQL/Postgres), and worker processes.
- **Software lifecycle primitives.** Temporal provides generic durable functions. Intercore provides artifacts, gates, verdicts, and dispatch tracking — primitives shaped for the specific problem of shipping software with agents. Building these on Temporal would mean encoding domain logic in Temporal workflows, losing the kernel/OS separation.
- **Kernel/OS separation.** Temporal workflows embed business logic in code (Go/Java/Python activities). Intercore separates mechanism (kernel) from policy (OS), enabling Interspect to modify workflow behavior through configuration changes, not code deploys. This separation is what makes evidence-based self-improvement tractable — the profiler changes config, not code.
- **Operational footprint.** A team already running Temporal could implement similar orchestration on it. Intercore's bet is that a purpose-built, local-first kernel with a ~5MB binary is a better fit for autonomous development environments than adapting a distributed workflow engine.

## Concrete Scenario: A Feature Sprint

To ground the abstract primitives, here is how a typical feature flows through the kernel:

```
1. OS creates a run:
   ic run create --project=. --goal="Add user auth" --complexity=3 \
     --phases='["brainstorm","strategize","plan","plan-review","execute","test","review","ship"]'

2. Kernel records: run_id=R42, phase=brainstorm, complexity=3

3. User completes brainstorm. OS calls:
   ic run advance R42

   Kernel evaluates gate for brainstorm→strategize transition.
   Gate rule: artifact_exists(phase=brainstorm).
   Kernel checks artifacts table → brainstorm doc exists → gate passes.
   Kernel records: phase transition event, gate evidence (pass).

4. OS dispatches review agents at plan-review phase:
   ic dispatch spawn --run=R42 --name=fd-architecture --prompt-file=review.md
   ic dispatch spawn --run=R42 --name=fd-quality --prompt-file=review.md

   Kernel records dispatches, tracks PIDs, enforces spawn limits.

5. Agents complete. OS calls:
   ic run advance R42

   Gate rule: agents_complete(phase=plan-review).
   Kernel checks dispatch table → all dispatches completed → gate passes.

6. At each step, kernel emits events:
   phase.advanced, gate.passed, dispatch.spawned, dispatch.completed, ...

   Interspect consumer tails these events via cursor.
   TUI consumer tails them for display.
   Neither needs to know about the other.
```

The kernel doesn't know what "brainstorm" means. It knows that phase 0 requires an artifact to advance to phase 1, that phases 3-4 have active dispatches that must complete, and that all of this is recorded durably.

## Assumptions and Constraints

**Single-machine, single-database.** The kernel assumes a single-machine deployment where all callers can access the same SQLite database file. Multiple OS processes can read concurrently (WAL mode); writes are serialized by SQLite's write lock, with filesystem locks providing application-level mutual exclusion for read-modify-write operations. Multiple repos can use separate databases (one `.db` per project) or share one — the kernel scopes by run ID, not by filesystem location. Multi-machine coordination (distributed locks, replicated event logs) is explicitly out of scope. If the system needs to scale beyond a single machine, the database layer would need replacement — but single-machine operation is the expected deployment model for autonomous development environments.

**Callers are cooperative.** The kernel enforces invariants (gate conditions, spawn limits, optimistic concurrency) but does not defend against malicious callers. An agent could bypass the kernel entirely by writing to the filesystem. The security model assumes trusted but fallible callers — the kernel prevents mistakes, not attacks.

**Token tracking is self-reported.** The kernel records token counts that dispatched agents report. It cannot independently verify these counts. An agent that misreports its token usage undermines budgeting. Tier 1 mitigation: the OS can cross-reference reported counts with API billing data. Tier 2: the runner could inject token tracking at the API call layer.

**Event and data retention.** The event log grows unboundedly without pruning. The kernel provides `ic events prune --older-than=30d` for event retention and `ic run prune --older-than=90d` for completed run cleanup. The OS is responsible for scheduling these — the kernel does not auto-prune. SQLite `VACUUM` should be run periodically by the OS after significant pruning to reclaim disk space. Pre-pruning backup is recommended (the kernel's existing auto-backup mechanism covers migration scenarios, not routine maintenance).

**Clock monotonicity.** TTL computations, sentinel timing, and event ordering assume monotonic system time. NTP jumps or clock skew on the host could cause incorrect TTL expirations or event ordering anomalies. The kernel uses Go's `time.Now().Unix()` (not SQLite's `unixepoch()`) to avoid float promotion, but doesn't guard against backward clock jumps.

**Open-source product.** Intercore is designed for community adoption, not just personal use. This creates obligations that personal tools don't have:
- **API stability.** CLI flags, event schemas, and database schemas need backward compatibility discipline from v1 onward. Breaking changes require migration paths and deprecation periods.
- **Documentation for strangers.** The vision doc and AGENTS.md are written for the maintainer. An open-source product needs installation guides, quickstart tutorials, concept explanations, and configuration references for people who don't know what Clavain is or why temp files are a problem.
- **Sensible defaults.** The kernel should work out of the box with zero configuration. Phase chains, gate rules, and throttle intervals should have defaults that cover common cases. Power users customize; new users get something useful immediately.
- **Error messages for humans.** Every `ic` error should explain what went wrong and suggest a fix. "ErrStalePhase" is an internal name; the CLI should say "Another process already advanced this run. Re-read the current phase with `ic run phase <id>` and decide if your advance still applies."

## Success at Each Horizon

| Horizon | Timeframe | What Success Looks Like |
|---|---|---|
| v1 | Current | Gates enforce real conditions. Events flow. Dispatches are tracked. The kernel is the system of record. |
| v1.5 | 1-2 months | Big-bang hook cutover — all Clavain hooks call `ic` instead of temp files. Sprint skill enters hybrid mode (calls `ic run` alongside existing logic). Fully custom phase chains with sprint as default preset. API stability contract established for open-source readiness. Autarch merged into Interverse monorepo. |
| v2 | 2-4 months | Sprint skill hands over phase control to kernel (hybrid→kernel-driven). Lane-based scheduling. Token tracking per dispatch with OS-level billing verification. Discovery events flow through the kernel event bus. Scheduled scanning runs autonomously. Interspect Phase 1 (kernel event consumer). Rollback primitives for workflow state. Bigend migrated to kernel backend (read-only dashboard over `ic` state). Minimal `ic tui` subcommand using `pkg/tui` components. |
| v3 | 4-8 months | Interspect Phase 2-3 (correction events, retire own DB). Sandboxing Tier 1 (tool allowlists, multi-agent isolation). Confidence-tiered autonomy gates enforce discovery → backlog policy. Backlog refinement runs as an event consumer. Code and backlog rollback. Cross-project event relay. Pollard migrated to kernel discovery pipeline. Gurgeh PRD generation backed by kernel runs. Installation guide and quickstart for community adopters. |
| v4 | 8-14 months | Portfolio-level runs across multiple projects. Dependency graph awareness with auto-verification. Resource scheduling across competing priorities. Sandboxing Tier 2 (containers). Discovery pipeline feeds portfolio prioritization. Coldwine task orchestration backed by kernel dispatches. Full Autarch TUI suite reads kernel state as single source of truth. The kernel orchestrates a fleet and knows what it should be working on next. |

## What This Is Not

- **Not an LLM framework.** Intercore doesn't call LLMs, manage context windows, or process natural language. That's what the dispatched agents do.
- **Not a Claude Code plugin.** Intercore is a Go CLI binary with a SQLite database. It's infrastructure that plugins call, not a plugin itself.
- **Not a workflow DSL.** The kernel provides primitives (phases, gates, events). It doesn't provide a language for defining workflows. The OS maps its domain concepts to kernel primitives.
- **Not a replacement for Clavain.** Clavain is the developer experience layer — the opinionated workflow, the sprint sequence, the quality gates philosophy. Intercore is the engine beneath it. Clavain is the car; Intercore is the engine. Both are necessary. Neither subsumes the other.
- **Not self-modifying.** The kernel's behavior is determined by its code and configuration. Interspect can modify OS-level configuration (routing rules, agent prompts, gate policies). It cannot modify the kernel itself. This is a deliberate safety boundary.
