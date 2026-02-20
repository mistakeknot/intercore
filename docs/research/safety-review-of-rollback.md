# Safety Review: Intercore Rollback and Recovery Feature

**Reviewer:** Flux-drive Safety Reviewer (Claude Sonnet 4.6)
**Date:** 2026-02-20
**Diff reviewed:** `/tmp/qg-diff-1771610172.txt`
**Scope:** Security threats, input validation, SQL injection, trust boundaries, deployment safety
**Change risk classification:** Medium — new stateful mutation path that is partially irreversible at the application layer (artifact status change is a soft-delete, not a physical delete; phase rewind touches runs/dispatches/agents/artifacts)

---

## Threat Model

This is a local CLI tool with no network exposure, no authentication boundary, and a single-user trust model. The prior security review (`docs/research/review-intercore-security.md`) established the baseline threat model:

- **Trusted:** User's filesystem, bash hooks in the project repo
- **Untrusted:** Malicious project repos whose hook scripts or `--db` paths could influence `ic` behavior
- **No:** network listeners, credentials, setuid bits, multi-tenancy

The rollback feature adds one new material surface: **user-supplied strings that flow into SQL via CLI flags** (`--to-phase`, `--reason`, `--phase`, `--format`). This is the primary security lens for this review.

---

## Architecture Summary of the Change

The diff adds:

1. `cmd/ic/run.go` — `cmdRunRollback`, `cmdRunRollbackWorkflow`, `cmdRunRollbackCode`: CLI routing and orchestration
2. `internal/phase/store.go` — `RollbackPhase`: UPDATE on `runs` table
3. `internal/phase/machine.go` — `Rollback`: orchestrates store call + event recording + callback
4. `internal/phase/phase.go` — `ChainPhasesBetween`, `ChainPhaseIndex`: pure chain arithmetic
5. `internal/runtrack/store.go` — `MarkArtifactsRolledBack`, `FailAgentsByRun`, `ListArtifactsForCodeRollback`: bulk UPDATE + LEFT JOIN query
6. `internal/dispatch/dispatch.go` — `CancelByRun`: UPDATE dispatches
7. `internal/db/db.go` + `schema.sql` — schema v8: adds `status TEXT NOT NULL DEFAULT 'active'` to `run_artifacts`
8. `lib-intercore.sh` — bash wrappers: `intercore_run_rollback`, `intercore_run_rollback_dry`, `intercore_run_code_rollback`
9. `test-integration.sh` — E6 test suite

---

## Security Analysis

### 1. CLI Flag Parsing — Injection Risk Evaluation

**Code path** (`cmd/ic/run.go:62-76`):

```go
for i := 1; i < len(args); i++ {
    switch {
    case strings.HasPrefix(args[i], "--to-phase="):
        toPhase = strings.TrimPrefix(args[i], "--to-phase=")
    case strings.HasPrefix(args[i], "--reason="):
        reason = strings.TrimPrefix(args[i], "--reason=")
    case strings.HasPrefix(args[i], "--layer="):
        layer = strings.TrimPrefix(args[i], "--layer=")
    case strings.HasPrefix(args[i], "--phase="):
        filterPhase = strings.TrimPrefix(args[i], "--phase=")
    case strings.HasPrefix(args[i], "--format="):
        format = strings.TrimPrefix(args[i], "--format=")
    ...
    }
}
```

`strings.HasPrefix`/`TrimPrefix` is the same manual flag parser used throughout the rest of `run.go`. This approach is consistent with the existing codebase pattern. There is no injection risk from the parsing itself — it simply extracts the string after the `=`. The question is what happens to each extracted value downstream.

**Assessment per flag:**

| Flag | Downstream usage | Injection risk |
|------|-----------------|----------------|
| `--to-phase` | `ChainPhasesBetween` (pure string comparison) + `RollbackPhase` (parameterized SQL `?`) | None |
| `--reason` | Stored as `?` parameter in `phase_events.reason` + passed to `event.Notify` as struct field | None |
| `--layer` | Equality check: `if layer == "code"` | None |
| `--phase` | Passed as `*string` to `ListArtifactsForCodeRollback`, used as `?` parameter | None |
| `--format` | Equality check: `if format == "text"` | None |
| `--dry-run` | Boolean flag, no string value | None |

