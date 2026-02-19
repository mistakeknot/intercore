# PRD: Intercore Vision Roadmap — Epic Decomposition

**Bead:** iv-c6az

## Problem

The intercore vision doc (v1.6, 893 lines) describes a comprehensive kernel for autonomous software development, but there's no structured execution roadmap. Without prioritized epics with explicit dependencies, work risks being done out of order, blocking downstream capabilities, or building primitives nobody uses yet.

## Solution

Decompose the vision into 10 prioritized epics organized by the autonomy ladder (Level 0-4), with cross-cutting kernel primitives as separate epics. Each epic is sprintable in 1-5 sessions and has clear entry/exit criteria.

## Features

### E1: Kernel Primitives Completion (P1)
**What:** Complete the missing kernel-level primitives that all subsequent work depends on.
**Acceptance criteria:**
- [ ] `ic run create --phases='["a","b","c"]'` accepts arbitrary phase arrays
- [ ] `ic run skip <id> <phase> --reason="..."` records phase skip with audit trail
- [ ] `run_artifacts` table has `content_hash` (SHA256) and `dispatch_id` columns
- [ ] `dispatches` table has `tokens_in`, `tokens_out`, `cache_hits` columns
- [ ] `ic run tokens <id>` shows per-phase and total token aggregation
- [ ] `budget.warning` and `budget.exceeded` events emitted at configurable thresholds
- [ ] All existing tests pass, new tests cover additions

### E2: Level 2 — React (P1)
**What:** Wire the event reactor so kernel events trigger automatic actions.
**Acceptance criteria:**
- [ ] `dispatch.Store.SpawnByAgentID` implemented
- [ ] SpawnHandler registered in Notifier, fires on `runtrack.agent_added` events
- [ ] `ic events tail -f --consumer=<name> --poll-interval=500ms` works for long-running consumers
- [ ] Documented pattern for OS-level event reactor (Clavain hook or standalone process)
- [ ] Integration test: add agent → event fires → SpawnHandler invoked

### E3: Hook Cutover — Big Bang (P1)
**What:** Migrate all ~20 Clavain hooks from temp files to `ic` in one release.
**Acceptance criteria:**
- [ ] No Clavain hook reads or writes `/tmp/clavain-*` files
- [ ] All sentinel-based throttling uses `ic sentinel check`
- [ ] All dispatch state queries use `ic dispatch status/list`
- [ ] Phase sideband uses `ic run phase`
- [ ] Session handoff uses `ic run` + `ic state`
- [ ] `ic` is a hard dependency in Clavain's CLAUDE.md
- [ ] All existing Clavain sprint/quality-gates workflows pass end-to-end

### E4: Level 3 — Adapt (P2)
**What:** Connect Interspect to kernel events for evidence-based self-improvement.
**Acceptance criteria:**
- [ ] Interspect registered as durable consumer (`ic events cursor register --durable interspect`)
- [ ] Interspect reads phase, gate, and dispatch events from kernel
- [ ] `correction` and `override` event types added to kernel event schema
- [ ] Interspect's own SQLite DB retired (state is materialized view from kernel events)
- [ ] Routing proposals produced from kernel evidence, not hook-collected data

### E5: Discovery Pipeline (P2)
**What:** Build kernel primitives for the discovery → backlog pipeline.
**Acceptance criteria:**
- [ ] `discoveries` table with embeddings, source metadata, confidence score, lifecycle state
- [ ] `ic discovery scan/submit/search/promote/dismiss` CLI commands
- [ ] Confidence-tiered autonomy gates (high/medium/low/discard) kernel-enforced
- [ ] Discovery events flow through event bus (scanned, scored, promoted, proposed, dismissed)
- [ ] Backlog events (refined, merged, submitted, prioritized) in event bus
- [ ] Feedback ingestion updates interest profile (promote/dismiss → vector shift)
- [ ] Interject connected as scanner → kernel event emitter

### E6: Rollback & Recovery (P2)
**What:** Add structured rollback at three layers.
**Acceptance criteria:**
- [ ] `ic run rollback <id> --to-phase=<phase>` resets phase with audit trail
- [ ] `ic run rollback <id> --layer=code` generates git revert plan from dispatch metadata
- [ ] `ic discovery rollback --source=<src> --since=<ts>` proposes bead cleanup
- [ ] Rollback events recorded (run.rolled_back, phase.rolled_back, discovery.rolled_back)
- [ ] In-progress dispatches in rolled-back phases are cancelled
- [ ] Rolled-back phases are marked `rolled_back`, not deleted

### E7: Autarch Migration Phase 1 — Bigend + ic tui (P2)
**What:** Migrate Bigend to kernel backend and build minimal `ic tui`.
**Acceptance criteria:**
- [ ] Bigend's project discovery uses `ic run list` instead of filesystem scanning
- [ ] Bigend's agent monitoring uses `ic dispatch list --active` instead of tmux scraping
- [ ] Bigend's event stream uses `ic events tail` instead of custom polling
- [ ] `ic tui` subcommand shows run list, event tail, dispatch dashboard
- [ ] `pkg/tui` components (ShellLayout, ChatPanel) are importable from hub/autarch/pkg/tui

### E8: Level 4 — Orchestrate (P3)
**What:** Multi-project coordination and portfolio management.
**Acceptance criteria:**
- [ ] Cross-project event relay process (tails multiple DBs, writes to shared relay)
- [ ] `ic run create --projects=a,b` creates portfolio-level run
- [ ] `project_deps` table with `dependency.upstream_changed` event emission
- [ ] Portfolio gates: all child runs must pass for portfolio to advance
- [ ] Resource scheduling: max concurrent dispatches across all runs

### E9: Autarch Migration Phase 2 — Pollard + Gurgeh (P3)
**What:** Migrate research and PRD tools to kernel backend.
**Acceptance criteria:**
- [ ] Pollard's hunters emit discovery events through kernel
- [ ] Gurgeh's spec sprint uses `ic run create` with custom phase chain
- [ ] Gurgeh's confidence scores become gate evidence
- [ ] Both tools remain functional during migration (parallel backends)

### E10: Sandboxing & Autarch Phase 3 (P4)
**What:** Agent sandboxing and final Autarch migration.
**Acceptance criteria:**
- [ ] Dispatch records include SandboxSpec (tool allowlist, working directory, access mode)
- [ ] Effective sandbox policy recorded on dispatch completion
- [ ] Container-based isolation support (image, mounts, network policy)
- [ ] Coldwine task orchestration uses `ic dispatch` for agent lifecycle
- [ ] Full Autarch TUI suite reads kernel state as single source of truth

## Non-goals

- Building a distributed/multi-machine deployment model
- Replacing Claude Code or Codex as agent runtimes
- Creating a workflow DSL
- Auto-generating agents or prompts (that's Clavain/OS layer)

## Dependencies

```
E1 ──→ E2 ──→ E3 (Hook Cutover)
           ├──→ E4 ──→ E5 (Discovery)
           ├──→ E6 (Rollback)
           └──→ E7 (Autarch P1)
                         ↓
     E5+E6 ──→ E8 (Orchestrate)
     E5+E7 ──→ E9 (Autarch P2)
     E8+E9 ──→ E10 (Sandbox+P3)
```

## Open Questions

- **Token tracking granularity:** Should we track per-tool-call tokens or just per-dispatch totals? Per-dispatch is simpler but less useful for cost optimization.
- **Discovery embedding model:** Use the same all-MiniLM-L6-v2 that interject uses, or upgrade to a larger model? Consistency vs quality tradeoff.
