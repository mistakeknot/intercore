# Quality Review — Rollback & Recovery Code

**Scope:** 18 files, 1126 lines added across Go, SQLite, Bash.
**Date:** 2026-02-20
**Full findings:** `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`

---

## Summary

The rollback and recovery feature is architecturally sound and consistent with established intercore conventions. No correctness bugs were found. All new code passes scrutiny for error handling, test coverage, naming, and Go idioms. The main issues are: (1) duplicate terminal-status and chain validation between `machine.Rollback` and `store.RollbackPhase`, creating maintenance surface; (2) three new instances of `err == phase.ErrNotFound` (direct equality) rather than `errors.Is`, consistent with the pre-existing pattern but widening the technical debt; and (3) `AGENTS.md` references `lib-intercore.sh` at `v0.6.0` after the bump to `v1.1.0`. Cleanup failures after phase rewind return exit 0 with only a warning message, which is a design trade-off, but the JSON output lacks a `warnings` field for caller detection.

## Key Findings (Summary)

| ID | Severity | Finding |
|----|----------|---------|
| Q1 | LOW | Duplicate validation: `Rollback` (machine) pre-validates what `RollbackPhase` (store) also validates — creates two divergence points |
| Q2 | LOW | `err == phase.ErrNotFound/ErrTerminalRun` direct comparison in rollback CLI code; project already uses `errors.Is` in `budget.go` |
| Q3 | LOW | Cleanup step failures (artifacts, dispatches, agents) return exit 0 with no structured way for callers to detect partial success |
| Q4 | LOW | `ListArtifactsForCodeRollback` returns all artifacts regardless of `status`; behavior undocumented in docstring |
| Q5 | LOW | `AGENTS.md:52,358` still shows `lib-intercore.sh v0.6.0` after version bump to `v1.1.0` |
| Q6 | INFO | `CancelByRun` scope coupling (scope_id = run_id) undocumented at CLI call site |
| Q7 | INFO | `AddArtifact` INSERT omits `status` column (relies on DEFAULT); inconsistent with explicit scan |
| Q8 | INFO | `CancelByRun` has no unit test; only covered by integration tests |

## Strengths

- Migration guard (`currentVersion >= 4 && currentVersion < 8`) is correct and idiomatic; idempotency via `isDuplicateColumnError` is maintained
- `RollbackResult.RolledBackPhases` correctly excludes the target phase and includes the current phase
- `MarkArtifactsRolledBack` builds safe parameterized IN clause — no string interpolation of user-controlled values
- `CountArtifacts` gate method correctly adds `AND status = 'active'` to exclude rolled-back artifacts from gate evaluation
- Integration tests cover all major paths: dry-run, actual rollback, completed-run revert, cancelled-run rejection, forward-target rejection
- Bash wrappers follow established patterns: `intercore_available` guard, `${INTERCORE_DB:+--db=}` expansion, stderr suppression
- Shell script uses `set -euo pipefail` (verified at line 2 of `test-integration.sh`)
- Event bus callback is wired correctly: fires after the DB write, consistent with `Advance`
