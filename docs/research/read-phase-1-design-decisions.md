# Phase 1 Architecture: Extension Points for Phase 2 (Event Bus, Policy Engine, TUI)

Date: 2026-02-18
Source files analyzed:
- `/root/projects/Interverse/infra/intercore/internal/phase/machine.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/phase.go`
- `/root/projects/Interverse/infra/intercore/internal/db/schema.sql`

---

## 1. machine.go — Gate Stub Framework Extension Points

### Key Types for Phase 2

```go
// GateConfig — the primary knob for gate policy. Phase 2 policy engine plugs in here.
type GateConfig struct {
    Priority   int    // 0-1 = hard, 2-3 = soft, 4+ = none (tier derivation)
    DisableAll bool   // bypass all gate logic
    SkipReason string // explicit override with audit reason
}

// AdvanceResult — the return value of Advance(). TUI reads this to decide what to render.
type AdvanceResult struct {
    FromPhase  string
    ToPhase    string
    EventType  string  // one of: advance, skip, pause, block, cancel, set
    GateResult string  // pass, fail, warn, none
    GateTier   string  // hard, soft, none
    Reason     string
    Advanced   bool
}
```

### Primary Extension Point: `evaluateGate`

```go
func evaluateGate(cfg GateConfig, from, to string) (result, tier string)
```

This is the stub. The comment block inside it enumerates exactly what Phase 2 must implement:

- Does a brainstorm artifact exist? (brainstorm -> brainstorm-reviewed)
- Is there a strategy doc? (brainstorm-reviewed -> strategized)
- Has a plan been written? (strategized -> planned)
- Are all plan tasks done? (executing -> review)
- Has code review happened? (review -> polish)

Phase 2 policy engine replaces or wraps `evaluateGate`. The signature must remain compatible: it takes `(cfg GateConfig, from, to string)` and returns `(result, tier string)`. The constants `GatePass`, `GateFail`, `GateWarn`, `GateNone`, `TierHard`, `TierSoft`, `TierNone` are already defined in phase.go.

### Gate Tier Derivation (current logic)

Tier is derived from `cfg.Priority` inside `evaluateGate`:
- Priority 0-1 -> `TierHard` (blocks advance on GateFail)
- Priority 2-3 -> `TierSoft` (warns but allows advance)
- Priority 4+  -> `TierNone` (no gate at all)

Phase 2 policy engine could either keep this mapping or replace it with per-transition tier definitions. The current design makes tier a function of caller-supplied priority, not of the transition itself. If Phase 2 needs transition-specific tiers (e.g., always hard-block on planned->executing regardless of caller), `evaluateGate` needs to be refactored to accept a policy lookup table.

### `Advance` Function Signature (do not break)

```go
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig) (*AdvanceResult, error)
```

The event bus in Phase 2 should subscribe to `AdvanceResult` values emitted by this function, not try to intercept `Advance` itself. The cleanest integration is a thin wrapper or post-`Advance` hook that publishes `AdvanceResult` to the event bus.

### Errors to Handle in TUI/Policy

- `ErrTerminalRun` — run is completed/cancelled/failed; TUI should disable advance button
- `ErrTerminalPhase` — phase is `done`; same TUI treatment
- Optimistic concurrency error from `store.UpdatePhase` — Phase 2 TUI must handle retry

---

## 2. phase.go — Phase Chain and Complexity Skip Logic

### Phase Constants (the full chain)

```go
const (
    PhaseBrainstorm         = "brainstorm"
    PhaseBrainstormReviewed = "brainstorm-reviewed"
    PhaseStrategized        = "strategized"
    PhasePlanned            = "planned"
    PhaseExecuting          = "executing"
    PhaseReview             = "review"
    PhasePolish             = "polish"
    PhaseDone               = "done"
)
```

8 phases total. TUI must render all 8 as a timeline/stepper, with complexity-aware dimming for skipped phases.

### Run Struct (the unit TUI displays)

