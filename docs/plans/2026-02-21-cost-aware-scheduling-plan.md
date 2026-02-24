# Implementation Plan: Cost-Aware Agent Scheduling

**Bead:** iv-suzr | **IC Run:** er6dgx4p | **Phase:** executing

## Step 1: Schema Migration v11→v12

**Files:** `internal/db/db.go`, `internal/db/schema.sql`

1. Add to `schema.sql`:
   - `budget_enforce INTEGER DEFAULT 0` on `runs`
   - `max_agents INTEGER DEFAULT 0` on `runs`
   - `spawn_depth INTEGER DEFAULT 0` on `dispatches`
   - `parent_dispatch_id TEXT DEFAULT ''` on `dispatches`

2. Add migration block in `db.go`:
   - `currentVersion >= 3 && currentVersion < 12` for runs columns
   - `currentVersion >= 2 && currentVersion < 12` for dispatches columns
   - Bump `currentSchemaVersion` and `maxSchemaVersion` to 12

3. Update the `schemaDDL` (schema.sql) to include new columns for fresh installs.

## Step 2: Update Phase/Run Struct + Store

**Files:** `internal/phase/phase.go`, `internal/phase/store.go`, `internal/phase/tx_queriers.go`

1. Add `BudgetEnforce bool` and `MaxAgents int` to `Run` struct
2. Update `Create()` INSERT to include new columns
3. Update all `scanRun()` paths to read new columns
4. Update `CreatePortfolio()` and `CreateChild()` similarly

## Step 3: Update Dispatch Struct + Store

**Files:** `internal/dispatch/dispatch.go`, `internal/dispatch/spawn.go`

1. Add `SpawnDepth int` and `ParentDispatchID string` to `Dispatch` struct
2. Update `Create()` INSERT to include new columns
3. Update all scan paths to read new columns
4. Add `CountActiveByScope(ctx, scopeID) (int, error)` — count active dispatches for a scope (run)
5. Add `CountActiveGlobal(ctx) (int, error)` — count all active dispatches
6. Add `CountTotalByScope(ctx, scopeID) (int, error)` — count total dispatches for a run

## Step 4: Spawn Policy + Enforcement

**Files:** `internal/dispatch/policy.go` (new)

1. Define `SpawnPolicy` struct
2. Define `SpawnRejection` error type
3. `CheckPolicy(ctx, store, phaseStore, stateStore, policy, dispatch) error`
   - Budget enforcement: if run has budget_enforce, check budget exceeded
   - Per-run concurrency: count active dispatches for scope, compare to max_dispatches
   - Global concurrency: count all active dispatches, compare to state key
   - Agent cap: count total dispatches for scope, compare to max_agents
   - Spawn depth: if parent_dispatch_id set, get parent depth + 1, compare to max

## Step 5: Wire Policy into Spawn Path

**Files:** `internal/dispatch/spawn.go`, `cmd/ic/dispatch.go`

1. `Spawn()` accepts optional `SpawnPolicy` parameter
2. Before `store.Create()`, call `CheckPolicy()`
3. CLI reads policy from run config + global state + flags
4. New CLI flags: `--budget-enforce`, `--max-agents`, `--parent-dispatch`
5. CLI `ic run create` accepts `--budget-enforce`, `--max-agents`

## Step 6: Budget Gate Type

**Files:** `internal/phase/gate.go`, `internal/phase/machine.go`

1. Add `budget` gate type to `evaluateGate()`
2. When run has `budget_enforce=true`, inject budget gate into gate evaluation
3. Budget gate fails if budget is exceeded
4. Gate evidence includes current/total token counts

## Step 7: Global Config Commands

**Files:** `cmd/ic/main.go` or new `cmd/ic/config.go`

1. `ic config set max-global-dispatches N` — writes to state key `_kernel/global_max_dispatches`
2. `ic config set max-spawn-depth N` — writes to state key `_kernel/max_spawn_depth`
3. `ic config get <key>` — reads from state
4. `ic config list` — list all `_kernel/*` state keys

## Step 8: Integration Tests

**File:** `test-integration.sh`

1. Budget enforcement: create run with budget, report tokens past budget, try spawn → rejected
2. Per-run concurrency: create run with max_dispatches=1, spawn 1 → ok, spawn 2 → rejected
3. Global concurrency: set global max, spawn up to limit → ok, one more → rejected
4. Agent caps: create run with max_agents=2, spawn 2 → ok, spawn 3 → rejected
5. Spawn depth: set max depth=1, spawn with parent → ok, spawn child of child → rejected
6. Budget gate: create run with budget_enforce, exceed budget, try advance → blocked
7. Gate override: budget gate blocked, override → advances

## Step 9: Update CLI Help + AGENTS.md

1. Update `ic dispatch spawn --help` with new flags
2. Update `ic run create --help` with `--budget-enforce`, `--max-agents`
3. Update AGENTS.md Resource Management section
4. Update CLAUDE.md quick reference
