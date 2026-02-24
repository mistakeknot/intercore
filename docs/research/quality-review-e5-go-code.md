# Quality Review: E5 Discovery Pipeline CLI + Event Bus Go Code

**Date:** 2026-02-20
**Scope:** 8 files changed in the E5 Discovery Pipeline diff
**Reviewer:** Flux-drive Quality & Style Reviewer

---

## Overview

The E5 diff introduces the `ic discovery` subcommand (11 sub-operations, 642 lines in `cmd/ic/discovery.go`), extends the event bus cursor system with a third `sinceDiscovery` dimension, adds a `SourceDiscovery` constant, extends `ListEvents`/`ListAllEvents` with a third UNION ALL leg and matching cursor tracking, adds `MaxDiscoveryEventID`, and provides 90 lines of new unit tests plus 109 lines of integration tests.

The code is well-structured and follows established project conventions. All errors are handled; the flag parsing style matches the rest of the codebase. A small set of correctness and maintainability issues were found; none are blocking but two are worth fixing before wider use.

---

## Findings

### F1 — MEDIUM: Silently discarded `json.Marshal` errors in `--json` output paths

**Location:** `cmd/ic/discovery.go` lines 163, 215, 393, 604

```go
data, _ := json.Marshal(disc)
fmt.Println(string(data))
```

`json.Marshal` can only fail when a value contains non-serializable types (channels, functions, cyclic pointers). For the discovery structs returned from the store this is extremely unlikely, but the pattern produces silent partial output (`null`) if it ever fires. The project's existing `--json` paths (e.g., `cmd/ic/run.go`, `cmd/ic/dispatch.go`) do not use `json.Marshal` directly — they pipe through `json.NewEncoder(os.Stdout).Encode()` which cannot silently discard output errors.

The fix is a one-liner error check:

```go
data, err := json.Marshal(disc)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: discovery status: marshal: %v\n", err)
    return 2
}
fmt.Println(string(data))
```

Or, following the established `json.NewEncoder` pattern used in `cmdEventsTail`:

```go
enc := json.NewEncoder(os.Stdout)
if err := enc.Encode(disc); err != nil {
    fmt.Fprintf(os.Stderr, "ic: discovery status: encode: %v\n", err)
    return 2
}
```

The encoder pattern is preferable because it writes directly without buffering into `data` and naturally includes a newline, matching how events are emitted.

All four call sites have this pattern (`status`, `list`, `profile`, `search`); they should all be updated.

---

### F2 — MEDIUM: `saveCursor` hardcodes `interspect:0`, dropping any existing interspect cursor value

**Location:** `cmd/ic/events.go`, `saveCursor` function (diff line ~796)

