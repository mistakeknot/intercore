# Safety Review: E5 Discovery Pipeline — Full Analysis

**Date:** 2026-02-20
**Reviewer:** Flux Safety Reviewer (Claude Sonnet 4.6)
**Files reviewed:**
- `/root/projects/Interverse/infra/intercore/cmd/ic/discovery.go` (642 lines, new)
- `/root/projects/Interverse/infra/intercore/cmd/ic/events.go` (cursor system changes)
- `/root/projects/Interverse/infra/intercore/internal/discovery/store.go`
- `/root/projects/Interverse/infra/intercore/internal/discovery/discovery.go`
- `/root/projects/Interverse/infra/intercore/internal/discovery/errors.go`
- `/root/projects/Interverse/infra/intercore/internal/db/schema.sql`
- `/root/projects/Interverse/infra/intercore/cmd/ic/main.go` (openDB / validateDBPath reference)

---

## 1. Threat Model and Architecture Context

**System type:** Local-only CLI tool (`ic`). There is no network listener, no HTTP layer, no RPC surface. The tool is invoked by operators and by automated Clavain/Interverse bash scripts. The DB is a single-writer SQLite WAL file on the local filesystem. UFW firewall is active on the host; the tool has no inbound network exposure.

**Risk classification:** Medium risk. New data storage subsystem (discoveries, feedback, interest profile) with a new `@file` file-reading pattern not previously present in the CLI. No auth changes, no credential flows, no permission model changes. The primary concern is input validation consistency and the new file-reading surface.

**Untrusted input sources:**
- Flag values passed from bash scripts that may incorporate data from external APIs (Exa, other sources)
- The `@file` content — files on disk whose provenance depends on who wrote them
- `--source`, `--title`, `--url`, `--signal`, `--actor` free-form strings

**Trusted:**
- The local operator invoking `ic` directly
- SQLite driver (parameterized queries)
- Go standard library file I/O

---

## 2. SQL Injection Analysis

**Finding: No SQL injection risk.**

Every SQL operation in `internal/discovery/store.go` uses parameterized queries (`?` placeholders) with `ExecContext` and `QueryRowContext`. Examined:

- `Submit` (line 39-43): All 12 columns bound via `?`
- `SubmitWithDedup` (line 78-79): WHERE clause uses `? AND embedding IS NOT NULL`
- `Get` (line 160-164): `WHERE id = ?`
- `List` (line 207-225): Dynamic WHERE construction uses only hardcoded SQL fragment strings; user values go into the `args` slice, not the query string. Column names (`source`, `status`, `confidence_tier`) are compile-time constants.
- `Score` (line 255, 271): `WHERE id = ?`, UPDATE binds all values via `?`
- `Promote` (line 305, 329): Same pattern
- `Dismiss` (line 361, 370): Same pattern
- `RecordFeedback` (line 408-411): All five columns bound via `?`, including `signal_type` and `actor`
- `UpdateProfile` (line 451-458): All columns bound via `?`
- `Decay` (line 472-474, 508-510): Parameterized throughout
- `Search` (line 553-568): Same dynamic WHERE construction as `List` — safe
- `Rollback` (line 609-612): `UPDATE ... WHERE source = ? AND discovered_at >= ?`

The `dispatch.UpdateStatus` column-allowlist pattern (called out in AGENTS.md) is not needed here because no user-supplied column names are ever interpolated into the query string.

**Conclusion: SQL injection is not a risk in this change.**

---

## 3. Command Injection Analysis

**Finding: No command injection risk.**

The discovery CLI does not spawn subprocesses, execute shell commands, or use `os/exec`. All data paths lead to SQLite via `database/sql`. The event bus integration adds a `SourceDiscovery` constant to `internal/event/event.go`; the HookHandler that executes `.clavain/hooks/on-event.sh` receives discovery events as JSON on stdin, which is safe — the JSON content is not interpolated into shell arguments.

**Conclusion: No command injection surface introduced.**

---

## 4. File Reading: @file Syntax — Path Traversal Gap

**Finding: LOW severity — readFileArg has no path containment check.**

`readFileArg` in `cmd/ic/discovery.go:655-660`:

```go
func readFileArg(arg string) ([]byte, error) {
    if strings.HasPrefix(arg, "@") {
        return os.ReadFile(arg[1:])
    }
    return []byte(arg), nil
}
```

This performs an unconstrained `os.ReadFile` on whatever path follows `@`. There is no `filepath.Clean`, no rejection of `..` components, and no CWD containment assertion.

