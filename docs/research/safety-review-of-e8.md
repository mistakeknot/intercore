# Safety Review: E8 Portfolio Orchestration Changes

**Reviewer:** Flux-drive Safety Reviewer
**Date:** 2026-02-20
**Scope:** `internal/portfolio/dbpool.go`, `internal/portfolio/relay.go`, `internal/portfolio/deps.go`, `cmd/ic/run.go`, `cmd/ic/dispatch.go`, `internal/db/db.go`
**Change classification:** Medium-High risk — new filesystem access paths (child DB reads), new DB tables, new CLI flags, schema migration.

---

## Threat Model

intercore is a **local-only developer tool**. The attack surface is:

- Shell input from the operator running `ic` commands (trusted but must still be validated — shell expansion mistakes and script injection are realistic)
- Project directory paths supplied on the CLI (`--projects=`, `--upstream=`, `--downstream=`)
- Child SQLite databases opened read-only by the portfolio relay (files at attacker-controlled paths if a child path is tampered with)
- The portfolio relay writes aggregated data back into the portfolio DB (trusted path, but sourced from child DB content)

There is no network exposure, no web interface, no multi-tenant model. The relevant threat categories are: path traversal causing unintended file access, SQL injection via unsanitized string interpolation, privilege boundary confusion between portfolio DB and child DBs, and deployment risk from the new schema migration.

---

