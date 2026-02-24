# Brainstorm: Cost-Aware Agent Scheduling with Token Budgets

**Bead:** iv-suzr
**IC Run:** tkjd6vhn
**Date:** 2026-02-21
**Phase:** brainstorm

---

## Problem Statement

Intercore has token budget primitives (E1 shipped): per-run `token_budget`, `budget_warn_pct`, budget threshold events (`budget.warning`, `budget.exceeded`), and a `Budget` struct with multi-dimensional limits (tokens, time, phases). Portfolio runs have `max_dispatches` for concurrency limits.

**What's missing:** The kernel *tracks and warns* but doesn't *enforce or schedule*. Today:

1. **Budget exceeded = log line, not gate.** `dispatch.go` checks budget at spawn time but only prints to stderr. A run that's blown its budget can still spawn agents.
2. **No per-run concurrency limits.** `max_dispatches` exists on portfolio runs but individual runs have no dispatch cap. A single run can spawn unlimited parallel agents.
3. **No global concurrency limits.** Nothing prevents 50 simultaneous dispatches across all runs from saturating the system.
4. **No agent caps.** No spawn-depth limit, no fan-out limit, no max-agents-per-run. Recursive sub-agent spawning is unchecked.
5. **No model-cost awareness.** The kernel tracks token counts but doesn't distinguish opus from haiku. A 100k token haiku run costs ~$2.50, a 100k opus run costs ~$30. Budget in tokens ≠ budget in dollars.
6. **No scheduling policy.** When multiple runs compete for dispatch slots, there's no priority mechanism. First-come-first-served is the only policy.

## What Already Exists

### Budget Infrastructure (E1)
- `internal/budget/budget.go` — `Checker` with dedup via state store flags
- `internal/budget/composition.go` — `Budget` struct with `Meet()` (tropical semiring min) and `Exceeded()`
- `budget.warning` and `budget.exceeded` events emitted through the event bus
- Per-run `token_budget` and `budget_warn_pct` columns in `runs` table

### Dispatch Token Tracking
- `ic dispatch tokens <id> --set --in=N --out=N --cache=N` — agents self-report
- `dispatch.Store.AggregateTokens(runID)` — sum across dispatches for a run
- Budget check in `cmd/ic/dispatch.go` at spawn time (warn-only)

### Portfolio Concurrency
- `max_dispatches` column on `runs` table (used by portfolio runs)
- Portfolio relay maintains `active-dispatch-count` in state table
- Advisory check at spawn time (TOCTOU-vulnerable, documented)

### Multi-dimensional Budget (composition.go)
- `Budget{TokenLimit, TimeLimitSec, PhaseCount}` with `Meet()` and `Exceeded()`
- Currently unused in enforcement paths — exists as a composition primitive

## Scope Decision: What This Epic Should Cover

### In Scope
1. **Budget enforcement at spawn time** — reject `ic dispatch spawn` when budget is exceeded
2. **Per-run concurrency limits** — `max_dispatches` for individual runs (not just portfolios)
3. **Global concurrency limit** — system-wide cap on active dispatches
4. **Agent caps** — max agents per run, max spawn depth
5. **Budget gate** — budget as a gate check (block phase advancement when budget exceeded)
6. **Cost model** — optional per-model cost multipliers so budget can be in "cost units" not raw tokens

### Out of Scope (Future)
- Lane-based scheduling (named concurrency lanes with per-lane limits) — more complex, separate epic
- Billing API reconciliation (cross-referencing with Anthropic billing) — OS concern
- Model selection policy (auto-downgrade to cheaper model) — OS concern
- Time-based budgets (wall-clock limits) — low value, agents already have timeouts
- Priority-based scheduling / preemption — requires queueing, significant complexity

## Design Options

### Option A: Enforcement at CLI Layer (Simple)

Add checks in `cmd/ic/dispatch.go` before spawning:

```
ic dispatch spawn → check budget → check per-run concurrency → check global concurrency → check agent caps → spawn or reject
```

**Pros:** Minimal code change. CLI is the only spawn path. No schema change needed.
**Cons:** Enforcement is in the CLI, not the store. A Go library caller could bypass it. Not transactional with the spawn.

### Option B: Enforcement at Store Layer (Correct)

Add pre-spawn validation in `dispatch.Store.Create()`:

```go
type SpawnPolicy struct {
    MaxActivePerRun    int    // 0 = unlimited
    MaxActiveGlobal    int    // 0 = unlimited
    MaxAgentsPerRun    int    // total ever spawned, not just active
    MaxSpawnDepth      int    // 0 = unlimited
    BudgetEnforce      bool   // reject if budget exceeded
}
```

Store counts active dispatches, checks budget, all in one transaction.

**Pros:** Correct enforcement. Transactional. Can't be bypassed.
**Cons:** `dispatch.Store` needs access to `phase.Store` (for budget) and itself (for counts). Adds coupling.

### Option C: Pre-Spawn Hook (Extensible)

A `PreSpawnCheck` interface that the CLI wires up:

