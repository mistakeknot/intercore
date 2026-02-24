# Architecture Review: E5 Discovery Pipeline CLI + Event Bus Integration

**Date:** 2026-02-20
**Scope:** 8 changed files — cmd/ic/discovery.go (new, 642 lines), cmd/ic/events.go, cmd/ic/main.go, internal/event/event.go, internal/event/store.go, internal/event/store_test.go, test-integration.sh, CLAUDE.md

---

## Codebase Context

intercore is described in AGENTS.md as "mechanism, not policy — the kernel layer." It provides the durable system of record for runs, phases, gates, dispatches, events, and token budgets. The E5 changes add a Discovery Pipeline: a new `internal/discovery` package (pre-existing in this diff's context), a full 11-subcommand CLI surface in `cmd/ic/discovery.go`, a third UNION ALL leg in the event store, and an extended three-dimensional cursor system.

---

## Boundaries and Coupling Analysis

### Layer Placement

The discovery module fits correctly within intercore's single-layer CLI-over-SQLite model. `cmd/ic/discovery.go` calls `discovery.NewStore(d.SqlDB())` directly, following the same pattern as all other command files (run.go, dispatch.go, gate.go). There is no new cross-package coupling.

The `internal/discovery` package is self-contained: it imports only `database/sql`, standard library packages, and defines its own types, constants, errors, and store. It does not import from `internal/event`, `internal/phase`, or any other intercore package. This is correct — discovery is a peer subsystem, not a dependent.

### Event Bus Integration

The event bus integration has two distinct surfaces:

1. **`SourceDiscovery` constant in `internal/event/event.go`** — a single-line addition that extends the source enum. Well-placed.

2. **Third UNION ALL leg in `internal/event/store.go`** — extends both `ListEvents` (run-scoped) and `ListAllEvents` (system-wide) to include `discovery_events`. This is where the only substantive architectural defect lives (see below).

### CLI Dispatch Pattern

`cmdDiscovery` in `discovery.go` follows the same `switch args[0]` dispatch pattern established by `cmdRun`, `cmdDispatch`, `cmdGate`, `cmdLock`, `cmdSentinel`, `cmdState`. The wiring in `main.go` is a single `case "discovery": exitCode = cmdDiscovery(ctx, subArgs)` line, consistent with all peers.

One exception: `cmdDiscoveryProfile` at line 413 uses an `if len(args) > 0 && args[0] == "update"` guard rather than a `switch`. This means `ic discovery profile unknown-subcommand` silently executes the profile-read path instead of returning exit 3 (usage error), which is inconsistent with every other subcommand group.

---

## Pattern Analysis

### UNION ALL Architecture

The event store uses a three-table UNION ALL pattern to present a unified, time-ordered event stream from independent AUTOINCREMENT sequences. This is a deliberate and documented design: per-table cursors (`sincePhaseID`, `sinceDispatchID`, `sinceDiscoveryID`) avoid the ID space collision that would occur if a single sequential cursor were used across tables with independent autoincrement sequences.

The pattern scales correctly for `ListAllEvents`. The issue is with `ListEvents`:

**`ListEvents` (run-scoped) — boundary defect:**

```sql
-- phase_events: filtered to run
WHERE run_id = ? AND id > ?

-- dispatch_events: filtered to run (or '' for all)
WHERE (run_id = ? OR ? = '') AND id > ?

-- discovery_events: NOT filtered by run — all discoveries returned
WHERE id > ?
```

`discovery_events` has no `run_id` column. The UNION ALL column alignment comment (`-- discovery_id AS run_id is for column alignment only`) correctly documents this. However, the consequence is that `ic events tail <run_id>` returns every discovery event in the database, not only those related to the named run. This breaks the semantic contract of the run-scoped tail: consumers using `ic events tail <run_id>` to monitor a specific run's activity see unrelated discovery events interleaved.

The `interspect_events` table faces the same structural issue (it also lacks a `run_id` column) but is handled differently: interspect events are not included in the UNION ALL at all, and are only accessible via `ic interspect query`. That approach — separate query path for non-run-scoped event types — is the correct pattern to apply to discovery events in `ListEvents`.

**Recommended minimal fix:** Remove the discovery leg from `ListEvents` only. Keep it in `ListAllEvents`. Consumers wanting discovery events should use `--all`. This is a two-line change (remove the UNION ALL block and the `sinceDiscoveryID` bind parameter from the `ListEvents` query). The `ListAllEvents` signature and cursor system remain unchanged.

### Cursor System Extension

The cursor system extension is mechanically correct for the discovery leg. `loadCursor` reads `.discovery`, the high-water mark is advanced on each emitted discovery event, and `saveCursor` persists the updated value.

One silent issue: `saveCursor` hardcodes `"interspect":0` rather than preserving the existing `interspect` field from the loaded cursor. Any durable cursor that had a non-zero `interspect` value (from manual `cursor register`) will have it zeroed on the first save under the new code. The field is currently unused (no `SourceInterspect` advancement in the event loop), but the zeroing is a lossy round-trip that could cause problems if `interspect` gains cursor semantics in a future sprint.

### Error Mapping Pattern

`discoveryError` in `discovery.go` correctly maps sentinel errors from `internal/discovery/errors.go` to CLI exit codes. All four sentinels (`ErrNotFound`, `ErrGateBlocked`, `ErrLifecycle`, `ErrDuplicate`) are mapped to exit 1 (expected negative result), with unrecognized errors returning exit 2. This matches the project's documented exit code table.

Note: all four error branches produce identical `fmt.Fprintf` + `return 1` behavior. The `if errors.Is(err, discovery.ErrNotFound)` chain could be collapsed to a single check against `discovery.IsExpectedNegative(err)` or a direct comparison table, but the current form is harmless.

### Security: `readFileArg`

The `@file` argument pattern is used in `submit`, `feedback`, `profile update`, and `search` subcommands via `readFileArg`. Unlike `cmdStateSet`'s `@filepath` handling, `readFileArg` performs no CWD containment check. The state module validates `filepath.Abs(path)` is under `cwd + separator`. `readFileArg` calls `os.ReadFile(arg[1:])` directly.

This creates two different security postures for the same `@file` syntax. For the current threat model (local agents, operator-controlled prompts), the risk is low. The inconsistency is worth noting because the `--embedding=@<file>` flag in `ic discovery submit` and `ic discovery search` could accept paths like `--embedding=@/etc/passwd` without restriction.

---

## Simplicity and YAGNI Assessment

### Discovery Store (internal/discovery)

The store is appropriately sized for the feature set. Each operation (Submit, Score, Promote, Dismiss, RecordFeedback, Decay, Rollback, Search, GetProfile, UpdateProfile) maps to a clear lifecycle action with transactional semantics. No speculative abstraction exists.

`SubmitWithDedup` is the most complex operation: it scans all embeddings for a source in-transaction, computes cosine similarity in Go, and conditionally inserts or returns the duplicate. The comment correctly notes this is a brute-force scan sufficient for <10K rows. The approach is appropriate for the scale described.

The `Decay` operation loads all eligible rows, applies decay in Go (per the project's documented pattern of computing in Go rather than SQL), and writes back in a single transaction. This is consistent with the `TTL computation in Go (time.Now().Unix()) not SQL (unixepoch())` design decision in CLAUDE.md.

### CLI Surface (cmd/ic/discovery.go)

642 lines for 11 subcommands is proportional. Each subcommand is a flat parse-and-dispatch function with consistent structure: parse flags, validate required args, open DB, create store, call store method, handle errors, format output. There is no abstraction beyond the `discoveryError` helper and `readFileArg` utility.

The `--json` output path uses `json.Marshal(disc)` and ignores the error. This is consistent with the pattern used by other commands in the codebase (run.go does the same in multiple places). It is safe for the struct types involved.

### Test Coverage

90 new test lines in `store_test.go` cover `TestListAllEvents_IncludesDiscovery`, `TestMaxDiscoveryEventID`, and extend `TestMaxEventIDs_EmptyTables`. The tests verify that discovery events appear in the unified stream and that the cursor helper returns 0 on an empty table. The `TestListAllEvents_IncludesDiscovery` test inserts a discovery event directly into the DB (bypassing the discovery store) — this is appropriate for testing the event store layer in isolation.

109 lines of integration tests in `test-integration.sh` cover the full CLI lifecycle: submit, status, list, score, promote, dismiss, feedback, profile, decay, rollback, and the critical event bus integration check (`ic events tail --all | grep '"source":"discovery"'`). The integration tests do not cover the run-scoped `ic events tail <run_id>` path with discovery events — the A1 defect is therefore not caught by the test suite.

---

## Findings Summary

| ID | Severity | Description |
|---|---|---|
| A1 | MEDIUM | `ListEvents` discovery leg has no `run_id` filter — discovery events appear in run-scoped tails |
| A2 | LOW | `saveCursor` hardcodes `interspect:0` — lossy round-trip zeroes existing cursor field |
| A3 | LOW | `Event.Source` comment says `"phase" or "dispatch"` — stale after E5 and E6 |
| A4 | INFO | `cmdDiscoveryProfile` uses `if` guard instead of `switch` — unknown sub-subcommand silently executes get-profile |
| A5 | INFO | `readFileArg` skips CWD containment check — inconsistent security posture with `state set @filepath` |

**Verdict: needs-changes** (A1 is the only blocking item; A2–A5 are low-risk cleanup)

---

## Recommended Action Sequence

1. **Fix A1** (must-fix before this is used in production event consumers): In `internal/event/store.go`, remove the `discovery_events` UNION ALL leg from `ListEvents` only. `ListAllEvents` keeps it. Update `ListEvents` signature back to four parameters (`runID, sincePhase, sinceDispatch, limit`) or retain the `sinceDiscovery` parameter if desired but simply do not include the discovery leg in the run-scoped query. Update call sites in `events.go` and `store_test.go`.

2. **Fix A2** (low risk, one line): In `saveCursor`, change the format string to preserve the loaded `interspect` value by adding it as a parameter to `loadCursor`'s return or by reading it separately.

3. **Fix A3** (documentation only): Update the `Event.Source` comment in `internal/event/event.go` to list all four sources.

4. **Fix A4** (consistency): Convert `cmdDiscoveryProfile` to use `switch args[0]` with a `default: return 3` case.

5. **Consider A5** (security posture): Apply the CWD containment check from `cmdStateSet` to `readFileArg`, or document that `ic discovery` `@file` arguments are intentionally unrestricted.