## Finding 1 — Child Project Paths Are Not Validated Before DB Open (Medium Risk)

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/dbpool.go`, lines 33–62

**What the code does:**

```go
func (p *DBPool) Get(projectDir string) (*sql.DB, error) {
    if !filepath.IsAbs(projectDir) {
        return nil, fmt.Errorf("dbpool: project dir must be absolute: %q", projectDir)
    }
    // ...
    dbPath := filepath.Join(projectDir, ".clavain", "intercore.db")
    dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout%%3D%d", dbPath, p.busyTimeout.Milliseconds())
    db, err := sql.Open("sqlite", dsn)
```

**Issue — no symlink or containment check on the child project directory.**

The main DB (`--db` flag) goes through `validateDBPath()` which enforces: `.db` extension, no `..` components, must resolve under CWD, and parent directory must not be a symlink (the `db.Open` symlink check in `internal/db/db.go` line 41–43). The child DB paths stored in the `runs` table (`project_dir` column) receive **none of these checks** when opened by the relay.

A child path like `/etc` would cause the relay to attempt opening `/etc/.clavain/intercore.db` (which would fail because it doesn't exist, so low exploit value). However, a path like `/proc/1/fd/3` or a directory that is itself a symlink to an arbitrary location receives no rejection. More practically: if a `project_dir` value was written to the `runs` table by any means other than `ic run create`, the relay would obediently attempt to open whatever path is stored there.

The current code does reject relative paths, which covers the most common accidental traversal. The risk is bounded because:
1. `mode=ro` prevents any write to the child DB file.
2. The child `project_dir` values are written by `CreatePortfolio` from CLI-supplied paths that go through `filepath.Abs`.
3. The relay only reads from child DBs it discovers through the portfolio's `runs` table — an adversary would need write access to the portfolio DB to inject a malicious path, at which point they have already compromised the host.

**Verdict:** Low exploitability given the local-only, single-operator threat model. However, the asymmetry between the main DB path validation (strict) and child DB path validation (absolute path only) is a correctness gap that could become a problem if portfolio orchestration is later extended to accept remote project registries or cross-machine paths.

**Mitigation (recommended, not blocking):** Add a symlink check on the child `projectDir` before calling `filepath.Join`. Mirror the logic in `db.Open`:

```go
if info, err := os.Lstat(projectDir); err == nil && info.Mode()&os.ModeSymlink != 0 {
    return nil, fmt.Errorf("dbpool: project dir is a symlink: %q", projectDir)
}
```

---

## Finding 2 — DSN Construction Does Not URL-Encode the File Path (Low-Medium Risk)

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/dbpool.go`, line 47

```go
dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout%%3D%d", dbPath, p.busyTimeout.Milliseconds())
```

**Issue:** The `dbPath` is interpolated directly into the DSN string. If the resolved path contains URL-special characters (spaces, `%`, `?`, `#`, `&`), the DSN will be malformed, and the `_pragma` portion could be lost or the path could be misinterpreted by the SQLite URI parser.

In practice, project paths on this system are under `/root/projects/Interverse/...` with no special characters, so this is not currently exploitable. However, the existing code for the main DB in `db.go` line 49 has the same pattern:

```go
dsn := fmt.Sprintf("file:%s?_pragma=journal_mode%%3DWAL&...", path, ...)
```

Both should use `url.PathEscape(path)` or `net/url` to construct the file URI properly.

**Verdict:** Low risk in the current deployment environment. A project directory with a space or `%` in its name would silently open the wrong DB (or fail to open) rather than escalating privileges. Worth fixing for robustness.

**Mitigation:** Use `url.PathEscape`:

```go
import "net/url"
dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout%%3D%d",
    url.PathEscape(dbPath), p.busyTimeout.Milliseconds())
```

---

## Finding 3 — Portfolio Dep Paths Are Stored Unvalidated (Medium Risk)

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/portfolio.go`, `cmdPortfolioDepAdd` (lines ~52–84)
**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps.go`, `Add` (lines 30–43)

The `--upstream=` and `--downstream=` arguments to `ic portfolio dep add` are passed directly to `depStore.Add()` and stored in `project_deps.upstream_project` / `project_deps.downstream_project` without any path normalization or validation.

The `deps.go` Add function does one check:

```go
if upstream == downstream {
    return fmt.Errorf("add dep: upstream and downstream cannot be the same project")
}
```

It does not verify that the paths are absolute, that they exist, that they don't contain `..`, or that they match any of the child project directories registered in the portfolio's `runs` table. A user could add a dependency edge pointing to a path that has no corresponding child run, causing the relay to emit spurious `upstream_changed` events for a project that is not in the portfolio.

More significantly, the relay in `relay.go` uses these stored paths for the `dep.UpstreamProject == child.ProjectDir` comparison (line 141). If `UpstreamProject` was stored as a relative path while `child.ProjectDir` was stored as an absolute path (or vice versa), the match will silently fail. There is no normalization at read time either.

**Verdict:** Medium risk operationally (silent event-routing mismatches), low security risk (no privilege escalation path). The comparison logic relies on exact string equality between two values stored at different times, with no canonical form enforced.

**Mitigation:** In `cmdPortfolioDepAdd`, resolve both paths to absolute before storing:

```go
upstream, err = filepath.Abs(upstream)
// ... error check
downstream, err = filepath.Abs(downstream)
```

Optionally add a check that both paths exist as registered child runs in the portfolio.

---

## Finding 4 — Dispatch Limit Check Is Racy and Advisory-Only (Low Risk, Deployment Note)

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/dispatch.go`, lines 108–125

```go
if opts.ScopeID != "" {
    phaseStore := phase.New(d.SqlDB())
    stateStore := state.New(d.SqlDB())
    if run, err := phaseStore.Get(ctx, opts.ScopeID); err == nil && run.ParentRunID != nil {
        if parent, err := phaseStore.Get(ctx, *run.ParentRunID); err == nil && parent.MaxDispatches > 0 {
            if payload, err := stateStore.Get(ctx, "active-dispatch-count", *run.ParentRunID); err == nil {
                var countStr string
                if json.Unmarshal(payload, &countStr) == nil {
                    if count, err := strconv.Atoi(countStr); err == nil && count >= parent.MaxDispatches {
                        fmt.Fprintf(os.Stderr, "ic: dispatch spawn: portfolio dispatch limit reached (%d/%d)\n", count, parent.MaxDispatches)
                        return 1
                    }
                }
            }
            // If state entry doesn't exist (no relay running), degrade gracefully — allow spawn
        }
    }
}
```

The dispatch limit is read from the relay-maintained `active-dispatch-count` state entry, which is a cached count written by the relay's poll cycle (every 2 seconds by default). This means:

1. The limit is **not enforced if the relay is not running** — the comment documents this as intentional degraded behavior, but it means `MaxDispatches` is silently unenforced in non-relay deployments.
2. The check is **not atomic** — between reading the count and actually inserting the dispatch record, another `ic dispatch spawn` could pass the same check. With 2-second relay refresh intervals, a burst of concurrent spawns could overshoot the limit significantly.
3. The count is sourced from a `SELECT COUNT(*) FROM dispatches WHERE status IN ('spawned', 'running')` in the child DB (relay.go `countActiveDispatches`), which reflects the child's actual dispatch table, not the portfolio's. This is correct but means the limit applies to the portfolio level based on aggregated child counts.

**Verdict:** This is a known architectural tradeoff (documented in code comments). The risk is operational — a runaway portfolio job could overshoot its dispatch budget by one relay poll cycle's worth of spawns. This is acceptable for a local developer tool. No security escalation path exists.

**Mitigation (operational):** Document in AGENTS.md that `MaxDispatches` is a soft limit enforced only when the relay is active and subject to a 2-second window of over-spawn. If hard enforcement is needed in a future iteration, move the limit check into the dispatch INSERT as a transactional check against a live count query.

---

## Finding 5 — Relay Event Data Flows Into Portfolio DB Without Sanitization (Low Risk)

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/relay.go`, lines 116–150

The relay reads `from_phase`, `to_phase`, and `event_type` from child DBs and writes them directly into the portfolio's `phase_events` table via `r.store.AddEvent`. The relay also constructs a `reason` string by interpolating `child.ProjectDir` from the portfolio DB:

```go
relayReason := fmt.Sprintf("relay:%s", child.ProjectDir)
reason := fmt.Sprintf("%s:%s→%s", EventUpstreamChanged, evt.FromPhase, evt.ToPhase)
```

These strings are stored as the `reason` field in `phase_events`. Since they are inserted via parameterized queries (the `AddEvent` method uses `?` placeholders), there is no SQL injection risk. The values are stored as text and later echoed back to the CLI or consumed by the event bus. No shell execution or template rendering happens with these values.

The `from_phase` and `to_phase` values read from child DBs are untrusted in the sense that a compromised child DB could contain arbitrary phase strings. These would be stored verbatim in the portfolio DB and shown in `ic run events` output. This is data integrity noise (a child DB with mangled phase values would produce nonsense events) but not a security issue.

**Verdict:** No exploitable risk in the current architecture. The relay correctly uses parameterized queries throughout. The trust boundary between portfolio DB and child DBs is well-managed: child DBs are opened read-only, and their data is inserted into the portfolio DB through the same parameterized store layer used for all other writes.

---

## Finding 6 — Schema Migration v9-to-v10 Has Safe Construction But Needs Rollback Note

**File:** `/root/projects/Interverse/infra/intercore/internal/db/db.go`, lines 172–185

```go
if currentVersion >= 3 && currentVersion < 10 {
    v10Stmts := []string{
        "ALTER TABLE runs ADD COLUMN parent_run_id TEXT",
        "ALTER TABLE runs ADD COLUMN max_dispatches INTEGER DEFAULT 0",
    }
    for _, stmt := range v10Stmts {
        if _, err := tx.ExecContext(ctx, stmt); err != nil {
            if !isDuplicateColumnError(err) {
                return fmt.Errorf("migrate v9→v10: %w", err)
            }
        }
    }
}
```

**What is correct:**
- ALTER TABLE statements run inside the migration transaction alongside the schema DDL.
- Duplicate-column errors are swallowed to make the migration idempotent.
- A pre-migration backup is created automatically before any migration attempt.
- `PRAGMA user_version` is updated in the same transaction, so a crash mid-migration leaves version unchanged and a retry will re-run.
- The new `project_deps` table is created with `CREATE TABLE IF NOT EXISTS` in `schema.sql`, which is idempotent.

**What requires attention — rollback is not possible after migration.**

Adding columns with `ALTER TABLE` is irreversible in SQLite. Once a DB is migrated to v10, rolling back to a v9 binary will result in the v9 binary encountering `user_version = 10` and returning `ErrSchemaVersionTooNew`, which blocks all operations. The pre-migration backup is the only recovery path.

This is consistent with the existing design for all prior migrations (v5→v6, v4→v8). The existing AGENTS.md documents the backup-and-restore recovery procedure. The new migration adds no new risk pattern beyond what was already present.

**Mitigation (existing, documented):** The pre-migration backup at `.clavain/intercore.db.backup-YYYYMMDD-HHMMSS` is the rollback artifact. Operators rolling back the binary must also restore the backup. This must be verified in the deployment runbook: if the binary is downgraded without restoring the DB, all `ic` commands will return `ErrSchemaVersionTooNew`.

**Deployment sequencing requirement:** Rebuild binary first, then `ic init`. Do not run `ic init` with the old binary against a v10 DB (version check in `Open` will block it). Standard sequence from AGENTS.md is correct:

```bash
go build -o /home/mk/go/bin/ic ./cmd/ic
ic init
ic version
```

---

## Finding 7 — SQL Injection Analysis: All Portfolio SQL Is Parameterized (No Issue)

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps.go`

All five SQL operations (`Add`, `List`, `Remove`, `GetDownstream`, `GetUpstream`) use `?` placeholders for all user-supplied values (`portfolioRunID`, `upstream`, `downstream`). No string concatenation is used in any query. This is correct.

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/relay.go`

The relay's two read queries (`queryChildEvents`, `countActiveDispatches`) use `?` for the `sinceID` parameter. The dispatch count query uses no parameters (it selects a constant status set). No injection surface exists.

The existing column-allowlist protection documented in AGENTS.md for `dispatch.UpdateStatus` remains in place and is not affected by the E8 changes.

---

## Finding 8 — `--projects` Flag Resolves to Absolute Paths Correctly

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/run.go`, lines 161–168

```go
for i, p := range projectPaths {
    abs, err := filepath.Abs(strings.TrimSpace(p))
    if err != nil {
        fmt.Fprintf(os.Stderr, "ic: run create: invalid project path %q: %v\n", p, err)
        return 3
    }
    projectPaths[i] = abs
}
```

The `--projects=path1,path2` flag correctly resolves all comma-separated paths to absolute paths via `filepath.Abs`. `strings.TrimSpace` handles accidental whitespace in the comma-separated list. These absolute paths are stored as `project_dir` in the child `runs` rows, which is what the relay later uses for `DBPool.Get` (which requires absolute paths).

One gap: `filepath.Abs` on a path that does not exist will still succeed (it just joins with CWD). There is no existence check at portfolio creation time. The relay will silently skip any child project directory that cannot be opened (logged as `[relay] skip %s: %v`). This is documented in relay.go and is acceptable behavior.

---

## Summary Table

| Finding | Severity | Exploitability | Blocking? |
|---------|----------|----------------|-----------|
| F1: Child DB path has no symlink check | Low | Very low (local tool, requires DB write access to exploit) | No |
| F2: DSN path not URL-encoded | Low | Very low (requires special chars in project path) | No |
| F3: Dep paths stored without normalization | Medium (operational) | None (no privilege escalation) | No |
| F4: Dispatch limit is racy and relay-dependent | Low | None | No |
| F5: Relay data in portfolio DB | None | None (parameterized throughout) | No |
| F6: Migration rollback requires backup restore | Deployment note | N/A | No |
| F7: SQL injection in deps.go | None | None | No |
| F8: --projects flag resolution | None | None | No |

---

## Pre-Deploy Checklist

1. Run `go test ./...` and `go test -race ./...` — must pass clean.
2. Run `bash test-integration.sh` — must pass all ~93 tests.
3. On a test DB: run `ic init` and verify `ic version` reports schema v10.
4. Verify that `ic run create --projects=...` stores absolute paths in the `runs` table (check with `ic run status <id> --json`).
5. Verify that `ic portfolio dep add` followed by `ic portfolio relay` produces correct `upstream_changed` events for a known path pair.
6. Confirm `.clavain/intercore.db.backup-*` file is created by `ic init` before migration.

## Post-Deploy Verification

- `ic health` must return exit 0 on all project DBs.
- `ic version` must report `schema: v10`.
- Monitor relay stderr for `[relay] skip` messages indicating unreachable child DBs.

## Rollback Plan

1. Stop any running relay processes (`ic portfolio relay` invocations).
2. Restore backup: `cp .clavain/intercore.db.backup-YYYYMMDD-HHMMSS .clavain/intercore.db`
3. Install the prior binary.
4. Verify with `ic version` and `ic health`.

If no backup exists (first-time migration, empty DB), rollback is safe because the DB has no pre-migration data to recover.

---

## Residual Risk

The portfolio feature introduces a new trust boundary: the relay reads from child SQLite databases on the filesystem. This is a read-only, local-only operation. The residual risk is that a maliciously crafted child DB (e.g., one containing extreme values in `phase_events`) could cause relay behavior issues. Given the fully local threat model, this risk is negligible and does not require mitigation before shipping.

The dispatch limit enforcement gap (Finding F4) is residual risk that depends on operational discipline: the relay must be running for the limit to be enforced. This should be documented.
