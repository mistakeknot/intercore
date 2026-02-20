# PRD: Intercore E1 — Kernel Primitives Completion

**Bead:** iv-som2

## Problem

The intercore kernel has Level 0 (Record) and Level 1 (Enforce) complete, but all subsequent epics — E2 (React), E3 (Hook Cutover), and everything downstream — are blocked by missing kernel primitives. Phase chains are hardcoded to Clavain's 8-phase lifecycle, making non-Clavain workflows impossible. No token tracking means no cost visibility. Skip logic embeds OS policy in kernel code.

## Solution

Complete six kernel primitives in a single schema migration (v5→v6): configurable phase chains, explicit skip command, artifact content hashing, token tracking, token aggregation, and budget threshold events. All follow the mechanism/policy separation principle — the kernel provides primitives, the OS provides opinions.

## Features

### F1: Configurable Phase Chains
**What:** Accept arbitrary phase chain arrays at `ic run create` time instead of hardcoded 8-phase chain.
**Acceptance criteria:**
- [ ] `ic run create --phases='["a","b","c"]'` stores chain as JSON column on `runs` table
- [ ] `ic run advance` validates transitions against the stored chain (walking forward), not static map
- [ ] `ic run create` without `--phases` defaults to the legacy 8-phase Clavain chain
- [ ] Existing runs in DB (NULL phases column) use legacy chain via fallback logic
- [ ] Phase chain is immutable after run creation (no mid-run chain modification)
- [ ] `ic run status --json` includes the phases array in output
- [ ] `transitionTable`, `validTransitions`, and `complexityWhitelist` maps removed from phase.go
- [ ] Gate rules supplied alongside chain at create time as optional `--gate-rules` JSON
- [ ] Unit tests cover: custom chain creation, advance through custom chain, fallback to legacy chain, invalid phase validation

### F2: Explicit Skip Command
**What:** Standalone `ic run skip` command that records phase skips with audit trail, replacing implicit auto-skip in Advance.
**Acceptance criteria:**
- [ ] `ic run skip <id> <phase> --reason="..." --actor="clavain"` skips a phase with audit trail
- [ ] Validates: phase exists in chain, not already completed/skipped, run is on or before this phase
- [ ] Records `phase_events` row with `event_type=skip`, reason, actor, and timestamp
- [ ] `ic run advance` no longer auto-skips — only moves to immediate next non-skipped phase
- [ ] Skip events visible in `ic run events <id>` output
- [ ] `NextRequiredPhase()`, `ShouldSkip()`, and `complexityWhitelist` removed from kernel code
- [ ] Unit tests cover: valid skip, skip already-completed phase (error), skip nonexistent phase (error), advance after skip, multiple skips in sequence

### F3: Artifact Content Hashing + dispatch_id
**What:** Add SHA256 content hash and dispatch provenance to artifact records.
**Acceptance criteria:**
- [ ] `run_artifacts` table gains `content_hash TEXT` and `dispatch_id TEXT` columns (nullable)
- [ ] `ic run artifact add <run> --path=<f> --phase=<p> [--dispatch=<d>]` computes SHA256 of file and stores hash
- [ ] `ic run artifact list` output includes content_hash and dispatch_id
- [ ] `artifact_exists` gate check unchanged (record-only, no re-hash)
- [ ] If file doesn't exist at add time, command fails with clear error (not a NULL hash)
- [ ] Schema migration v6 adds columns, existing records get NULL
- [ ] Unit tests cover: artifact add with hash, artifact add with dispatch_id, artifact list with new columns

### F4: Token Tracking on Dispatches
**What:** Record per-dispatch token counts (input, output, cache hits) reported by agents.
**Acceptance criteria:**
- [ ] `dispatches` table gains `tokens_in INTEGER`, `tokens_out INTEGER`, `cache_hits INTEGER` (all nullable)
- [ ] `ic dispatch complete <id> --tokens-in=X --tokens-out=Y --cache-hits=Z` reports tokens at completion
- [ ] `ic dispatch tokens <id> --set --in=X --out=Y --cache=Z` reports tokens standalone (mid-dispatch update)
- [ ] `ic dispatch status <id> --json` includes token fields in output
- [ ] NULL means "not reported", 0 means "reported as zero" — semantic distinction preserved
- [ ] Token values must be non-negative integers (reject negative values)
- [ ] Schema migration v6 adds columns, existing dispatches get NULL
- [ ] Unit tests cover: token reporting at completion, standalone token update, negative value rejection, status output with tokens

### F5: Token Aggregation Queries
**What:** Query-time aggregation of token counts at run and project level.
**Acceptance criteria:**
- [ ] `ic run tokens <id>` displays per-phase subtotals, run total, and cache hit ratio
- [ ] `ic run tokens <id> --json` outputs structured JSON for programmatic consumption
- [ ] `ic run tokens --project=<dir>` aggregates across all runs for a project path
- [ ] Dispatches with NULL tokens excluded from aggregation (not counted as zero)
- [ ] Cache hit ratio computed as `SUM(cache_hits) / SUM(tokens_in)` (0 if no tokens_in)
- [ ] Human-readable output includes formatted numbers and percentages
- [ ] Unit tests cover: single-run aggregation, per-phase breakdown, project-level aggregation, runs with no token data

### F6: Budget Threshold Events
**What:** Emit events when cumulative run token usage crosses configurable thresholds.
**Acceptance criteria:**
- [ ] `runs` table gains `token_budget INTEGER` and `budget_warn_pct INTEGER DEFAULT 80` (nullable)
- [ ] `ic run create --token-budget=100000 --budget-warn-pct=80` sets budget thresholds
- [ ] When token report pushes cumulative total past warn threshold: `budget.warning` event emitted
- [ ] When cumulative total exceeds budget: `budget.exceeded` event emitted
- [ ] Each threshold event emitted at most once per run (dedup tracked in state store)
- [ ] Event payload: run_id, current_total, budget, threshold_pct, triggering dispatch_id
- [ ] Events are informational only — no enforcement (no dispatch killing, no advancement blocking)
- [ ] Runs without token_budget skip all budget checks (zero overhead)
- [ ] Unit tests cover: warning event emission, exceeded event emission, dedup (no repeated events), no-budget runs skip checks

## Non-goals

- Token cost-in-dollars computation (OS policy, not kernel)
- Enforcement of budget limits (OS decides response to events)
- Phase chain DAGs or parallel phases (linear-with-skip only)
- Re-hashing artifacts at gate evaluation time
- Materialized token totals (query-time aggregation only)
- Phase chain presets (`--preset=clavain`) — OS can provide this via wrapper scripts

## Dependencies

- Schema at v5 (current state)
- `internal/phase/`, `internal/runtrack/`, `internal/dispatch/`, `internal/event/` packages (all exist)
- No external dependencies

## Open Questions

1. **Budget event dedup mechanism:** State store key per run, or a flag column on the runs table? State store is more general; column is simpler to query.
2. **Gate rules with custom chains:** Should `--gate-rules` be required when `--phases` is provided? Or default to no gates (gate-free runs)?
3. **Token tracking for sub-dispatches:** Independent per-dispatch (aggregation handles rollup at run level). Confirm this doesn't create blind spots.
