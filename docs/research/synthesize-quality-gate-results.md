# Synthesis: Quality Gate Results — E1 Kernel Primitives

Date: 2026-02-19
Context: 25 files changed across Go, SQL, and Bash. Risk domains: schema migration v5->v6, state machine correctness, concurrent budget dedup, input validation, legacy removal.
Mode: quality-gates

---

## Validation

4/4 agents valid. 0 failed. All files begin with `### Findings Index` and contain `Verdict:` lines.

| Agent | File | Verdict | Findings |
|-------|------|---------|----------|
| fd-architecture | `.clavain/quality-gates/fd-architecture.md` | needs-changes | 7 (1 HIGH, 2 MED, 4 LOW) + 1 IMP |
| fd-correctness | `.clavain/quality-gates/fd-correctness.md` | needs-changes | 7 (2 MED, 3 LOW, 2 INFO) |
| fd-quality | `.clavain/quality-gates/fd-quality.md` | needs-changes | 8 (2 MED, 3 LOW, 3 INFO) |
| fd-safety | `.clavain/quality-gates/fd-safety.md` | safe | 5 (3 LOW, 2 INFO) |

---

## Overall Verdict: needs-changes | Gate: FAIL

Three of four agents rated the diff `needs-changes`. The safety reviewer rated it `safe` — this is not a conflict. Safety evaluates exploitability in the internal threat model (local CLI, no network exposure), which is appropriately lower than the functional correctness bar. The two P1 findings — both flagged by 3/4 agents — are functional correctness bugs that must be fixed before callers depend on this surface.

---

## Verdict Files

| Agent | Status | Summary |
|-------|--------|---------|
| fd-architecture | NEEDS_ATTENTION | Phase-chain unmarshal fallback + budget coupling require fixes before callers depend on this surface |
| fd-correctness | NEEDS_ATTENTION | Migration no-commit path, budget error masking, and skip-walk silent halt are pre-production blockers |
| fd-quality | NEEDS_ATTENTION | Error swallowing in budget.Check and concrete-type store fields break established codebase patterns |
| fd-safety | CLEAN | No exploitable findings in internal threat model; all issues are advisory low/info severity |

---

## P1 Findings — Must Fix Before Ship

### [A1/C7/Q4] Silent phase-chain JSON unmarshal fallback changes run semantics invisibly

**Convergence: 3/4 agents (fd-architecture HIGH, fd-correctness INFO, fd-quality LOW)**

- **File:** `internal/phase/store.go` — `Get`, `Current`, `queryRuns`
- **Pattern:**
  ```go
  if err := json.Unmarshal([]byte(phasesJSON.String), &chain); err == nil {
      r.Phases = chain
  }
  // error path: silently discarded, r.Phases stays nil, DefaultPhaseChain used
  ```
- **Impact:** A run created with `--phases='["a","b","c"]'` that experiences DB column corruption silently executes the 8-phase default lifecycle instead. `ChainIsTerminal` returns false when it should return true, allowing `Advance` to step the run past the intended terminal phase into unintended phases (brainstorm-reviewed, strategized, etc.). The rest of the codebase surfaces decode errors via the `(*T, error)` return pattern; this path is an unexcused exception.
- **Fix:** Return error when `phasesJSON.Valid && json.Unmarshal fails`. Extract to shared helper `parsePhasesJSON(sql.NullString) ([]string, error)` (consolidates three duplicate unmarshal blocks — also resolves I1 in architecture and quality reviews).

---

### [C2/Q1/S-04] budget.Check() silently maps all DB errors to "no budget" result

**Convergence: 3/4 agents (fd-correctness MEDIUM, fd-quality MEDIUM, fd-safety INFO)**

- **File:** `internal/budget/budget.go:57-60`
- **Pattern:**
  ```go
  run, err := c.phaseStore.Get(ctx, runID)
  if err != nil {
      return nil, nil // run not found or error — no budget to check
  }
  ```
