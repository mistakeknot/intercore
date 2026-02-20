# Intercore — Vision Document

**Version:** 1.7
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
├── State: namespace-isolated key-value store (`_kernel/*`, `os/*`, `app/*`)
├── Coordination: locks, sentinels, lane-based scheduling
├── Rollback: phase rewind, dispatch cancellation, discovery revert with full audit trail
├── Portfolio: cross-project runs, dependency graph, composite gate evaluation
├── Sandbox specs: stores requested/effective isolation contracts (enforcement by drivers)
└── Resource management: concurrency limits, token tracking, cost reconciliation, agent caps

Interspect (Profiler) — cross-cutting
├── Reads kernel events (phase results, gate evidence, dispatch outcomes)
├── Correlates with human corrections
├── Proposes changes to OS configuration (routing rules, agent prompts)
└── Never modifies the kernel — only the OS layer

Autarch (Apps — Layer 3) — interactive TUI surfaces (see Autarch vision doc)

Companion Plugins (OS Extensions)
├── interflux: multi-agent review dispatch
├── interject: ambient research and discovery engine
├── interlock: file-level coordination
├── intermux: agent visibility
├── tldr-swinton: token-efficient code context
└── ... each extends the OS with one capability, all route through kernel primitives
```

### Three-Layer Architecture

The ecosystem has three distinct layers, each with clear ownership:

```
Layer 3: Apps (Autarch)
├── Interactive TUI surfaces: Bigend, Gurgeh, Coldwine, Pollard
├── Renders OS opinions into interactive experiences
└── Swappable — Autarch is one set of apps, not the only possible set (see Autarch vision doc for transitional caveats)

Layer 2: OS (Clavain)
├── The opinionated workflow — macro-stages, quality gates, brainstorm→plan→ship
├── Skills orchestrate by calling `ic` (state/gates/events) and companion plugins (capabilities)
├── Clavain is the developer experience: slash commands, session hooks, routing tables
├── Companion plugins (interflux, interlock, interject, etc.) are OS extensions — each wraps one capability
└── If the host platform changes, Clavain's opinions survive; the UX wrappers are rewritten

Layer 1: Kernel (Intercore)
├── Host-agnostic Go CLI + SQLite — works from Claude Code, Codex, bare shell, or any future platform
├── State, gates, events, dispatch, discovery — the durable system of record
├── If the UX layer disappears, the kernel and all its data survive untouched
└── Mechanism, not policy — the kernel doesn't know what "brainstorm" means

