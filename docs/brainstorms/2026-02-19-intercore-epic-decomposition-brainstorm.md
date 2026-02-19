# Intercore Vision — Epic Decomposition Brainstorm
**Bead:** iv-c6az
**Phase:** brainstorm (as of 2026-02-19T15:39:14Z)

**Date:** 2026-02-19
**Goal:** Decompose the intercore vision doc (v1.6) into prioritized epics with dependencies, establishing the execution roadmap from current state (~40% built) to v4.

---

## What We're Building

A prioritized execution roadmap that turns the vision doc's 893 lines into actionable epics, sequenced by the autonomy ladder (Level 0-4) with cross-cutting kernel primitives as separate epics.

## Current State

**Built (Level 0 + Level 1):**
- State subsystem (scoped KV with TTL)
- Sentinel subsystem (time-based throttle guards)
- Dispatch subsystem (spawn, liveness, verdict collection)
- Lock subsystem (filesystem-based mutex)
- Phase state machine (8-phase chain, optimistic concurrency)
- Gate subsystem (artifact_exists, agents_complete, verdict_exists; hard/soft tiers)
- Event bus (typed events, consumer cursors, durable/ephemeral consumers)
- Run tracking (agents + artifacts within runs)
- Schema at v5, ~130 unit tests + ~93 integration tests

**Partially built:**
- SpawnHandler (implemented but not wired — dispatch.Store.SpawnByAgentID missing)
- Hook adapter migration (5 research docs done, bash changes not executed)

**Not built:**
- Configurable phase chains (currently hardcoded 8-phase)
- Token tracking per dispatch
- SkipPhase as explicit primitive
- Discovery subsystem
- Rollback primitive
- Portfolio / cross-project runs
- Artifact content hashing
- Hook cutover (big-bang)
- Autarch migration

## Why This Approach

**Autonomy-level epics** because:
1. Each level builds on the one below — natural dependency chain
2. Level completion = meaningful capability milestone (not just "feature X done")
3. Cross-cutting primitives (phase chains, tokens, rollback) are explicitly tracked as enablers
4. Maps 1:1 to the vision doc's own organizing structure

**Primitives before cutover** because:
1. Hooks get the full `ic` API from day one — no rework
2. The cutover is a forcing function, not a discovery exercise
3. Risk of dual-path consistency issues is eliminated

## Key Decisions

1. **Decomposition approach:** By autonomy level, not by subsystem or horizon
2. **Hook cutover timing:** After kernel primitives are ready, then big-bang
3. **Goal:** Execution roadmap with clear dependencies, not completeness inventory
4. **Epic granularity:** ~8-10 epics, each sprintable in 1-3 sessions

## Proposed Epic Structure

### Epic 1: Kernel Primitives Completion (P1, unblocks everything)
Complete the missing kernel-level primitives that v1.5+ requires:
- Configurable phase chains at `ic run create` time (currently hardcoded)
- `SkipPhase(run_id, phase_id, reason, actor)` as explicit CLI command
- Artifact content hashing (SHA256) + dispatch_id column
- Token tracking columns on dispatches (tokens_in, tokens_out, cache_hits)
- Token aggregation queries (per-run, per-project)
- Budget threshold events (budget.warning, budget.exceeded)

**Why first:** Every subsequent epic assumes these primitives exist. Configurable phase chains are needed for custom workflows. Token tracking is needed for cost awareness. SkipPhase is needed for complexity-based routing.

### Epic 2: Level 2 — React (P1, unblocks event-driven automation)
Wire the event reactor so the system does "the next obvious thing":
- Wire SpawnHandler end-to-end (implement dispatch.Store.SpawnByAgentID, register in Notifier)
- OS-level event reactor: `ic events tail -f --consumer=clavain-reactor` pattern
- Dispatch completion → phase advancement automation
- Gate failure → notification event

**Why second:** Level 2 is what makes the kernel useful beyond a logbook. Without reactions, every state transition requires manual intervention.

### Epic 3: Hook Cutover — Big Bang (P1, the integration forcing function)
Migrate all ~20 Clavain hooks from temp files to `ic`:
- Sentinel-based hooks (compound throttle, drift throttle, autopub lock, catalog reminder)
- Dispatch state hooks (dispatch progress, inflight agents)
- Phase/state hooks (bead phase sideband, context pressure accumulator)
- Session handoff hooks (handoff state, session-tmux mapping)
- Update lib-intercore.sh wrappers as needed
- One release, no fallback paths

**Why third:** Requires Epics 1+2 to be complete so hooks have real capabilities to switch to. This is where the vision becomes operationally real — the ~15 temp files in `/tmp/` disappear.

### Epic 4: Level 3 — Adapt (P2, Interspect integration)
Connect Interspect to kernel events for evidence-based self-improvement:
- Phase 1: Interspect becomes kernel event consumer (ic events tail --consumer=interspect --durable)
- Phase 2: Correction and override event types added to kernel
- Phase 3: Retire Interspect's own SQLite DB (materialized view from kernel events)
- Structured evidence with dimensionality for weighted confidence scoring

