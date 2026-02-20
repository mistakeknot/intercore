# Intercore E1: Kernel Primitives Completion — Brainstorm
**Bead:** iv-som2
**Phase:** brainstorm (as of 2026-02-19T15:54:01Z)
**Date:** 2026-02-19
**Goal:** Complete the missing kernel-level primitives that all subsequent epics depend on (phase chains, tokens, skip, hash).

---

## What We're Building

Six kernel primitives that unblock E2 (React), E3 (Hook Cutover), and everything downstream. These fill the gap between the current Level 0+1 kernel (~40% of vision scope) and the v1.5 milestone.

### Primitive 1: Configurable Phase Chains

**Current state:** Phase chain is hardcoded as 8 Clavain-specific phases in `internal/phase/phase.go` (`brainstorm → brainstorm-reviewed → strategized → planned → executing → review → polish → done`). Transition table, valid transitions graph, and complexity whitelists are all compile-time constants.

**What changes:**
- `ic run create --phases='["brainstorm","plan","execute","ship"]'` accepts an arbitrary ordered array
- Chain stored as a `phases TEXT NOT NULL` JSON column on the `runs` table
- Transition validation derived from the chain at runtime (walking forward), not from a static map
- Gate rules supplied alongside the chain at create time (run config snapshot)
- Existing runs in DB default to current 8-phase chain via schema migration
- Complexity whitelists become OS-supplied "required phases" per tier, passed at create time alongside the chain

**Decision:** JSON column (not join table). Simpler schema, one read to get the chain, matches the `--phases='[...]'` CLI syntax.

### Primitive 2: Explicit `ic run skip` Command

**Current state:** Phase skipping is implicit — `NextRequiredPhase()` walks forward, skipping phases not in a hardcoded complexity whitelist. No separate event per skipped phase.

**What changes:**
- `ic run skip <id> <phase> --reason="..." --actor="clavain"` as a standalone command
- Validates: phase exists in chain, not already completed, run is on or before this phase
- Records a `phase_events` row with `event_type=skip`, reason, and actor
- `ic run advance` becomes simpler — only moves to the immediate next non-skipped phase. No auto-skipping logic in the kernel.
- All skip-decision heuristics (complexity whitelists) move to the OS layer. The kernel provides the mechanism; the OS decides which phases to skip.

**Decision:** Standalone command, not auto-skip in Advance. Clean mechanism/policy separation.

### Primitive 3: Artifact Content Hashing + dispatch_id

**Current state:** `run_artifacts` table has `path` and `type` columns. No content hash, no link to which dispatch produced the artifact.

**What changes:**
- `content_hash TEXT` column (SHA256, computed at `ic run artifact add` time)
- `dispatch_id TEXT` column (nullable FK to dispatches — artifacts can be manually added)
- `ic run artifact add` reads the file, computes SHA256, stores alongside the record
- Schema migration v5→v6 adds both columns (nullable for existing records)

**Decision:** Hash is provenance metadata, not gate enforcement. `artifact_exists` gate checks DB record existence only — does NOT re-hash at evaluation time.

### Primitive 4: Token Tracking on Dispatches

**Current state:** No token columns on dispatches table.

**What changes:**
- `tokens_in INTEGER`, `tokens_out INTEGER`, `cache_hits INTEGER` columns on `dispatches` (all nullable — NULL means "not reported", 0 means "reported as zero")
- Two reporting paths:
  - At completion: `ic dispatch complete <id> --tokens-in=X --tokens-out=Y --cache-hits=Z`
  - Standalone: `ic dispatch tokens <id> --set --in=X --out=Y --cache=Z` (for incremental mid-dispatch updates)
- Schema migration adds columns (nullable, no default — existing dispatches get NULL)

**Decision:** Nullable columns. Both completion-time and standalone reporting. Kernel records raw counts; cost-per-token computation is OS policy.

### Primitive 5: Token Aggregation + `ic run tokens`

**Current state:** No aggregation queries.

**What changes:**
- `ic run tokens <id>` computes and displays:
  - Per-phase subtotals (SUM tokens grouped by phase of associated run_agents)
  - Run total (SUM across all dispatches in the run)
  - Cache hit ratio (total cache_hits / total tokens_in)
- Query-time computation (no materialized totals)
- `--json` flag for programmatic consumption
- Per-project aggregation: `ic run tokens --project=.` sums across all runs for a project path

**Decision:** Query-time aggregation. Always correct, no consistency issues. Performance is fine at single-project scale.

### Primitive 6: Budget Threshold Events