```go
type Run struct {
    ID          string
    ProjectDir  string
    Goal        string
    Status      string    // active, completed, cancelled, failed
    Phase       string    // current phase constant
    Complexity  int       // 1-5; controls which phases are required
    ForceFull   bool      // override complexity skip logic
    AutoAdvance bool      // if false, advance pauses before each gate
    CreatedAt   int64
    UpdatedAt   int64
    CompletedAt *int64
    ScopeID     *string
    Metadata    *string   // JSON blob; Phase 2 can extend without schema change
}
```

`Metadata *string` is the schema-free extension point for Phase 2 policy state. Any per-run policy configuration (e.g., custom gate thresholds, policy profile name) can be stored here as JSON without a schema migration.

### PhaseEvent Struct (the audit trail TUI reads)

```go
type PhaseEvent struct {
    ID         int64
    RunID      string
    FromPhase  string
    ToPhase    string
    EventType  string   // advance, skip, pause, block, cancel, set
    GateResult *string  // nullable: pass, fail, warn, none
    GateTier   *string  // nullable: hard, soft, none
    Reason     *string  // nullable: free text
    CreatedAt  int64
}
```

Event bus in Phase 2 can be backed by polling `phase_events` (the table already exists and is append-only). The TUI history panel just queries `phase_events WHERE run_id = ?`.

### Complexity Whitelist (skip logic TUI must replicate)

```go
var complexityWhitelist = map[int]map[string]bool{
    1: { PhaseBrainstorm, PhasePlanned, PhaseExecuting, PhaseDone },       // 4 phases
    2: { PhaseBrainstorm, PhaseBrainstormReviewed, PhasePlanned, PhaseExecuting, PhaseDone }, // 5 phases
    // 3-5: all 8 phases (no whitelist entry = full lifecycle)
}
```

TUI stepper must call `ShouldSkip(phase, complexity)` or replicate the same logic to know which phases to dim vs. highlight. Do not hardcode — call the existing exported function.

### Transition Validation (policy engine must respect)

```go
var validTransitions = map[string]map[string]bool{ ... }

func IsValidTransition(from, to string) bool
```

`IsValidTransition` is the guard for any forced/manual phase set. Phase 2 policy engine must check this before permitting a manual jump.

### Key Exported Functions Policy Engine Needs

| Function | Purpose |
|---|---|
| `NextPhase(current string) (string, error)` | Single-step forward (strict chain) |
| `ShouldSkip(p string, complexity int) bool` | Whether a phase is skipped at given complexity |
| `NextRequiredPhase(current string, complexity int, forceFull bool) string` | The actual next target (skips omitted phases) |
| `IsValidTransition(from, to string) bool` | Gate for manual jumps |
| `IsTerminalPhase(p string) bool` | TUI disable-advance guard |
| `IsTerminalStatus(s string) bool` | TUI run-complete guard |

---

## 3. schema.sql — Tables Phase 2 Needs to Read or Extend

### Tables Phase 2 Reads (no schema changes needed)

**`runs`** — the primary entity the TUI lists and the policy engine evaluates against.
- `status` + `phase` = the two fields TUI polls for live updates
- `auto_advance` = determines whether TUI shows a manual-advance button
- `complexity` + `force_full` = controls which phases TUI dims
- `metadata TEXT` = JSON extension point for Phase 2 policy config (no migration needed)

**`phase_events`** — append-only audit log; TUI history panel reads this.
- `gate_result` + `gate_tier` = what the policy engine decided
- `event_type` = what actually happened (advance/skip/pause/block)
- `reason` = why (policy engine should populate this with machine-readable codes)

**`run_artifacts`** — the table gates should check for artifact presence.
- Keyed by `(run_id, phase, path, type)`
- Phase 2 policy engine gates (e.g., "does a brainstorm artifact exist?") should query: `SELECT 1 FROM run_artifacts WHERE run_id = ? AND phase = ? LIMIT 1`
- Index `idx_run_artifacts_phase` on `(run_id, phase)` already exists — gate queries are O(log n)

