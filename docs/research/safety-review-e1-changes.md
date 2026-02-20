# Safety Review — E1 Kernel Primitives Changes

Reviewer: Flux-drive Safety Reviewer
Date: 2026-02-19
Primary output: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md`

## Threat Model

- **System type:** Local CLI tool (`ic`) backed by a single SQLite file, consumed by bash hooks. Not network-facing. No public endpoints.
- **Untrusted inputs:** CLI flag values (`--phases`, `--path`, `--reason`, `--actor`, `--in/--out/--cache`, `--token-budget`, `--budget-warn-pct`). All come from the operator or scripts the operator controls.
- **Credentials/secrets:** None in this change. The DB contains run/dispatch state only.
- **Deployment path:** Manual `go build`, binary copied into path. Schema migration is auto-applied on `ic init` / first use. Pre-migration backup is created automatically.
- **Change risk classification:** Low-Medium. No auth changes, no network exposure, no credential flows. New migration (v5→v6) is additive-only with idempotency guard.

## SQL Injection Assessment

All new queries in this diff are clean:

- `AggregateTokens` (dispatch.go): `WHERE scope_id = ?` — parameterized.
- `UpdateTokens` (dispatch.go): column names validated against `allowedUpdateCols` before string-interpolation into SET clause; values bound as `?` parameters.
- `SkippedPhases` (phase/store.go): `WHERE run_id = ? AND event_type = 'skip'` — parameterized; `event_type` is a hardcoded literal.
- `SkipPhase` (phase/store.go): calls `AddEvent` with struct fields; underlying INSERT is parameterized.
- New `runCols` constant: column list used in SELECT, not user-controlled.
- Budget checker: reads via `phaseStore.Get()` and `AggregateTokens()`, both parameterized.

No SQL injection risk in the E1 diff.

## Path Traversal Assessment

The `hashFile` function in `internal/runtrack/store.go` opens the caller-supplied `a.Path` directly via `os.Open`. The path comes from `--path=` on the CLI:

```go
func hashFile(path string) (string, error) {
    f, err := os.Open(path)
    ...
    io.Copy(h, f)
    ...
}
```

This is a local CLI where the operator controls the input. The primary concern is:
1. Scripts that construct `--path` from agent output or directory listings could inadvertently supply symlinks or very large files.
2. No size cap — `io.Copy` on `/dev/urandom` or a 10 GB file would run until OOM or cancellation.

The `--db` flag has a traversal guard (AGENTS.md: "no `..`, `.db` extension, under CWD"). The `--path` flag has no such guard. This is acceptable for the current threat model but should be documented.

## Input Validation Assessment

**`--phases` JSON parsing:** `ParsePhaseChain` validates JSON array structure, minimum 2 phases, and no duplicates. It does not restrict the character set of phase names. Phase names stored in `phase_events.to_phase`/`from_phase` via parameterized INSERT (no injection risk), but control characters in names would corrupt text output.

**`--token-budget`:** Parsed with `strconv.ParseInt`, must be positive (`v <= 0` rejected). Clean.

**`--budget-warn-pct`:** Parsed with `strconv.Atoi`, range check `v < 1 || v > 99`. Clean.

**`--in`, `--out`, `--cache` token counts:** Parsed with `strconv.Atoi`. No upper bound — a caller could supply `math.MaxInt32`. In practice these are token counts so extremes are self-limiting. The aggregation query uses `COALESCE(SUM(...), 0)` which handles NULL correctly.

**`--actor` and `--reason`:** Free text strings. Written into the phase_events audit log as `"actor=<actor>: <reason>"`. No injection risk (parameterized INSERT), but no length cap.

## Column Allowlist Assessment

`allowedUpdateCols` in `internal/dispatch/dispatch.go` is the gating mechanism for dynamic UPDATE queries. The diff adds `"cache_hits": true` to the existing allowlist. Both `UpdateStatus` and the new `UpdateTokens` check this allowlist before interpolating column names into the SET clause.

Verification: the column name string `col+" = ?"` is concatenated where `col` is from the allowlist map — only alphanumeric/underscore names are in the map. Values are always bound as `?`. This is safe.

## Schema Migration Assessment

The v5→v6 migration in `internal/db/db.go` runs inside the existing migration transaction:

```go
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
        if !isDuplicateColumnError(err) {
            return fmt.Errorf("migrate v5→v6: %w", err)
        }
    }
}
```

- All statements are `ALTER TABLE ... ADD COLUMN` with no user input — no injection risk.
- All new columns are nullable or have defaults, so existing rows are unaffected.
- The transaction wraps all statements; a failure on any non-duplicate-column error rolls back.
- The idempotency guard `isDuplicateColumnError` uses `strings.Contains(err.Error(), "duplicate column name")` — correct for modernc.org/sqlite but string-fragile.
- Two tests cover: happy-path migration, and double-migration (idempotency). No test for a partial failure mid-migration.
- Pre-migration backup is created by the existing backup mechanism (not changed in this diff).

Safety: the migration is safe to run on a live DB (SQLite single-writer, WAL mode). It is additive-only. Rollback is the pre-migration backup.

## Key Findings Summary

Five findings, all LOW or INFO severity:

1. **S-01 (LOW):** `hashFile` opens any user-supplied path without size cap or traversal guard.
2. **S-02 (LOW):** Custom phase names stored verbatim — no character allowlist; control chars could corrupt text output.
3. **S-03 (LOW):** `isDuplicateColumnError` uses fragile string match on SQLite error text.
4. **S-04 (INFO):** `budget.Check()` swallows all phaseStore errors, returning `(nil, nil)` for both "not found" and actual DB errors.
5. **S-05 (INFO):** `${INTERCORE_DB:+--db="$INTERCORE_DB"}` is unquoted in shell wrappers (pre-existing pattern replicated into new wrappers).

**Overall verdict: safe.** No exploitable security issues in the current threat model. The deployment (migration) is additive and reversible via backup. No auth, credential, or network surface changes.

Full findings at: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md`
