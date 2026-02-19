# Autarch — Vision Document

**Version:** 1.0
**Date:** 2026-02-19
**Status:** Draft

---

## The Core Idea

Autarch is the application layer of the Interverse stack — the interactive surfaces through which Clavain's agency is experienced. Where Clavain (the OS) provides the developer experience via CLI skills, hooks, and slash commands, Autarch provides it via rich terminal UIs.

Autarch sits above the OS, which sits above the kernel:

```
Apps (Autarch)
├── Interactive TUI tools: Bigend, Gurgeh, Coldwine, Pollard
├── Shared component library: pkg/tui (Bubble Tea + lipgloss)
├── Renders OS opinions into interactive experiences
└── Swappable — Autarch is one set of apps, not the only possible set

Drivers (Companion Plugins)
├── Each wraps one capability (review, coordination, code mapping, research)
├── Call the kernel directly for shared state — no Clavain bottleneck
└── Examples: interflux (review), interlock (coordination), interject (research)

OS (Clavain)
├── The autonomous software agency — macro-stages, quality gates, model routing
├── Skills, prompts, routing tables, workflow definitions
├── Configures the kernel: phase chains, gate rules, dispatch policies
└── Reacts to kernel events (agent completed → advance phase)

Kernel (Intercore)
├── Runs, phases, gates, dispatches, events — the durable system of record
├── Host-agnostic Go CLI + SQLite
└── Mechanism, not policy — the kernel doesn't know what "brainstorm" means

Profiler: Interspect (cross-cutting)
├── Reads kernel events (phase results, gate evidence, dispatch outcomes)
├── Proposes changes to OS configuration (routing, agent selection, gate rules)
└── Never modifies the kernel — only the OS layer
```

### Apps Are Swappable

Autarch is one realization of the application layer. The kernel and OS are designed so that any set of apps can render them. A different team could build a web dashboard, a VS Code extension, or a mobile client — all reading the same kernel state, all driven by the same OS policies. Autarch is the reference implementation, not the only implementation.

This is the same principle that makes Clavain portable across host platforms: if Claude Code disappears, the OS and kernel survive. Similarly, if Autarch is replaced, the OS and kernel are unaffected. Apps render; the OS decides; the kernel records.

### Relationship to Clavain

In the target architecture, apps don't contain agency logic. Autarch doesn't decide which model to route a review to, or what gates a phase requires, or when to advance a run. Those are OS decisions. Autarch renders those decisions into interactive experiences:

- Bigend renders kernel run state as a monitoring dashboard
- Gurgeh renders kernel phase chains as a PRD generation workflow
- Coldwine renders kernel dispatches as a task coordination interface
- Pollard renders kernel discoveries as a research intelligence tool

When Clavain's policies change (new gate rules, different model routing), Autarch's UIs reflect the change without code modification — they read kernel state, which the OS controls.

## The Four Tools

**Bigend** — Multi-project mission control. A read-only aggregator that monitors agent activity, displays run progress, and provides a dashboard view across projects. Currently discovers projects via filesystem scanning and monitors agents via tmux session heuristics. Has both a web interface (htmx + Tailwind) and an in-progress TUI.

**Gurgeh** — PRD generation and validation. The most mature tool. Drives an 8-phase spec sprint with per-phase AI generation, confidence scoring (0.0-1.0 across completeness, consistency, specificity, and research axes), cross-section consistency checking, assumption confidence decay, and spec evolution versioning. Specs persist as YAML.

**Coldwine** — Task orchestration. Reads Gurgeh PRDs, generates epics/stories/tasks, manages git worktrees, coordinates agent execution, and integrates with Intermute for messaging. Has a full Bubble Tea TUI (the largest single view at 2200+ lines).

**Pollard** — Research intelligence. Multi-domain hunters (tech, academic, medical, legal, GitHub), continuous watch mode, and insight synthesis. CLI-first with integration into Gurgeh and Coldwine.

## Shared Component Library: `pkg/tui`

Autarch's shared TUI component library is fully portable and immediately reusable:

- `ShellLayout` — split-pane layout with resizable panels
- `ChatPanel` — streaming chat interface with message history
- `Composer` — text input with command completion
- `CommandPicker` — fuzzy-searchable command palette
- `AgentSelector` — agent selection with status indicators
- `View` interface — clean abstraction for pluggable view implementations
- Tokyo Night color scheme — consistent theming across all views

These components depend only on Bubble Tea and lipgloss. They have no Autarch domain coupling.

## Migration to Intercore Backend

Each tool migrates from its own storage backend (YAML files, tool-specific SQLite) to Intercore's kernel as the shared state layer. The migration follows coupling depth — least coupled tools migrate first:

**1. Bigend (read-only — migrate first).** Bigend is a pure observer. Today it discovers projects via filesystem scanning and monitors agents by scraping tmux panes. Migration swaps these data sources:
- Project discovery → `ic run list` across project databases
- Agent monitoring → `ic dispatch list --active`
- Run progress → `ic events tail --all --consumer=bigend`
- Dashboard metrics → kernel aggregates (runs per state, dispatches per status, token totals)

Bigend never writes to the kernel — it only reads. This makes it the lowest-risk migration and the first validation that the kernel provides sufficient observability data.

**2. Pollard (research → discovery pipeline).** Pollard's multi-domain hunters map directly to Intercore's discovery subsystem. Migration connects Pollard's research output to the kernel:
- Hunter results → `ic discovery` events through the kernel event bus
- Insight scoring → kernel confidence scoring with Pollard's domain-specific weights
- Research queries → `ic discovery search` for semantic retrieval
- Watch mode → kernel event consumer that triggers targeted scans