**`run_agents`** — track which agents are active in a run.
- `status` filter index already exists for `status = 'active'`
- TUI can show active agents per phase using this table

**`dispatches`** — agent dispatch tracking.
- `verdict_status` + `verdict_summary` = policy engine gate inputs (did review pass?)
- `scope_id` links dispatches to runs via scope
- `parent_id` supports dispatch trees (Phase 2 orchestration)

**`sentinels`** — debounce/rate-limit table for repeated triggers.
- Phase 2 event bus should use this to avoid duplicate gate evaluations when the same condition fires repeatedly

**`state`** — generic key-value with TTL and scope.
- Phase 2 policy engine can store transient gate state here (e.g., "gate X was last evaluated at T, result was pass")
- `expires_at` supports self-expiring policy cache entries

### Schema Extension Points for Phase 2 (new tables likely needed)

The current schema has no:
1. **Event bus subscriptions table** — Phase 2 needs a way to register watchers (e.g., "notify when run X advances to executing"). Could be a `subscriptions(id, run_id, trigger_phase, webhook_url, created_at)` table.
2. **Policy profiles table** — If policy engine supports named profiles (e.g., "strict", "relaxed"), a `policy_profiles(name, config_json, created_at)` table would be needed. Alternatively, store profiles in `state` with key `policy::<name>`.
3. **Gate evaluation log** — The `phase_events` table captures gate results but only at transition time. If Phase 2 needs to record pre-flight gate checks (dry-run evaluations), a separate `gate_evals(id, run_id, from_phase, to_phase, result, tier, reason, evaluated_at)` table is appropriate.

### Existing Indexes Relevant to Phase 2

| Index | Table | Used by |
|---|---|---|
| `idx_runs_status` | runs | TUI run list (active only) |
| `idx_phase_events_run` | phase_events | TUI history panel |
| `idx_run_artifacts_phase` | run_artifacts | Policy gate artifact checks |
| `idx_run_agents_run` | run_agents | TUI agent panel |
| `idx_dispatches_status` | dispatches | Policy gate verdict checks |
| `idx_dispatches_scope` | dispatches | Linking dispatches to runs |
| `idx_state_expires` | state | TTL-based policy cache eviction |

---

## Summary of Phase 2 Integration Strategy

### Event Bus

- Simplest approach: polling `phase_events` in a goroutine with `WHERE id > last_seen_id`. No schema change. The `idx_phase_events_run` index makes this efficient.
- `PhaseEvent` struct already carries all event metadata needed for routing (run_id, event_type, from/to phase, gate_result).
- Sentinel table (`sentinels`) can gate event bus debounce without a new table.

### Policy Engine

- Replace `evaluateGate(cfg GateConfig, from, to string) (result, tier string)` with a real implementation. Signature is unchanged.
- Gate checks map directly to `run_artifacts` queries (artifact presence) and `dispatches` queries (verdict presence).
- Policy config per run lives in `runs.metadata` (JSON) — no new table needed for simple cases.
- Tier overrides per transition require either extending `GateConfig` with a `TierOverrides map[string]string` field, or moving tier derivation out of `evaluateGate` into a policy profile lookup.

### TUI

- Primary poll loop: `SELECT status, phase, updated_at FROM runs WHERE id = ?` on interval.
- Phase stepper: iterate `allPhases` (exported slice or replicated), call `ShouldSkip(phase, complexity)` to determine rendering for each node.
- History panel: `SELECT * FROM phase_events WHERE run_id = ? ORDER BY id ASC`.
- Gate status: last `phase_events` row for the run, read `gate_result` and `gate_tier`.
- Auto-advance toggle: `ic run set <id> --auto-advance=false` (shells out to CLI per CLAUDE.md design: "CLI only, no Go library API in v1").
- Manual advance button: disabled when `IsTerminalPhase(phase) || IsTerminalStatus(status)`.
