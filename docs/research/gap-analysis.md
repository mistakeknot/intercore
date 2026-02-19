# Claude Code Limitations & Intercore Gap Analysis

**Date:** 2026-02-19
**Purpose:** Catalog of Claude Code platform limitations we've experienced, the workarounds we built, and how Intercore/Clavain handle them more elegantly.

---

## 1. State Evaporates at Session Boundaries

**The core problem:** Claude Code sessions are stateless. When a session ends — whether from context exhaustion, network drop, or user action — all in-memory state is gone. There's no built-in mechanism to pass context forward.

**What we built:**
- **Handoff files** (`.clavain/scratch/handoff-*.md`) — Stop hooks detect uncommitted changes and in-progress beads, write a markdown summary, symlink `handoff-latest.md`. SessionStart reads it back. Pure file-based state bridge.
- **Inflight agent manifests** (`.clavain/scratch/inflight-agents.json`) — Stop hooks reverse-engineer Claude's internal `agent-*.jsonl` files to discover background agents still running, then write a manifest the next session can read.
- **~15 temp files in `/tmp/`** — The entire intercore project exists to replace these. Sprint state, dispatch progress, throttle sentinels, bead phases, discovery caches — all tracked via touch files, JSON blobs, and mkdir-based locks because there's no persistent state layer.

**Intercore solution:** A SQLite WAL database that outlives any session. `ic run`, `ic state`, `ic events` provide durable, queryable state.

**Key quote** (intercore-vision.md):
> "LLMs forget. Context windows compress. Sessions end. Networks drop. Processes crash. [...] Clavain started this way: ~15 temp files in `/tmp/`, bash variables that lived and died with the shell, gate logic embedded in LLM prompts that the model could simply ignore."

---

## 2. Gate Enforcement Is Just Prompting

**The core problem:** When Clavain says "you must write a brainstorm doc before advancing to strategy," that's a *prompt instruction*. The LLM can simply ignore it. There's no mechanism below the model to enforce workflow discipline.

**What we built:**
- Gate checking in bash hooks (`lib-gates.sh`) that block phase advancement — but these run as hook scripts that inject text into context. The model processes the text and *usually* complies, but there's no hard enforcement.
- The interphase plugin tracks phase state in beads metadata, but advancement is still an honor system.

**Intercore solution:** `ic gate check` evaluates real conditions (`artifact_exists`, `agents_complete`, `verdict_exists`) and returns exit code 1 to block advancement. The kernel says "no" regardless of what the LLM requests. Hard gates are **kernel-enforced invariants**, not prompt suggestions.

**Key quote** (intercore-vision.md):
> "Kernel-enforced invariants below the agent — Spawn depth limits, concurrency lanes, and gate enforcement are kernel-level invariants that cannot be bypassed by prompts, agent code, or OS configuration."

---

## 3. Hooks Are Stateless Subprocesses

**The core problem:** Every hook invocation is a fresh bash subprocess. There's no way to accumulate state across invocations within a session, and no way to coordinate between hooks.

**What we built (20+ distinct workaround patterns):**

| Workaround | Temp/State Location | Why |
|---|---|---|
| Stop hook anti-cascade | `/tmp/clavain-stop-${SID}` | Multiple Stop hooks fire; need to run only once |
| Compound throttle (5 min) | `/tmp/clavain-compound-last-${SID}` | auto-compound is expensive |
| Drift throttle (10 min) | `/tmp/clavain-drift-last-${SID}` | auto-drift-check is expensive |
| Autopub throttle (60s) | `/tmp/clavain-autopub.lock` | Prevent redundant publish checks |
| Catalog reminder gate | `/tmp/clavain-catalog-remind-${SID}.lock` | Once-per-session dedup |
| Dispatch progress | `/tmp/clavain-dispatch-$$.json` | Statusline can't see dispatch state |
| Context pressure accumulator | `/tmp/intercheck-${SID}.json` | Count tool calls across hook invocations |
| Read dedup flag | `/tmp/interserve-read-denied-*` | Know if 1st or 2nd read attempt |
| Session-tmux mapping | `/tmp/intermux-mapping-${SID}.json` | MCP server can't inspect CC session |
| Phase sideband | `/tmp/clavain-bead-${SID}.json` | Statusline runs outside CC, no IPC |
| Discovery brief cache | `/tmp/clavain-discovery-brief-*.cache` | `bd list` expensive on every start |
| Routing overrides lock | `.clavain/interspect/.git-lock` | Atomic read-modify-write for JSON |
| Fallback mkdir locks | `/tmp/intercore/locks/*/` | No native cross-process mutex |
| Shadow review prompts | `/tmp/shadow-review-XXXXXX.md` | Shell arg length limits |
| Debate round state | `/tmp/debate-r{1,2}-*.md` | Inter-invocation state for multi-round |
| Sprint claim lock | `/tmp/intercore/locks/sprint-claim/` | No atomic CAS in beads |
| Auto-refresh lock | `~/.local/share/clavain/codex-auto-refresh.lock` | Single-instance guard |
| Telemetry append log | `~/.clavain/telemetry.jsonl` | No structured event logging in hooks |
| Handoff state | `.clavain/scratch/handoff-*.md` | No cross-session state |
| Inflight agents | `.clavain/scratch/inflight-agents.json` | No agent visibility across sessions |

