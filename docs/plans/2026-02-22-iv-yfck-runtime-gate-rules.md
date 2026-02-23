# Plan: Runtime-Configurable Gate Rules (iv-yfck)
**Bead:** iv-yfck
**Phase:** shipping (as of 2026-02-23)
**Status:** All 7 tasks complete. Integration tests added and passing.

## Goal

Move gate rules from the hardcoded `gateRules` map to per-run runtime configuration. Runs supply custom gate rules at creation time via `--gates` or `--gates-file`. Runs without custom gates continue using the hardcoded defaults. Zero behavioral change for existing users.

## Architecture

**Current flow:**
```
ic gate check <run_id>  →  load spec rules from state table  →  evaluateGate(cfg.SpecRules)
                                                                    ↓
                                                          SpecRules > hardcoded gateRules map
```

**Target flow:**
```
ic run create --gates=JSON  →  store gate_rules in runs table
ic gate check <run_id>      →  load run.GateRules  →  evaluateGate()
                                                        ↓
                                              run.GateRules > cfg.SpecRules > hardcoded gateRules map
```

**Precedence order (highest to lowest):**
1. Per-run stored gate rules (`runs.gate_rules` column)
2. Agency spec rules (`cfg.SpecRules` from state table)
3. Hardcoded defaults (`gateRules` map in `gate.go`)
4. Injected rules (portfolio, upstream, budget) — always appended regardless

## JSON Schema

Gate rules are keyed by transition pair `"from→to"`, each mapping to an array of checks:

```json
{
  "brainstorm→brainstorm-reviewed": [
    {"check": "artifact_exists", "phase": "brainstorm", "tier": "hard"}
  ],
  "planned→executing": [
    {"check": "artifact_exists", "phase": "planned", "tier": "hard"}
  ]
}
```

Valid check types: `artifact_exists`, `agents_complete`, `verdict_exists`, `budget_not_exceeded`.
Valid tiers: `hard`, `soft` (empty = inherit from `cfg.Priority`).

This matches the existing `gateRule` struct shape and the `SpecGateRule` type.

## Tasks

### Task 1: Add `gate_rules` column to schema + migration

**Files:**
- `internal/db/schema.sql` — add `gate_rules TEXT` to `runs` table definition
- `internal/db/db.go` — add v15→v16 migration with `ALTER TABLE runs ADD COLUMN gate_rules TEXT`
- `internal/db/db.go` — bump `currentSchemaVersion` from 15 to 16

**Details:**
- Column is nullable TEXT (JSON). NULL = use defaults.
- Migration guard: `if currentVersion >= 3 && currentVersion < 16`
- Use existing `isDuplicateColumnError` pattern for idempotency.

**Tests:** Existing migration tests cover the pattern. Add a v15→v16 migration test in `db_test.go`.

### Task 2: Add `GateRules` field to `Run` struct + store CRUD

**Files:**
- `internal/phase/phase.go` — add `GateRules` field to `Run` struct
- `internal/phase/store.go` — update `runCols`, `Create()`, `Get()`, `queryRuns()`, `scanRun()` (if exists)
- `internal/phase/tx_queriers.go` — update `GetQ()` to scan `gate_rules`

**Details:**
- `GateRules` type: `map[string][]SpecGateRule` (keyed by `"from→to"` string)
- In `Create()`: marshal `GateRules` to JSON if non-nil, store as nullable TEXT
- In `Get()`/`GetQ()`/`queryRuns()`: scan nullable string, unmarshal to map
- Add `runCols` entry: `gate_rules` after `max_agents`
- Helper: `parseGateRulesJSON(sql.NullString) (map[string][]SpecGateRule, error)` — mirrors `parsePhasesJSON`
- Helper: `ParseGateRules(jsonStr string) (map[string][]SpecGateRule, error)` — exported, validates check types and tier values

**Tests:** Unit test `ParseGateRules` with valid/invalid JSON, unknown check types, empty map.