**Verdict:** No injection risk in CLI flag parsing. All user-supplied strings are either compared by equality, used as `?` parameterized SQL values, or stored as data fields in parameterized inserts.

---

### 2. SQL Injection Analysis

#### 2a. `MarkArtifactsRolledBack` — Dynamic IN Clause

**Code** (`internal/runtrack/store.go:1037-1053`):

```go
func (s *Store) MarkArtifactsRolledBack(ctx context.Context, runID string, phases []string) (int64, error) {
    if len(phases) == 0 {
        return 0, nil
    }

    placeholders := make([]string, len(phases))
    args := make([]interface{}, 0, len(phases)+1)
    args = append(args, runID)
    for i, p := range phases {
        placeholders[i] = "?"
        args = append(args, p)
    }

    query := fmt.Sprintf(
        "UPDATE run_artifacts SET status = 'rolled_back' WHERE run_id = ? AND status = 'active' AND phase IN (%s)",
        strings.Join(placeholders, ", "),
    )
    result, err := s.db.ExecContext(ctx, query, args...)
```

This is the pattern flagged as a concern. The `fmt.Sprintf` call interpolates the IN clause, but it only interpolates the **placeholder string** `"?, ?, ?"` — **not the actual phase values**. The values are passed as `args` to `ExecContext`, which is the correct parameterized query approach for a variable-length IN clause.