**Intercore solution:** `ic sentinel check` provides atomic, TOCTOU-safe throttling. `ic state get/set` provides scoped key-value storage. `ic lock acquire/release` provides proper mutual exclusion. All backed by SQLite transactions, not touch-file mtime checks.

---

## 4. Context Window Exhaustion Destroys Earlier Work

**The core problem:** When parallel review agents each produce 3-5K tokens and the orchestrator reads them all, 20-40K tokens flood the context window. Earlier phases (brainstorm, plan) get compressed away. The model literally forgets what it was building.

**What we built:**
- **Synthesis subagent pattern** — Instead of the host reading 30-40K tokens of agent output, a synthesis agent reads them in an isolated context, deduplicates findings, writes a compact verdict JSON, and returns ~500 tokens. **60-80x reduction.**
- **Token accounting discovery** — Billing tokens and effective context tokens differ by up to **630x** because cache hits are free for billing but consume context space. We couldn't even *measure* the problem correctly until we understood this.

**Intercore solution:** Token tracking per dispatch, budget events when thresholds are crossed, and the OS can make informed decisions about model selection and agent spawning. The kernel records what the OS needs to optimize.

**Key quote** (synthesis solution doc):
> "Lost coherence when context compresses and earlier phases are forgotten."

**Key quote** (token-accounting solution doc):
> "Never use billing tokens to reason about context window capacity. Cache hits are invisible to billing but fully visible to the model."

---

## 5. No Agent Lifecycle Management

**The core problem:** Claude Code can spawn subagents, but there's no visibility into what's running, no spawn limits, no fan-out tracking, and no way to kill runaway agents. Background agents from previous sessions can survive context exhaustion — you don't even know they're there.

**What we built:**
- The inflight agent manifest hack (reading internal `agent-*.jsonl` files)
- intermux plugin that maps tmux sessions to agent activity
- Manual PID tracking in dispatch scripts

**Intercore solution:** `ic dispatch spawn/status/poll/kill/list` — full process lifecycle. Fan-out tracking (parent-child relationships). Spawn depth limits and concurrency caps that are **kernel-enforced** — an LLM cannot bypass `maxSpawnDepth` regardless of prompting.

**Key quote** (multi-agent guide):
> "Background agents from previous sessions survive context exhaustion — check for in-flight predecessors before launching."

---

## 6. No PostInstall Hook for Plugins

