# Security and Safety Review — Run Tracking Implementation (v4)

Generated: 2026-02-18
Reviewer: Flux-drive Safety Reviewer (claude-sonnet-4-6)
Diff: /tmp/qg-diff-1771457938.txt
Primary output: `.clavain/quality-gates/fd-safety.md`

---

## System Context

intercore is a local-only Go CLI (`ic`) backed by a single-writer SQLite WAL database. It is invoked by bash hooks inside the Clavain multi-agent framework. There is no network exposure, no authentication layer, and no remote callers. All inputs arrive via the process argv of a local CLI. This shapes which risks are realistic.

Threat model:
- Callers are trusted local processes (bash hooks, Clavain agents, developer shell).
- The DB is at `.clavain/intercore.db`, path-controlled by `validateDBPath()`.
- No credentials, API keys, or secrets pass through any changed path.
- No network binding, no HTTP layer, no RPC.

Risk classification for this change: LOW-MEDIUM. New user-facing flags (`--path=`, `--type=`, `--status=`, `--phase=`) and new bash wrappers expand the input surface, but the backend is trusted. The schema migration is additive and reversible.

---

## Areas Reviewed

### 1. CLI Argument Parsing (cmd/ic/run.go, cmd/ic/dispatch.go)

The `--flag=value` parsing pattern used throughout is the project standard. All new commands in `run.go` (cmdRunCurrent, cmdRunAgentAdd, cmdRunAgentList, cmdRunAgentUpdate, cmdRunArtifactAdd, cmdRunArtifactList) follow the same switch-on-HasPrefix pattern used by the existing dispatch commands that were extracted to `dispatch.go`. There is no shell interpolation, no exec call, and no template rendering of flag values.

Numeric flags (`--priority=`, `--complexity=`) are validated with `strconv.Atoi` and range checks before use. Duration flags (`--timeout=`, `--older-than=`) are validated with `time.ParseDuration`. Boolean flags (`--auto-advance=`, `--force-full=`) are validated with `strconv.ParseBool`. These types are all correctly handled.

The one gap: `--status=` in `cmdRunAgentUpdate` (run.go line 689) accepts any string without a whitelist check. The value is passed to `store.UpdateAgent`, which writes it directly to the `run_agents.status` column. This is described in finding S-01.

No injection risk exists in the CLI parsing: flag values become SQL parameters or struct fields, never interpolated into queries or shell commands.

### 2. SQL Queries (internal/runtrack/store.go)

All six SQL operations in `store.go` use parameterized queries exclusively:

- `AddAgent`: `INSERT INTO run_agents ... VALUES (?, ?, ?, ?, ?, ?, ?, ?)` — 8 positional placeholders
- `UpdateAgent`: `UPDATE run_agents SET status = ?, updated_at = ? WHERE id = ?` — 3 placeholders
- `GetAgent`: `SELECT ... FROM run_agents WHERE id = ?` — 1 placeholder
- `ListAgents`: `SELECT ... FROM run_agents WHERE run_id = ? ORDER BY created_at ASC` — 1 placeholder
- `AddArtifact`: `INSERT INTO run_artifacts ... VALUES (?, ?, ?, ?, ?, ?)` — 6 placeholders
- `ListArtifacts`: two variants both use `WHERE run_id = ?` (and `AND phase = ?` when filtering) — parameterized

The `Current()` addition to `internal/phase/store.go` uses:
```go
SELECT `+runCols+` FROM runs WHERE status = 'active' AND project_dir = ? ORDER BY created_at DESC LIMIT 1
```
`status = 'active'` is a string literal in the query (not user-supplied). `project_dir = ?` is parameterized. `runCols` is a package-level `const` string, not user-controlled. This is safe.

No string interpolation into any SQL query was found across the entire diff. This is consistent with the existing codebase standard.

ID generation uses `crypto/rand` with the 36-char alphanumeric alphabet and 8-char length, consistent with the `generateID` function used in the `dispatch` and `phase` packages. Entropy is sufficient for a local tracking database (36^8 ≈ 2.8 trillion values).

### 3. Path Handling (--project=, --path= flags)

**`--project=`** (cmdRunCreate, cmdRunCurrent): the project directory string is passed to `store.Create()` and `store.Current()` as a SQL parameter. It is stored as `project_dir TEXT` and compared in `WHERE project_dir = ?`. No filesystem access is performed on the value by intercore itself. The value is provided by the caller (typically `$(pwd)` from bash) and is descriptive. No traversal check is needed because intercore does not open files at that path.