Pollard becomes the scanner component that feeds the discovery → backlog pipeline (see [Clavain vision doc](../../../../hub/clavain/docs/vision.md) for the full pipeline workflow). Its hunters become Intercore source adapters.

**3. Gurgeh (PRD generation → run lifecycle).** Gurgeh's 8-phase spec sprint maps to Intercore's run lifecycle with a custom phase chain. Migration creates runs for PRD generation:
- Spec sprint → `ic run create --phases='["vision","problem","users","features","cujs","requirements","scope","acceptance"]'`
- Phase confidence scores → kernel gate evidence (Gurgeh's confidence thresholds become gate rules)
- Spec artifacts → `ic run artifact add` for each generated section
- Spec evolution → run versioning (new run per spec revision, linked via portfolio)

Gurgeh's arbiter (the sprint orchestration engine) remains as tool-specific logic during the migration — it drives the LLM conversation that generates each spec section. The kernel tracks the lifecycle; Gurgeh provides the intelligence.

> **Transitional state.** The arbiter is agency logic (it decides how to sequence LLM calls, when to accept confidence scores, and when to advance). In the target architecture, this intelligence migrates to the OS layer (Clavain), making Gurgeh a pure rendering surface for PRD generation. Until that migration, the "apps are swappable" claim is partially false for Gurgeh and Coldwine — a replacement app would need to reimplement the arbiter and orchestration logic, not just render kernel state. This is an acknowledged architectural debt, not an intentional design choice.

**4. Coldwine (task orchestration — migrate last).** Coldwine has the deepest coupling to Autarch's domain model (`Initiative → Epic → Story → Task`). Its migration is the most complex:
- Task hierarchy → beads (Coldwine's planning hierarchy maps to bead types and dependencies)
- Agent coordination → `ic dispatch` for agent lifecycle
- Git worktree management → remains in Coldwine (kernel doesn't manage git)
- Intermute integration → remains in Coldwine (kernel doesn't manage messaging)

Coldwine's migration overlaps with Clavain's sprint skill — both orchestrate task execution with agent dispatch. The resolution is that Coldwine provides TUI-driven orchestration while Clavain provides CLI-driven orchestration, both calling the same kernel primitives.

## Relationship to the Three-Layer Architecture

Autarch sits as the application layer atop the OS:

```
Apps (Autarch)
├── Bigend: multi-project mission control (monitoring dashboard)
├── Gurgeh: PRD generation with confidence scoring (spec workflow)
├── Coldwine: task orchestration with agent coordination (execution interface)
├── Pollard: research intelligence with multi-domain hunters (research tool)
└── pkg/tui: shared Bubble Tea components (ShellLayout, ChatPanel, Tokyo Night)

User Interaction
├── Clavain (CLI: slash commands, hooks, skills) → calls ic
├── Autarch (TUI: Bigend, Gurgeh, Coldwine, Pollard) → calls ic
└── Direct CLI (ic run, ic dispatch, ic events) → for power users and scripts

All three surfaces share the same kernel state. A run created via Clavain's /sprint
is visible in Bigend's dashboard. A discovery from Pollard's hunters triggers the same
kernel events that Clavain's hooks consume. The kernel is the single source of truth.
```

## What `pkg/tui` Enables

Beyond the four Autarch tools, the shared component library enables a lightweight `ic tui` subcommand — a kernel-native TUI that provides basic observability without requiring the full Autarch tool suite:

- Run list with phase progress bars
- Event stream tail (live-updating)
- Dispatch status dashboard
- Discovery inbox for confidence-tiered review

This minimal TUI would be built on `pkg/tui` components and call `ic` directly. It's the kernel's own status display — simpler than Bigend but always available wherever `ic` is installed.

## Signal Architecture

When latency-sensitive consumers (TUI dashboards, live event streams) need sub-second event delivery, the kernel's pull-based `ic events tail` API may be insufficient. Autarch's signal broker addresses this with an app-layer real-time projection:

- **In-process pub/sub fan-out** with typed subscriptions — each TUI view subscribes to the event types it renders
- **WebSocket streaming** to TUI and web consumers — Bigend's dashboard and `ic tui` connect via WebSocket rather than polling
- **Backpressure handling** — evict-oldest-on-full for non-durable consumers (TUI views that can tolerate dropped frames), blocking for durable consumers (audit trails)

The kernel's durable event log remains the source of truth. The broker is a real-time projection of it — a convenience for latency-sensitive rendering, not a replacement for the event bus. If the broker crashes, consumers fall back to `ic events tail` with their cursor position intact.

> **Status:** This architecture exists in Autarch's current codebase (the signal broker pattern is proven in Bigend and Coldwine). It has not yet been connected to Intercore's event bus — that integration is part of the Bigend migration (see Migration to Intercore Backend above).

## What Autarch Is Not

- **Not the OS.** Autarch renders OS decisions; it doesn't make them. Routing, gate policies, and model selection are Clavain's domain.
- **Not the kernel.** Autarch reads and writes kernel state through `ic`; it doesn't own the system of record.
- **Not required.** Everything Autarch does can be done via Clavain CLI or direct `ic` commands. Autarch is a convenience layer that makes the system more accessible, not a dependency.
- **Not a single monolith.** Each tool is independently useful. You can run Bigend for monitoring without Coldwine for orchestration.

---

*This document was extracted from the Intercore vision doc on 2026-02-19 to establish the application layer as a distinct architectural concern, separate from both the kernel (Intercore) and the OS (Clavain).*
