# Shared Glossary — Interverse Architecture

Referenced by: intercore-vision.md, clavain-vision.md, autarch-vision.md

| Term | Meaning | Layer |
|------|---------|-------|
| **Work item** | Generic kernel record for a trackable unit of work (has state machine, metadata, provenance) | Kernel |
| **Bead** | Clavain's rendering of a work item — adds priority semantics, sprint association, phase tracking | OS |
| **Run** | Kernel lifecycle primitive — a named execution with a phase chain, gate rules, and event trail | Kernel |
| **Sprint** | OS-level run template with preset phases (brainstorm → plan → execute → ship) | OS |
| **Phase** | A named stage within a run. Kernel enforces ordering and gate checks; OS defines phase names and semantics | Kernel (mechanism) / OS (policy) |
| **Gate** | Enforcement point between phases. Kernel evaluates pass/fail; OS defines what evidence is required | Kernel (mechanism) / OS (policy) |
| **Dispatch** | Kernel record tracking an agent spawn — PID, status, token usage, artifacts | Kernel |
| **Companion plugin** | An `inter-*` capability module (interflux, interlock, interject, etc.) — wraps one capability as an OS extension | OS extension |
| **Host adapter** | Platform integration layer (Claude Code plugin interface, Codex CLI, bare shell) — not a companion plugin | App/OS boundary |
| **Dispatch driver** | Agent execution backend (Claude CLI, Codex CLI, container runtime) — the runtime that executes a dispatch | Kernel |
| **Event** | Typed, immutable record of a state change in the kernel. Append-only log with consumer cursors | Kernel |
| **Macro-stage** | OS-level workflow grouping: Discover, Design, Build, Ship. Each maps to sub-phases in the kernel | OS |
| **Interspect** | Adaptive profiler — reads kernel events, proposes OS config changes. Cross-cutting, not a layer | Cross-cutting |
| **Autarch** | The application layer — four TUI tools (Bigend, Gurgeh, Coldwine, Pollard) plus shared `pkg/tui` library | Apps (Layer 3) |
| **interphase** | Legacy compatibility shim for phase/gate tracking. Delegates to `ic` primitives; provides no independent state | OS (deprecated) |
