# Intercore E1: Kernel Primitives Completion — Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Goal:** Complete six kernel primitives in one schema migration: configurable phase chains, explicit skip command, artifact content hashing, token tracking (cache_hits + nullable), aggregation queries, and budget threshold events.

**Architecture:** Schema migration v5→v6 adds columns to `runs`, `dispatches`, and `run_artifacts`. The phase package is refactored to derive transitions from a stored JSON chain instead of compile-time constants. A new `ic run skip` command provides mechanism/policy separation for phase skipping. Token aggregation is query-time (no materialized views). Budget events use the existing dispatch event infrastructure.

**Tech Stack:** Go 1.22, modernc.org/sqlite (pure Go, no CGO), existing intercore CLI patterns.

**Bead:** iv-som2
**Phase:** planned (as of 2026-02-19T16:03:00Z)

---

## Plan Review Amendments (flux-drive 2026-02-19)

The following amendments incorporate P1/P2 findings from the 3-agent flux-drive review. Apply these during execution:

1. **Task 1 — Migration idempotency**: Use `ALTER TABLE ... ADD COLUMN ... IF NOT EXISTS` on every ALTER statement (modernc.org/sqlite bundles SQLite 3.43+ which supports this). Prevents "duplicate column" failure on migration retry after transient disk error.

2. **Task 2 — Naming convention**: Rename `Chain*` functions to match existing verb-first pattern: `IsChainTerminal`, `IsValidChainTransition`, `NextInChain`. Use `slices.Equal` from Go 1.22 stdlib instead of hand-rolled `slicesEqual`.

3. **Task 3 — Terminal phase hardcoding**: Replace ALL uses of `IsTerminalPhase(p)` (which hardcodes `"done"`) with `ChainIsTerminal(chain, p)`. This includes the completion hook in Advance and the terminal check in EvaluateGate. Without this, custom chains never reach `status=completed`.

4. **Task 4 — Skip-walk robustness**: (a) Add filtered `SkippedPhases(ctx, runID)` store method with `WHERE event_type = 'skip'` instead of reading full event log. (b) Validate `fromPhase` exists in chain before entering skip-walk loop. (c) Guard against skipping terminal phase: `if ChainIsTerminal(chain, targetPhase) { return error }`. (d) When Advance walks past pre-skipped phases, record intermediate transition events to preserve audit trail. (e) When all remaining phases are skipped, emit proper transition events before jumping to terminal.

5. **Task 7 — Aggregation NULL semantics**: Return `*int64` for `TotalCache` in `TokenAggregation`. Add `COUNT(CASE WHEN cache_hits IS NOT NULL THEN 1 END)` to the query. Suppress cache ratio line in `ic run tokens` output when no dispatches have reported cache data.

6. **Task 8 — BudgetChecker interfaces**: Define narrow interfaces (`RunBudgetReader`, `TokenAggregator`, `StateReader`) instead of holding concrete `*phase.Store` / `*dispatch.Store` / `*state.Store` pointers. Document that budget event consumers must be idempotent (events may re-emit after DB restore).

---

### Task 1: Schema Migration v5→v6

**Files:**
- Modify: `infra/intercore/internal/db/schema.sql`
- Modify: `infra/intercore/internal/db/db.go:20-23` (bump version constants)
- Test: `infra/intercore/internal/db/db_test.go`

**Step 1: Write the failing test**

Add a migration test that verifies v6 schema has the new columns. Add to `db_test.go`:

