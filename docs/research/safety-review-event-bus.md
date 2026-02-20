# Safety Review: Wave 2 Event Bus Implementation

**Date:** 2026-02-18
**Reviewer:** Flux-drive Safety Reviewer (Claude Sonnet 4.6)
**Scope:** 27 files, ~1,500 lines — Go CLI, SQLite backend, shell hook execution handler, CLI event commands, bash wrappers.
**Change version:** v0.2.0 → v0.3.0 (binary), schema v4 → v5

---

## Context and Threat Model

intercore is a local CLI tool (`ic`) backed by a single-user SQLite database. It is invoked by Clavain hook scripts running on a trusted developer workstation or server. There is no network-facing API surface, no remote authentication, no web server. All callers are the local user or scripts run as that user.

**Trust boundaries:**
- The hook scripts in `.clavain/hooks/` are operator-controlled files. They are trusted by definition.
- The event data (run IDs, phase names, status strings) originates from the SQLite database, which the local user controls.
- The `--consumer` name in `ic events tail` comes from the calling hook script (operator-controlled).
- No network-accessible input paths exist in this diff.

This means the realistic threat model is: accidental self-injection via mis-quoting, unbounded resource use by a buggy hook, and data loss or migration failure during the v4→v5 upgrade.

---

## 1. Shell Hook Execution (handler_hook.go) — PRIMARY CONCERN

### What was added

`internal/event/handler_hook.go` adds a `NewHookHandler` that:
1. Resolves a hook path: `filepath.Join(projectDir, ".clavain", "hooks", hookName)` where `hookName` is one of two hard-coded constants (`on-phase-advance`, `on-dispatch-change`).
2. Checks the file exists, is not a directory, and has at least one execute bit set.
3. Serializes the event to JSON via `json.Marshal`.
4. Runs the hook in a goroutine via `exec.CommandContext(hookCtx, hookPath)` — **no shell, no interpolation**.
5. Passes event data exclusively via **stdin** as JSON bytes.
6. Captures stderr into a `bytes.Buffer` for logging.

### Shell injection assessment: NONE

There is no shell interpolation. `exec.CommandContext` is used with the hook path as the first argument and no additional arguments. Event data (run_id, phase names, status strings) is passed as JSON on stdin, not as command-line arguments or environment variables. The hook name is one of two compile-time constants. The project directory comes from the database record, not from untrusted user input.

**Verdict: No shell injection risk in this implementation.**

### Path traversal assessment: NONE

The hook path is constructed with `filepath.Join(projectDir, ".clavain", "hooks", hookName)`. The hook name is a compile-time constant. `projectDir` comes from the `runs` table `project_dir` column, which is set at run creation time by the CLI user. Since the tool runs as the local user and all paths are under that user's control, there is no cross-user path traversal risk. The existing `--db` path validation (no `..`, must be under CWD, parent not a symlink) is not replicated for `projectDir`, but this is consistent with the trust model: the user who created the run controls that directory.

### Finding S1: Unbounded stderr buffer (LOW)

The goroutine in `handler_hook.go` captures hook stderr into a `bytes.Buffer` with no size limit:

```go
var stderr bytes.Buffer
cmd.Stderr = &stderr
```

A hook that writes large output to stderr (e.g., a debugging hook that `cat`s a large file to stderr) will hold that memory until the goroutine exits (up to 5 seconds via `hookTimeout`). Multiple concurrent phase advances could accumulate multiple such goroutines. In this local-only, single-user context the blast radius is a transient memory spike on the `ic` process — it cannot be triggered by an external attacker. But it is inconsistent with the fire-and-forget design intent.

**Mitigation:** Replace `var stderr bytes.Buffer` with `stderr := io.LimitedReader{R: cmd.Stderr, N: 4096}` or use `cmd.Stderr = io.Discard` and only log that a failure occurred. File: `internal/event/handler_hook.go:61`.

### Goroutine leak assessment: LOW concern

The hook goroutine uses `context.WithTimeout(context.Background(), hookTimeout)` (5s). This correctly detaches from the parent context, which is appropriate since the DB connection is single-writer and the hook should not hold it. The 5-second timeout is enforced. There is no leak path; goroutines will always exit at or before the timeout. The one residual concern is that if `ic run advance` is called in a tight loop (unlikely for this use case), many goroutines could accumulate briefly. This is acceptable given the operational context.

---

## 2. Dynamic SQL Column Construction (dispatch.go) — LOW

### What exists (pre-existing, not new)

`UpdateStatus` builds a dynamic SET clause by concatenating map keys as column names:

```go
for col, val := range fields {
    sets = append(sets, col+" = ?")
    args = append(args, val)
}
query := "UPDATE dispatches SET " + joinStrings(sets, ", ") + " WHERE id = ?"
```

Values are parameterized (`?`). Column names are not.

### Changed in this diff

The diff wraps `UpdateStatus` in a transaction (previously it was a bare `ExecContext`). The event recorder is fired outside the transaction. This change does not introduce the dynamic SQL pattern — it pre-existed.