### Task 3: Update gate evaluation to use per-run rules

**Files:**
- `internal/phase/gate.go` — modify `evaluateGate()` rule lookup to check `run.GateRules` first

**Details:**
- In rule collection (lines 141-148), add a new precedence level:
  ```go
  if run.GateRules != nil {
      key := from + "→" + to
      if rr, ok := run.GateRules[key]; ok {
          for _, r := range rr {
              rules = append(rules, gateRule{check: r.Check, phase: r.Phase, tier: r.Tier})
          }
      }
  } else if len(cfg.SpecRules) > 0 {
      // existing spec rules path
  } else if hr, ok := gateRules[[2]string{from, to}]; ok {
      // existing hardcoded path
  }
  ```
- Portfolio, upstream, and budget rules continue to be injected unconditionally after.

**Tests:** Add test in `gate_test.go`:
- Run with `GateRules` set → per-run rules used, hardcoded ignored
- Run with `GateRules` set + `SpecRules` in config → per-run rules win
- Run without `GateRules` → hardcoded defaults still work (regression)
- Per-run rules with tier override → tier escalation works

### Task 4: Add `--gates` and `--gates-file` to `ic run create`

**Files:**
- `cmd/ic/run.go` — add flag parsing in `cmdRunCreate()`

**Details:**
- `--gates=JSON` — inline JSON string, parsed with `ParseGateRules()`
- `--gates-file=PATH` — reads file, parses with `ParseGateRules()`
- Mutually exclusive (error if both provided)
- Set `run.GateRules` before `store.Create()`
- Apply to both single-project and portfolio modes (portfolio parent gets gates, children inherit)

**Tests:** Integration test in `test-integration.sh`:
- Create run with `--gates=JSON`, verify stored via `ic run status --json`
- Create run with `--gates-file`, verify stored
- Both flags → error
- Invalid JSON → error with helpful message

### Task 5: Update `ic gate check` to use per-run rules

**Files:**
- `cmd/ic/gate.go` — modify `cmdGateCheck()` to pass per-run rules

**Details:**
- After loading the run (line 78), check `run.GateRules`
- If per-run rules exist, convert to `SpecGateRule` slice for the current transition and pass as `cfg.SpecRules`
- Precedence: per-run rules override agency spec rules (don't load spec rules when per-run rules exist for the transition)
- The `evaluateGate()` change in Task 3 handles this automatically — `ic gate check` just needs to ensure the run is loaded with `GateRules` populated

**Tests:** Integration test: create run with custom gates, advance to a phase, verify `ic gate check` evaluates the custom rules.

### Task 6: Update `ic gate rules` to show per-run rules

**Files:**
- `cmd/ic/gate.go` — modify `cmdGateRules()` to accept optional `--run=ID`

**Details:**
- Without `--run`: show hardcoded defaults (existing behavior)
- With `--run=ID`: load run, show per-run rules if set, otherwise show defaults
- JSON output includes a `"source"` field: `"run"`, `"spec"`, or `"default"`

**Tests:** Integration test: `ic gate rules` (defaults), `ic gate rules --run=<id>` (per-run).

### Task 7: Update `ic run status` JSON to include gate_rules

**Files:**
- `cmd/ic/run.go` — add `gate_rules` to JSON output in `cmdRunStatus()`

**Details:**
- Include `gate_rules` in the `--json` output when non-nil
- Omit when nil (no noise for runs without custom gates)

**Tests:** Integration test: create run with gates, verify `ic run status --json` includes them.

## Execution Order

Tasks 1 → 2 → 3 → 4, 5, 6, 7 (Tasks 4-7 are independent after Task 3)

## Risk Assessment

- **Low risk:** All changes are additive. NULL `gate_rules` = existing behavior. No breaking changes.
- **Migration:** Single `ALTER TABLE ADD COLUMN` — idempotent, fast on any table size.
- **TOCTOU:** Gate rules are loaded from the run inside the same transaction in `Advance()` — no race.