Interspect (Profiler) — cross-cutting
├── Reads kernel events, correlates with human corrections
├── Proposes changes to OS configuration (routing, agent selection, gate rules)
└── Never modifies the kernel — only the OS layer
```

**The guiding principle:** the system of record is in the kernel; the policy authority is in the OS; the interactive surfaces are swappable apps. If the host platform changes, you lose UX convenience (slash commands, hooks) but not capability (state, gates, events, evidence, workflow logic). Companion plugins are swappable. The OS is portable. The kernel is permanent.

> **Terminology:** This doc uses kernel vocabulary (work items, runs, dispatches). For mappings to OS vocabulary (beads, sprints) and app vocabulary, see the [shared glossary](glossary.md).

**What this means for plugin design:** Every plugin currently doing state management through temp files, OS-level metadata, or its own SQLite database should instead call `ic`. The big-bang hook cutover (see Migration Strategy below) is the forcing function — when Clavain hooks switch from temp files to `ic`, plugins that share state with those hooks must follow.

### What the Kernel Owns

The kernel provides **mechanism, not policy**. It says "a gate can block a transition" — it doesn't say "brainstorm requires an artifact." That's policy, which lives in the OS layer.

| Subsystem | Primitive | What It Provides |
|---|---|---|
| **Lifecycle** | Runs + Phases | Configurable phase chains with transition validation |
| **Gates** | Conditions at transitions | Pluggable checks evaluated before phase advancement |
| **Dispatch** | Agent processes | Spawn, liveness, collection, timeout, fan-out |
| **Events** | Typed event bus | Append-only log with consumer cursors |
| **Discovery** | Scored discoveries | Confidence-tiered autonomy gates for research intake and backlog generation |
| **State** | Scoped key-value | TTL-based storage with namespace isolation (`_kernel/*`, `os/*`, `app/*`) |
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

### Write-Path Contract

Who can mutate kernel state, and how:

| Caller | Allowed mutations | Not allowed |
|--------|------------------|-------------|
| **Kernel (internal)** | All state transitions, event emission, gate evaluation | n/a |
| **OS (Clavain)** | Run lifecycle (create, advance, pause, cancel), phase chain configuration, gate rule definition, dispatch spawn, discovery policy | n/a |
| **Companion plugins** | Artifact registration, evidence submission, telemetry, capability results within their namespace | Run/phase/gate mutations, policy definitions |
| **Apps (Autarch)** | Read all kernel state, submit intents to OS | Direct kernel mutations — apps call OS operations, not kernel primitives, for anything implying policy |
| **Interspect** | Read events, propose OS config changes | Direct kernel or OS mutations |

**Enforcement model:** callers are cooperative (no ACL tokens in v1). The contract is enforced by convention and code review. The kernel reserves the right to add namespace-scoped write restrictions in future versions.

> This contract is aspirational for v1 — today, companion plugins and apps call `ic` directly. The migration path is: (1) define the contract (this section), (2) route policy-governing writes through OS abstractions, (3) enforce namespace boundaries in the kernel.

**Phased enforcement plan:**
- **v1 (current):** Convention-only. The contract is documented. Code review catches violations. All callers can call any `ic` command.
- **v1.5:** Namespace validation. `ic state set` rejects writes outside the caller's declared namespace. Companion plugins declare their namespace at init time. OS-level writes require the `os/` prefix.
- **v2:** Write-path auditing. Every `ic` mutation records the caller identity (derived from `--caller` flag or `$IC_CALLER` env var). Interspect flags contract violations as governance events. Enforcement remains cooperative but violations are visible.
- **v3 (stretch):** Capability tokens. Callers authenticate with scoped tokens that limit which subsystems they can mutate. Full enforcement of the write-path contract.

## Process Model

Intercore is a **CLI binary**, not a daemon. Every `ic` invocation opens the SQLite database, performs its operation, and exits. There is no long-running server process.

This has important implications:

**Who calls the kernel?** The OS layer (Clavain's bash hooks) shells out to `ic` at workflow boundaries — session start, phase transitions, dispatch spawn. Claude Code plugins call `ic` from hook scripts. Interspect calls `ic` from analysis scripts. Every caller is a short-lived process.

**No background event loop.** The kernel does not poll, watch, or react on its own. Event consumption is **pull-based**: consumers call `ic events tail --consumer=<name>` to retrieve events since their last cursor position. The kernel writes events; consumers decide when to read them.

**Consumer patterns:** OS-level event reactors, TUI event tails, and one-off analysis scripts all use the same cursor-based API (`ic events tail --consumer=<name>`). The kernel is stateless between calls — consumers decide when and how to poll. For the OS event reactor lifecycle (who starts it, crash behavior, gate failure handling), see the [Clavain vision doc](../../../../hub/clavain/docs/clavain-vision.md) Track A3 section.

**Why not a daemon?** Daemons add operational complexity — process management, health monitoring, restart policies, port conflicts. A CLI binary is zero-ops: it works when called, requires no lifecycle management, and has no background process that can crash or become stale between calls. If `ic` crashes *during* a call, SQLite's transaction semantics ensure either the full operation committed or nothing did (see Recovery Semantics below). The SQLite database is the persistent state; the binary is stateless.

**Future consideration:** If event-driven reactions (Level 2 on the autonomy ladder) require sub-second latency, a lightweight daemon or socket-activated service could be introduced. The kernel's API surface (CLI commands) would remain the same — the daemon would be an optimization, not a new architecture. This is explicitly deferred until pull-based polling proves insufficient. The durable event log remains the source of truth; any real-time projection layer would be an app-layer concern (see the [Autarch vision doc](../../../../hub/autarch/docs/autarch-vision.md) Signal Architecture section).

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

This means the OS doesn't need to poll state tables for changes — it tails the event log and reacts. The TUI doesn't need to query every table — it tails the event stream. Interspect doesn't need custom instrumentation — it reads the same events everyone else reads. All event consumption is pull-based (consumers call `ic events tail`); there is no push/subscribe at the kernel level. "Follow mode" (`-f`) is client-side polling of the event log, not a server-side push.

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

Events trigger automatic reactions. When a run advances to `review`, the kernel emits an event. The OS tails the event log and spawns review agents. When all agents complete, the OS advances the phase. The human observes and intervenes only on exceptions.

*This is the event bus milestone — the system does the next obvious thing.*

### Level 3: Adapt

Interspect reads kernel events and correlates them with outcomes. Agents that consistently produce false positives get downweighted. Phases that never produce useful artifacts get skipped by default. Gate rules tighten or relax based on evidence.

The kernel supports this by recording structured evidence with enough dimensionality for meaningful analysis. Gate evaluations include not just pass/fail but the specific conditions checked and the artifacts examined. Dispatch outcomes include verdict quality, token cost, and wall-clock time. Over many runs, this evidence enables weighted confidence scoring across multiple dimensions — completeness, consistency, cost-effectiveness — following the pattern of Autarch's `ConfidenceScore` (see [Autarch vision doc](../../../../hub/autarch/docs/autarch-vision.md) for the scoring model) to produce an actionable composite score rather than a binary judgment.

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

**Gate tiers.** Each gate rule has a tier that controls enforcement behavior:

| Tier | On pass | On fail | Exit code (`ic gate check`) |
|------|---------|---------|---------------------------|
| **hard** | Advance allowed | Advance blocked — run cannot proceed | 1 |
| **soft** | Advance allowed | Advance allowed with warning — gate failure recorded as evidence but does not block | 0 (with warning on stderr) |
| **none** | Evaluation skipped | Evaluation skipped | 0 |

Soft gates are for advisory checks that the OS wants to track (e.g., "code coverage above 80%") without blocking advancement. The evidence is still recorded, enabling Interspect to analyze patterns (e.g., "runs that skip soft gates have 2x the defect rate").

**Gate override.** A hard gate can be overridden via `ic gate override <run_id> --reason=<text>`. The override:
- Records the override as a `gate.override` event with the caller's identity and reason text
- Advances the run past the failed gate as if it had passed
- Preserves the original gate failure evidence alongside the override evidence

Override authority is cooperative in v1 (any caller with DB access can override). The OS may restrict overrides to human-initiated commands by convention. Override frequency is tracked — Interspect flags runs with high override rates as process health signals.

**Artifact identity:** An artifact is a kernel-tracked record of a file produced during a run. Each artifact has: a filesystem path, a content hash (SHA256), the dispatch that produced it, a type label (plan, brainstorm, review, diff, test-report), and a timestamp. The `artifact_exists` gate check verifies that at least one artifact record exists for the specified phase — it checks the database, not the filesystem directly. Artifact content lives on disk; the kernel tracks metadata. This follows Autarch's `RunArtifact` model (type, label, path, mime type, run ID) adapted for kernel use.

**Artifact lifecycle contract:**
- **Registration.** Artifacts are registered via `ic run artifact add <run> --phase=<p> --path=<f> --type=<t>`. The kernel computes and stores the SHA256 hash at registration time. The path must exist and be readable; the kernel rejects registration of nonexistent files.
- **Validity at gate time.** The `artifact_exists` gate checks the database record, not the filesystem. If the file is deleted after registration, the gate still passes — the kernel recorded that the artifact was produced. A separate `artifact_integrity` gate type (future) could verify the hash at gate time for higher assurance.
- **No garbage collection.** The kernel does not delete artifact files. Artifact records are pruned with their parent run during `ic run prune`. The OS is responsible for filesystem cleanup of orphaned artifact files.

Every gate evaluation — pass or fail — produces structured evidence recorded in the event log. This evidence is the foundation for Interspect's analysis.

### Dispatch

The dispatch subsystem manages agent process lifecycle:

```
spawn → running → completed | failed | timeout | cancelled
```

Each dispatch carries a **dispatch config** — a structured record of how the agent should be executed: preferred agent backend (Claude CLI, Codex CLI), timeout, sandbox mode, model override, working directory, and extra CLI flags. This config is captured at spawn time and stored with the dispatch record, giving provenance for every agent invocation. The pattern follows Autarch's `DispatchConfig` model, which separates billing path (subscription-cli vs api), execution constraints (sandbox, timeout), and runtime parameters (model, working directory) into a single declarative struct.

**Secret handling.** Environment variables are not stored in the dispatch config. Secrets (API keys, tokens) must not enter the kernel database — the DB is a readable file and dispatch records are queryable by any caller. The dispatch runner inherits environment variables from the OS process that calls `ic dispatch spawn`; the kernel records only the variable *names* (not values) for provenance tracking.

Key capabilities:
- **Fan-out tracking** — parent-child dispatch relationships for parallel agent patterns.
- **Liveness detection** — the kernel's `ic dispatch poll` checks process liveness via `kill(pid, 0)`. This is the primary signal. Two supplementary signals handle edge cases: (a) a state file at a well-known path (`<workdir>/.ic-dispatch-<id>.alive`) that the dispatch runner touches periodically — covers processes reparented to init where PID checks may be ambiguous; (b) the dispatch's stdout/stderr file modification time as a staleness heuristic. The kernel uses convergent evaluation: a dispatch is considered alive if any signal indicates liveness, and dead only when all signals agree.
- **Verdict collection** — structured results (verdict status, summary, artifact paths) collected from completed dispatches. Follows Autarch's `Outcome` pattern: success/failure, human-readable summary, and a list of produced artifacts.
- **Spawn limits** (v1.5, planned) — maximum concurrent dispatches per run, per project, globally. Prevents runaway agent proliferation (informed by OpenClaw's `maxSpawnDepth` and `maxChildrenPerAgent` patterns).
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

**Consumer cursors:** Each consumer tracks its high-water mark (a monotonic event ID) of the last processed event. On restart, it replays from the cursor position. Cursors are stored in the state table.

**Cursor advancement contract.** `ic events tail` is a read-only operation — it does not advance the cursor. The consumer must explicitly advance its cursor after processing events by calling `ic events cursor set <consumer> <event_id>`. This two-step pattern (read then advance) lets consumers implement at-least-once delivery: if the consumer crashes after reading but before advancing, it replays the same events on restart. Two consumers with the same name (e.g., two reactor instances) would share a cursor and race — this is prevented by the single-instance constraint on the reactor (see Clavain vision doc, Event Reactor Operational Contracts).

Two consumer classes exist:

- **Durable consumers** (e.g., Interspect, Clavain's event reactor) register with `ic events cursor register --durable`. Their cursors never expire. The kernel guarantees no event loss for durable consumers as long as events haven't been pruned by the retention policy.
- **Ephemeral consumers** (e.g., TUI sessions, one-off tails) have a TTL on their cursor (default: 7 days). After TTL expiry, the cursor is garbage-collected. On next poll, an expired ephemeral consumer receives events from the oldest retained event, not from the beginning of time.

**Event retention:** Events are pruned on demand via `ic events prune --older-than=<duration>` (recommended default: 30 days). The kernel does not auto-prune — the OS is responsible for scheduling prune operations (e.g., via cron or a session hook). The kernel guarantees that no event is pruned while any durable consumer's cursor still points before it. This means a durable consumer that falls behind can block event pruning — the OS should monitor consumer lag and alert on stale durable consumers.

**Event ID stability.** Consumer cursors reference event IDs, not SQLite `rowid`. The events table uses an explicit `INTEGER PRIMARY KEY` (which aliases `rowid`) that is never reused. `VACUUM` can reassign `rowid` values on tables without an explicit integer primary key, but the events table's explicit primary key is stable across `VACUUM`. Consumers can safely persist and compare event IDs across vacuum operations.

**Delivery guarantee:** At-least-once from the consumer's perspective. The consumer is responsible for idempotent processing. The kernel does not track acknowledgments — cursor advancement is the consumer's responsibility (call `ic events cursor set`).

**Go API pattern:** For programmatic consumers (Interspect, TUI, future daemon), the kernel exposes a `Replay(sinceID, filter, handler)` function that iterates events matching a filter since a given cursor position, calling the handler for each. Filters are fluent builders — `NewEventFilter().WithEventTypes("phase.advanced", "dispatch.completed").WithSince(t).WithLimit(100)` — enabling consumers to express complex queries without string manipulation. This follows the pattern proven in Autarch's event spine, where the same `EventFilter` serves both CLI queries and programmatic replay.

### State

A scoped key-value store with TTL and namespace isolation. Supports three namespaces:

- **`_kernel/*`** — reserved for kernel-internal coordination (consumer cursors, lease tokens, transient signals). Callers outside the kernel should not read or write these keys.
- **`os/*`** — OS-layer coordination data (session accumulators, handoff state, plugin counters). Written by Clavain and companion plugins.
- **`app/*`** — App-layer ephemeral data (UI preferences, view state). Written by Autarch tools.

All namespaces share the same TTL and scoping mechanics. This is not a general-purpose config store — it is for coordination data with bounded lifetimes.

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

| Category | Enforced | Recorded | Horizon |
|---|---|---|---|
| Gate conditions | Hard gates block advancement | All evaluations (pass/fail/override) with evidence | v1 (current) |
| Spawn limits | Max concurrent dispatches, max depth | All spawn attempts (including rejected) | v1.5 (planned) |
| Phase transitions | Optimistic concurrency, gate checks | Every transition with timestamp, actor, evidence | v1 (current) |
| Coordination locks | Mutual exclusion via filesystem | Lock acquire/release/break events | v1 (current) |
| Token usage | — | Reported counts per dispatch (self-reported by agents) | v1 (current) |
| Sandbox contracts | — | Requested vs effective policy per dispatch | v3 (planned) |
| Discovery autonomy | Confidence tier gates (auto/propose/log/discard) | All discoveries with scores, sources, feedback signals | v3 (planned) |
| Backlog changes | Dedup threshold (blocks duplicate item creation) | All refinements with evidence (merge, priority shift, decay) | v3 (planned) |
| Rollback | Phase reset validation, dispatch cancellation | All rollbacks with initiator, scope, reason, and affected records | v2 (planned) |
| Cross-project deps | Portfolio gate rollup (all children must pass) | Dependency change events, upstream verification triggers | v4 (planned) |
| Cost/billing | Budget threshold events | Self-reported tokens, reconciliation discrepancies | v2 (planned) |

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
2. **The dispatch driver enforces the spec** — Claude Code, Codex, or a container runtime reads the spec and applies it using their native isolation mechanisms. The kernel cannot guarantee enforcement — a misconfigured or compromised driver may not honor the spec. The kernel can **refuse to dispatch** without a registered driver, but cannot verify enforcement at runtime.

**Driver registration.** A driver is registered by adding an entry to the OS-managed driver config (`drivers.json`): driver name, binary path, supported sandbox capabilities (tool allowlists, filesystem isolation, container support), and a compliance self-declaration. The kernel reads this config at dispatch time to validate that the requested driver exists and declares support for the requested sandbox capabilities. Registration is cooperative — the driver declares what it supports; the kernel trusts the declaration. Interspect audits actual compliance by comparing requested vs effective sandbox policies in dispatch completion events.
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

The Interverse workspace contains 25+ modules, each with its own git repository. A change in intercore can break Clavain hooks. A plugin update may need testing across multiple modules. Today, there is no cross-project visibility — each project has its own `ic` database, and that per-project isolation is the design default.

Multi-project coordination extends the kernel with three complementary mechanisms:

### Cross-Project Event Bus

Kernel events from one project are visible to consumers in other projects. When intercore ships a new feature, the event is readable by Clavain's event consumer. When a plugin publishes a new version, downstream projects see the event.

**Mechanism:** A relay process (OS-layer, not kernel) tails events from multiple project databases and writes them to a shared relay database. Consumers tail the relay with the same cursor-based API. The kernel provides the event format and cursor primitives; the OS provides the relay topology.

**Why relay, not shared database:** Each project's SQLite database is its transactional boundary. A shared database would serialize writes across all projects, creating contention. A relay provides eventual consistency (sub-second latency) while preserving per-project write independence.

### Portfolio-Level Runs

A run can span multiple projects. `ic run create --projects=intercore,clavain --goal="Migrate hooks"` creates a portfolio run with per-project scoping for artifacts, gates, and dispatches, but a unified run ID for tracking and reporting.

**Kernel mechanism:** The portfolio parent record lives in a designated portfolio database (separate from any project's database), while child runs live in their respective project databases. Each child run's `portfolio_id` links back to the parent. The relay process (see Cross-Project Event Bus) aggregates child run events into the portfolio database. Phase advancement in the portfolio requires gate passage in all child runs — the kernel evaluates composite gates by querying child run state via the relay.

**Discovery model.** The OS enumerates project databases via a registry file (`~/.config/ic/projects.json`) or filesystem convention (`<workspace>/*/.ic/`). Bigend's multi-project dashboard uses this same registry to discover and aggregate across databases. There is no global shared database — cross-project queries always go through the relay or by iterating the registry.

**OS policy:** Which projects participate in a portfolio run, how gates compose across projects (all-pass vs majority-pass), and how dispatches are allocated across projects are OS-level decisions configured at portfolio creation time.

### Dependency Graph Awareness

The kernel knows that Clavain depends on intercore. When intercore ships a change (a `run.completed` event fires), the kernel emits `dependency.upstream_changed` events for each dependent project. The OS decides how to react — auto-create a test run, create a work item, send a notification, or ignore.

**Mechanism:** A `project_deps` table maps project → dependency relationships. When a `run.completed` event fires for a project, the kernel checks for dependents and emits `dependency.upstream_changed` events for each. The OS decides what to do — auto-create a test run, create a work item, send a notification, or ignore.

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

When a run goes wrong — bad code, skipped gates, or erroneous discovery-created work items — there is no structured way to revert. Git handles code rollback. Nothing handles workflow state rollback or backlog rollback. This is a gap.

### Three Rollback Layers

**Code rollback.** Git revert handles code. The kernel records which commits were produced by which dispatches (artifact metadata includes git SHA). The kernel provides a query to list all commits associated with a run's dispatches. The OS or human generates and executes the `git revert` sequence — the kernel stores provenance, not VCS-specific operational plans.

**Workflow state rollback.** When a run advances too fast or skips a gate incorrectly, the run's phase needs to reset. `ic run rollback <id> --to-phase=plan-review` resets the run's current phase, marks intervening phase transitions as `rolled_back` (not deleted — audit trail is preserved), and re-evaluates gates for the target phase. Dispatches spawned in rolled-back phases are cancelled if still running.

**Backlog rollback.** When the discovery pipeline auto-creates work items from a bad signal (noisy source, miscalibrated profile), the backlog needs cleanup. `ic discovery rollback --source=<source> --since=<timestamp>` identifies all work items created from discoveries by that source since the given time. It proposes closing them (with reason `rolled_back:discovery`) — the human confirms. Priority shifts and dependency suggestions triggered by rolled-back discoveries are also reverted.

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

### OS Workflow Migration: Hybrid to Kernel-Driven

The OS currently orchestrates its workflow through slash commands, with its own phase state in external metadata and checkpoint JSON. Migration to kernel-driven workflows is staged:

**Phase 1 (Hybrid):** The sprint skill calls `ic run create` at sprint start and `ic run advance` alongside its existing logic. Both systems track phase state. The skill still drives the workflow. This phase provides kernel tracking and event emission without requiring a skill rewrite.

**Phase 2 (Handover):** `ic run advance` becomes the authoritative advancement call. The skill stops maintaining its own phase state. Gate enforcement is real — `ic run advance` returns exit code 1 and the phase doesn't change. The skill becomes a UX wrapper that translates user intent into kernel calls.

**Phase 3 (Kernel-driven):** The sprint skill is a thin experience layer. It prompts the user, invokes slash commands, and displays results. All state, gates, events, and dispatch tracking flow through the kernel. If the sprint skill breaks, the run state is intact and recoverable via `ic` CLI.

**Key milestone:** Phase 2 completion means the gap analysis item #2 ("gate enforcement is just prompting") is fully resolved. Gates become kernel-enforced invariants, not prompt suggestions.

### Interspect Migration: Staged to Kernel Events

Interspect currently operates with its own SQLite database and hook-based evidence collection. Migration to kernel events preserves the mechanism/policy separation — Interspect reads kernel data but doesn't own it.

**Phase 1:** Interspect becomes a consumer of existing kernel events (phase transitions, gate evaluations, dispatch outcomes) via `ic events tail --consumer=interspect --durable`. Its own DB supplements with Interspect-specific data (agent quality scores, false-positive patterns, routing proposals).

**Phase 2:** Add correction and override event types to the kernel. When a human overrides an agent finding or marks a false positive, the kernel records it as a typed event. Interspect reads these events instead of collecting them via its own hooks.

**Phase 3:** Retire Interspect's own SQLite database. Interspect's state becomes a materialized view derived entirely from kernel events. Single source of truth.

## Apps Layer (Autarch)

Autarch provides the interactive TUI surfaces for kernel state — Bigend (monitoring), Gurgeh (PRD generation), Coldwine (task orchestration), and Pollard (research intelligence). Each tool is migrating from its own backend to the kernel as the shared state layer. For full details on the four tools, `pkg/tui`, and the migration plan, see the [Autarch vision doc](../../../../hub/autarch/docs/autarch-vision.md).

## Autonomous Research and Backlog Intelligence

The kernel's first three levels — Record, Enforce, React — focus on work that's already been defined. A human creates a run, the kernel tracks it. But where does the work come from? Autonomous research and backlog intelligence close the loop between **what the world knows** and **what the system is working on**.

### What the Kernel Provides (Mechanism)

The kernel provides primitives for tracking discoveries, scoring confidence, and emitting events. The OS decides what sources to scan, what confidence thresholds to use, and how aggressively to act on discoveries.

| Primitive | What It Does |
|---|---|
| **Discovery records** | Durable storage for scored discoveries with embedding vectors, source metadata, and lifecycle state |
| **Confidence scoring** | Stores a numeric confidence score (0.0–1.0) with provenance metadata. The kernel accepts scores from any caller — it does not define the scoring algorithm. Scoring algorithms (embedding similarity, weight multipliers, profile vectors) are OS policy |
| **Confidence-tiered action gates** | Kernel-enforced thresholds that control which autonomy tier a discovery reaches — auto-execute, propose-to-human, log-only, or discard |
| **Discovery events** | Typed events (`discovery.scanned`, `discovery.scored`, `discovery.promoted`, `discovery.proposed`, `discovery.dismissed`) that flow through the same event bus as phase and dispatch events |
| **Backlog events** | Typed events (`backlog.refined`, `backlog.merged`, `backlog.submitted`, `backlog.prioritized`) for tracking how the backlog evolves from research signals |
| **Feedback ingestion** | Structured recording of human actions (promote, dismiss, adjust priority) that update the interest profile and source trust weights |

### Confidence-Tiered Autonomy

The kernel enforces a confidence-gated autonomy model. Each discovery is scored and assigned to a tier. The tier determines what the system can do without human approval.

| Tier | Score Range | Kernel Event | Horizon |
|---|---|---|---|
| **High** | ≥ 0.8 | `discovery.promoted` | v3 |
| **Medium** | 0.5 – 0.8 | `discovery.proposed` | v3 |
| **Low** | 0.3 – 0.5 | `discovery.scored` | v3 |
| **Discard** | < 0.3 | Recorded with `discarded` status | v3 |

The kernel enforces tier boundaries as gate invariants — the scoring model produces a number, the tier boundaries are configuration, and the kernel rejects promotions that violate tier constraints. The human can always override (promote a low-scoring discovery manually), and that override is recorded as a feedback signal. For the OS-level actions at each tier (work item creation, briefing docs, inbox notifications), see the [Clavain vision doc](../../../../hub/clavain/docs/clavain-vision.md) Discovery → Backlog Pipeline section.

> **Horizon note:** The discovery subsystem is planned for product horizon v3 (see Success at Each Horizon table). The `discoveries` table, confidence scoring, and tier enforcement do not exist in the current database schema (schema revision 5, tracked via `PRAGMA user_version`). These are different version axes: product horizons (v1–v4) describe feature milestones; schema revisions (1–N) track database migrations. The table above describes the target design.

### Backlog Refinement Primitives

The kernel provides two backlog enforcement mechanisms:

**Dedup threshold enforcement.** When a new discovery arrives, the kernel compares its embedding against existing items using a configurable similarity threshold (provided by the OS at scan time). If similarity exceeds the threshold, the discovery is linked as additional evidence to the existing item rather than creating a new one. The dedup rate is tracked for OS-level analysis. The threshold value is OS policy, not a kernel default.

**Staleness decay mechanism.** Discovery records that are never promoted and receive no activity can be decayed. The kernel provides a `decay` operation that the OS invokes with a rate parameter — decay is computed lazily at query time (virtual priority), not by a background process. The OS decides when to decay, at what rate, and whether fresh evidence reverses it.

Additional backlog refinement (priority escalation, dependency suggestion, weekly digests, feedback loops) is OS-level policy. See the [Clavain vision doc](../../../../hub/clavain/docs/clavain-vision.md) for the full discovery → backlog pipeline workflow, including source configuration, trigger modes, and backlog refinement rules.

### What the OS Provides (Policy)

The kernel provides mechanism; the OS provides the discovery pipeline workflow. Key OS responsibilities (defined in the Clavain vision doc):

- Source configuration and scan scheduling
- Trigger modes (scheduled, event-driven, user-initiated)
- Confidence threshold tuning and adaptive thresholds
- Autonomy policy (what actions at each tier)
- Backlog refinement rules (priority escalation, dependency suggestion, digests)
- Interest profile management and feedback loop

## What Makes This Different

### Landscape

Existing agent orchestration systems address parts of this problem. Understanding where they overlap and diverge with Intercore clarifies what the kernel contributes.

**Single-agent toolkits** (pi-mono, Claude Code, Cursor) manage one agent's tool loop, context window, and session state. pi-mono's EventStream primitive and two-phase context transform are best-in-class for single-agent operation. These systems focus on the agent-level problem and don't attempt multi-agent coordination or workflow lifecycle management.

**Multi-agent frameworks** (LangGraph, CrewAI, AutoGen) orchestrate multiple agents working on a task. LangGraph provides checkpoint-based persistence, durable execution, and human-in-the-loop interrupts with time-travel replay. CrewAI Flows offers persistence with a SQLite backend and state recovery. These are real, production-grade persistence capabilities — not in-memory toys. However, they are general-purpose graph/pipeline engines. Their primitives (nodes, edges, state channels) are domain-agnostic. Building software development lifecycle concepts (artifact-gated advancement, review verdict collection, phase-specific dispatch policies) on top of them requires custom application code.

**Personal AI gateways** (OpenClaw, NullClaw) route messages to agents across channels. OpenClaw has lane-based scheduling with concurrency caps, three-layer security (sandbox + tool policy + elevated), and sub-agent spawn limits with depth tracking. NullClaw achieves similar goals with a minimal Zig implementation and multi-layer sandbox auto-detection (Landlock → Firejail → Bubblewrap → Docker → noop). These systems excel at message routing and agent containment. They don't model task decomposition, workflow phases, or quality enforcement.

**Durable execution engines** (Temporal, Restate) provide crash-proof workflows with state captured at each step, retry policies, and saga patterns. Temporal is the closest conceptual relative to Intercore's durability story. The key differences are deployment model and domain specialization (see "Why not Temporal?" below).

**Autarch** (merged into the Interverse monorepo) is a tool-first approach to the same problem space — four Go TUI tools (PRD generation, task orchestration, research intelligence, mission control) sharing a `pkg/contract` entity model, `pkg/events` event spine, and `pkg/db` SQLite helper. Autarch and Intercore share the same SQLite driver (`modernc.org/sqlite`), the same WAL/NORMAL/MaxOpenConns(1) configuration, and overlapping domain concepts (runs, artifacts, dispatches). Intercore adopts several Autarch patterns directly: the fluent `EventFilter` and `Replay()` API for event consumption, the fingerprint-based reconciliation engine for detecting state drift, the `DispatchConfig` struct for agent spawn parameters, and the `ConfidenceScore` model for weighted evidence quality analysis. Autarch is merging into the Interverse monorepo as the apps/TUI layer, with its tools progressively migrating from their own YAML/SQLite backends to intercore's kernel as the shared state backend (see [Autarch vision doc](../../../../hub/autarch/docs/autarch-vision.md)).

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

## Concrete Scenario: A Run Through the Kernel

To ground the abstract primitives, here is how a typical workflow flows through the kernel. The phase names below are opaque labels — the kernel accepts any phase chain the OS provides.

```
1. OS creates a run with its own phase chain:
   ic run create --project=. --goal="Add user auth" --complexity=3 \
     --phases='["phase-0","phase-1","phase-2","phase-3","phase-4","phase-5","phase-6","phase-7"]'