### Injection risk assessment

All current call sites use hardcoded string literal keys:
- `collect.go`: `"turns"`, `"commands"`, `"messages"`, `"completed_at"`, `"verdict_status"`, `"verdict_summary"`, `"input_tokens"`, `"output_tokens"`, `"exit_code"`, `"error_message"`
- `spawn.go`: `"pid"`, `"error_message"`
- Tests: same literals

No call site accepts user-controlled column names. A future caller passing an attacker-controlled key would produce a SQL syntax error (not data exfiltration) because values remain parameterized. The risk is structural: the API design invites future misuse.

**Finding S2:** Add a compile-time or runtime allowlist of valid `dispatches` column names. This eliminates the risk class before it can be exercised. File: `internal/dispatch/dispatch.go:218-223`.

---

## 3. Cursor State: JSON Payload from DB Without Re-validation

### loadCursor behavior

`loadCursor` reads from the `state` table (key=`"cursor"`, scope=consumer+runID), then `json.Unmarshal`s the payload into a struct with two `int64` fields:

```go
var cursor struct {
    Phase    int64 `json:"phase"`
    Dispatch int64 `json:"dispatch"`
}
if err := json.Unmarshal(payload, &cursor); err != nil {
    return 0, 0
}
```

If the JSON is malformed or contains unexpected fields, the function safely returns `(0, 0)` — full re-read from the beginning. The extracted values (`cursor.Phase`, `cursor.Dispatch`) are used as `WHERE id > ?` parameters in parameterized SQLite queries. There is no interpolation path.

**Assessment: Safe.** The cursor payload is correctly validated through typed deserialization, and the extracted values are integer IDs used only in parameterized queries.

### Finding S4 (INFO): Consumer name + run_id key without character sanitization

The cursor key is constructed as `consumer + ":" + scope` and stored as a state table row. Because the state table uses parameterized queries throughout, there is no SQL injection. However, a consumer name containing a tab character would corrupt the `cursor list` output parsed by the bash wrapper `intercore_events_cursor_get`, because it uses tab as the field separator. This is an operational hygiene note, not a security issue, given that consumer names are operator-chosen.

---

## 4. lib-intercore.sh Bash Wrappers

### intercore_events_tail

```bash
$INTERCORE_BIN events tail "$run_id" "$@" 2>/dev/null
```

`$run_id` is correctly double-quoted. `"$@"` passes remaining arguments safely. The `$INTERCORE_BIN` variable expands to the binary path (set earlier in the file). No word splitting or glob expansion risk.

### intercore_events_cursor_set

```bash
echo "{\"phase\":${phase_id},\"dispatch\":${dispatch_id}}" | \
    $INTERCORE_BIN state set "cursor" "$consumer" --ttl=24h 2>/dev/null
```

`phase_id` and `dispatch_id` come from the caller as arguments. If a caller passes a non-numeric value (e.g., `phase_id="1; rm -rf /"`), this would produce malformed JSON that fails state validation. This is a local, operator-controlled context, and the state store's JSON size and depth limits provide a backstop. Not a realistic attack path.

### Finding S3: Consumer name unquoted in grep pattern (LOW)

```bash
$INTERCORE_BIN events cursor list 2>/dev/null | grep "^${consumer}	" | cut -f2 || echo ""
```

`${consumer}` is interpolated into a grep ERE pattern without escaping. If the consumer name contains grep metacharacters (`.`, `[`, `*`, `^`, `$`), the pattern will match unintended lines or fail. Using `grep -F` (fixed-string) would eliminate this. File: `lib-intercore.sh:408`.

---

## 5. Schema Migration v4 to v5

### What changes

The v5 migration adds one new table:

```sql
CREATE TABLE IF NOT EXISTS dispatch_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    dispatch_id     TEXT NOT NULL,
    run_id          TEXT,
    from_status     TEXT NOT NULL,
    to_status       TEXT NOT NULL,
    event_type      TEXT NOT NULL DEFAULT 'status_change',
    reason          TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
```

Plus three indexes on `dispatch_id`, `run_id`, and `created_at`.

### Data loss risk: NONE

The migration is purely additive. `CREATE TABLE IF NOT EXISTS` is idempotent. No existing tables are altered. No columns are dropped. No data is backfilled. Existing rows in `state`, `sentinels`, `dispatches`, `runs`, `phase_events`, `run_agents`, `run_artifacts` are untouched.

### Migration safety: GOOD

- Pre-migration backup is created automatically (`copyFile` in `db.go:Migrate`) before any DDL is applied.
- Schema version is read inside the transaction to prevent TOCTOU.
- The migration runs inside a transaction; if it fails, no partial schema is committed.
- Multiple concurrent migrations are serialized via `CREATE TABLE IF NOT EXISTS _migrate_lock`.
- `ic init` is idempotent if run twice.