**`--path=`** (cmdRunArtifactAdd): the artifact path is stored as `run_artifacts.path TEXT`. No filesystem access is performed on the value in this diff. It is returned as-is via `ic run artifact list`. However, the value is stored without `filepath.Clean` normalization, so a caller can persist `../../etc/passwd` or similar. This is a latent risk: if a future consumer reads the stored path and opens it, the traversal survives. See finding S-02.

**`--db=`** (existing flag, unchanged): continues to use `validateDBPath()` which enforces `.db` extension, no `..` components, and path within CWD. This is not changed by the diff.

The `--dispatch-sh=` path handling (existing, in dispatch.go) was reviewed for consistency: it passes the path to `dispatch.Spawn`, which hands it to the OS for process execution. This pre-existed the diff and is unchanged.

### 4. Bash Wrappers (lib-intercore.sh)

Four new wrapper functions were added: `intercore_run_current`, `intercore_run_phase`, `intercore_run_agent_add`, `intercore_run_artifact_add`.

All four use the Bash array pattern for argument construction:
```bash
local args=(run agent add "$run_id" --type="$agent_type")
if [[ -n "$name" ]]; then args+=(--name="$name"); fi
"$INTERCORE_BIN" "${args[@]}"
```

This pattern correctly handles values with spaces, special characters, and does not invoke the shell to interpret them. It is the same pattern used by the existing dispatch wrappers and is safe against word-splitting and globbing.

Direct flag construction in `intercore_run_artifact_add`:
```bash
"$INTERCORE_BIN" run artifact add "$run_id" --phase="$phase" --path="$path" --type="$type" 2>/dev/null
```
All variables are double-quoted, preventing word-splitting. If `$path` contained a single quote, that would be passed literally to the CLI (not interpreted by the shell), which is correct behavior.

The one naming issue: `path` is a bash built-in (used by `cd` with `CDPATH`). Declaring `local path="$3"` shadows it within the function scope. This is harmless because `local` restricts the shadow and `cd` is not called in the function, but it is a maintenance concern. See finding S-03.

### 5. Schema Migration Safety

The v3→v4 migration adds two new tables with `CREATE TABLE IF NOT EXISTS`. It does not modify any existing table, column, or index. Foreign key enforcement (`PRAGMA foreign_keys = ON`) is newly enabled in `db.Open()`, added in the `internal/db/db.go` change. The test `TestOpen_ForeignKeysEnabled` verifies this.

The migration follows the project's expand-only pattern: no column drops, no renames, no data transforms. Pre-migration backup is created automatically at `.clavain/intercore.db.backup-YYYYMMDD-HHMMSS`. Rollback is: restore the backup file, redeploy the previous binary.

The `TestMigrate_V3ToV4` test verifies that v3 `runs` data is preserved across the migration, which is the critical invariant.

The 3-step deploy sequence (rebuild → `ic init` → `ic version`) is documented in AGENTS.md and remains correct for this change.

### 6. Deployment and Operational Safety

No irreversible data changes. No backfill. No lock-risk DDL (no ALTER TABLE, no DROP, no UPDATE on existing rows). The new tables start empty.

Post-deploy verification: `ic health` confirms schema v4, `ic run agent list <id>` and `ic run artifact list <id>` return empty lists for existing runs (expected). No monitoring gaps introduced.

Rollback feasibility: high. Restore `.backup-YYYYMMDD-HHMMSS`, redeploy v3 binary. The two new tables are simply absent in the v3 binary and the v3 schema; v3 code does not reference them. Code and data can roll back together cleanly.

---

## Findings Summary

| ID | Severity | Title |
|----|----------|-------|
| S-01 | LOW | Agent status field accepts arbitrary strings without whitelist check |
| S-02 | LOW | Artifact --path= stored without normalization; latent traversal risk |
| S-03 | LOW | `path` variable name shadows bash built-in in intercore_run_artifact_add |
| S-04 | INFO | --goal= flag has no length bound |

**Verdict: safe** — no blocking issues. S-01 warrants a quick code fix before the next minor version. S-02 is a latent risk worth addressing while the code is fresh. S-03 is a one-line rename.

---

## What Was Not Flagged

- Manual `--flag=value` parsing: this is the project standard, not a vulnerability. All values become SQL parameters, not interpolated strings.
- No authentication on CLI commands: this is an intentional design decision for a local-only tool, noted in CLAUDE.md.
- `--scope-id=`, `--type=`, `--phase=` accepting arbitrary strings: these are metadata/label fields stored as TEXT with no enforcement semantics in this version. The phase field in artifacts is a freeform label, not routed through the phase state machine. No risk.
- `ic run agent list <run>` returning data for an invalid run_id: the query returns empty, no error. This is the correct behavior for a list operation and is consistent with the dispatch list pattern.
- ID collision probability: 36^8 ≈ 2.8T, adequate for a local single-machine tool that will never have more than a few thousand records.
