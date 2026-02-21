# Intercore Roadmap — Epic Decomposition

**Bead:** iv-c6az (closed)

Decomposition of the intercore vision into 10 prioritized epics organized by the autonomy ladder (Level 0-4). Each epic is sprintable in 1-5 sessions with clear entry/exit criteria.

## Shipped Epics

### E1: Kernel Primitives Completion (P1) — SHIPPED
**What:** Core kernel-level primitives that all subsequent work depends on.
**Shipped:** Custom phase chains, phase skip with audit trail, artifact SHA256 hashing, dispatch token tracking, budget threshold events, token aggregation.

### E2: Level 2 — React (P1) — SHIPPED
**What:** Event reactor wiring so kernel events trigger automatic actions.
**Shipped:** `dispatch.Store.SpawnByAgentID`, SpawnHandler in Notifier, `ic events tail -f` with durable consumers, OS-level event reactor pattern.

### E3: Hook Cutover — Big Bang (P1) — SHIPPED
**What:** Migrated all Clavain hooks from temp files to `ic`.
**Shipped:** No hooks read/write `/tmp/clavain-*`. Sentinel, dispatch, phase, and session operations all use `ic`. End-to-end sprint workflows pass.

### E4: Level 3 — Adapt (P2) — SHIPPED
**What:** Connected Interspect to kernel events for evidence-based self-improvement.
**Shipped:** `ic interspect record/query`, durable consumer cursors, correction and override event types.

### E5: Discovery Pipeline (P2) — SHIPPED
**What:** Kernel primitives for the discovery → backlog pipeline.
**Shipped:** `discoveries` table with embeddings, full `ic discovery` CLI (submit/status/list/score/promote/dismiss/feedback/profile/decay/rollback/search), confidence-tiered autonomy gates, discovery + backlog events in event bus, interest profile with feedback-driven vector shift.

### E6: Rollback & Recovery (P2) — SHIPPED
**What:** Structured rollback at three layers (workflow, code, discovery).
**Shipped:** `ic run rollback --to-phase` with audit trail, `--layer=code` for git revert plan from dispatch metadata, `ic discovery rollback`, rollback events, dispatch cancellation on rollback, rolled_back phase marking.

### E7: Autarch Phase 1 — Bigend + ic tui (P2) — SHIPPED
**What:** Migrated Bigend to kernel backend with kernel-aware dashboard.
**Shipped:** Kernel enrichment pipeline in aggregator, unified 6-state status model, dispatch + token metrics on dashboard, event source tags, kernel run count in sidebar, run list + detail pane with four-pane layout, icdata package extraction. 9 commits, 20 files, +1342/-652 lines.

### E8: Level 4 — Orchestrate (P3) — SHIPPED
**What:** Multi-project coordination and portfolio management.
**Shipped:** Cross-project event relay (`ic portfolio relay`), portfolio runs (`ic run create --projects=`), project dependencies (`project_deps` table, `upstream_changed` events), portfolio gates (`children_at_phase`), dispatch budget (`max_dispatches` with relay-maintained counter). Schema v10.

## Open Epics

### E9: Autarch Phase 2 — Pollard + Gurgeh (P3)
**What:** Migrate research and PRD tools to kernel backend.
**Bead:** iv-6376
**Dependencies:** E5 (shipped), E7 (shipped) — **unblocked**
**Acceptance criteria:**
- [ ] Pollard's hunters emit discovery events through kernel
- [ ] Gurgeh's spec sprint uses `ic run create` with custom phase chain
- [ ] Gurgeh's confidence scores become gate evidence
- [ ] Both tools remain functional during migration (parallel backends)

### E10: Sandboxing & Autarch Phase 3 (P4)
**What:** Agent sandboxing and final Autarch migration.
**Bead:** iv-qr0f
**Dependencies:** E8, E9 — **blocked**
**Acceptance criteria:**
- [ ] Dispatch records include SandboxSpec (tool allowlist, working directory, access mode)
- [ ] Effective sandbox policy recorded on dispatch completion
- [ ] Container-based isolation support (image, mounts, network policy)
- [ ] Coldwine task orchestration uses `ic dispatch` for agent lifecycle
- [ ] Full Autarch TUI suite reads kernel state as single source of truth

## Dependency Graph

```
E1 ──→ E2 ──→ E3 (Hook Cutover)     ✓ all shipped
           ├──→ E4 ──→ E5 (Discovery) ✓
           ├──→ E6 (Rollback)          ✓
           └──→ E7 (Autarch P1)        ✓
                         ↓
     E5+E6 ──→ E8 (Orchestrate)        ✓
     E5+E7 ──→ E9 (Autarch P2)        ← unblocked
     E8+E9 ──→ E10 (Sandbox+P3)       ← E9 remains
```

## Non-goals

- Building a distributed/multi-machine deployment model
- Replacing Claude Code or Codex as agent runtimes
- Creating a workflow DSL
- Auto-generating agents or prompts (that's Clavain/OS layer)

## Resolved Questions

- **Token tracking granularity:** Per-dispatch totals (self-reported by agents via `ic dispatch tokens`). Per-tool-call tracking deferred — not needed for budget enforcement.
- **Discovery embedding model:** Using all-MiniLM-L6-v2 (same as interject) for consistency. Brute-force cosine search in SQLite with BLOB storage. Upgrade path exists when corpus exceeds ~10k entries.
