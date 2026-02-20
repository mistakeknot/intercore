# Quality Review: Wave 2 Event Bus (Go)

**Date:** 2026-02-18
**Scope:** 27 files, ~1,500 lines. New `internal/event/` package, modified dispatch/phase signatures, new CLI command, bash wrappers, integration tests.
**Output contract file:** `.clavain/quality-gates/fd-quality.md`

---

## Overall Assessment

The implementation is well-structured and merits a **needs-changes** verdict due to two medium-severity issues. The event package decomposition is clean: `event.go` (types), `notifier.go` (fan-out), `store.go` (persistence), and three handlers follow the project's established separation of concerns. The dual-cursor design (independent `sincePhaseID` / `sinceDispatchID` per AUTOINCREMENT table) is correct and well-tested. Error handling in the new `internal/event/` package follows `%w` wrapping conventions throughout. Test coverage is broad with unit tests for every handler and store method plus integration test coverage.

---

## Key Findings

### Medium

**Q1 — Column injection in UpdateStatus (`internal/dispatch/dispatch.go:217`)**

`UpdateFields` is `map[string]interface{}` and column names are interpolated directly into SQL without an allowlist. The type and method are exported. While all current callers are internal, the pattern is exploitable if a caller ever passes external input as field names. Fix: add an allowlist of permitted column names checked before building the SET clause.

**Q2 — Discarded AddDispatchEvent error in dispatchRecorder closure (`cmd/ic/run.go:231`)**

The closure calls `evStore.AddDispatchEvent(...)` without capturing the error. A DB failure (disk full, FK violation) is invisible while the notifier still fires, producing hooks and log lines that imply a persisted event that does not exist. Fix: capture and log the error before calling `notifier.Notify`.

### Low

**Q3 — Missing error context in ListPendingAgentIDs scan (`internal/runtrack/store.go:159`)**

`rows.Scan` error returned bare; inconsistent with the rest of the store which wraps with `fmt.Errorf("...: %w", err)`.

**Q4 — enc.Encode error discarded in tail loop (`cmd/ic/events.go:128`)**

Silent broken-pipe failures on stdout; the rest of the CLI checks write errors.

**Q5 — time.Sleep in hook handler goroutine tests (`internal/event/handler_hook_test.go:851,906,927`)**

Three tests sleep 200ms to synchronize a detached goroutine — inherently racy under load, adds 600ms to test time. A channel or `sync.WaitGroup` signal from a test-only path would be more robust.

**Q6 — loadCursor swallows genuine DB errors (`cmd/ic/events.go:303`)**

All `store.Get` errors are treated as not-found, silently resetting the cursor to 0 on transient failures, causing event replay.

### Informational

**Q7 — OR-empty-string SQL trick in ListEvents needs a comment (`internal/event/store.go:50`)**

`WHERE (run_id = ? OR ? = '')` is correct but the double-binding of the same parameter is non-obvious. A comment prevents misread.

**Q8 — Schema inconsistency: dispatch_events uses SQL DEFAULT (unixepoch()) (`internal/db/schema.sql:594`)**

CLAUDE.md advises using Go's `time.Now().Unix()` rather than SQL `unixepoch()`. The float-promotion concern cited in CLAUDE.md applies to comparisons, not DEFAULT values, but the split (phase_events = Go-supplied, dispatch_events = SQL default) should be noted as deliberate.

---

## Files Reviewed

- `/root/projects/Interverse/infra/intercore/internal/event/event.go`
- `/root/projects/Interverse/infra/intercore/internal/event/notifier.go`
- `/root/projects/Interverse/infra/intercore/internal/event/store.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_hook.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_log.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn.go`
- `/root/projects/Interverse/infra/intercore/internal/event/store_test.go`
- `/root/projects/Interverse/infra/intercore/internal/event/notifier_test.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_hook_test.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_log_test.go`
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn_test.go`
- `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/machine.go`
- `/root/projects/Interverse/infra/intercore/internal/runtrack/store.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/events.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/run.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/dispatch.go`
- `/root/projects/Interverse/infra/intercore/internal/db/schema.sql`
- `/root/projects/Interverse/infra/intercore/lib-intercore.sh`
- `/root/projects/Interverse/infra/intercore/test-integration.sh`