Compare with `validateDBPath` in `cmd/ic/main.go:218-238`, which:
1. Calls `filepath.Clean`
2. Rejects paths containing `..`
3. Resolves to absolute path
4. Asserts the resolved path is under `os.Getwd()`
5. Checks the parent directory for symlinks (in some paths)

**Affected flags:**
- `--embedding=@<file>` (submit, search)
- `--metadata=@<file>` (submit)
- `--data=@<file>` (feedback)
- `--keyword-weights=@<file>` (profile update)
- `--source-weights=@<file>` (profile update)

**Exploitation scenario in current threat model:** Low likelihood. An operator invoking the CLI directly would not accidentally traverse out of CWD. The risk becomes relevant when:
1. Bash automation scripts construct `--embedding=@${path}` where `${path}` is partially derived from external API responses (e.g., a cached file path from Exa that contains `..` segments)
2. A discovery source ID or title that gets reflected into a subsequent file path argument

**Impact:** Read of arbitrary files on the filesystem (SSH keys, `/etc/passwd`, other secrets) which are then stored as BLOB/TEXT in the SQLite DB. This is a data confidentiality issue, not a system compromise.

**Mitigation:**

```go
func readFileArgContained(arg string) ([]byte, error) {
    if !strings.HasPrefix(arg, "@") {
        return []byte(arg), nil
    }
    raw := arg[1:]
    cleaned := filepath.Clean(raw)
    if strings.Contains(cleaned, "..") {
        return nil, fmt.Errorf("path traversal not allowed: %s", raw)
    }
    abs, err := filepath.Abs(cleaned)
    if err != nil {
        return nil, err
    }
    cwd, err := os.Getwd()
    if err != nil {
        return nil, err
    }
    if !strings.HasPrefix(abs, cwd+string(filepath.Separator)) && abs != cwd {
        return nil, fmt.Errorf("file path must be under current directory: %s", raw)
    }
    return os.ReadFile(abs)
}
```

This aligns the `@file` reader with the stated security posture documented in AGENTS.md ("Path Traversal Protection").

---

## 5. Trust Boundary: Input Validation

### 5a. score and dedup-threshold (float parsing)

`strconv.ParseFloat` with an error check at the CLI layer. Passing score values outside [0.0, 1.0] is not rejected at the CLI — the store's `TierFromScore` gracefully handles scores >1.0 (maps to `high`) and <0.0 (maps to `discard`). This is not a security concern — it is a functional concern. No security impact.

### 5b. signal_type (feedback command)

`--signal=<type>` is accepted as a free-form string and stored verbatim in `feedback_signals.signal_type`. The store defines five canonical signal constants (`promote`, `dismiss`, `adjust_priority`, `boost`, `penalize`) but does not enforce them. Arbitrary values (including very long strings) can be stored. No SQL injection risk (parameterized), but the column semantics are not enforced.

**Recommendation:** Validate against the canonical set in the store's `RecordFeedback` or in the CLI handler.

### 5c. actor (feedback command)

`--actor=<name>` defaults to `"system"` and is stored verbatim. No length cap. Non-issue for SQL safety, but unbounded length could bloat the feedback table over time.

### 5d. source, title, url (submit command)

Stored via parameterized queries. No length validation at CLI layer. AGENTS.md documents 1MB max payload and 100KB max string values for the state store, but these limits are not applied to the discovery store columns. In practice, source names and URLs are short; title could be longer but is unlikely to approach MB range in normal Exa usage.

### 5e. metadata and profile JSON (readFileArg + store)

Content read from `@file` is passed as a raw string. The CLI does not call `json.Valid()` before passing it to the store. Invalid JSON would be stored verbatim in `raw_metadata`, `keyword_weights`, or `source_weights`. Downstream consumers that `json.Unmarshal` these columns would fail. The existing state store validates JSON before storage (from AGENTS.md); the discovery store does not.

**Recommendation:** Call `json.Valid()` in the CLI handlers for metadata, keyword-weights, and source-weights before passing to the store.

---

## 6. Embedding BLOB Handling

**Finding: Safe.**

Embeddings are passed as `[]byte` through the database/sql driver. The driver handles BLOB binding natively — no string interpolation, no escaping issues. `CosineSimilarity` in `discovery.go:130-147` validates that both byte slices are non-nil, equal length, and divisible by 4 before iterating. Length mismatches return 0.0 rather than panicking.