- **Impact:** `ErrNotFound` (expected — run doesn't exist) and transient DB errors (`SQLITE_BUSY`, context cancellation) are treated identically. A budget check during a high-load period silently produces a false "OK" result. Budget warning events go unemitted. Over time the dedup state key never gets set, so the next successful check emits a stale event. Hooks relying on budget exit codes receive false-OK on transient failures.
- **Fix:**
  ```go
  run, err := c.phaseStore.Get(ctx, runID)
  if err != nil {
      if errors.Is(err, phase.ErrNotFound) {
          return nil, nil // run doesn't exist — no budget to check
      }
      return nil, fmt.Errorf("budget check: get run: %w", err)
  }
  ```

---

## P2 Findings — Should Fix

### [C1] Migration early-return inside transaction does not commit

**Convergence: 1/4 agents (fd-correctness MEDIUM)**

- **File:** `internal/db/db.go:132`
- **Issue:** `return nil` inside the `BeginTx` block when `currentVersion >= currentSchemaVersion`. The `defer tx.Rollback()` fires on exit, rolling back the `_migrate_lock` table DDL. Concurrent migration correctness holds (DDL is idempotent), but the `_migrate_lock` guard is undermined on the already-migrated fast path, and the code path is misleading.
- **Fix:** Call `tx.Rollback()` explicitly before returning nil on the already-migrated fast path to clarify intent.

---

### [A2/Q3] Budget policy side-effect injected into cmdDispatchTokens write handler

**Convergence: 2/4 agents (fd-architecture MEDIUM, fd-quality LOW)**

- **File:** `cmd/ic/dispatch.go:224-239`
- **Issue:** After updating token fields, `cmdDispatchTokens` constructs `phase.Store`, `state.Store`, and a `budget.Checker` inline and runs a budget check, writing results to stderr as a side effect. `ic run budget <id>` already exists for explicit budget evaluation. This creates two code paths for the same check, leaks budget policy into a write handler, adds three store constructions inside a CLI function, and uses a `[budget]` stderr prefix that differs from every other error in the file.
- **Fix:** Remove the budget side-effect from `cmdDispatchTokens`. Callers that need a post-update budget check call `ic run budget` explicitly.

---

### [A3/Q2] budget.Checker takes concrete store types inconsistently with gate pattern

**Convergence: 2/4 agents (fd-architecture MEDIUM, fd-quality MEDIUM)**

- **File:** `internal/budget/budget.go:40-41`
- **Issue:** `Checker.dispatchStore` is `*dispatch.Store` and `Checker.stateStore` is `*state.Store`, while `Checker.phaseStore` is a narrow `PhaseStoreQuerier` interface. The gate system uses `RuntrackQuerier` and `VerdictQuerier` interfaces throughout for test isolation and to avoid coupling. The budget package carries hard import-time dependencies on `internal/dispatch` and `internal/state`.
- **Fix:** Define `DispatchTokenAggregator` and `BudgetStateStore` narrow interfaces in the budget package, following the existing `PhaseStoreQuerier` pattern.

---

### [A6] intercore_run_budget returns 0 on unavailability — unsafe default for a budget guard

**Convergence: 1/4 agents (fd-architecture LOW)**

- **File:** `lib-intercore.sh:3183-3188`
- **Issue:** `intercore_run_budget` returns 0 when `intercore_available` fails. `intercore_run_skip` and `intercore_dispatch_tokens` return 1 on unavailability. A hook using `intercore_run_budget` to guard on budget-exceeded will silently pass (exit 0) when the binary is missing — the opposite of a safe default for a budget guard.
- **Fix:** Return 1 on unavailability, or document the asymmetry with a comment explaining the intentional degradation behavior.

---

### [C4] cmdDispatchTokens uses ScopeID as run ID with no FK constraint

**Convergence: 1/4 agents (fd-correctness LOW)**

- **File:** `cmd/ic/dispatch.go:120`
- **Issue:** `budget.Checker.Check(ctx, *disp.ScopeID)` assumes `scope_id` is always a run ID. The column is `TEXT` with no FK. Dispatches spawned with non-run scope IDs (session ID, project ID) produce a silent no-op budget check that returns "no budget set".
- **Fix:** Document the invariant that `dispatch.ScopeID` must be a run ID when budget checks are desired, or pass the run ID explicitly to `ic dispatch tokens`.

---

### [A7] --complexity and ForceFull retained with no operational effect after v6

**Convergence: 1/4 agents (fd-architecture LOW)**

- **File:** `cmd/ic/run.go` (cmdRunCreate)
- **Issue:** After v6, `Advance` and `EvaluateGate` call `ResolveChain(run)` which reads `run.Phases` only. `Complexity` and `ForceFull` are never consulted. A user passing `--complexity=1` receives no error and no warning, but gets an 8-phase run. The CLI help advertises the flag with no deprecation notice.
- **Fix:** Emit a deprecation warning to stderr, or add `[deprecated]` to the usage string for both flags.

---

### [C5] Cache ratio denominator conflates two token domains

**Convergence: 1/4 agents (fd-correctness LOW)**

- **File:** `cmd/ic/run.go` (cmdRunTokens)
- **Issue:** `cacheRatio = float64(agg.TotalCache) / float64(agg.TotalIn+agg.TotalCache) * 100` — `cache_hits` is not constrained to be `<= input_tokens` per dispatch, so the ratio can exceed 100% if any tool reports differently.
- **Fix:** Document the ratio definition at the site, or add a clamp/validation on `cache_hits` at `UpdateTokens` time.

---

### [S-01] hashFile reads arbitrary caller-supplied paths without traversal guard

**Convergence: 1/4 agents (fd-safety LOW)**

- **File:** `internal/runtrack/store.go` (hashFile)
- **Issue:** Called unconditionally from `AddArtifact` when `a.Path != ""`. No size cap. The `--path` flag has no analogous guard to the `--db` flag's traversal validation. A very large file or `/dev/urandom` symlink would block the caller on `io.Copy`.
- **Fix:** Add a file size cap (skip hashing if `stat.Size() > 100 MB`). Add a comment in `cmdRunArtifactAdd` that `--path` is operator-trusted input.

---

### [S-02] Phase names stored verbatim with no character allowlist

**Convergence: 1/4 agents (fd-safety LOW)**

- **File:** `internal/phase/phase.go` (ParsePhaseChain)
- **Issue:** Phase names validated for uniqueness and minimum count (`>= 2`) only. Names containing `\n`, NUL bytes, or control characters produce malformed text output in `ic run phase` and similar commands.
- **Fix:** Add allowlist regex `^[a-zA-Z0-9_-]+$` in `ParsePhaseChain` before the seen-map loop.

---

### [S-03] isDuplicateColumnError uses fragile substring match on SQLite error text

**Convergence: 1/4 agents (fd-safety LOW)**

- **File:** `internal/db/db.go` (isDuplicateColumnError)
- **Issue:** `strings.Contains(err.Error(), "duplicate column name")` — if `modernc.org/sqlite` changes this error message text in a future version, migration idempotency silently breaks. The negative branch (non-duplicate error that contains the substring) is not tested.
- **Fix:** Use a SQLite error code check (extended error code for duplicate column), or add a version-dependency comment and a negative-branch unit test.

---

## IMP Findings — Optional Improvements

### [A5/Q6/C3] Skip-walk break on ChainNextPhase error leaves toPhase ambiguous

**Convergence: 3/4 agents (fd-architecture LOW, fd-quality INFO, fd-correctness LOW)**

- **File:** `internal/phase/machine.go:74-80`
- **Pattern:** `break` on `ChainNextPhase` error inside the skip-walk for-loop. If reached, `toPhase` stays at a skipped phase; `UpdatePhase` writes that skipped phase as the new phase value. The current chain validation makes this reachable only via corrupt data, but a `break` with no diagnostic is strictly worse than an error return.
- **Recommendation:** Replace `break` with `return nil, fmt.Errorf("advance: skip walk: %w", err)`, or add an invariant comment explaining why `break` is safe.

### [A4] GateRulesInfo documents only DefaultPhaseChain

- **File:** `internal/phase/gate.go` (GateRulesInfo)
- **Recommendation:** Add docstring "rules apply to the default chain only" and note in `ic gate rules --help`.

### [I1] Extract phasesJSON unmarshal to shared helper

- **File:** `internal/phase/store.go`
- **Recommendation:** `parsePhasesJSON(sql.NullString) ([]string, error)` — consolidates three duplicate unmarshal blocks and ensures consistent error behavior with a single change point. Combined with P1 fix for A1, this is a one-shot cleanup.

### [Q5] json.NewEncoder writes unchecked

- **File:** Multiple CLI commands in `cmd/ic/`
- **Recommendation:** Check `Encode` return value in `cmdRunBudget` at minimum, since its exit code signals budget state. A broken pipe followed by exit 1 would be misinterpreted by callers.

### [S-05] INTERCORE_DB unquoted in shell wrapper args

- **File:** `lib-intercore.sh`
- **Recommendation:** Move `${INTERCORE_DB:+--db="$INTERCORE_DB"}` inside the args array to prevent word-split on paths containing spaces.

### [C6] Advance does not record intermediate skip events during skip-walk

- **File:** `internal/phase/machine.go`
- **Recommendation:** When `Advance` walks past multiple pre-skipped phases, record an intermediate skip event in the audit trail for each walked phase, not just the final `EventAdvance`.

### [Q7] Redundant nil-and-length check on r.Phases

- **File:** `internal/phase/store.go:50`
- **Pattern:** `if r.Phases != nil && len(r.Phases) > 0` — nil guard is unnecessary since `len` handles nil slices correctly.

### [Q8] Test helpers discard errors from Create/UpdateStatus

- **File:** `internal/budget/budget_test.go:894-910`
- **Recommendation:** Use `if _, err := ds.Create(...); err != nil { t.Fatal(err) }` consistently for clear test failure attribution.

---

## Conflicts

None. All four agents are consistent in their findings. Three issues received multi-agent convergence. The safety reviewer's "safe" verdict vs. "needs-changes" from the other three is not a conflict — safety evaluates exploitability in the current internal threat model, which is appropriately lower than functional correctness criteria.

---

## Files

- Architecture report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-architecture.md`
- Correctness report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md`
- Quality report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`
- Safety report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md`
- Synthesis: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/synthesis.md`
- Findings JSON: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/findings.json`
- Verdict files: `/root/projects/Interverse/infra/intercore/.clavain/verdicts/fd-{architecture,correctness,quality,safety}.json`
