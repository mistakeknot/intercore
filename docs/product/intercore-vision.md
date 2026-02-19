# Intercore — Vision Document

**Version:** 1.3
**Date:** 2026-02-18
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
├── State: scoped key-value store for kernel coordination
├── Coordination: locks, sentinels, lane-based scheduling
├── Sandbox specs: stores requested/effective isolation contracts (enforcement by drivers)
└── Resource management: concurrency limits, token tracking, agent caps

Interspect (Profiler)
├── Reads kernel events (phase results, gate evidence, dispatch outcomes)
├── Correlates with human corrections
├── Proposes changes to OS configuration (routing rules, agent prompts)
└── Never modifies the kernel — only the OS layer

Companion Plugins (Drivers)
├── interflux: multi-agent review dispatch
├── interlock: file-level coordination
├── intermux: agent visibility
├── tldr-swinton: token-efficient code context
└── ... each extends the OS, all route through kernel primitives
```

### What the Kernel Owns

The kernel provides **mechanism, not policy**. It says "a gate can block a transition" — it doesn't say "brainstorm requires an artifact." That's policy, which lives in the OS layer.

| Subsystem | Primitive | What It Provides |
|---|---|---|
| **Lifecycle** | Runs + Phases | Configurable phase chains with transition validation |
| **Gates** | Conditions at transitions | Pluggable checks evaluated before phase advancement |
| **Dispatch** | Agent processes | Spawn, liveness, collection, timeout, fan-out |
| **Events** | Typed event bus | Append-only log with consumer cursors |
| **State** | Scoped key-value | TTL-based storage for coordination data |
| **Coordination** | Locks + Sentinels | Mutual exclusion and time-based throttling |
| **Run Tracking** | Agents + Artifacts | What agents are active, what files were produced |

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

An urgent hotfix preempts a routine refactor. A high-complexity feature gets more review agents than a documentation update. Token budgets prevent runaway costs.

*This is fleet management — the system balances competing demands.*

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

## What Makes This Different

### Landscape

Existing agent orchestration systems address parts of this problem. Understanding where they overlap and diverge with Intercore clarifies what the kernel contributes.

**Single-agent toolkits** (pi-mono, Claude Code, Cursor) manage one agent's tool loop, context window, and session state. pi-mono's EventStream primitive and two-phase context transform are best-in-class for single-agent operation. These systems focus on the agent-level problem and don't attempt multi-agent coordination or workflow lifecycle management.

**Multi-agent frameworks** (LangGraph, CrewAI, AutoGen) orchestrate multiple agents working on a task. LangGraph provides checkpoint-based persistence, durable execution, and human-in-the-loop interrupts with time-travel replay. CrewAI Flows offers persistence with a SQLite backend and state recovery. These are real, production-grade persistence capabilities — not in-memory toys. However, they are general-purpose graph/pipeline engines. Their primitives (nodes, edges, state channels) are domain-agnostic. Building software development lifecycle concepts (artifact-gated advancement, review verdict collection, phase-specific dispatch policies) on top of them requires custom application code.

**Personal AI gateways** (OpenClaw, NullClaw) route messages to agents across channels. OpenClaw has lane-based scheduling with concurrency caps, three-layer security (sandbox + tool policy + elevated), and sub-agent spawn limits with depth tracking. NullClaw achieves similar goals with a minimal Zig implementation and multi-layer sandbox auto-detection (Landlock → Firejail → Bubblewrap → Docker → noop). These systems excel at message routing and agent containment. They don't model task decomposition, workflow phases, or quality enforcement.

**Durable execution engines** (Temporal, Restate) provide crash-proof workflows with state captured at each step, retry policies, and saga patterns. Temporal is the closest conceptual relative to Intercore's durability story. The key differences are deployment model and domain specialization (see "Why not Temporal?" below).

**Autarch** (sibling project) is a tool-first approach to the same problem space — four Go TUI tools (PRD generation, task orchestration, research intelligence, mission control) sharing a `pkg/contract` entity model, `pkg/events` event spine, and `pkg/db` SQLite helper. Autarch and Intercore share the same SQLite driver (`modernc.org/sqlite`), the same WAL/NORMAL/MaxOpenConns(1) configuration, and overlapping domain concepts (runs, artifacts, dispatches). Intercore adopts several Autarch patterns directly: the fluent `EventFilter` and `Replay()` API for event consumption, the fingerprint-based reconciliation engine for detecting state drift, the `DispatchConfig` struct for agent spawn parameters, and the `ConfidenceScore` model for weighted evidence quality analysis. Where Autarch is tool-first (TUI apps that happen to need shared state), Intercore is infrastructure-first (a kernel that tools call). They complement rather than compete — Autarch tools could become Intercore OS-layer consumers, using the kernel as their shared state backend instead of the current file-first YAML approach.

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

## Success at Each Horizon

| Horizon | Timeframe | What Success Looks Like |
|---|---|---|
| v1 | Current | Gates enforce real conditions. Events flow. Dispatches are tracked. The kernel is the system of record. |
| v2 | 1-3 months | Configurable phase chains. Lane-based scheduling. Token tracking per dispatch. The OS configures the kernel, not the other way around. |
| v3 | 3-6 months | Interspect reads kernel events and proposes improvements. Sandboxing Tier 1 (tool allowlists). TUI control room reads kernel state. |
| v4 | 6-12 months | Multi-run portfolio management. Resource scheduling across competing priorities. Sandboxing Tier 2 (containers). The kernel orchestrates a fleet. |

## What This Is Not

- **Not an LLM framework.** Intercore doesn't call LLMs, manage context windows, or process natural language. That's what the dispatched agents do.
- **Not a Claude Code plugin.** Intercore is a Go CLI binary with a SQLite database. It's infrastructure that plugins call, not a plugin itself.
- **Not a workflow DSL.** The kernel provides primitives (phases, gates, events). It doesn't provide a language for defining workflows. The OS maps its domain concepts to kernel primitives.
- **Not a replacement for Clavain.** Clavain is the developer experience. Intercore is the infrastructure beneath it. Both are necessary. Neither subsumes the other.
- **Not self-modifying.** The kernel's behavior is determined by its code and configuration. Interspect can modify OS-level configuration (routing rules, agent prompts, gate policies). It cannot modify the kernel itself. This is a deliberate safety boundary.