**Current state:** No budget tracking or events.

**What changes:**
- `token_budget INTEGER` and `budget_warn_pct INTEGER DEFAULT 80` columns on `runs` (nullable — budget enforcement is opt-in)
- `ic run create --token-budget=100000 --budget-warn-pct=80` sets thresholds
- When `ic dispatch tokens` or `ic dispatch complete` reports tokens, the kernel checks cumulative run tokens against the budget:
  - If cumulative >= budget * warn_pct/100: emit `budget.warning` event (once per threshold crossing, tracked via state store to avoid duplicate events)
  - If cumulative >= budget: emit `budget.exceeded` event
- Events are informational — the kernel does NOT kill dispatches or block advancement. The OS decides the response.
- Event payload includes: run_id, current_total, budget, threshold_pct, dispatch_id that triggered

**Decision:** Run-level flags. Budget is part of the run's contract, set at creation time.

---

## Why This Approach

1. **Mechanism/policy separation:** Every primitive pushes decision-making to the OS. The kernel provides skip, the OS decides when to skip. The kernel tracks tokens, the OS computes cost. The kernel emits budget events, the OS decides the response.

2. **Minimal schema changes:** One migration (v5→v6) covers all six primitives. Two columns on `run_artifacts`, three on `dispatches`, one JSON column + two budget columns on `runs`.

3. **Backward compatible:** All new columns are nullable. Existing runs, dispatches, and artifacts continue working. The hardcoded 8-phase chain becomes the default when `phases` is NULL.

4. **Incremental value:** Each primitive is independently useful. Phase chains unlock non-Clavain workflows. Token tracking enables cost visibility. Skip auditing improves observability. They don't need to ship together.

## Key Decisions

| # | Decision | Choice | Alternative Considered |
|---|----------|--------|----------------------|
| 1 | Phase chain storage | JSON column on runs | Join table (more normalized, more complex) |
| 2 | Skip model | Standalone `ic run skip` command | Auto-skip in Advance with events |
| 3 | Complexity whitelist | OS-supplied required-phases per tier | Per-run skip config, or kernel-embedded heuristic |
| 4 | Artifact hash at gate time | Record-only check (no re-hash) | Re-hash verification, configurable per gate |
| 5 | Token reporting | Both completion-time + standalone | Completion-only (simpler, less flexible) |
| 6 | Token columns | Nullable (NULL = not reported) | DEFAULT 0 (simpler queries, loses semantic) |
| 7 | Token aggregation | Query-time SUM | Materialized running totals |
| 8 | Budget thresholds | Run-level columns | State store (more dynamic, less discoverable) |

## Open Questions

1. **Phase chain presets:** Should the kernel provide a `--preset=clavain` that expands to the default 8-phase chain? Or is that purely OS-level?
2. **Budget event dedup:** Using state store to track "warning already emitted" adds a cross-subsystem dependency. Alternative: just emit the event every time tokens are reported above threshold and let consumers dedup.
3. **Token tracking for sub-dispatches:** When a dispatch spawns child dispatches, should the parent's token count include children? Or is each dispatch independent? (Likely: independent, with fan-out queries at the run level.)

## Schema Changes (Preview)

```sql
-- v6 migration
-- runs: add phases, budget columns
ALTER TABLE runs ADD COLUMN phases TEXT;              -- JSON array, NULL = legacy 8-phase
ALTER TABLE runs ADD COLUMN token_budget INTEGER;     -- NULL = no budget
ALTER TABLE runs ADD COLUMN budget_warn_pct INTEGER DEFAULT 80;

-- dispatches: add token columns
ALTER TABLE dispatches ADD COLUMN tokens_in INTEGER;
ALTER TABLE dispatches ADD COLUMN tokens_out INTEGER;
ALTER TABLE dispatches ADD COLUMN cache_hits INTEGER;

-- run_artifacts: add hash and dispatch linkage
ALTER TABLE run_artifacts ADD COLUMN content_hash TEXT;
ALTER TABLE run_artifacts ADD COLUMN dispatch_id TEXT REFERENCES dispatches(id);
```

---

**Sources:**
- `infra/intercore/docs/product/intercore-vision.md` (v1.5)
- `infra/intercore/docs/prds/2026-02-19-intercore-vision-roadmap.md`
- `infra/intercore/docs/brainstorms/2026-02-19-intercore-epic-decomposition-brainstorm.md`
- `infra/intercore/docs/research/scan-intercore-codebase-for-epic-context.md`
- `infra/intercore/internal/phase/phase.go` (current hardcoded chain)