The `phases` slice originates from `phase.ChainPhasesBetween`, which returns values from the run's phase chain (loaded from DB at rollback time). These are internal values, not raw user input. Even if a malicious value were injected into the phase chain at creation time (e.g., via `--phases='["; DROP TABLE...]'`), it would only affect this query as a parameterized `?` argument, not as SQL text.

**Verdict: Safe.** The `fmt.Sprintf` only produces `"?, ?, ?"` strings — no user content is concatenated into SQL text.

#### 2b. `RollbackPhase` — Phase Name in UPDATE

**Code** (`internal/phase/store.go:843-847`):

```go
result, err := s.db.ExecContext(ctx, `
    UPDATE runs SET phase = ?, status = 'active', updated_at = ?, completed_at = NULL
    WHERE id = ?`,
    targetPhase, now, id,
)
```

`targetPhase` is a user-supplied string from `--to-phase`. It is passed as a parameterized `?` argument. Additionally, before this call, `RollbackPhase` validates that `targetPhase` is a member of the run's chain via `ChainContains(chain, targetPhase)` — so only valid phase names from the chain can reach the SQL.

**Verdict: Safe.** Parameterized, and additionally constrained by chain membership validation.

#### 2c. `ListArtifactsForCodeRollback` — LEFT JOIN with phase filter

**Code** (`internal/runtrack/store.go:1076-1092`):

```go
if filterPhase != nil {
    query = `
        SELECT a.dispatch_id, d.name, a.phase, a.path, a.content_hash, a.type
        FROM run_artifacts a
        LEFT JOIN dispatches d ON a.dispatch_id = d.id
        WHERE a.run_id = ? AND a.phase = ?
        ORDER BY a.phase, a.created_at ASC`
    args = []interface{}{runID, *filterPhase}
}
```

`filterPhase` comes from the user's `--phase=<value>`. It is passed as a parameterized `?` argument. No string interpolation.

**Verdict: Safe.**

#### 2d. `CancelByRun` — dispatch cancellation

**Code** (`internal/dispatch/dispatch.go:493-499`):

```go
result, err := s.db.ExecContext(ctx, `
    UPDATE dispatches SET status = ?, completed_at = ?
    WHERE scope_id = ? AND status NOT IN ('completed', 'failed', 'cancelled', 'timeout')`,
    StatusCancelled, now, runID,
)
```

`runID` is user-supplied from the CLI positional argument. It is a parameterized `?` argument. The `StatusCancelled` is a Go constant, not user input.

**Verdict: Safe.**

**Overall SQL injection verdict: No SQL injection risk in any new query.**

---

### 3. Format String Injection Risk

**Code** (`cmd/ic/run.go:250`):

```go
fmt.Printf("%-20s %-20s %-40s %s\n", e.Phase, dispatchName, e.Path, hash)
```

This is the `--format=text` output path. The values `e.Phase`, `dispatchName`, `e.Path`, and `hash` come from the database and are passed as **explicit positional arguments** to `fmt.Printf`, not as format string input. The format string `"%-20s %-20s %-40s %s\n"` is a hardcoded literal.

In Go, `fmt.Printf(format, args...)` is only vulnerable to format string injection if user input flows into the **first argument** (the format string itself). Here, user data is in the positional arguments, which is safe.

The closest risk would be if a path contained `%` characters that were interpreted as format directives — but since the path is in the argument position (4th positional), Go's `fmt` package treats it as a plain string value, not a format directive. This is fundamentally different from C's `printf(user_input)`.

**Verdict: Safe. No format string injection.**

---

### 4. Phase Name Validation — Edge Cases

The rollback validation path is:

```
CLI --to-phase=<x>
  → cmdRunRollbackWorkflow(toPhase=x)
    → pStore.Get(runID)  → loads run with its chain
    → phase.ResolveChain(run)  → returns chain (explicit or default)
    → phase.ChainPhasesBetween(chain, toPhase, run.Phase)
      → returns nil if toPhase not in chain OR not behind current phase
    → if nil: reject with exit 1 ("target phase is not behind current phase")
    → if non-nil: proceed
    → phase.Rollback(ctx, ...)
      → store.RollbackPhase(ctx, ...)
        → double-validates: ChainContains(chain, targetPhase) + ChainPhaseIndex comparison
```

This is double-validated: once in `cmdRunRollbackWorkflow` via `ChainPhasesBetween`, and again inside `store.RollbackPhase` via `ChainContains` + `ChainPhaseIndex`. The validation logic itself:

```go
func ChainPhasesBetween(chain []string, from, to string) []string {
    fromIdx := ChainPhaseIndex(chain, from)
    toIdx := ChainPhaseIndex(chain, to)
    if fromIdx < 0 || toIdx < 0 || fromIdx >= toIdx {
        return nil
    }
    ...
}
```

**Edge cases evaluated:**

1. **Empty string phase name:** `ChainPhaseIndex` iterates the chain and returns -1 for `""` (unless the chain contains `""`). An empty `--to-phase=` value will be caught as "not in chain" and rejected. Safe.

2. **Phase name with SQL metacharacters (e.g., `'; DROP TABLE--`):** The chain is loaded from the DB at runtime. If someone created a run with a malicious chain phase name at creation time via `--phases='["plan","; DROP..."]'`, that name is already in the DB as a parameterized value. When rollback uses it, it remains parameterized. No SQL injection path. Safe.

3. **Same phase as current:** `ChainPhasesBetween` returns `nil` when `fromIdx >= toIdx`, which includes the case where `from == to` (same index). The CLI would print "target phase is not behind current phase" and exit 1. Safe.

4. **Phase not in chain at all:** Returns `nil` from `ChainPhasesBetween`. Rejected. Safe.

5. **Unicode phase names:** Phase names are stored and compared as Go `string` values, which are UTF-8 byte sequences. No length limits are enforced on phase names at rollback time (the limit would apply at creation). This is consistent with the existing codebase — phase names are internal identifiers, not user-visible text with injection risk.

**Verdict: Phase validation is thorough.** The double-validation is slightly redundant but harmless.

---

### 5. `--format` Flag — Missing Input Validation

**Code** (`cmd/ic/run.go:238`):

```go
if format == "text" {
    // text output
    return 0
}
// Default JSON output
enc := json.NewEncoder(os.Stdout)
```

The `--format` flag is handled by equality check: only `"text"` triggers the text path; everything else (including `""`, `"json"`, garbage values) falls through to JSON output. This is correct behavior for a CLI tool — unexpected format values silently produce JSON, which is the safe default. There is no injection risk.

**Minor quality finding:** The code does not validate that `format` is one of `{"", "json", "text"}` and silently accepts any value as equivalent to `"json"`. This is not a security issue, but it could confuse users who pass `--format=csv` and get JSON. A warning or usage error for unknown format values would improve UX.

---

### 6. `reason` String — Stored and Logged Without Redaction

The `--reason` string is stored in `phase_events.reason` (via parameterized SQL) and emitted on stderr via `LogHandler`. It is also passed to `evStore.AddDispatchEvent` as a reason string.

From `cmd/ic/run.go:185`:
```go
evStore.AddDispatchEvent(ctx, "", runID, "", dispatch.StatusCancelled, "rollback", reason)
```

The `reason` parameter is user-controlled. Since intercore stores no credentials and `reason` is a user intent annotation, there is no credential leakage risk. If a user accidentally puts a secret in `--reason="API_KEY=sk-..."`, it lands in the event store — but this is the same risk that exists for the `--goal` field in `ic run create`, which is already stored without redaction. This is acceptable for a local tool.

**Verdict: Acceptable residual risk.** Document in AGENTS.md that `--reason` is stored persistently in the audit trail.

---

### 7. Bash Wrapper Shell Injection Risk

**Code** (`lib-intercore.sh:1306-1334`):

```bash
intercore_run_rollback() {
    local run_id="$1" target_phase="$2" reason="${3:-}"
    if ! intercore_available; then return 1; fi
    local args=(run rollback "$run_id" --to-phase="$target_phase")
    [[ -n "$reason" ]] && args+=(--reason="$reason")
    "$INTERCORE_BIN" "${args[@]}" ${INTERCORE_DB:+--db="$INTERCORE_DB"} 2>/dev/null
}
```

Arguments are passed via an array (`args`), not string concatenation with `eval`. Using `"${args[@]}"` is the correct bash pattern for safe argument passing — each element is a separate word, preventing word-splitting and glob expansion attacks.

However, there is one subtle concern: `--reason="$reason"` uses string concatenation to build the flag. If `reason` contains `"` or `$`, this is included literally because the assignment is inside `[[ ]]` and `args+=(...)` with double quotes. In bash, `args+=(--reason="$reason")` creates a single array element with the literal value of `$reason` appended to `--reason=`. This is safe because the `args` array is later expanded with `"${args[@]}"` which passes each element as a single word to `exec`. No shell injection.

**Verdict: Safe.** Bash array-based argument building with `"${args[@]}"` is correct.

---

### 8. `--dry-run` Non-Atomicity — Not a Security Issue, Deployment Consideration

The dry-run path in `cmdRunRollbackWorkflow` (`cmd/ic/run.go:124-135`) reads the run and computes `rolledBackPhases`, then returns without modifying the DB. This is a read-only operation.

There is no race condition concern here — the dry-run reads state at a point in time. If another process advances the run between the dry-run and the actual rollback, the actual rollback will still correctly recompute the chain from fresh state (it calls `pStore.Get` again inside `phase.Rollback`). The dry-run output may be stale by the time the user acts on it, but that is inherent to the pattern and not a security issue.

**Verdict: Safe.**

---

### 9. Rollback on Completed Runs — Status Reversion Risk

`RollbackPhase` explicitly reverts `status='completed'` runs back to `status='active'`:

```go
result, err := s.db.ExecContext(ctx, `
    UPDATE runs SET phase = ?, status = 'active', updated_at = ?, completed_at = NULL
    WHERE id = ?`,
    targetPhase, now, id,
)
```

This is intentional per the design (`RollbackPhase` doc comment: "If the run is in a terminal status (completed), it reverts to active"). Cancelled and failed runs are explicitly rejected.

The security consideration is: could an operator accidentally roll back a completed run they did not intend to? The CLI requires `--to-phase=<phase>` to be specified explicitly; there is no "rollback everything" shorthand. The dry-run flag (`--dry-run`) is available to preview before committing. This is adequate user-visible safety.

**Verdict: Acceptable as designed.** No additional safeguard needed.

---

### 10. Schema Migration v7→v8 — Deployment Safety

The migration adds `status TEXT NOT NULL DEFAULT 'active'` to `run_artifacts`. Analysis:

1. **Idempotency:** Uses `isDuplicateColumnError` guard — if the column already exists, the error is suppressed. Safe to re-run.
2. **Backward data compatibility:** Existing rows get `status='active'` as the default, which is correct — pre-migration artifacts should be treated as active.
3. **Forward compatibility:** The new `status` column has a non-null constraint with a default, so existing code that inserts without `status` will still work (SQLite will use the default). This is important for rollback: if the new binary is reverted to the old binary, the old binary's INSERT statements will still succeed (default fills the column).
4. **Rollback feasibility:** The old binary cannot run against a v8 schema (it would see `schema version is newer than this binary supports`). To revert, the user must restore from the pre-migration backup. This is the standard intercore rollback procedure documented in AGENTS.md.
5. **Backup creation:** Pre-migration backup is created automatically before migration runs, following the pattern established in v1-v7 migrations.

**One gap found:** The migration guard condition is `if currentVersion >= 4 && currentVersion < 8`. There is no migration path from `currentVersion < 4` to v8 for the `run_artifacts` table. However, v4 is when `run_artifacts` was created (per the test DDL in the new `TestMigrate_V7ToV8_ArtifactStatus`), and the comment in the code confirms: "Guard: run_artifacts exists from v4+. For v0-v3, the DDL below creates it with the column." The schema DDL includes `status TEXT NOT NULL DEFAULT 'active'` in the `CREATE TABLE IF NOT EXISTS run_artifacts` statement, so fresh installs and v0-v3 upgrades will get the column from the DDL apply step. Safe.

**Verdict: Migration is safe and idempotent.** Rollback requires backup restore, which is documented.

---

### 11. Trust Boundary Verification for New Functions

| Function | Callers | User input reaches? | Trust boundary |
|----------|---------|--------------------|----|
| `RollbackPhase` | `phase.Rollback` → `cmdRunRollbackWorkflow` | `targetPhase` (validated against chain) | Boundary: chain membership check |
| `MarkArtifactsRolledBack` | `cmdRunRollbackWorkflow` | `phases` from `ChainPhasesBetween` (derived from DB) | No direct user input |
| `FailAgentsByRun` | `cmdRunRollbackWorkflow` | `runID` only (parameterized) | No user-controlled data in SQL |
| `CancelByRun` | `cmdRunRollbackWorkflow` | `runID` only (parameterized) | No user-controlled data in SQL |
| `ListArtifactsForCodeRollback` | `cmdRunRollbackCode` | `runID`, `filterPhase` (both parameterized) | Parameterized |
| `Rollback` (machine.go) | `cmdRunRollbackWorkflow` | All inputs validated upstream | Internal |

**No trust boundary violations found.**

---

## Deployment Safety Analysis

### Pre-Deploy Invariants

| Invariant | Verification method |
|-----------|-------------------|
| Schema v8 migration completes idempotently | `ic init && ic version` → should show `schema: v8` |
| Existing artifacts show `status='active'` | `SELECT status, COUNT(*) FROM run_artifacts GROUP BY status` — only `active` before any rollback |
| New binary accepts old DB (v7 → v8 auto-migrate) | `ic health` exits 0 after binary swap |
| Old binary rejects new DB (schema too new) | Expected: error message "schema version is newer than this binary supports" |

### Rollout Strategy

This is a local CLI tool with no staged rollout or canary mechanism. The deploy path is:

```bash
go build -o ic ./cmd/ic
ic init    # triggers v7→v8 migration with automatic backup
ic health  # verify
```

The migration is the only step that requires care.

### Rollback Feasibility

- **Code rollback:** Revert binary to prior version. The v7 binary will refuse to open the v8 DB ("schema version is newer"). The user must also restore the DB from backup.
- **Data rollback:** Restore from the pre-migration backup (auto-created as `.clavain/intercore.db.backup-YYYYMMDD-HHMMSS`).
- **Artifact status column:** The `status` column uses a soft-delete pattern (`active`/`rolled_back`). There is no physical deletion of artifact rows. Rolling back a rollback operation would require updating `status='active'` for the affected rows — this is not yet exposed as a CLI command.
- **Irreversible operations:** `CancelByRun` transitions dispatches to `cancelled` status. If the underlying dispatch process was actually running and gets orphaned (the OS process still runs but intercore no longer tracks it as active), the process continues but has no tracking. This is a pre-existing pattern for `ic dispatch kill` and is acceptable.

### Partial Failure Handling

The rollback workflow in `cmdRunRollbackWorkflow` performs 4 sequential operations after the phase rewind:

1. `phase.Rollback` — phase + event + callback (one transaction-like unit)
2. `MarkArtifactsRolledBack` — separate DB call, returns warning on failure
3. `CancelByRun` — separate DB call, returns warning on failure
4. `FailAgentsByRun` — separate DB call, returns warning on failure

**Gap identified:** Operations 2-4 are independent and non-transactional. If `phase.Rollback` succeeds but `MarkArtifactsRolledBack` fails (e.g., disk error), the run's phase is rewound but artifacts remain `status='active'`. The CLI prints a warning (`"warning: artifact marking failed: ..."`) and continues. The final JSON output still reflects `marked_artifacts: 0`.

This is not a security issue, but it is a **consistency risk**: the system is in a partially-rolled-back state. There is no idempotent retry command for the artifact marking step alone.

**Mitigation:** Document in AGENTS.md that `ic run rollback` may leave artifacts in `active` status if the artifact marking step fails, and provide the manual recovery query.

### Post-Deploy Verification

```bash
# Verify schema version
ic version

# Verify rollback command is available
ic run rollback --help 2>&1 | grep -q "to-phase"

# Run integration test suite
bash test-integration.sh 2>&1 | grep -E "(E6|FAIL|pass)"

# Verify artifact status column
sqlite3 .clavain/intercore.db "SELECT name FROM pragma_table_info('run_artifacts') WHERE name='status'"
```

### Monitoring and Alert Coverage

First-hour failure modes specific to this feature:

1. **Migration failure:** `ic health` exits non-zero after binary swap → restore backup
2. **Rollback leaving active dispatches:** `ic dispatch list --active` shows unexpected dispatches after rollback → check `CancelByRun` log
3. **Artifact status not updated:** Query `run_artifacts WHERE status='active' AND phase IN (...)` after rollback — should return 0 active rows for rolled-back phases

---

## Summary of Findings

### Security Findings

**No exploitable security vulnerabilities found.** All SQL is parameterized correctly, including the dynamic IN clause in `MarkArtifactsRolledBack`. Phase names are validated against the chain before any SQL operation. The `fmt.Printf` text output is not a format string injection risk in Go. Bash wrappers use array-based argument passing.

### Quality / Improvement Findings

1. **Unknown `--format` values silently treated as JSON** — minor UX issue, not a security concern.
2. **`--reason` stored persistently in audit trail without redaction** — residual risk if users put secrets there; consistent with existing `--goal` behavior.
3. **Partial failure leaves system in inconsistent state** — artifact marking, dispatch cancellation, and agent failure marking are not transactional with the phase rewind.

### Deployment Findings

4. **Schema rollback requires both binary revert and DB restore** — the v7 binary will refuse the v8 schema. Both steps must be performed in concert. Document this explicitly.
5. **No retry path for partial rollback** — if `MarkArtifactsRolledBack` fails mid-operation, there is no `ic run rollback --retry-artifacts` command. Manual SQL required.

---

## Verdict

**safe** — No security changes required before ship. The implementation is consistent with the existing security posture of the codebase: parameterized SQL throughout, no format string vulnerabilities, no trust boundary violations. Deployment improvements (transactionality of side-effect steps, documented rollback procedure) are recommended but not blocking.