2. Kernel records: run_id=R42, phase=phase-0, complexity=3

3. User completes phase-0 work. OS calls:
   ic run advance R42

   Kernel evaluates gate for phase-0→phase-1 transition.
   Gate rule: artifact_exists(phase=phase-0).
   Kernel checks artifacts table → artifact exists → gate passes.
   Kernel records: phase transition event, gate evidence (pass).

4. OS dispatches agents at phase-3 (a review phase in the OS's workflow):
   ic dispatch spawn --run=R42 --name=reviewer-1 --prompt-file=review.md
   ic dispatch spawn --run=R42 --name=reviewer-2 --prompt-file=review.md

   Kernel records dispatches, tracks PIDs, enforces spawn limits.

5. Agents complete. OS calls:
   ic run advance R42

   Gate rule: agents_complete(phase=phase-3).
   Kernel checks dispatch table → all dispatches completed → gate passes.

6. At each step, kernel emits events:
   phase.advanced, gate.passed, dispatch.spawned, dispatch.completed, ...

   Interspect consumer tails these events via cursor.
   TUI consumer tails them for display.
   Neither needs to know about the other.
```

The kernel doesn't know what any phase name means. It knows that phase 0 requires an artifact to advance to phase 1, that phases 3-4 have active dispatches that must complete, and that all of this is recorded durably. The OS maps its domain concepts (Clavain's "brainstorm", "plan-review", "ship") onto these opaque kernel phases at run creation time.

## Assumptions and Constraints

**Single-machine, per-project databases.** The kernel assumes a single-machine deployment with one SQLite database per project (stored at `<project>/.ic/intercore.db`). Multiple OS processes can read concurrently (WAL mode); writes are serialized by SQLite's write lock, with filesystem locks providing application-level mutual exclusion for read-modify-write operations. Cross-project visibility is provided by the relay process and project registry (see Multi-Project Coordination), not by shared databases. Multi-machine coordination (distributed locks, replicated event logs) is explicitly out of scope — single-machine operation is the expected deployment model for autonomous development environments.

**Callers are cooperative.** The kernel enforces invariants (gate conditions, spawn limits, optimistic concurrency) but does not defend against malicious callers. An agent could bypass the kernel entirely by writing to the filesystem. The security model assumes trusted but fallible callers — the kernel prevents mistakes, not attacks.

**Security baseline.** While the kernel does not defend against malicious callers, it maintains a security floor:
- **No secrets in the database.** The kernel never stores API keys, tokens, passwords, or credentials. Environment variables are recorded by name only, never by value. Dispatch configs store model names and flags, not authentication material. This is a hard invariant — any code path that would write a secret to the database is a security bug.
- **File permissions.** The database file is created with mode `0600` (owner read/write only). The kernel does not modify permissions after creation. Multi-user access (e.g., `claude-user` via ACLs) is an OS-level concern.
- **Threat model.** The kernel assumes a single trusted operator on a single machine. It defends against accidental corruption (transactions, WAL mode, optimistic concurrency) and operational mistakes (gate enforcement, spawn limits). It does not defend against adversarial agents, prompt injection attacks on dispatched LLMs, or supply chain compromise of companion plugins. These threats are real but are addressed at the OS and driver layers (sandbox specs, tool allowlists, code review gates), not in the kernel.
- **No network surface.** The kernel is a CLI binary. It opens no ports, listens on no sockets, and accepts no remote connections. The attack surface is the filesystem (database file, lock directories) and the caller's process environment.

**Single-machine invariant.** The kernel assumes single-machine deployment through v2. This is a hard constraint, not a convenience default:
- One SQLite database per project, on local disk. No networked filesystems, no shared storage.
- Filesystem locks (`mkdir`) assume local POSIX semantics. NFS or distributed filesystems may not honor `mkdir` atomicity.
- PID-based liveness detection (`kill(pid, 0)`) assumes all processes share a PID namespace. Containers with separate PID namespaces require the state-file liveness signal as primary.
- The relay process for cross-project events runs on the same machine, reading multiple local databases.
- Multi-machine coordination (distributed locks, replicated event logs, consensus) is explicitly deferred to v3+ and will require a different coordination substrate (not SQLite).

**Token tracking is self-reported.** The kernel records token counts that dispatched agents report. It cannot independently verify these counts. An agent that misreports its token usage undermines budgeting. Tier 1 mitigation: the OS can cross-reference reported counts with API billing data. Tier 2: the runner could inject token tracking at the API call layer.

**Event and data retention.** The event log grows unboundedly without pruning. The kernel provides `ic events prune --older-than=30d` for event retention and `ic run prune --older-than=90d` for completed run cleanup. The OS is responsible for scheduling these — the kernel does not auto-prune. SQLite `VACUUM` should be run periodically by the OS after significant pruning to reclaim disk space. Pre-pruning backup is recommended (the kernel's existing auto-backup mechanism covers migration scenarios, not routine maintenance).

**Clock monotonicity.** TTL computations, sentinel timing, and event ordering assume monotonic system time. NTP jumps or clock skew on the host could cause incorrect TTL expirations or event ordering anomalies. The kernel uses Go's `time.Now().Unix()` (not SQLite's `unixepoch()`) to avoid float promotion, but doesn't guard against backward clock jumps.

**Open-source product.** Intercore is designed for community adoption, not just personal use. This creates obligations that personal tools don't have:
- **API stability.** CLI flags, event schemas, and database schemas need backward compatibility discipline from v1 onward. Breaking changes require migration paths and deprecation periods.
- **Documentation for strangers.** The vision doc and AGENTS.md are written for the maintainer. An open-source product needs installation guides, quickstart tutorials, concept explanations, and configuration references for people who don't know what Clavain is or why temp files are a problem.
- **Sensible defaults.** The kernel should work out of the box with zero configuration. Phase chains, gate rules, and throttle intervals should have defaults that cover common cases. Power users customize; new users get something useful immediately.
- **Error messages for humans.** Every `ic` error should explain what went wrong and suggest a fix. "ErrStalePhase" is an internal name; the CLI should say "Another process already advanced this run. Re-read the current phase with `ic run phase <id>` and decide if your advance still applies."

## The v1 Kernel Wedge

The kernel's minimum viable scope — what must exist before Clavain hooks can migrate from temp files:

**Ships in v1:**
- `ic run create/advance/phase/status/list/cancel` — full run lifecycle with configurable phase chains
- `ic gate check/override/rules` — gate evaluation with hard/soft tiers and evidence recording
- `ic dispatch spawn/status/poll/wait/list/kill/prune/tokens` — agent process lifecycle with liveness detection
- `ic events tail/cursor` — append-only event log with durable and ephemeral consumer cursors
- `ic state set/get/delete` — scoped key-value store with TTL and namespace isolation
- `ic lock acquire/release/list/stale/clean` — filesystem-based mutual exclusion
- `ic sentinel check` — time-based throttle guards
- `ic run artifact add/list` — artifact registration with SHA256 hashing
- `ic run agent add/list/update` — agent tracking within runs
- `ic init` — database initialization with auto-migration

**Does not ship in v1:**
- Discovery subsystem (v3)
- Rollback primitives (v2)
- Multi-project portfolio runs (v4)
- Lane-based scheduling (v2)
- Sandbox spec enforcement (v3)
- Cost reconciliation (v2)
- Capability tokens for write-path enforcement (v3)

**The wedge test:** Can Clavain's `lib-sprint.sh` create a run, advance through phases with gate enforcement, dispatch agents, track their completion, emit events, and record artifacts — all via `ic` instead of temp files? If yes, v1 is sufficient for the hook cutover.

## Compatibility Contract

The kernel is designed for open-source adoption. Breaking changes have real costs for external consumers. Starting with v1, the following stability guarantees apply:

**CLI surface (stable from v1):**
- Command names, required flags, and exit codes are backward-compatible across minor versions.
- New commands and optional flags may be added in minor versions.
- Existing commands are deprecated (with warnings) for one minor version before removal.
- Exit codes: 0 = success, 1 = gate failure / expected rejection, 2 = usage error, 3+ = internal error.

**Event schema (stable from v1):**
- Event type strings (e.g., `phase.advanced`, `dispatch.completed`) are stable across minor versions.
- New event types may be added. Existing event types are never renamed or removed without a major version bump.
- Event payload fields may gain new optional fields. Existing fields are never removed or type-changed without a major version bump.

**Database schema (migration-safe):**
- Schema changes use `PRAGMA user_version` for versioning and `ic init` for auto-migration.
- Migrations are forward-only (no downgrades). `ic init` creates a timestamped backup before any migration.
- New columns use `ALTER TABLE ADD COLUMN` with defaults (never `NOT NULL` without a default on existing tables).

**What is NOT stable:**
- Internal Go API (no library API in v1 — the CLI is the contract).
- Database file layout and internal table structures (callers should use `ic` commands, not raw SQL).
- Debug output format (stderr messages may change without notice).

## Success at Each Horizon

| Horizon | Timeframe | What Success Looks Like |
|---|---|---|
| v1 | Current | Gates enforce real conditions. Events flow. Dispatches are tracked. The kernel is the system of record. |
| v1.5 | 1-2 months | Big-bang hook cutover — all Clavain hooks call `ic` instead of temp files. Sprint skill enters hybrid mode (calls `ic run` alongside existing logic). Fully custom phase chains (OS provides preset templates like sprint). API stability contract established for open-source readiness. Autarch merged into Interverse monorepo. |
| v2 | 2-4 months | Sprint skill hands over phase control to kernel (hybrid→kernel-driven). Lane-based scheduling. Token tracking per dispatch with OS-level billing verification. Discovery events flow through the kernel event bus. Scheduled scanning runs autonomously. Interspect Phase 1 (kernel event consumer). Rollback primitives for workflow state. Bigend migrated to kernel backend (read-only dashboard over `ic` state). Autarch status tool provides minimal TUI over kernel state (see [Autarch vision doc](../../../../hub/autarch/docs/autarch-vision.md)). |
| v3 | 4-8 months | Interspect Phase 2-3 (correction events, retire own DB). Sandboxing Tier 1 (tool allowlists, multi-agent isolation). Confidence-tiered autonomy gates enforce discovery → backlog policy. Backlog refinement runs as an event consumer. Code and backlog rollback. Cross-project event relay. Pollard migrated to kernel discovery pipeline. Gurgeh PRD generation backed by kernel runs. Installation guide and quickstart for community adopters. |
| v4 | 8-14 months | Portfolio-level runs across multiple projects. Dependency graph awareness with auto-verification. Resource scheduling across competing priorities. Sandboxing Tier 2 (containers). Discovery pipeline feeds portfolio prioritization. Coldwine task orchestration backed by kernel dispatches. Full Autarch TUI suite reads kernel state as single source of truth. The kernel orchestrates a fleet and knows what it should be working on next. |

## What This Is Not

- **Not an LLM framework.** Intercore doesn't call LLMs, manage context windows, or process natural language. That's what the dispatched agents do.
- **Not a Claude Code plugin.** Intercore is a Go CLI binary with a SQLite database. It's infrastructure that plugins call, not a plugin itself.
- **Not a workflow DSL.** The kernel provides primitives (phases, gates, events). It doesn't provide a language for defining workflows. The OS maps its domain concepts to kernel primitives.
- **Not a replacement for Clavain.** Clavain is the developer experience layer — the opinionated workflow, the sprint sequence, the quality gates philosophy. Intercore is the engine beneath it. Clavain is the car; Intercore is the engine. Both are necessary. Neither subsumes the other.
- **Not self-modifying.** The kernel's behavior is determined by its code and configuration. Interspect can modify OS-level configuration (routing rules, agent prompts, gate policies). It cannot modify the kernel itself. This is a deliberate safety boundary.