```go
func TestMigrateV6Columns(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	d, err := db.Open(dbPath, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != 6 {
		t.Errorf("schema version = %d, want 6", v)
	}

	// Verify new columns exist by inserting rows that use them
	sqlDB := d.SqlDB()

	// runs: phases, token_budget, budget_warn_pct
	_, err = sqlDB.Exec(`INSERT INTO runs (id, project_dir, goal, phases, token_budget, budget_warn_pct)
		VALUES ('test1', '/tmp', 'test', '["a","b"]', 100000, 80)`)
	if err != nil {
		t.Fatalf("runs insert with new columns: %v", err)
	}

	// dispatches: cache_hits (nullable, unlike existing input_tokens/output_tokens)
	_, err = sqlDB.Exec(`INSERT INTO dispatches (id, project_dir, cache_hits)
		VALUES ('d1', '/tmp', 5000)`)
	if err != nil {
		t.Fatalf("dispatches insert with cache_hits: %v", err)
	}

	// run_artifacts: content_hash, dispatch_id
	_, err = sqlDB.Exec(`INSERT INTO runs (id, project_dir, goal) VALUES ('r2', '/tmp', 'test2')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = sqlDB.Exec(`INSERT INTO run_artifacts (id, run_id, phase, path, content_hash, dispatch_id)
		VALUES ('a1', 'r2', 'plan', '/tmp/plan.md', 'sha256:abc123', 'd1')`)
	if err != nil {
		t.Fatalf("run_artifacts insert with new columns: %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/db/ -run TestMigrateV6 -v`
Expected: FAIL — columns don't exist yet, version is 5.

**Step 3: Update schema.sql with new columns**

In `schema.sql`, add after existing column definitions:

For `runs` table, after the `metadata TEXT` line (before the closing paren):
```sql
    phases          TEXT,
    token_budget    INTEGER,
    budget_warn_pct INTEGER DEFAULT 80
```

For `dispatches` table, after `parent_id TEXT`:
```sql
    cache_hits      INTEGER
```

For `run_artifacts` table, after `type TEXT NOT NULL DEFAULT 'file'`:
```sql
    content_hash    TEXT,
    dispatch_id     TEXT
```

Note: `input_tokens` and `output_tokens` already exist on dispatches with DEFAULT 0. They stay as-is — changing them to nullable would break existing code. The semantic "not reported" will use the convention that 0 means not reported when `cache_hits` is also NULL.

In `db.go`, update:
```go
const (
	currentSchemaVersion = 6
	maxSchemaVersion     = 6
)
```

The `Migrate` function uses `CREATE TABLE IF NOT EXISTS` which works for fresh DBs. For existing v5 DBs, add ALTER TABLE statements to the migration path. Since the current migration runs the full schema DDL inside a transaction, adding new columns to CREATE TABLE works for fresh DBs. For existing DBs at v5, we need an explicit migration block.

Add after `currentVersion >= currentSchemaVersion` check but before `schemaDDL` application:

```go
// v5 → v6: add new columns
if currentVersion == 5 {
	v6Stmts := []string{
		"ALTER TABLE runs ADD COLUMN phases TEXT",
		"ALTER TABLE runs ADD COLUMN token_budget INTEGER",
		"ALTER TABLE runs ADD COLUMN budget_warn_pct INTEGER DEFAULT 80",
		"ALTER TABLE dispatches ADD COLUMN cache_hits INTEGER",
		"ALTER TABLE run_artifacts ADD COLUMN content_hash TEXT",
		"ALTER TABLE run_artifacts ADD COLUMN dispatch_id TEXT",
	}
	for _, stmt := range v6Stmts {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("migrate v5→v6: %w", err)
		}
	}
}
```

**Step 4: Run test to verify it passes**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/db/ -run TestMigrateV6 -v`
Expected: PASS

**Step 5: Run full test suite to verify no regressions**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./...`
Expected: All tests pass.

**Step 6: Commit**

```bash
git add infra/intercore/internal/db/schema.sql infra/intercore/internal/db/db.go infra/intercore/internal/db/db_test.go
git commit -m "feat(intercore): schema v6 — phases, cache_hits, artifact hash, budget columns"
```

---

### Task 2: Configurable Phase Chains — Store + Types

**Files:**
- Modify: `infra/intercore/internal/phase/phase.go`
- Modify: `infra/intercore/internal/phase/store.go`
- Test: `infra/intercore/internal/phase/phase_test.go`
- Test: `infra/intercore/internal/phase/store_test.go`

**Step 1: Write failing tests for chain parsing and validation**

Add to `phase_test.go`:

```go
func TestParsePhaseChain(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    []string
		wantErr bool
	}{
		{"valid 3-phase", `["a","b","c"]`, []string{"a", "b", "c"}, false},
		{"valid 8-phase clavain", `["brainstorm","brainstorm-reviewed","strategized","planned","executing","review","polish","done"]`, DefaultPhaseChain, false},
		{"empty array", `[]`, nil, true},
		{"single phase", `["a"]`, nil, true},
		{"invalid json", `not json`, nil, true},
		{"duplicates", `["a","b","a"]`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePhaseChain(tt.json)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePhaseChain() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr && !slicesEqual(got, tt.want) {
				t.Errorf("ParsePhaseChain() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestChainNextPhase(t *testing.T) {
	chain := []string{"draft", "review", "publish", "done"}
	tests := []struct {
		current string
		want    string
		wantErr bool
	}{
		{"draft", "review", false},
		{"review", "publish", false},
		{"publish", "done", false},
		{"done", "", true}, // terminal
		{"nonexistent", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.current, func(t *testing.T) {
			got, err := ChainNextPhase(chain, tt.current)
			if (err != nil) != tt.wantErr {
				t.Errorf("ChainNextPhase() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ChainNextPhase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChainIsValidTransition(t *testing.T) {
	chain := []string{"a", "b", "c", "d"}
	// a→b valid (next), a→c valid (skip), a→d valid (skip), b→a invalid (backward)
	if !ChainIsValidTransition(chain, "a", "b") {
		t.Error("a→b should be valid")
	}
	if !ChainIsValidTransition(chain, "a", "c") {
		t.Error("a→c should be valid (skip)")
	}
	if ChainIsValidTransition(chain, "b", "a") {
		t.Error("b→a should be invalid (backward)")
	}
	if ChainIsValidTransition(chain, "a", "nonexistent") {
		t.Error("a→nonexistent should be invalid")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/phase/ -run "TestParsePhaseChain|TestChainNextPhase|TestChainIsValidTransition" -v`
Expected: FAIL — functions don't exist yet.

**Step 3: Implement chain functions in phase.go**

Add to `phase.go` (keep existing constants for backward compat, add new chain-based functions):

```go
// DefaultPhaseChain is the legacy 8-phase Clavain lifecycle.
// Used when a run has no explicit phases column (NULL in DB).
var DefaultPhaseChain = []string{
	PhaseBrainstorm,
	PhaseBrainstormReviewed,
	PhaseStrategized,
	PhasePlanned,
	PhaseExecuting,
	PhaseReview,
	PhasePolish,
	PhaseDone,
}

// ParsePhaseChain parses and validates a JSON phase chain.
// Returns error if: not valid JSON array, fewer than 2 phases, or contains duplicates.
func ParsePhaseChain(jsonStr string) ([]string, error) {
	var chain []string
	if err := json.Unmarshal([]byte(jsonStr), &chain); err != nil {
		return nil, fmt.Errorf("parse phase chain: %w", err)
	}
	if len(chain) < 2 {
		return nil, fmt.Errorf("parse phase chain: need at least 2 phases, got %d", len(chain))
	}
	seen := make(map[string]bool, len(chain))
	for _, p := range chain {
		if seen[p] {
			return nil, fmt.Errorf("parse phase chain: duplicate phase %q", p)
		}
		seen[p] = true
	}
	return chain, nil
}

// ChainNextPhase returns the next phase in the chain after current.
func ChainNextPhase(chain []string, current string) (string, error) {
	for i, p := range chain {
		if p == current {
			if i+1 >= len(chain) {
				return "", ErrNoTransition
			}
			return chain[i+1], nil
		}
	}
	return "", fmt.Errorf("phase %q not found in chain", current)
}

// ChainIsValidTransition checks if from→to is a forward transition in the chain.
func ChainIsValidTransition(chain []string, from, to string) bool {
	fromIdx := -1
	toIdx := -1
	for i, p := range chain {
		if p == from {
			fromIdx = i
		}
		if p == to {
			toIdx = i
		}
	}
	return fromIdx >= 0 && toIdx > fromIdx
}

// ChainIsTerminal returns true if phase is the last in the chain.
func ChainIsTerminal(chain []string, p string) bool {
	return len(chain) > 0 && chain[len(chain)-1] == p
}

// ChainContains returns true if the chain contains the given phase.
func ChainContains(chain []string, p string) bool {
	for _, cp := range chain {
		if cp == p {
			return true
		}
	}
	return false
}
```

Add `"encoding/json"` to the imports at the top of `phase.go`.

**Step 4: Update Run struct to include Phases**

In `phase.go`, add to the `Run` struct:

```go
type Run struct {
	// ... existing fields ...
	Phases       []string // parsed from JSON; nil = legacy chain
	TokenBudget  *int64
	BudgetWarnPct int
}
```

**Step 5: Update Store to read/write phases**

In `store.go`:

- Update `Create` to include `phases` JSON column:
  - If `r.Phases` is non-nil, marshal to JSON and store
  - If nil, store NULL (legacy chain)
- Update `Get`, `Current`, `queryRuns` to read `phases` column and parse from JSON
- Update `runCols` to include `phases, token_budget, budget_warn_pct`

**Step 6: Run tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/phase/ -v`
Expected: All tests pass.

**Step 7: Commit**

```bash
git add infra/intercore/internal/phase/phase.go infra/intercore/internal/phase/store.go infra/intercore/internal/phase/phase_test.go infra/intercore/internal/phase/store_test.go
git commit -m "feat(intercore): configurable phase chains — parse, validate, store, query"
```

---

### Task 3: Refactor Advance to Use Stored Chain

**Files:**
- Modify: `infra/intercore/internal/phase/machine.go`
- Modify: `infra/intercore/internal/phase/gate.go`
- Test: `infra/intercore/internal/phase/machine_test.go`
- Test: `infra/intercore/internal/phase/gate_test.go`

**Step 1: Write failing test for Advance with custom chain**

Add to `machine_test.go`:

```go
func TestAdvanceCustomChain(t *testing.T) {
	// Create a run with a custom 3-phase chain: "draft" → "review" → "done"
	// Advance should follow the stored chain, not the hardcoded default
	db := setupTestDB(t) // existing helper
	store := phase.New(db)

	run := &phase.Run{
		ProjectDir: "/tmp/test",
		Goal:       "test custom chain",
		Phases:     []string{"draft", "review", "done"},
	}
	id, err := store.Create(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}

	// Initial phase should be first in chain
	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != "draft" {
		t.Errorf("initial phase = %q, want %q", got.Phase, "draft")
	}

	// Advance should go to "review" (second in chain)
	cfg := phase.GateConfig{Priority: 4} // no gates
	result, err := phase.Advance(context.Background(), store, id, cfg, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.ToPhase != "review" {
		t.Errorf("advance to = %q, want %q", result.ToPhase, "review")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/phase/ -run TestAdvanceCustomChain -v`
Expected: FAIL — Create always starts at PhaseBrainstorm.

**Step 3: Refactor Create to use first phase from chain**

In `store.go`, update `Create`:

```go
initialPhase := PhaseBrainstorm // default for legacy
if r.Phases != nil && len(r.Phases) > 0 {
	initialPhase = r.Phases[0]
}
```

Use `initialPhase` instead of hardcoded `PhaseBrainstorm` in the INSERT.

**Step 4: Refactor Advance to use run's stored chain**

In `machine.go`, update `Advance`:

Replace:
```go
toPhase := NextRequiredPhase(fromPhase, run.Complexity, run.ForceFull)
```

With:
```go
chain := run.Phases
if chain == nil {
	chain = DefaultPhaseChain
}
toPhase, err := ChainNextPhase(chain, fromPhase)
if err != nil {
	return nil, err
}
```

Remove the skip-detection logic that uses `NextPhase` — with configurable chains, skip is handled by the explicit `ic run skip` command (Task 4).

Update the terminal phase check:
```go
if ChainIsTerminal(chain, run.Phase) {
	return nil, ErrTerminalPhase
}
```

**Step 5: Refactor gate.go to use stored chain**

Update `evaluateGate` and `EvaluateGate` to use the run's stored chain instead of `NextRequiredPhase`. The `gateRules` map stays but becomes a default — runs with custom chains that have no matching gate rules get no gates (pass by default).

Update `EvaluateGate`:
```go
chain := run.Phases
if chain == nil {
	chain = DefaultPhaseChain
}
toPhase, err := ChainNextPhase(chain, run.Phase)
if err != nil {
	return nil, fmt.Errorf("evaluate gate: %w", err)
}
```

**Step 6: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/phase/ -v`
Expected: All tests pass (existing + new).

**Step 7: Commit**

```bash
git add infra/intercore/internal/phase/machine.go infra/intercore/internal/phase/gate.go infra/intercore/internal/phase/machine_test.go infra/intercore/internal/phase/gate_test.go
git commit -m "feat(intercore): Advance uses stored phase chain, not hardcoded constants"
```

---

### Task 4: Explicit `ic run skip` Command

**Files:**
- Modify: `infra/intercore/internal/phase/store.go` (add SkipPhase method)
- Modify: `infra/intercore/internal/phase/phase.go` (remove ShouldSkip, NextRequiredPhase, complexityWhitelist)
- Modify: `infra/intercore/cmd/ic/run.go` (add skip subcommand)
- Test: `infra/intercore/internal/phase/store_test.go`

**Step 1: Write failing tests for SkipPhase store method**

Add to `store_test.go`:

```go
func TestSkipPhase(t *testing.T) {
	db := setupTestDB(t)
	store := phase.New(db)

	run := &phase.Run{
		ProjectDir: "/tmp/test",
		Goal:       "test skip",
		Phases:     []string{"a", "b", "c", "d"},
	}
	id, err := store.Create(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}

	// Skip phase "b" while at phase "a"
	err = store.SkipPhase(context.Background(), id, "b", "complexity 1", "clavain")
	if err != nil {
		t.Fatalf("SkipPhase: %v", err)
	}

	// Verify event was recorded
	events, err := store.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range events {
		if e.EventType == phase.EventSkip && e.ToPhase == "b" {
			found = true
		}
	}
	if !found {
		t.Error("expected skip event for phase 'b'")
	}
}

func TestSkipPhaseErrors(t *testing.T) {
	db := setupTestDB(t)
	store := phase.New(db)

	run := &phase.Run{
		ProjectDir: "/tmp/test",
		Goal:       "test skip errors",
		Phases:     []string{"a", "b", "c"},
	}
	id, err := store.Create(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}

	// Skip nonexistent phase
	err = store.SkipPhase(context.Background(), id, "nonexistent", "test", "test")
	if err == nil {
		t.Error("expected error for nonexistent phase")
	}

	// Skip phase already passed (current is "a", can't skip "a" itself)
	err = store.SkipPhase(context.Background(), id, "a", "test", "test")
	if err == nil {
		t.Error("expected error for skipping current phase")
	}
}
```

**Step 2: Run tests to verify they fail**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/phase/ -run TestSkipPhase -v`
Expected: FAIL — SkipPhase doesn't exist.

**Step 3: Implement SkipPhase in store.go**

```go
// SkipPhase marks a phase as skipped with an audit trail.
// The phase must exist in the run's chain and must be ahead of the current phase.
func (s *Store) SkipPhase(ctx context.Context, runID, targetPhase, reason, actor string) error {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return err
	}
	if IsTerminalStatus(run.Status) {
		return ErrTerminalRun
	}

	chain := run.Phases
	if chain == nil {
		chain = DefaultPhaseChain
	}

	// Validate target phase exists in chain
	if !ChainContains(chain, targetPhase) {
		return fmt.Errorf("skip: phase %q not in chain", targetPhase)
	}

	// Validate target is ahead of current (can't skip current or past phases)
	if !ChainIsValidTransition(chain, run.Phase, targetPhase) {
		return fmt.Errorf("skip: phase %q is not ahead of current phase %q", targetPhase, run.Phase)
	}

	// Record skip event
	reasonStr := reason
	if actor != "" {
		reasonStr = fmt.Sprintf("actor=%s: %s", actor, reason)
	}

	return s.AddEvent(ctx, &PhaseEvent{
		RunID:     runID,
		FromPhase: run.Phase,
		ToPhase:   targetPhase,
		EventType: EventSkip,
		Reason:    strPtrOrNil(reasonStr),
	})
}
```

Where `strPtrOrNil` already exists in `machine.go`.

**Step 4: Remove legacy skip functions from phase.go**

Remove these functions and variables:
- `complexityWhitelist` map
- `ShouldSkip()` function
- `NextRequiredPhase()` function

Keep the `validTransitions` map for now — it may be removed in a later cleanup, but existing tests reference it.

**Step 5: Add CLI subcommand in run.go**

In `cmdRun` switch, add:
```go
case "skip":
	return cmdRunSkip(ctx, args[1:])
```

Implement `cmdRunSkip`:

```go
func cmdRunSkip(ctx context.Context, args []string) int {
	var reason, actor string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		case strings.HasPrefix(args[i], "--actor="):
			actor = strings.TrimPrefix(args[i], "--actor=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 {
		fmt.Fprintf(os.Stderr, "ic: run skip: usage: ic run skip <id> <phase> --reason=<text> [--actor=<name>]\n")
		return 3
	}
	runID := positional[0]
	targetPhase := positional[1]

	if reason == "" {
		fmt.Fprintf(os.Stderr, "ic: run skip: --reason is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run skip: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	if err := store.SkipPhase(ctx, runID, targetPhase, reason, actor); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run skip: %v\n", err)
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]string{
			"status": "skipped",
			"phase":  targetPhase,
		})
	} else {
		fmt.Printf("skipped: %s\n", targetPhase)
	}
	return 0
}
```

**Step 6: Update Advance to skip over already-skipped phases**

In `machine.go`, after computing `toPhase` from `ChainNextPhase`, check if that phase has a skip event. If so, walk forward until finding a non-skipped phase. This can be done by having Advance check the phase_events table for skip events.

Alternatively (simpler): the OS calls `ic run skip` for each phase to skip, which records events. Then `ic run advance` moves to the next phase in the chain. The OS is responsible for calling advance enough times (or the OS can call advance with `--to=<target>` flag).

For v1, keep it simple: Advance moves to the literal next phase in the chain. The OS calls skip first, then advance, iterating as needed. If the OS wants to jump from "a" to "d" (skipping "b" and "c"), it calls: skip b, skip c, advance (→b fails because...actually skip doesn't advance the pointer).

Better approach: SkipPhase records the event AND the Advance function checks for skip events when determining the next phase. When Advance runs, it walks the chain forward, skipping any phase that has a skip event for this run.

Update `machine.go` to add a method that loads skipped phases:

```go
// skippedPhases returns the set of phases that have been explicitly skipped for a run.
func skippedPhases(ctx context.Context, store *Store, runID string) (map[string]bool, error) {
	events, err := store.Events(ctx, runID)
	if err != nil {
		return nil, err
	}
	skipped := make(map[string]bool)
	for _, e := range events {
		if e.EventType == EventSkip {
			skipped[e.ToPhase] = true
		}
	}
	return skipped, nil
}
```

Then in Advance, after getting the chain:
```go
skipped, err := skippedPhases(ctx, store, runID)
if err != nil {
	return nil, fmt.Errorf("advance: %w", err)
}

// Walk forward from current, skipping marked phases
toPhase := ""
for _, next := range chain {
	// Find phases after current
	if next == fromPhase {
		continue
	}
	// Only consider phases after current
	if !ChainIsValidTransition(chain, fromPhase, next) {
		continue
	}
	if !skipped[next] {
		toPhase = next
		break
	}
}
if toPhase == "" {
	// All remaining phases skipped — go to terminal
	toPhase = chain[len(chain)-1]
}
```

**Step 7: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./... -v`
Expected: All pass.

**Step 8: Commit**

```bash
git add infra/intercore/internal/phase/ infra/intercore/cmd/ic/run.go
git commit -m "feat(intercore): explicit ic run skip command with audit trail"
```

---

### Task 5: Artifact Content Hashing + dispatch_id

**Files:**
- Modify: `infra/intercore/internal/runtrack/runtrack.go` (update Artifact struct)
- Modify: `infra/intercore/internal/runtrack/store.go` (update AddArtifact, ListArtifacts)
- Modify: `infra/intercore/cmd/ic/run.go` (update artifact add CLI)
- Test: `infra/intercore/internal/runtrack/store_test.go`

**Step 1: Write failing test**

Add to `store_test.go`:

```go
func TestArtifactWithHash(t *testing.T) {
	db := setupTestDB(t)
	store := runtrack.New(db)

	// Create a temp file to hash
	tmpFile := filepath.Join(t.TempDir(), "plan.md")
	os.WriteFile(tmpFile, []byte("# My Plan"), 0644)

	// Create parent run
	// ... (use phase store or direct SQL)

	artifact := &runtrack.Artifact{
		RunID:       "testrun",
		Phase:       "plan",
		Path:        tmpFile,
		Type:        "file",
		DispatchID:  strPtr("dispatch1"),
	}

	id, err := store.AddArtifact(context.Background(), artifact)
	if err != nil {
		t.Fatal(err)
	}

	// Retrieve and check hash was computed
	artifacts, err := store.ListArtifacts(context.Background(), "testrun", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 {
		t.Fatalf("expected 1 artifact, got %d", len(artifacts))
	}
	if artifacts[0].ContentHash == nil {
		t.Error("expected content_hash to be set")
	}
	if artifacts[0].DispatchID == nil || *artifacts[0].DispatchID != "dispatch1" {
		t.Error("expected dispatch_id to be 'dispatch1'")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/runtrack/ -run TestArtifactWithHash -v`
Expected: FAIL — Artifact struct has no ContentHash/DispatchID fields.

**Step 3: Update Artifact struct in runtrack.go**

```go
type Artifact struct {
	ID          string
	RunID       string
	Phase       string
	Path        string
	Type        string
	ContentHash *string
	DispatchID  *string
	CreatedAt   int64
}
```

**Step 4: Update AddArtifact in store.go**

Compute SHA256 hash of the file at `a.Path` if the file exists:

```go
func (s *Store) AddArtifact(ctx context.Context, a *Artifact) (string, error) {
	id, err := generateID()
	if err != nil {
		return "", err
	}

	// Compute content hash if file exists
	var contentHash *string
	if a.Path != "" {
		if h, err := hashFile(a.Path); err == nil {
			contentHash = &h
		}
		// If file doesn't exist or can't be read, contentHash stays nil
	}
	// Override with explicitly set hash
	if a.ContentHash != nil {
		contentHash = a.ContentHash
	}

	now := time.Now().Unix()
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO run_artifacts (
			id, run_id, phase, path, type, content_hash, dispatch_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id, a.RunID, a.Phase, a.Path, a.Type, contentHash, a.DispatchID, now,
	)
	if err != nil {
		if isFKViolation(err) {
			return "", ErrRunNotFound
		}
		return "", fmt.Errorf("artifact add: %w", err)
	}
	return id, nil
}
```

Add helper:
```go
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", h.Sum(nil)), nil
}
```

Add `"crypto/sha256"`, `"io"`, `"os"` to imports.

**Step 5: Update ListArtifacts to scan new columns**

Update the SELECT and Scan to include `content_hash` and `dispatch_id` (as NullString).

**Step 6: Update CLI artifact add to accept --dispatch flag**

In `cmdRunArtifactAdd`, add `--dispatch=` flag parsing. Pass to Artifact struct.

**Step 7: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./...`
Expected: All pass.

**Step 8: Commit**

```bash
git add infra/intercore/internal/runtrack/ infra/intercore/cmd/ic/run.go
git commit -m "feat(intercore): artifact content hashing (SHA256) + dispatch_id tracking"
```

---

### Task 6: Token Tracking — cache_hits + Dispatch CLI

**Files:**
- Modify: `infra/intercore/internal/dispatch/dispatch.go` (add CacheHits field)
- Modify: `infra/intercore/cmd/ic/dispatch.go` (add tokens subcommand, update complete)
- Test: `infra/intercore/internal/dispatch/dispatch_test.go`

**Step 1: Write failing test**

Add to `dispatch_test.go`:

```go
func TestDispatchCacheHits(t *testing.T) {
	db := setupTestDB(t)
	store := dispatch.New(db, nil)

	d := &dispatch.Dispatch{
		ProjectDir: "/tmp/test",
		AgentType:  "codex",
	}
	id, err := store.Create(context.Background(), d)
	if err != nil {
		t.Fatal(err)
	}

	// Update with token data including cache_hits
	err = store.UpdateStatus(context.Background(), id, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens":  1000,
		"output_tokens": 500,
		"cache_hits":    3000,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.CacheHits == nil || *got.CacheHits != 3000 {
		t.Errorf("cache_hits = %v, want 3000", got.CacheHits)
	}
}
```

**Step 2: Run test to verify it fails**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./internal/dispatch/ -run TestDispatchCacheHits -v`
Expected: FAIL — CacheHits field doesn't exist, cache_hits not in allowedUpdateCols.

**Step 3: Update Dispatch struct**

Add to `Dispatch` struct in `dispatch.go`:
```go
CacheHits    *int
```

Update `allowedUpdateCols` to include `"cache_hits": true`.

Update `Get` and `queryDispatches` to scan `cache_hits` column (as NullInt64).

Update `dispatchCols` to include `cache_hits`.

**Step 4: Add `ic dispatch tokens` subcommand**

In `dispatch.go` CLI, add the tokens subcommand that sets token fields on a dispatch without changing status:

```go
func cmdDispatchTokens(ctx context.Context, args []string) int {
	var tokensIn, tokensOut, cacheHits int
	var hasIn, hasOut, hasCache bool
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--in="):
			// parse int
		case strings.HasPrefix(args[i], "--out="):
			// parse int
		case strings.HasPrefix(args[i], "--cache="):
			// parse int
		default:
			positional = append(positional, args[i])
		}
	}
	// ... validate, build UpdateFields, call store.UpdateTokens(id, fields)
}
```

Add a `UpdateTokens` method to dispatch.Store that updates token fields without changing status (uses same UpdateFields mechanism but doesn't require a status transition).

**Step 5: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./...`
Expected: All pass.

**Step 6: Commit**

```bash
git add infra/intercore/internal/dispatch/ infra/intercore/cmd/ic/dispatch.go
git commit -m "feat(intercore): cache_hits column + ic dispatch tokens command"
```

---

### Task 7: Token Aggregation — `ic run tokens`

**Files:**
- Modify: `infra/intercore/internal/dispatch/dispatch.go` (add aggregation queries)
- Modify: `infra/intercore/cmd/ic/run.go` (add tokens subcommand)
- Test: `infra/intercore/internal/dispatch/dispatch_test.go`

**Step 1: Write failing test for token aggregation**

```go
func TestTokenAggregation(t *testing.T) {
	db := setupTestDB(t)
	store := dispatch.New(db, nil)

	// Create dispatches with scope_id = run ID
	runID := "testrun"
	for i, tokens := range []struct{ in, out, cache int }{
		{1000, 500, 3000},
		{2000, 1000, 5000},
	} {
		d := &dispatch.Dispatch{
			ProjectDir: "/tmp",
			AgentType:  "codex",
			ScopeID:    &runID,
		}
		id, _ := store.Create(context.Background(), d)
		store.UpdateStatus(context.Background(), id, dispatch.StatusCompleted, dispatch.UpdateFields{
			"input_tokens":  tokens.in,
			"output_tokens": tokens.out,
			"cache_hits":    tokens.cache,
		})
		_ = i
	}

	agg, err := store.AggregateTokens(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if agg.TotalIn != 3000 {
		t.Errorf("TotalIn = %d, want 3000", agg.TotalIn)
	}
	if agg.TotalOut != 1500 {
		t.Errorf("TotalOut = %d, want 1500", agg.TotalOut)
	}
	if agg.TotalCache != 8000 {
		t.Errorf("TotalCache = %d, want 8000", agg.TotalCache)
	}
}
```

**Step 2: Implement AggregateTokens**

```go
type TokenAggregation struct {
	TotalIn    int64
	TotalOut   int64
	TotalCache int64
}

func (s *Store) AggregateTokens(ctx context.Context, scopeID string) (*TokenAggregation, error) {
	agg := &TokenAggregation{}
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_hits), 0)
		FROM dispatches WHERE scope_id = ?`, scopeID).Scan(
		&agg.TotalIn, &agg.TotalOut, &agg.TotalCache,
	)
	if err != nil {
		return nil, fmt.Errorf("aggregate tokens: %w", err)
	}
	return agg, nil
}
```

**Step 3: Add `ic run tokens` CLI subcommand**

In `cmdRun` switch, add `case "tokens"`. Format output as a table:

```
Run: <id>
  Input tokens:  3,000
  Output tokens: 1,500
  Cache hits:    8,000
  Cache ratio:   72.7%
  Total tokens:  4,500
```

With `--json` flag for programmatic output.

**Step 4: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./...`
Expected: All pass.

**Step 5: Commit**

```bash
git add infra/intercore/internal/dispatch/ infra/intercore/cmd/ic/run.go
git commit -m "feat(intercore): ic run tokens — query-time token aggregation"
```

---

### Task 8: Budget Threshold Events

**Files:**
- Modify: `infra/intercore/internal/phase/store.go` (budget fields on Run)
- Modify: `infra/intercore/internal/dispatch/dispatch.go` (budget check after token update)
- Modify: `infra/intercore/internal/event/event.go` (budget event types)
- Modify: `infra/intercore/cmd/ic/run.go` (--token-budget, --budget-warn-pct flags)
- Test: `infra/intercore/internal/dispatch/dispatch_test.go`

**Step 1: Write failing test for budget event emission**

```go
func TestBudgetWarningEvent(t *testing.T) {
	db := setupTestDB(t)

	// Create a run with token budget
	phaseStore := phase.New(db)
	run := &phase.Run{
		ProjectDir:    "/tmp/test",
		Goal:          "test budget",
		TokenBudget:   int64Ptr(10000),
		BudgetWarnPct: 80,
	}
	runID, _ := phaseStore.Create(context.Background(), run)

	// Create dispatch linked to run
	dispStore := dispatch.New(db, nil)
	d := &dispatch.Dispatch{
		ProjectDir: "/tmp/test",
		AgentType:  "codex",
		ScopeID:    &runID,
	}
	dispID, _ := dispStore.Create(context.Background(), d)

	// Report 9000 tokens (exceeds 80% of 10000)
	err := dispStore.UpdateStatus(context.Background(), dispID, dispatch.StatusCompleted, dispatch.UpdateFields{
		"input_tokens":  9000,
		"output_tokens": 0,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Check that budget.warning event was emitted
	// ... query dispatch_events or state for budget warning marker
}
```

**Step 2: Implement budget check**

The budget check runs inside the dispatch store after token updates. It:
1. Queries the run's token_budget and budget_warn_pct (if scope_id is set)
2. Aggregates current total tokens for the run
3. If total >= budget * warn_pct / 100 and warning not yet emitted: emit `budget.warning` event
4. If total >= budget and exceeded not yet emitted: emit `budget.exceeded` event
5. Uses state store to track "already emitted" (key: `budget.warning.<run_id>`)

The dispatch store needs access to the phase store (for run budget info) and state store (for dedup tracking). This creates a dependency — pass these as constructor parameters or use a callback.

Cleanest approach: a `BudgetChecker` that's called from the CLI layer after any token update, keeping the dispatch store simple.

```go
// In a new file: internal/budget/budget.go
type Checker struct {
	phaseStore    *phase.Store
	dispatchStore *dispatch.Store
	stateStore    *state.Store
}

func (c *Checker) CheckBudget(ctx context.Context, runID string) error {
	run, err := c.phaseStore.Get(ctx, runID)
	if err != nil || run.TokenBudget == nil {
		return nil // no budget set
	}

	agg, err := c.dispatchStore.AggregateTokens(ctx, runID)
	if err != nil {
		return err
	}

	total := agg.TotalIn + agg.TotalOut
	budget := *run.TokenBudget
	warnThreshold := budget * int64(run.BudgetWarnPct) / 100

	// Check warning
	if total >= warnThreshold {
		key := fmt.Sprintf("budget.warning.%s", runID)
		if !c.stateStore.Exists(ctx, key, runID) {
			// Emit event, set flag
		}
	}
	// Check exceeded
	if total >= budget {
		key := fmt.Sprintf("budget.exceeded.%s", runID)
		if !c.stateStore.Exists(ctx, key, runID) {
			// Emit event, set flag
		}
	}
	return nil
}
```

**Step 3: Add budget event types to event package**

```go
const (
	EventBudgetWarning  = "budget.warning"
	EventBudgetExceeded = "budget.exceeded"
)
```

**Step 4: Wire budget check into CLI**

After any `ic dispatch tokens` or `ic dispatch complete` call that includes token data, check if the dispatch has a scope_id (run link) and call `BudgetChecker.CheckBudget`.

**Step 5: Run all tests**

Run: `cd /root/projects/Interverse/infra/intercore && go test ./...`
Expected: All pass.

**Step 6: Run integration tests**

Run: `cd /root/projects/Interverse/infra/intercore && bash test-integration.sh`
Expected: All pass.

**Step 7: Commit**

```bash
git add infra/intercore/internal/budget/ infra/intercore/internal/event/ infra/intercore/cmd/ic/
git commit -m "feat(intercore): budget threshold events (warning + exceeded)"
```

---

### Task 9: CLI Updates + Integration Tests

**Files:**
- Modify: `infra/intercore/cmd/ic/run.go` (--phases flag on create, --token-budget, --budget-warn-pct)
- Modify: `infra/intercore/test-integration.sh` (add E1 integration tests)
- Modify: `infra/intercore/lib-intercore.sh` (update bash wrappers)

**Step 1: Add --phases flag to `ic run create`**

Parse `--phases='["a","b","c"]'` in `cmdRunCreate`. Call `ParsePhaseChain` to validate. Store on the Run struct.

**Step 2: Add --token-budget and --budget-warn-pct flags**

Parse in `cmdRunCreate`. Store as `TokenBudget` and `BudgetWarnPct` on Run.

**Step 3: Update `ic run status --json` output**

Include `phases`, `token_budget`, `budget_warn_pct` in the JSON output.

**Step 4: Add integration tests**

Add to `test-integration.sh`:

```bash
# --- E1: Configurable phase chains ---
echo "=== Custom phase chain ==="
RUN_ID=$(ic run create --project=. --goal="test custom" --phases='["draft","review","done"]')
PHASE=$(ic run phase "$RUN_ID")
assert_eq "$PHASE" "draft" "initial phase should be first in chain"

ic run advance "$RUN_ID" --disable-gates
PHASE=$(ic run phase "$RUN_ID")
assert_eq "$PHASE" "review" "advance should go to second phase"

# --- E1: Skip command ---
echo "=== Skip command ==="
RUN_ID=$(ic run create --project=. --goal="test skip" --phases='["a","b","c","d"]')
ic run skip "$RUN_ID" b --reason="complexity 1" --actor="test"
ic run advance "$RUN_ID" --disable-gates
PHASE=$(ic run phase "$RUN_ID")
assert_eq "$PHASE" "c" "advance should skip 'b' and land on 'c'"

# --- E1: Artifact hashing ---
echo "=== Artifact hashing ==="
echo "test content" > /tmp/ic-test-artifact.md
RUN_ID=$(ic run create --project=. --goal="test hash")
ART_ID=$(ic run artifact add "$RUN_ID" --phase=brainstorm --path=/tmp/ic-test-artifact.md)
HASH=$(ic run artifact list "$RUN_ID" --json | jq -r '.[0].content_hash')
[ -n "$HASH" ] && [ "$HASH" != "null" ] || fail "artifact should have content_hash"

# --- E1: Token aggregation ---
echo "=== Token aggregation ==="
RUN_ID=$(ic run create --project=. --goal="test tokens" --token-budget=100000)
# ... create dispatch, report tokens, check aggregation
```

**Step 5: Update lib-intercore.sh wrappers**

Add:
```bash
intercore_run_skip() {
    local run_id="$1" phase="$2" reason="$3" actor="${4:-}"
    local args=(run skip "$run_id" "$phase" --reason="$reason")
    [[ -n "$actor" ]] && args+=(--actor="$actor")
    ic "${args[@]}"
}

intercore_run_tokens() {
    local run_id="$1"
    ic run tokens "$run_id" --json
}

intercore_dispatch_tokens() {
    local dispatch_id="$1" tokens_in="$2" tokens_out="$3" cache_hits="${4:-}"
    local args=(dispatch tokens "$dispatch_id" --set --in="$tokens_in" --out="$tokens_out")
    [[ -n "$cache_hits" ]] && args+=(--cache="$cache_hits")
    ic "${args[@]}"
}
```

**Step 6: Run integration tests**

Run: `cd /root/projects/Interverse/infra/intercore && bash test-integration.sh`
Expected: All pass.

**Step 7: Build binary and verify**

Run: `cd /root/projects/Interverse/infra/intercore && go build -o ic ./cmd/ic && ./ic run create --help`
Expected: Help shows new flags.

**Step 8: Commit**

```bash
git add infra/intercore/cmd/ic/ infra/intercore/test-integration.sh infra/intercore/lib-intercore.sh
git commit -m "feat(intercore): E1 CLI integration — phases, skip, tokens, budget flags"
```

---

### Task 10: Clean Up Legacy Code + Update Docs

**Files:**
- Modify: `infra/intercore/internal/phase/phase.go` (remove unused legacy maps)
- Modify: `infra/intercore/AGENTS.md` (document new commands)
- Modify: `infra/intercore/CLAUDE.md` (update quick reference)

**Step 1: Remove unused legacy code**

In `phase.go`, if `transitionTable`, `validTransitions`, `allPhases` are no longer referenced by any code or test, remove them. Run `go vet ./...` and `go build ./...` to verify.

Keep `DefaultPhaseChain` — it's the fallback for NULL phases.

**Step 2: Update AGENTS.md**

Add documentation for:
- `ic run create --phases='[...]' --token-budget=N --budget-warn-pct=N`
- `ic run skip <id> <phase> --reason=... [--actor=...]`
- `ic run tokens <id> [--project=<dir>] [--json]`
- `ic dispatch tokens <id> --set --in=N --out=N [--cache=N]`
- Schema v6 column descriptions

**Step 3: Update CLAUDE.md quick reference**

Add skip and tokens commands to the Run Quick Reference section.

**Step 4: Run full test suite one final time**

Run: `cd /root/projects/Interverse/infra/intercore && go test -race ./... && bash test-integration.sh`
Expected: All pass with no race conditions.

**Step 5: Commit**

```bash
git add infra/intercore/
git commit -m "docs(intercore): update AGENTS.md and CLAUDE.md for E1 primitives"
```