**The core problem:** Claude Code has no `postInstall` hook (requested in #9394, **closed NOT_PLANNED**). MCP servers launch *before* SessionStart hooks fire. So if a compiled Go binary is missing from the plugin cache (because `git clone` doesn't include gitignored binaries), the MCP server fails silently at startup and hooks can't fix it.

**What we built:** The **launcher script pattern** — a bash wrapper tracked in git that checks for the binary, builds it from source if missing (~15s first run), then `exec`s it. Every compiled MCP server in the ecosystem uses this now.

**Key quote** (critical-patterns.md):
> "Claude Code has no `postInstall` hook (requested in #9394, closed NOT_PLANNED). MCP servers launch *before* SessionStart hooks, so hooks can't fix a missing binary."

---

## 7. No Event System / No Reactivity

**The core problem:** Claude Code hooks are fire-and-forget. There's no event bus, no subscription model, no way for one component to react to what another component did. If a review agent completes, nothing happens automatically — someone has to check.

**What we built:** Telemetry append logs (`~/.clavain/telemetry.jsonl`), ad-hoc file watching, polling loops in dispatch scripts.

**Intercore solution:** `ic events tail` — a typed, append-only event log with consumer cursors. Phase transitions, gate evaluations, dispatch completions all produce events. Consumers (Interspect, TUI, OS reactor) subscribe with durable or ephemeral cursors and get at-least-once delivery. The same event stream feeds self-improvement analysis and operational visibility.

---

## 8. No Learning from Mistakes

**The core problem:** The same false-positive review agents get dispatched every time. The same irrelevant checks fire on every sprint. There's no feedback loop — override an agent's finding today, and it'll produce the same finding tomorrow.

**What we built:** Interspect — a profiler that reads kernel events, correlates with human corrections, and proposes routing changes (agent exclusions, prompt adjustments). But it needs structured evidence to work.

**Intercore solution:** Every gate evaluation, dispatch verdict, and human override is recorded as **structured evidence** with enough dimensionality for analysis. This is the data foundation for closed-loop improvement — the "Level 3: Adapt" on the autonomy ladder.

**Key quote** (interspect-prd):
> "the same false positives recur across sessions, the same irrelevant agents get dispatched, and prompt quality drifts without feedback."

---

## 9. Settings Permission Bloat from Heredocs

**The core problem:** When Claude Code uses a heredoc in a Bash tool call and the user clicks "Allow", the **entire command text** — including inline file contents — is saved verbatim as a permission entry. These entries never match again and accumulate as dead weight. Shell fragments (`do`, `done`, `fi`) get saved individually as permission entries.

**What we built:** Global rule in `~/.claude/CLAUDE.md`: "Never use heredocs in Bash tool calls. Write content with Write tool first, then reference the file."

---

## 10. Duplicate MCP Server Registration

**The core problem:** MCP servers registered at multiple levels (global settings, project `.mcp.json`, plugin manifests) get loaded **independently with no deduplication**. Result: ~12K extra tokens consumed per session for duplicate tool namespaces (26 tools x 2 = ~24K tokens wasted).

**What we built:** Manual deduplication — remove from `settings.json`, keep only plugin-based registration.

---

## 11. `disable-model-invocation` Has No Caller Granularity

**The core problem:** The flag blocks **all** model invocations regardless of context. An orchestrator command can't chain sub-commands that have this flag set. The entire `/lfg` pipeline (9 sub-commands) was broken by blanket application.

**What we built:** Removed blanket flag; evaluated per-command whether model invocation restriction was appropriate.

---

## 12. LLM Compounding Creates False-Positive Feedback Loops

**The core problem:** When prior findings are injected into agent context for "learning," the LLM reliably agrees with its own input — a primed confirmation looks identical to an independent discovery. This creates a self-reinforcing loop that permanently bakes false positives into the knowledge layer.

**What we built:** Provenance tracking (`independent` vs `primed`) in Interspect. Only independent confirmations count as evidence.

**Key quote** (compounding-false-positive-feedback-loop doc):
> "An independent discovery is evidence that the finding is real. A primed confirmation is just the agent agreeing with its own input — which LLMs reliably do."

---

## 13. hooks.json Errors Are Silent

**The core problem:** If you use the wrong format for `hooks.json` (flat array instead of event-type keys), hooks silently don't load. No error, no warning, no feedback. Valid JSON, semantically wrong for Claude Code.

**What we built:** Template-based format enforcement; troubleshooting guide; plugin-validator agent.

**Key quote** (critical-patterns.md):
> "The flat array format is syntactically valid JSON but semantically wrong for Claude Code — and there's no validation error, hooks just silently don't load."

---

## Summary: The Infrastructure Maturity Ladder

Each workaround started as a quick hack solving an immediate pain point — a touch file here, a JSON blob there. Over time, the hacks accumulated into a fragile web of ~20 independent state mechanisms, each with its own naming convention, TTL logic, and cleanup strategy. Intercore consolidates them into a single, transactional, observable system of record.

| CC Limitation | Workaround Layer | Intercore Primitive |
|---|---|---|
| Session state evaporates | Temp files, handoff.md | `ic state`, `ic run` (SQLite WAL) |
| Gates are just prompts | Hook scripts inject context | `ic gate check` (hard/soft enforcement) |
| Hooks are stateless | 20+ temp files in `/tmp/` | `ic sentinel`, `ic state`, `ic lock` |
| Context floods from agents | Synthesis subagent pattern | Token tracking + budget events |
| No agent lifecycle | Reverse-engineer agent JSONL | `ic dispatch` (spawn/poll/kill) |
| No reactivity | Telemetry JSONL, polling | `ic events tail` (typed event bus) |
| No learning | Same false positives recur | Structured evidence → Interspect |
| No postInstall hook | Launcher script pattern | (Platform gap, not kernel scope) |
| Silent hook failures | Troubleshooting guides | (Platform gap, not kernel scope) |

**Sources:**
- `infra/intercore/docs/product/intercore-vision.md`
- `infra/intercore/docs/prds/2026-02-17-intercore-state-database.md`
- `infra/intercore/docs/brainstorms/2026-02-17-intercore-state-database-brainstorm.md`
- `infra/intercore/PHILOSOPHY.md`
- `docs/solutions/patterns/synthesis-subagent-context-isolation-20260216.md`
- `docs/solutions/patterns/token-accounting-billing-vs-context-20260216.md`
- `docs/solutions/patterns/critical-patterns.md`
- `docs/solutions/integration-issues/plugin-loading-failures-interverse-20260215.md`
- `docs/guides/multi-agent-coordination.md`
- `infra/intercore/docs/product/interspect-prd.md`
- `hub/clavain/hooks/lib-intercore.sh`
- `hub/clavain/hooks/session-handoff.sh`
