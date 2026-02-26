# Safety Review: Coordination Bridge and Interlock icclient

**Date:** 2026-02-25
**Reviewer:** Flux-Drive Safety Reviewer
**Risk Classification:** Medium (new coordination pathway between two internal systems, no external network exposure, no user-facing auth changes)
**Files Reviewed:**
- `/home/mk/projects/Demarch/core/intermute/internal/storage/sqlite/coordination_bridge.go`
- `/home/mk/projects/Demarch/interverse/interlock/internal/icclient/icclient.go`
- `/home/mk/projects/Demarch/interverse/interlock/hooks/pre-edit.sh`

---

## Threat Model

- **Network exposure:** None. The bridge opens a local SQLite file directly. icclient shells out to the `ic` binary on PATH. The pre-edit hook runs on the developer's workstation as the agent's user.
- **Untrusted input:** File paths come from Claude Code's `tool_input.file_path` (agent-controlled), and conflict data comes from `ic` JSON output (subprocess-controlled). `INTERMUTE_AGENT_ID` and `INTERMUTE_PROJECT` come from the environment (trusted at agent session start). `REASON` and `HELD_BY` are extracted from intermute's HTTP response JSON, which is internal.
- **Credentials:** None stored or processed in these files. `INTERMUTE_AGENT_ID` is an identity, not a secret.
- **Deployment path:** Interlock is a Claude Code plugin; the bridge is compiled into the Intermute binary. No migration steps in scope.

---

## Findings

### FINDING 1 — HIGH: Unquoted shell variables interpolated directly into JSON heredoc (pre-edit.sh line 195)

**File:** `/home/mk/projects/Demarch/interverse/interlock/hooks/pre-edit.sh`, line 195

**Code:**
```bash
{\"decision\": \"block\", \"reason\": \"INTERLOCK: ${REL_PATH} is exclusively reserved by ${HELD_BY} (${REASON_DISPLAY}expires ${EXPIRES_DISPLAY}). Work on other files, use request_release(agent_name=\\\"${HELD_BY}\\\"), or wait for expiry.\"}
```

**Problem:** `REL_PATH`, `HELD_BY`, `REASON_DISPLAY`, and `EXPIRES_DISPLAY` are interpolated directly into the JSON heredoc string without jq protection. Any of these values containing a double-quote, backslash, newline, or control character will produce malformed JSON. More critically:

- `HELD_BY` comes from intermute's HTTP response via `jq -r '.held_by // "unknown"'`. If intermute is compromised or returns crafted data, an attacker can inject arbitrary JSON by embedding `", "decision": "allow"` into the `held_by` field.
- `REASON_DISPLAY` is assembled at line 190 as `"\"${REASON}\", "` where `REASON` is also from intermute response data. This wraps the raw value in escaped quotes but those are shell-level escapes, not JSON-level encoding. A value like `foo", "bar` breaks the outer structure.
- `REL_PATH` originates from `FILE_PATH` which comes from `tool_input.file_path` via `jq -r`. A filename containing a double quote would break the JSON.

**Impact:** Malformed JSON output from a Claude Code hook causes the hook to behave unpredictably. More seriously, a crafted `held_by` value from an intermute server could inject a `"decision": "allow"` field into the JSON, causing Claude Code to permit an edit that should have been blocked. This is a trust-boundary crossing from the HTTP API (even internal) into the hook decision channel.

**Mitigation:** Replace the bare heredoc interpolation with a `jq -nc` call for the block line, the same way the ic-path block at lines 143-144 already does it correctly:

```bash
jq -nc \
  --arg fp "$REL_PATH" \
  --arg hb "$HELD_BY" \
  --arg rd "$REASON_DISPLAY" \
  --arg ed "$EXPIRES_DISPLAY" \
  '{"decision": "block", "reason": ("INTERLOCK: " + $fp + " is exclusively reserved by " + $hb + " (" + $rd + "expires " + $ed + "). Work on other files, use request_release(agent_name=\"" + $hb + "\"), or wait for expiry.")}'
```

The ic-path at lines 143-144 uses `jq --arg` correctly and is safe. The intermute-HTTP path at line 195 does not and is the only affected location.

---

### FINDING 2 — MEDIUM: `normalizeScope()` runs `git rev-parse` without a working-directory constraint (coordination_bridge.go line 53)

**File:** `/home/mk/projects/Demarch/core/intermute/internal/storage/sqlite/coordination_bridge.go`, lines 49-62

**Code:**
```go
func normalizeScope(project string) string {
    if filepath.IsAbs(project) {
        return filepath.Clean(project)
    }
    out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
    if err == nil {
        return strings.TrimSpace(string(out))
    }
    abs, err := filepath.Abs(project)
    ...
}
```