No size cap is applied to embedding BLOBs at the CLI layer. A caller passing `--embedding=@/dev/zero` (or a large file) would cause `os.ReadFile` to attempt to read the entire file into memory. For the local-operator threat model, this is an operator foot-gun rather than a security issue, but it is worth noting alongside the path traversal finding since the same `readFileArg` function is responsible.

---

## 7. Event Bus Cursor Changes (events.go)

**Finding: Safe.**

The `sinceDiscovery` cursor is a `int64` parsed from `--since-discovery=` via `strconv.ParseInt`. The cursor JSON is formatted with `fmt.Sprintf` using `%d` for all three integer fields — no user string is interpolated into the cursor payload. The cursor key is `consumer` or `consumer:runID`, both of which come from operator-supplied flags — same trust level as existing cursor consumers. No new attack surface introduced.

The cursor registration payload change from `{"phase":0,"dispatch":0,"interspect":0}` to `{"phase":0,"dispatch":0,"interspect":0,"discovery":0}` is backward compatible — existing cursors missing the `discovery` field will unmarshal with `cursor.Discovery == 0` (Go zero value), which is correct for "start from beginning".

---

## 8. Rollback Command Safety

**Finding: Safe with one operational note.**

`ic discovery rollback --source=<s> --since=<ts>` mass-dismisses discoveries. The operation is:
1. Transactional (wraps UPDATE in BeginTx)
2. Selective (status NOT IN ('promoted', 'dismissed') guard prevents double-rollback)
3. Auditable (emits a `discovery.dismissed` event per affected row with `"reason": "rollback"`)

The `--since=0` invocation in the integration test (`test-integration.sh:1174`) dismisses all entries from the source since epoch — effectively a full wipe for that source. No confirmation prompt or dry-run flag exists. This is an operational concern, not a security flaw (local-only tool), but the blast radius is significant if `--since=0` is run in production.

**Recommendation:** Consider adding `--dry-run` support to print affected count without committing, consistent with `ic run rollback --dry-run` which already exists.

---

## 9. Schema (v9) Review

**Finding: Safe.**

New tables: `discoveries`, `discovery_events`, `feedback_signals`, `interest_profile`. Observations:

- `UNIQUE(source, source_id)` constraint on `discoveries` enforces dedup at DB level as a safety net behind the application-level `ErrDuplicate` check. Correct.
- `discovery_events.discovery_id` has no FK to `discoveries.id`. This is intentional (same pattern as `dispatch_events.dispatch_id` — events are retained after records are pruned). Documented implicitly by the UNION query comment "discovery_id AS run_id is for column alignment only".
- `interest_profile` uses `CHECK (id = 1)` to enforce singleton. Correct.
- No `ON DELETE CASCADE` on feedback_signals — orphaned signals remain if discoveries are deleted. This is acceptable given the local-only context and the absence of a discovery delete operation in the CLI.

---

## 10. Deployment Safety

**Schema migration:** v9 adds five tables via `CREATE TABLE IF NOT EXISTS`. Migration is idempotent. Standard `ic init` deploys it. Pre-migration backup is created automatically. No existing tables are altered — zero risk of data loss during upgrade.

**Rollback:** Downgrading the binary after v9 migration leaves the new tables intact but harmless — old binaries ignore unknown tables. The new tables have no FK references into pre-existing tables (discoveries.run_id is nullable and not FK-constrained), so a rollback to an older binary will not break existing phase/dispatch functionality.

**Operational invariants that must hold:**
- `ic init` must be run after deploying the new binary to create the v9 tables
- Any consumer using `ic events tail --consumer=<name>` will get a cursor upgraded to include the `discovery` field on next `saveCursor` call — backward-compatible
- The integration test covers the full lifecycle and event bus integration

---

## 11. Summary of Findings

| ID | Severity | Area | Finding |
|----|----------|------|---------|
| F1 | LOW | readFileArg | @file path traversal — no CWD containment |
| F2 | LOW | Profile update JSON | No json.Valid() or size cap before storage |
| F3 | LOW | Feedback signal_type | Free-form string, no allowlist enforcement |
| I1 | INFO | List/Search query | Dynamic WHERE safe; note pattern consistency |

**Verdict: needs-changes** — None of the findings are blockers for a local-only tool, but F1 (missing CWD containment on @file reads) is an inconsistency with the stated security posture and should be addressed before any script automation constructs these flags from external data. F2 and F3 are low-effort fixes that harden the store against bad inputs.