```go
payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":0,"discovery":%d}`, phaseID, dispatchID, discoveryID)
```

The `interspect` field is always written as zero. A consumer that uses both `--since-discovery` and `--consumer=` will lose its interspect cursor position on every `saveCursor` call. The pre-existing code before E5 had the same bug for interspect, so this is not a regression introduced by this diff, but the diff touched the same line and had the opportunity to fix it.

The `loadCursor` struct correctly reads the `interspect` field; `saveCursor` just does not accept it as a parameter nor round-trip it.

**Fix:** Extend `saveCursor` to accept and persist the interspect value, or add an explicit read-before-write to preserve the existing stored value. The former is cleaner:

```go
func saveCursor(ctx context.Context, store *state.Store, consumer, scope string, phaseID, dispatchID, interspectID, discoveryID int64) {
    ...
    payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":%d,"discovery":%d}`, phaseID, dispatchID, interspectID, discoveryID)
```

This is a pre-existing issue, but touching this function in the current diff is the right moment to fix it.

---

### F3 — LOW: `discoveryError` — all error cases share the same `fmt.Fprintf` + return, collapsible to a single path

**Location:** `cmd/ic/discovery.go` lines 664–683

```go
func discoveryError(cmd string, err error) int {
    if errors.Is(err, discovery.ErrNotFound) {
        fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
        return 1
    }
    if errors.Is(err, discovery.ErrGateBlocked) {
        fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
        return 1
    }
    if errors.Is(err, discovery.ErrLifecycle) {
        fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
        return 1
    }
    if errors.Is(err, discovery.ErrDuplicate) {
        fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
        return 1
    }
    fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
    return 2
}
```

The four sentinel cases all print the same message and return 1. This should be collapsed:

```go
func discoveryError(cmd string, err error) int {
    fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
    if errors.Is(err, discovery.ErrNotFound) ||
        errors.Is(err, discovery.ErrGateBlocked) ||
        errors.Is(err, discovery.ErrLifecycle) ||
        errors.Is(err, discovery.ErrDuplicate) {
        return 1
    }
    return 2
}
```

This reduces 20 lines to 7 with no behavior change. The current form will tempt future maintainers to add differentiated messages per sentinel; if that is the intent, the identical messages are misleading.

---

### F4 — LOW: `cmdDiscoveryProfileUpdate` passes `nil` as first arg to `UpdateProfile`

**Location:** `cmd/ic/discovery.go` line 478

```go
if err := store.UpdateProfile(ctx, nil, string(kw), string(sw)); err != nil {
```

`UpdateProfile`'s first positional argument (after `ctx`) is `nil`. Looking at the discovery store tests (`internal/discovery/store_test.go` lines 327, 354), `nil` is the expected value for the optional `tx *sql.Tx` parameter, so this is intentional. However, passing a `nil` of an untyped literal to a concrete `*sql.Tx` parameter silently succeeds in Go whereas passing a typed nil of the wrong interface type would panic. This is correct as written but warrants a comment at the call site to explain the nil is intentional and what it means (use the store's own internal transaction). Without the comment a reader or reviewer will think it is a mistake.

```go
// nil tx: UpdateProfile opens its own transaction.
if err := store.UpdateProfile(ctx, nil, string(kw), string(sw)); err != nil {
```

---

### F5 — LOW: `--since-discovery` not documented in the `events tail` usage help string

**Location:** `cmd/ic/main.go` line 151 (usage string), and `CLAUDE.md` event bus quick reference

```
events tail ... [--since-phase=N] [--since-dispatch=N] [--limit=N]
```

The `--since-discovery=N` flag was added to `cmdEventsTail` in `events.go` but the corresponding usage line in `printUsage()` was not updated. A user reading `ic --help` output would not know the flag exists. Similarly, `CLAUDE.md`'s "Event Bus Quick Reference" section (line 104) still lists only `--since-phase=N --since-dispatch=N`.

Both should be updated:

```
events tail ... [--since-phase=N] [--since-dispatch=N] [--since-discovery=N] [--limit=N]
```

---

### F6 — LOW: `TestListEvents_IncludesDiscovery` not added; only `TestListAllEvents_IncludesDiscovery` exists

**Location:** `internal/event/store_test.go`

The new `TestListAllEvents_IncludesDiscovery` test verifies that discovery events appear in the `ListAllEvents` (cross-run) path. The run-scoped `ListEvents` function also gained the `sinceDiscoveryID` parameter and the third UNION ALL leg. A corresponding test (`TestListEvents_IncludesDiscovery`) covering the run-scoped path would close the gap. This is particularly useful because the `ListEvents` leg also filters on `run_id` for the dispatch leg but intentionally omits the run filter for discovery events (discovery events are not run-scoped), and a test would confirm that behavior is intentional and correct.

---

### F7 — INFO: `ListEvents` includes all discovery events regardless of `runID` — comment explains but no test verifies the semantics

**Location:** `internal/event/store.go` lines 52–56

```sql
-- discovery_events: discovery_id AS run_id is for column alignment only
SELECT id, COALESCE(discovery_id, '') AS run_id, 'discovery' AS source, event_type,
    from_status, to_status, COALESCE(payload, '{}') AS reason, created_at
FROM discovery_events
WHERE id > ?
```

The comment is accurate: when a caller queries `ListEvents(ctx, "run001", ...)` they get all discovery events, not only those related to run001. This is a design choice (discovery events are cross-cutting), but it is a semantic surprise for callers expecting run-scoped results.

No action required if the design intent is deliberately global discovery events in per-run views, but the AGENTS.md "Event Bus Module" section under "Tables" should document this explicitly so maintainers do not add a run_id filter later thinking they are fixing a bug.

---

### F8 — INFO: Integration test `DISC_ID5` declared but its value is never used

**Location:** `test-integration.sh` line 1173

```bash
DISC_ID5=$(ic discovery submit --source=rollback-src --source-id=rb-002 --title="Rollback test 2" --score=0.5 --db="$TEST_DB")
```

`DISC_ID5` is assigned but never referenced afterward — only `DISC_ID4` has its post-rollback status checked. This is not a bug (the rollback count test implicitly covers both), but the unused variable is a minor clarity issue. Either check `DISC_ID5`'s status too, or use `_` assignment idiom in bash (i.e., drop the variable and run the submit bare since the output is not needed).

---

## Summary

The implementation is clean and consistent with project conventions. All errors are handled and exit codes are correct. The most actionable findings are:

1. **F1 (MEDIUM):** Four `json.Marshal` error values are silently discarded in `--json` output paths. Switch to `json.NewEncoder` with error checking, matching the pattern in `cmdEventsTail`.

2. **F2 (MEDIUM):** `saveCursor` always writes `interspect:0`, destroying any previously persisted interspect cursor. The interspect position should be threaded through as a parameter, following the same pattern now applied to the discovery position.

3. **F3–F5 (LOW):** `discoveryError` is verbose for no benefit (collapse the four identical branches), `nil` tx parameter needs a comment, and `--since-discovery` is missing from the help string and CLAUDE.md quick reference.

4. **F6–F8 (INFO):** Missing run-scoped discovery integration in `ListEvents` tests, undocumented cross-run semantics for discovery events in per-run queries, and an unused shell variable in the integration test.

**Verdict: needs-changes** (F1 and F2 should be fixed; F3–F5 are low-risk improvements).