**Why fourth:** Adapt requires a working React layer (Level 2) to have events worth consuming. The staged migration means Interspect continues working throughout.

### Epic 5: Discovery Pipeline (P2, autonomous research)
Build the kernel primitives for the discovery → backlog pipeline:
- Discovery records table (scored discoveries with embeddings, source metadata)
- Confidence-tiered autonomy gates (auto/propose/log/discard)
- Discovery events (scanned, scored, promoted, proposed, dismissed)
- Backlog events (refined, merged, submitted, prioritized)
- Feedback ingestion (promote/dismiss → profile update)
- Connect interject as scanner → kernel event emitter

**Why fifth:** Discovery is Level -1 on the autonomy ladder — it feeds the system with work. Requires the event bus (Level 2) and benefits from Interspect (Level 3) for profile tuning.

### Epic 6: Rollback & Recovery (P2)
Add the ability to revert when things go wrong:
- Workflow state rollback (phase rewind with audit trail)
- Code rollback plan generation (git revert sequence from dispatch metadata)
- Discovery/backlog rollback (close beads from bad signal sources)
- Rollback events (run.rolled_back, phase.rolled_back, etc.)
- Dispatch cancellation for in-progress agents in rolled-back phases

**Why sixth:** Rollback becomes critical once the system is doing more autonomously (post-Level 2+3). Before automation, humans catch mistakes before they compound.

### Epic 7: Autarch Migration — Phase 1 (P2, Bigend + pkg/tui)
Migrate the lowest-risk Autarch tool to kernel backend:
- Bigend: project discovery → `ic run list`, agent monitoring → `ic dispatch list --active`
- `ic tui` minimal subcommand using pkg/tui components
- Validate kernel provides sufficient observability data

**Why seventh:** Bigend is read-only — lowest risk migration. Validates the kernel's data model for TUI consumption. pkg/tui components become available for ic tui.

### Epic 8: Level 4 — Orchestrate (P3, cross-project)
Multi-project coordination and portfolio management:
- Cross-project event relay (OS-layer relay process)
- Portfolio-level runs (parent records grouping per-project child runs)
- Dependency graph awareness (project_deps table, upstream_changed events)
- Composite gate evaluation (all children must pass)
- Resource scheduling across competing priorities

**Why eighth:** Orchestrate requires everything below it. Cross-project coordination is the highest complexity and lowest urgency.

### Epic 9: Autarch Migration — Phase 2 (P3, Pollard + Gurgeh)
Migrate research and PRD tools to kernel backend:
- Pollard: hunters → kernel discovery events, insight scoring → confidence scoring
- Gurgeh: spec sprint → ic run with custom phase chain, confidence → gate evidence
- Both remain functional during migration (parallel backends)

### Epic 10: Sandboxing & Autarch Phase 3 (P4, long-term)
- Sandboxing Tier 1: tool allowlists, working directory isolation
- Sandboxing Tier 2: container-based isolation
- Coldwine migration (task orchestration → kernel dispatches)
- Full Autarch suite on kernel backend

## Dependency Chain (Finalized)

```
Epic 1 (Primitives) ──→ Epic 2 (React) ──→ Epic 3 (Hook Cutover)
                                       ├──→ Epic 4 (Adapt) ──→ Epic 5 (Discovery)
                                       ├──→ Epic 6 (Rollback)
                                       └──→ Epic 7 (Autarch P1, Bigend)
                                                                    ↓
                                            Epic 5+6+7 ──→ Epic 8 (Orchestrate)
                                                                    ↓
                                            Epic 5+7 ──→ Epic 9 (Autarch P2)
                                                                    ↓
                                            Epic 8+9 ──→ Epic 10 (Sandbox+P3)
```

Precise dependencies:
- Epic 2 blocked by Epic 1
- Epic 3 blocked by Epics 1+2 (hooks need event reactor for dispatch completion patterns)
- Epic 4 blocked by Epic 2
- Epic 5 blocked by Epic 4 (discovery needs Interspect for profile tuning feedback loop)
- Epic 6 blocked by Epic 2
- Epic 7 blocked by Epic 2 (Bigend is read-only, needs only run/dispatch/event queries — moved up)
- Epic 8 blocked by Epics 5+6
- Epic 9 blocked by Epics 5+7
- Epic 10 blocked by Epics 8+9

## Resolved Questions

1. **Hook cutover requires Epic 2 (React).** Hooks that poll dispatch state benefit from event-driven patterns. Worth the wait for a cleaner cutover.
2. **Discovery requires Interspect (sequential).** Profile tuning feedback loop should work from day one.
3. **Autarch P1 (Bigend) moves up to after Epic 2.** Read-only, validates kernel observability early.

---

**Sources:**
- `infra/intercore/docs/product/intercore-vision.md` (v1.6)
- `infra/intercore/docs/research/scan-intercore-codebase-for-epic-context.md`
- `infra/intercore/docs/research/gap-analysis.md`
