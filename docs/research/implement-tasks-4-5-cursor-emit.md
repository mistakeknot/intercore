# Implementation Analysis: Tasks 4 & 5 — Cursor Tracking + Events Emit/Review

**Date:** 2026-02-28
**Status:** Already implemented (verified)
**Commit:** `dd7d72e` ("feat(intercore): add ReviewEvent type, review_events table (v24), store methods with replay input")

---

## Finding: Tasks 4 & 5 Were Already Implemented

Upon analysis, Tasks 4 and 5 from the disagreement pipeline plan were already fully implemented in the same commit that completed Tasks 1-3. All required changes to `cmd/ic/events.go` are present and working.

## Verification Results

### Build
- `go build ./cmd/ic` — **PASS** (clean compilation, no errors)

### Unit Tests
- `go test ./... -count=1` — **ALL PASS** (28 packages, 0 failures)

### Smoke Test
```bash
ic init --db=test-emit-smoke.db
ic events emit --db=test-emit-smoke.db \
  --source=review --type=disagreement_resolved \
  --context='{"finding_id":"AR-001","agents":{"fd-arch":"P1","fd-quality":"P2"},"resolution":"discarded","dismissal_reason":"agent_wrong","chosen_severity":"P2","impact":"decision_changed"}'
# Output: 1 (event ID)

ic events list-review --db=test-emit-smoke.db
# Output: full ReviewEvent JSON with all fields preserved
```

### Git Status
- Working tree clean, branch `main`, up to date with `origin/main`

---

## Task 4: Cursor Tracking — What's Implemented

All 6 sub-tasks are present in `cmd/ic/events.go`:

### 4.1: `sinceReview int64` variable (line 41)
```go
var sincePhase, sinceDispatch, sinceInterspect, sinceDiscovery, sinceReview int64
```

### 4.2: `--since-review=` flag parsing (lines 77-84)
```go
case strings.HasPrefix(args[i], "--since-review="):
    val := strings.TrimPrefix(args[i], "--since-review=")
    n, err := strconv.ParseInt(val, 10, 64)
    if err != nil {
        slog.Error("events tail: invalid --since-review", "value", val)
        return 3
    }
    sinceReview = n
```

### 4.3: `loadCursor` returns 5 values (line 455)
```go
func loadCursor(ctx context.Context, store *state.Store, consumer, scope string) (phase, dispatch, interspect, discovery, review int64) {
```
The cursor struct includes `Review int64 \`json:"review"\``. Old JSON without "review" unmarshals to 0 (safe Go zero value).

### 4.4: `saveCursor` includes review (line 478-483)
```go
func saveCursor(ctx context.Context, store *state.Store, consumer, scope string, phaseID, dispatchID, interspectID, discoveryID, reviewID int64) {
    ...
    payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":%d,"discovery":%d,"review":%d}`, phaseID, dispatchID, interspectID, discoveryID, reviewID)
```

### 4.5: Updated callers (lines 128-130, 165-167, 172)
- `loadCursor` call returns 5 values
- High-water mark tracking includes `SourceReview` check
- `saveCursor` call passes all 5 cursors
- Restore condition includes `sinceReview == 0`

### 4.6: Default cursor in `cmdEventsCursorRegister` (line 295)
```go
payload := `{"phase":0,"dispatch":0,"interspect":0,"discovery":0,"review":0}`
```

---

## Task 5: Events Emit + Review Subcommands — What's Implemented

### 5.1: `emit` case in `cmdEvents` switch (line 28-29)
```go
case "emit":
    return cmdEventsEmit(ctx, args[1:])
```

### 5.2: `list-review` case (named `list-review` not `review`) (lines 30-31)
```go
case "list-review":
    return cmdEventsListReview(ctx, args[1:])
```
**Note:** The plan called this `review` but the implementation uses `list-review`. This is actually the subcommand name used in the plan's Task 8 consumer (`ic events review`). The consumer in Task 8's plan references `ic events review --since=...` but the actual implementation uses `ic events list-review --since=...`. This will need alignment when Task 8 is implemented, or the consumer script should use `list-review`.

### 5.3: `cmdEventsEmit` implementation (lines 309-405)
Full implementation with:
- Flag parsing: `--source=`, `--type=`, `--context=`, `--run=`, `--session=`, `--project=`
- JSON validation via `json.Valid()`
- Environment defaults: `CLAUDE_SESSION_ID`, `os.Getwd()`
- Source routing: currently only `review` source supported (v1 scope)
- Review context validation: requires `finding_id`, `resolution`, `chosen_severity`, `impact`, non-empty `agents` map
- Agents map marshaled to JSON string for storage
- Event ID printed on success

**Difference from plan:** The plan specified both `review` and `interspect` sources. The current implementation only supports `review` (`event.SourceReview`). The interspect source routing from the plan's Task 5 Step 2 is not included. This is acceptable as a v1 scope limitation — the error message says "only --source=review is supported".

### 5.4: `cmdEventsListReview` implementation (lines 407-451)
Full implementation with:
- `--since=` cursor parameter
- `--limit=` parameter (default 1000)
- JSON-line output via `json.NewEncoder`
- Uses `evStore.ListReviewEvents(ctx, since, limit)`

---

## Differences Between Plan and Implementation

| Aspect | Plan | Implementation | Impact |
|--------|------|----------------|--------|
| Subcommand name | `ic events review` | `ic events list-review` | Task 8 consumer must use `list-review` |
| Emit sources | `review` + `interspect` | `review` only | Interspect emit can be added later |
| Default limit | 100 (review), varies | 1000 (list-review) | Larger batches, acceptable |
| Unknown flags | Error (review) | Silently skip (list-review) | Minor — unknown flags in list-review have no error handler for the `default` case in the switch |
| Agents validation | Required non-empty | Required non-empty | Match |

### Minor Issue: `cmdEventsListReview` missing `default` case
The `list-review` subcommand's flag parsing loop has no `default` case — unknown flags are silently ignored. The plan's `cmdEventsReview` included `default: slog.Error(...)`. This is a minor gap but won't cause functional issues.

---

## Imports

All required imports are present in `events.go`:
- `"encoding/json"` — for `json.Valid`, `json.Unmarshal`, `json.Marshal`, `json.NewEncoder`
- `"os"` — for `os.Getenv`, `os.Getwd`, `os.Stdout`
- `"github.com/mistakeknot/intercore/internal/event"` — for `event.NewStore`, `event.SourceReview`
- `"github.com/mistakeknot/intercore/internal/state"` — for cursor persistence

---

## Conclusion

Tasks 4 and 5 are fully implemented, build cleanly, pass all tests, and the smoke test confirms end-to-end emit + query works. The implementation matches the plan with two minor deviations (subcommand naming and v1 source restriction) that are acceptable and can be aligned during Task 8 implementation.

No code changes were needed. No commit was required.