**Verdict: v4→v5 migration is safe with no data loss risk.**

### Finding S5 (INFO): dispatch_events.run_id lacks FK constraint

`phase_events.run_id` has `REFERENCES runs(id)` with FK enforcement on. `dispatch_events.run_id` is plain `TEXT` with no FK. This is intentional (dispatch events may not have a run) but means orphaned records will accumulate without a pruning mechanism. File: `internal/db/schema.sql:113`.

---

## 6. Event Bus Architecture: Transaction Boundaries and Fire-and-Forget

### UpdateStatus transaction upgrade

The diff wraps `UpdateStatus` in a transaction that:
1. Reads the previous status (SELECT).
2. Updates the dispatch status (UPDATE).
3. Commits.
4. Fires the event recorder **outside** the transaction.

The event recorder (`dispatchRecorder` in `run.go`) calls `evStore.AddDispatchEvent` (a separate INSERT) and then `notifier.Notify`. This means:

- If `AddDispatchEvent` fails, the status change is already committed. The event is lost but the state is consistent. This is correct for a fire-and-forget audit log.
- If the notifier fires the hook handler and the hook fails, neither the status update nor the event INSERT is affected. The hook failure is logged to stderr.

This is the correct design for an observability bus that must not block or undo business operations.

### Phase callback

`PhaseEventCallback` is called after `Advance` completes its DB writes but before returning to the caller. It is called synchronously in the caller's goroutine (not in a background goroutine) and the result is discarded:

```go
if callback != nil {
    callback(runID, eventType, fromPhase, toPhase, reason)
}
```

The callback in `run.go` calls `notifier.Notify`, which in turn calls the hook handler. The hook handler spawns a goroutine and returns immediately. So the chain is: Advance → callback → Notify → hook goroutine (async). The DB is not held during hook execution. This is correct.

---

## 7. Deployment Risk Assessment

**Change classification: LOW**

- No auth changes, no credential flows, no permission model updates.
- No irreversible data changes (migration is additive with backup).
- New endpoints (`events tail`, `events cursor`) are additive.
- Schema change is a new table only.

### Pre-deploy checks

1. `go build -o ic ./cmd/ic` — binary builds without errors.
2. `go test ./...` — all unit tests pass (114+).
3. `go test -race ./...` — no race conditions detected.
4. `bash test-integration.sh` — integration tests pass including new Event Bus section.
5. `ic version` — reports v0.3.0 and schema v5 after migration.

### Deploy sequence

```bash
go build -o /home/mk/go/bin/ic ./cmd/ic
ic init    # applies v5 migration, creates backup automatically
ic version # verify: "schema: v5"
ic health  # verify DB health
```

### Rollback feasibility

- Code rollback: rebuild from previous commit. The old binary will refuse to open a v5 DB (`ErrSchemaVersionTooNew`). However, the `dispatch_events` table is only written to by the new code path; the old binary does not read or write it.
- **Rollback blocker:** After running `ic init` with the v5 binary, the old v0.2.0 binary cannot open the database because `user_version = 5 > maxSchemaVersion = 4`. This is the standard intercore rollback constraint documented in `docs/solutions/patterns/intercore-schema-upgrade-deployment-20260218.md`.
- **Recovery path:** Restore the automatic backup (`intercore.db.backup-YYYYMMDD-HHMMSS`) if the old binary is needed. The backup is created before migration, so it is at v4 and fully compatible with the old binary.
- **Practical risk:** The only thing that changes at runtime is that `ic run advance` now fires the event bus. If that causes unexpected behavior, downgrading means restoring the backup and the old binary. No externally observable state (files, other services) is affected.

### Post-deploy verification

```bash
ic health
ic run create --project=. --goal="smoke test"  # get RUN_ID
ic run advance $RUN_ID --priority=4
ic events tail $RUN_ID                          # should return at least one phase event
ic events cursor list                           # should be empty (no named consumers yet)
```

### Monitoring

No new external dependencies. No new network calls. The event bus is entirely in-process. No additional monitoring beyond existing `ic health` checks is required.

---

## Summary of Findings

| ID | Severity | Title | Action |
|---|---|---|---|
| S1 | LOW | Hook stderr buffer unbounded | Cap at 4 KB with io.LimitedReader |
| S2 | LOW | Dynamic SQL column names lack allowlist | Add column name allowlist to UpdateStatus |
| S3 | LOW | Consumer name unquoted in grep pattern | Use grep -F |
| S4 | INFO | Cursor key unsanitized (tab/colon risk in shell output) | Document or validate consumer name charset |
| S5 | INFO | dispatch_events.run_id lacks FK constraint | Add ic events prune subcommand for future maintenance |

**Overall verdict: SAFE TO DEPLOY.** No findings block deployment. The hook handler avoids shell injection by design (exec without shell, data via stdin). The migration is safe and reversible via automatic backup. The dynamic SQL pattern (S2) is a pre-existing structural concern to address in a follow-up, not a current exploitable path.