```go
type PreSpawnCheck interface {
    Check(ctx context.Context, dispatch *Dispatch, run *Run) error
}
```

Budget checker, concurrency limiter, agent cap checker each implement this interface. CLI chains them.

**Pros:** Clean separation. Each check is independently testable. Easy to add new checks.
**Cons:** More ceremony than needed for 3-4 checks. Interface may be premature.

### Recommendation: Option B with Option C's testability

Put enforcement in the store layer where it's transactional and can't be bypassed, but structure the checks as composable functions (not a formal interface) so they're independently testable. The CLI wires policy from config/flags to the store.

## Feature Breakdown

### F1: Budget Enforcement Gate
- Budget check at spawn time becomes **blocking** (exit 1, no spawn) instead of warn-only
- New `budget` gate type for phase advancement: run can't advance if budget exceeded
- CLI flag `--budget-enforce` on `ic run create` (default: false for backward compat)
- When budget exceeded and enforce=true: `ic dispatch spawn` returns exit 1 with structured JSON error

### F2: Per-Run Concurrency Limits
- `max_dispatches` already exists on `runs` table but only used for portfolios
- Enable it for individual runs: `ic run create --max-dispatches=3`
- Count active dispatches (status=active) for the run at spawn time
- Reject spawn if count >= max_dispatches

### F3: Global Concurrency Limit
- New state key `_kernel/global_max_dispatches` (configurable via `ic config set`)
- Or simpler: `ic dispatch spawn --global-max=N` reads from a config/state
- Count all active dispatches across all runs at spawn time
- Reject if at global limit

### F4: Agent Caps
- `max_agents_per_run` column on `runs` table — total agents ever spawned for this run
- `max_spawn_depth` column on `dispatches` table — tracks nesting depth
- `ic dispatch spawn --parent=<dispatch-id>` increments depth from parent
- Reject if depth > max_spawn_depth or total agents > max_agents_per_run

### F5: Cost Model
- `ic config set cost-model '{"opus":15,"sonnet":3,"haiku":0.25}'` — cost per 1M tokens
- `dispatches` table gains `model` column (already has it? check)
- Budget check multiplies tokens by model cost factor
- Enables "spend no more than $10 on this run" instead of "use no more than 1M tokens"

### F6: CLI & Observability
- `ic run budget <id> --json` enhanced with enforcement status, remaining budget, active dispatch count
- `ic dispatch list --active --run=<id>` for quick concurrency check
- Budget enforcement events in event bus (spawn rejected, budget gate blocked)

## Schema Changes (v12)

```sql
-- New columns on runs table
ALTER TABLE runs ADD COLUMN budget_enforce INTEGER DEFAULT 0;
ALTER TABLE runs ADD COLUMN max_agents INTEGER DEFAULT 0;  -- 0 = unlimited

-- New columns on dispatches table
ALTER TABLE dispatches ADD COLUMN spawn_depth INTEGER DEFAULT 0;
ALTER TABLE dispatches ADD COLUMN parent_dispatch_id TEXT DEFAULT '';
ALTER TABLE dispatches ADD COLUMN model TEXT DEFAULT '';

-- Global config (reuse state table)
-- key: '_kernel/global_max_dispatches', scope: 'global'
```

## Complexity Assessment

- **F1 (Budget enforcement):** Low — change existing check from warn to reject, add gate type
- **F2 (Per-run concurrency):** Low — `max_dispatches` column already exists, add count check
- **F3 (Global concurrency):** Low — single count query + state/config lookup
- **F4 (Agent caps):** Medium — new columns, parent-child dispatch tracking, depth calculation
- **F5 (Cost model):** Medium — model column, config management, cost multiplication
- **F6 (CLI & Observability):** Low — enhance existing commands

**Total estimate:** ~3 sessions, complexity 3

## Open Questions

1. **Backward compatibility:** Should `budget_enforce` default to false (safe) or true (secure)? False seems right — existing runs shouldn't suddenly start rejecting spawns.
2. **TOCTOU on concurrency checks:** The count-then-spawn is inherently racy. Accept advisory enforcement (document it) or use `ic lock` to serialize spawns? Lock serialization would hurt throughput.
3. **Cost model storage:** Config file vs state table? Config file is simpler and doesn't need DB. State table is more flexible and queryable.
4. **Model column on dispatches:** Already exists (`model TEXT` in schema.sql). No schema change needed for cost model lookups.
5. **Should budget gate block advancement or just warn?** Phase advancement past an exceeded budget could be a hard gate or a soft gate (emits event but allows advance with override). Hard gate with `ic gate override` seems right.

## Risks

- **Over-engineering:** Lane-based scheduling, priority queues, preemption — tempting but not needed yet. Keep it simple: hard limits, count checks, budget enforcement.
- **Breaking existing workflows:** Anything that defaults to "enforce" could break existing sprint runs that don't set budgets. Must default to permissive.
- **TOCTOU in concurrency checks:** Documented and accepted. The alternative (lock serialization) is worse for throughput. Portfolio `max_dispatches` already has this documented trade-off.