**Problem 1 — No injection risk but incorrect fallback semantics:** `exec.Command("git", ...)` is safe against shell injection because it does not invoke a shell. Arguments are passed directly to `execve`. There is no injection vulnerability here.

**Problem 2 — CWD dependency is fragile:** When `project` is relative, `normalizeScope` calls `git rev-parse --show-toplevel` with no `Dir` constraint on the `exec.Cmd`. This runs in the Intermute server's CWD, which may have nothing to do with the project being mirrored. The result could be a completely unrelated git root, causing all cross-system conflict checks to silently use the wrong scope.

**Scenario:** Intermute is started from `/home/mk/projects/Demarch/core/intermute/`. Agent A works on project `/home/mk/projects/Demarch/apps/autarch/` and passes `project="autarch"`. `normalizeScope` returns `/home/mk/projects/Demarch/core/intermute` (Intermute's own repo root), not the autarch root. The mirror write succeeds but the scope is wrong; `ic coordination check` against the correct autarch scope finds no conflict.

**Problem 3 — Silent scope aliasing:** If two different relative project names map to the same git toplevel (possible in a monorepo), their scopes become indistinguishable. All reservations from both projects block each other.

**Mitigation:** Accept an optional working directory parameter or require callers to pass absolute paths. The caller in `MirrorReserve` already receives `project` from the Intermute HTTP handler; that handler can canonicalize the path before calling the bridge. Alternatively, add a `workDir string` parameter to `normalizeScope` and set `cmd.Dir = workDir` when non-empty.

---

### FINDING 3 — MEDIUM: Argument injection via `--flag=value` assignment form in icclient.go

**File:** `/home/mk/projects/Demarch/interverse/interlock/internal/icclient/icclient.go`, lines 56-64, 94, 110

**Code:**
```go
args := []string{"--json", "coordination", "reserve",
    "--owner=" + owner, "--scope=" + scope, "--pattern=" + pattern,
    fmt.Sprintf("--ttl=%d", ttlSec)}
if reason != "" {
    args = append(args, "--reason="+reason)
}
```

**Problem:** The `=`-assignment form is generally safe against shell injection because `exec.Command` does not invoke a shell. However, values prefixed with `-` could be misinterpreted by `ic`'s flag parser. If `owner`, `scope`, `pattern`, or `reason` begins with `--`, it could match a subsequent flag and alter `ic`'s behavior.

For example: `owner = "--exclusive=false"` would produce `--owner=--exclusive=false`. Most flag parsers (including Go's `flag` package and `cobra`) parse this as a single `--owner` assignment with value `--exclusive=false`, so this is likely benign with standard Go flag libraries. However it should be confirmed against the `ic` flag library in use.

**Immediate impact:** Low for the `ic` coordination subcommands reviewed. The `ic` binary's `--db` flag validates path traversal (per CLAUDE.md), and the coordination subcommands use these values as database query parameters (bound via `?`), not as shell commands. The blast radius if `ic` uses cobra/pflag is minimal.

**Mitigation (low urgency):** If any future `ic` subcommand echoes arguments into shell execution or file paths, this pattern becomes exploitable. Add input validation in icclient.go that rejects values starting with `-` for string parameters, or use separate flag positional forms: `"--owner", owner` (space-separated). The latter form is less ambiguous with standard flag parsers.

---

### FINDING 4 — LOW: `PULL_CONTEXT` heredoc at line 57-58 (pre-edit.sh) is safe but worth noting

**File:** `/home/mk/projects/Demarch/interverse/interlock/hooks/pre-edit.sh`, lines 55-59

**Code:**
```bash
cat <<ENDJSON
{"additionalContext": "${PULL_CONTEXT}"}
ENDJSON
```

`PULL_CONTEXT` is hardcoded to one of two literal string constants at lines 43 and 47. It does not incorporate any external data. This is safe as-is.

**Note for future maintainers:** If `PULL_CONTEXT` ever incorporates branch names, commit messages, or other git-derived strings, this heredoc becomes the same vulnerability as Finding 1. Use `jq -nc --arg ctx "$PULL_CONTEXT" '{"additionalContext": $ctx}'` preemptively if the variable source changes.

---

### FINDING 5 — LOW: `coordination_bridge.go` error messages include the database path

**File:** `/home/mk/projects/Demarch/core/intermute/internal/storage/sqlite/coordination_bridge.go`, line 33

**Code:**
```go
return nil, fmt.Errorf("coordination bridge open: %w", err)
```

The error wraps the SQL driver's error, which in turn includes the DSN: `"file:<dbPath>?_pragma=..."`. This path is exposed in:
1. Intermute's log output (if it logs the startup error)
2. Any API endpoint that proxies the initialization error to a caller

**Assessment:** Since `dbPath` is the Intercore SQLite file path under `.clavain/`, this reveals the project's `.clavain` directory location. For a local-only tool this is low risk. If Intermute ever exposes an HTTP endpoint that returns raw startup errors, this becomes a path disclosure finding worth elevating.

**Mitigation:** Wrap errors with a fixed message that excludes the path detail: `fmt.Errorf("coordination bridge: db open failed: %w", err)` and strip the DSN from the underlying error before wrapping, or use `errors.New("coordination bridge: db open failed")` and log the path separately to a privileged log channel.

---

## Fail-Open / Fail-Closed Analysis

### coordination_bridge.go — Correct fail-open for mirror writes

`MirrorReserve` and `MirrorRelease` log errors but never return them. This is the stated design ("Errors are logged but never returned — the bridge must not fail the primary operation"). For a dual-write migration layer this is the correct choice: Intermute's primary `file_reservations` table remains authoritative, and a bridge failure should not block an agent's reservation.

**Residual risk:** If the bridge consistently fails silently (e.g., `coordination_locks` table missing after a schema migration), the cross-system conflict detection degrades without alerting operators. Consider emitting a metric counter or rate-limiting the log output to detect persistent bridge failures during the migration window.

### pre-edit.sh — Correct fail-open for connectivity loss

Lines 154-165 correctly handle `interlock-check.sh` failures: if intermute is unreachable and no connected-flag exists, the hook exits 0 (allow) silently. If the connected flag exists, it emits a one-time warning and allows. This is correct for an advisory hook.

### icclient.go — Correct propagation of errors to callers

`run()` returns errors to callers. Callers (`Reserve`, `ReleaseAll`, `Check`) propagate them. The MCP tool layer above icclient decides fail-open vs fail-closed per tool. This is appropriate separation.

### `NewCoordinationBridge` — Correct fail-closed on table-missing

If `coordination_locks` does not exist, `NewCoordinationBridge` returns an error rather than silently constructing a disabled bridge. The caller (Intermute startup) must decide whether to abort or continue without the bridge. This is the right posture: fail loudly at startup rather than silently losing mirror writes.

---

## SQL Injection Assessment

All SQL in `coordination_bridge.go` uses parameterized queries exclusively:

- Line 38: `SELECT COUNT(*) FROM coordination_locks LIMIT 1` — no parameters needed, literal query, safe.
- Lines 80-83: `INSERT OR IGNORE INTO coordination_locks ... VALUES (?, ...)` — 9 positional `?` placeholders for all user-controlled values.
- Lines 94-95: `UPDATE coordination_locks SET released_at = ? WHERE id = ? AND released_at IS NULL` — 2 positional placeholders.

No dynamic table name construction, no string interpolation into SQL. **No SQL injection vulnerabilities found.**

---

## icclient.go — No Shell Injection

`exec.Command(c.binary, args...)` passes arguments directly to `execve` without a shell interpreter. String concatenation in argument construction (`"--owner=" + owner`) does not create shell injection because there is no shell involved. The binary path `c.binary` is from `exec.LookPath("ic")` which resolves to the first `ic` on `PATH`. **No shell injection vulnerabilities found in icclient.go.**

---

## Summary Table

| Finding | Severity | File | Risk | Status |
|---------|----------|------|------|--------|
| Unquoted vars in JSON heredoc (line 195) | HIGH | pre-edit.sh | Decision injection from crafted intermute response | Fix before merge |
| `normalizeScope` CWD dependency | MEDIUM | coordination_bridge.go | Wrong scope silently defeats conflict detection | Fix before merge |
| `--flag=value` argument form | MEDIUM | icclient.go | Low impact now, escalates if ic adds shell-exec paths | Fix opportunistically |
| `PULL_CONTEXT` heredoc (safe today) | LOW | pre-edit.sh | Future regression risk | Add comment warning |
| DB path in error messages | LOW | coordination_bridge.go | Path disclosure in logs | Fix opportunistically |

---

## Go / No-Go Decision

**No-Go for line 195 of pre-edit.sh without fix.** The intermute HTTP path produces an unprotected JSON heredoc that a crafted `held_by` value could turn into a `"decision": "allow"` injection. Given that the ic-path in the same file (lines 143-144) already uses `jq --arg` correctly, the fix is a one-line change following the established pattern.

All other findings are medium/low and do not block deployment, but Finding 2 (`normalizeScope` CWD) should be fixed before the dual-write migration goes to production, because a wrong scope silently makes the entire cross-system conflict detection ineffective for relative project identifiers.

The SQL and Go exec layers are clean.
