# Strategy: Cost-Aware Agent Scheduling

**Bead:** iv-suzr | **IC Run:** er6dgx4p | **Phase:** strategized

## Decision: Ship F1-F4, Defer F5-F6

From the brainstorm, 6 features were identified. The strategy is to ship the **enforcement core** (F1-F4) in this sprint and defer the cost model and enhanced observability to a follow-up.

### Sprint Scope (F1-F4)

| Feature | What | Complexity | Why Now |
|---------|------|-----------|---------|
| F1: Budget Enforcement | Spawn rejection + budget gate | Low | Core value — budget limits without enforcement are suggestions |
| F2: Per-Run Concurrency | `max_dispatches` for individual runs | Low | Column already exists, just needs enforcement |
| F3: Global Concurrency | System-wide dispatch cap | Low | Prevents resource exhaustion in multi-run scenarios |
| F4: Agent Caps | Max agents per run + spawn depth | Medium | Safety invariant — prevents recursive agent storms |

### Deferred (Separate Sprint)

| Feature | Why Deferred |
|---------|-------------|
| F5: Cost Model | Medium complexity, `model` column already exists but cost multiplier config + multiplication logic is a separate concern. Ship budget enforcement first, add cost-awareness later. |
| F6: Enhanced Observability | Nice-to-have polish. Current `ic run budget` already shows threshold status. Enhanced output can come after enforcement works. |

## Architecture: Store-Layer Enforcement

Enforcement goes in the store layer, not the CLI. This is a kernel invariant — like gate checks, it can't be bypassed by a different CLI caller.

### Spawn Policy

```go
// SpawnPolicy configures pre-spawn enforcement checks.
// Zero values mean "unlimited" (no constraint).
type SpawnPolicy struct {
    BudgetEnforce   bool   // reject spawn if run budget exceeded
    MaxActivePerRun int    // max concurrent active dispatches per run
    MaxActiveGlobal int    // max concurrent active dispatches across all runs
    MaxAgentsPerRun int    // max total dispatches ever spawned for this run
    MaxSpawnDepth   int    // max parent→child dispatch nesting depth
}
```

### Where Policy Comes From

1. `BudgetEnforce` — from `runs.budget_enforce` column (set at `ic run create`)
2. `MaxActivePerRun` — from `runs.max_dispatches` column (already exists)
3. `MaxActiveGlobal` — from state key `_kernel/global_max_dispatches`
4. `MaxAgentsPerRun` — from `runs.max_agents` column (new)
5. `MaxSpawnDepth` — from state key `_kernel/max_spawn_depth`

### Schema Changes (v12)

```sql
ALTER TABLE runs ADD COLUMN budget_enforce INTEGER DEFAULT 0;
ALTER TABLE runs ADD COLUMN max_agents INTEGER DEFAULT 0;

ALTER TABLE dispatches ADD COLUMN spawn_depth INTEGER DEFAULT 0;
ALTER TABLE dispatches ADD COLUMN parent_dispatch_id TEXT DEFAULT '';
```

Minimal migration — 4 columns, all with safe defaults.

### Error Model

Spawn rejection returns a structured error:

```go
type SpawnRejection struct {
    Reason    string // "budget_exceeded", "concurrency_limit", "agent_cap", "depth_limit"
    RunID     string
    Current   int64  // current value (tokens used, active count, etc.)
    Limit     int64  // configured limit
}
```

CLI exit code 1 with JSON on stderr. Events emitted for rejected spawns.

### Budget Gate Type

New gate type `budget` for phase advancement:

```go
// In gate evaluation: if run has budget_enforce=true and budget exceeded,
// the gate fails with reason "budget exceeded: N/M tokens"
case "budget":
    if !run.BudgetEnforce { return true }
    result, _ := budgetChecker.Check(ctx, runID)
    return result == nil || !result.Exceeded
```

This blocks `ic run advance` when the budget is blown, forcing human intervention (`ic gate override`).

## Delivery Order

1. **Schema migration (v12)** — add columns, update store scan lists
2. **F2: Per-run concurrency** — simplest, column already exists, just add count check in spawn path
3. **F1: Budget enforcement** — change warn→reject, add `budget_enforce` flag, add budget gate type
4. **F4: Agent caps** — parent dispatch tracking, depth calculation, cap enforcement
5. **F3: Global concurrency** — state-based config, global count query
6. **Integration tests** — end-to-end: spawn rejection, gate blocking, override
7. **CLI flags** — `--budget-enforce`, `--max-agents`, `--parent-dispatch`, `--max-spawn-depth`

## Risks & Mitigations

| Risk | Mitigation |
|------|-----------|
| Break existing workflows | All new constraints default to 0/false (unlimited/off) |
| TOCTOU on concurrency counts | Documented and accepted (same as portfolio `max_dispatches`) |
| Budget gate too strict | `ic gate override` provides escape hatch |
| Spawn depth tracking wrong | Depth = parent.depth + 1, default 0 for root dispatches |
