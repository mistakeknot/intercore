# Code Quality & Style Review: Run-Tracking Implementation

**Scope:** Schema v4 run-tracking — `internal/runtrack/`, `internal/phase/store.go` (`Current()`), `cmd/ic/dispatch.go`, `cmd/ic/run.go`, `lib-intercore.sh`, `test-integration.sh`
**Date:** 2026-02-18

## Summary

The implementation is well-aligned with established intercore conventions: `New(db *sql.DB) *Store`, 8-char crypto/rand IDs, `sql.NullString` for nullable columns, `%w` error wrapping, explicit error handling on all SQL calls, and thorough test coverage. The schema migration, FK enforcement (new `PRAGMA foreign_keys = ON` in `db.Open()`), and integration test coverage are solid.

Six findings were identified (verdict: needs-changes). Three are LOW severity, three are INFO.

## Key Findings

1. **`generateID` duplicated** (`internal/runtrack/store.go`): Identical `idChars`/`idLen`/`generateID` from `internal/phase/store.go` is copied verbatim. An `internal/idgen` package or shared function would eliminate the duplication.

2. **`AddEvent` errors silently discarded** (`cmd/ic/run.go`): `cmdRunCancel` and `cmdRunSet` call `store.AddEvent(...)` without checking the returned error. A failed `AddEvent` drops audit trail records silently while the status update succeeds and exit 0 is returned.

3. **Redundant pre-FK-check adds TOCTOU gap** (`cmd/ic/run.go`): `cmdRunAgentAdd` and `cmdRunArtifactAdd` call `phaseStore.Get(ctx, runID)` before the FK-protected insert. Since `PRAGMA foreign_keys = ON` is now enforced, the check is redundant and introduces a TOCTOU window.

4. (INFO) Test setup errors discarded in `store_test.go` multi-record setup helpers.

5. (INFO) `intercore_run_phase` exit-1 ambiguity (IC unavailable vs run-not-found) — consistent with existing wrappers.

6. (INFO) `path` and `type` local variable names in `intercore_run_artifact_add` shadow convention/builtin.

## Full Findings

See `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`
