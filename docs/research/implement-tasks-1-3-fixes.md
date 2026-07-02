# Implementation Analysis: Tasks 1-3 Disagreement Pipeline Fixes

## Summary

Tasks 1-3 of the disagreement pipeline plan were partially implemented. This analysis documents what was already done, what gaps were found, and what changes were made to complete the implementation.

## Pre-existing State

The following work was already completed before this session:

1. **`internal/event/event.go`**: `SourceReview` constant and `ReviewEvent` struct already present (Task 1 complete)
2. **`internal/db/schema.sql`**: `review_events` DDL already appended (lines 183-198)
3. **`internal/db/db.go`**: Version bumped to 24 (`currentSchemaVersion = 24`, `maxSchemaVersion = 24`)
4. **`internal/event/store.go`**: `AddReviewEvent`, `ListReviewEvents`, `MaxReviewEventID` store methods already present, plus `ListEvents` and `ListAllEvents` signatures updated to include `sinceReviewID` parameter and review_events in UNION ALL queries
5. **`internal/event/store_test.go`**: All four test functions already present (`TestAddReviewEvent`, `TestAddReviewEvent_OptionalFields`, `TestListReviewEvents_SinceCursor`, `TestMaxReviewEventID`)
6. **`cmd/ic/events.go`** and **`cmd/ic/run.go`**: CLI callers already updated with `sinceReview` parameter
7. **`internal/observation/observation.go`**: Interface and callers already updated

## Gaps Found and Fixed

### Gap 1: Missing migration block in db.go

The `currentSchemaVersion` was bumped to 24 but no v23->v24 migration block existed in `Migrate()`. Added the migration block after the v22->v23 block:

```go
// v23 -> v24: review events (disagreement resolution pipeline)
if currentVersion >= 20 && currentVersion < 24 {
    // CREATE TABLE IF NOT EXISTS review_events ...
    // CREATE INDEX IF NOT EXISTS idx_review_events_finding ...
    // CREATE INDEX IF NOT EXISTS idx_review_events_created ...
}
```

**File:** `/home/mk/projects/Demarch/core/intercore/internal/db/db.go` (lines 356-375)

### Gap 2: Missing review_events DDL in 020_baseline.sql

The baseline migration (used for fresh installs) was missing the review_events table. Added after the interspect_events section, matching the baseline pattern (plain `CREATE TABLE`, no `IF NOT EXISTS`):

```sql
-- v24: review events (disagreement resolution pipeline)
CREATE TABLE review_events ( ... );
CREATE INDEX idx_review_events_finding ON review_events(finding_id);
CREATE INDEX idx_review_events_created ON review_events(created_at);
```

**File:** `/home/mk/projects/Demarch/core/intercore/internal/db/migrations/020_baseline.sql` (lines 185-200)

### Gap 3: Missing 024_review_events.sql additive migration file

The migrator system reads numbered `.sql` files from `internal/db/migrations/`. Without a `024_*.sql` file, `MaxVersion()` returned 23, causing tests that verify schema version to fail. Created the new migration file.

**File:** `/home/mk/projects/Demarch/core/intercore/internal/db/migrations/024_review_events.sql`

### Gap 4: Missing reviewReplayPayload in replay_capture.go

The replay payload function for review events was missing. Added `reviewReplayPayload()` following the same pattern as `dispatchReplayPayload()` and `coordinationReplayPayload()`.

**File:** `/home/mk/projects/Demarch/core/intercore/internal/event/replay_capture.go` (lines 62-79)

### Gap 5: Missing replay input capture in AddReviewEvent

`AddReviewEvent` in store.go returned `result.LastInsertId()` directly without creating a replay input. Updated to capture the ID, then call `insertReplayInput()` with `SourceReview` event source, matching the dispatch/coordination pattern (per PRD F1).

**File:** `/home/mk/projects/Demarch/core/intercore/internal/event/store.go` (lines 338-351)

### Gap 6: ListEvents/ListAllEvents call site arity mismatches in store_test.go

The `ListEvents` function signature was already updated to accept `sinceReviewID int64`, but 7 test call sites in `store_test.go` still passed only 4 numeric args (missing the sinceReviewID). Fixed all calls to pass 5 numeric args (adding `0` for sinceReviewID).

**File:** `/home/mk/projects/Demarch/core/intercore/internal/event/store_test.go` (lines 75, 118, 173, 216, 225, 258, 325)

Similarly, 2 `ListAllEvents` calls were missing the `sinceReviewID` parameter (lines 293, 361).

### Gap 7: Migrator test expected stale migration count

`TestMigrator_V22ToV23_AuditTraceID` expected exactly 1 migration applied (v22->v23) with final version 23. With the new v24 migration file, 2 migrations are now applied and final version is 24. Updated expectations.

**File:** `/home/mk/projects/Demarch/core/intercore/internal/db/migrator_test.go` (lines 121-132)

## Files Modified

| File | Change |
|------|--------|
| `internal/db/db.go` | Added v23->v24 migration block |
| `internal/db/migrations/020_baseline.sql` | Added review_events DDL |
| `internal/db/migrations/024_review_events.sql` | **New file** - additive migration |
| `internal/db/migrator_test.go` | Updated v22->v23 test expectations |
| `internal/event/replay_capture.go` | Added `reviewReplayPayload()` |
| `internal/event/store.go` | Added replay input capture to `AddReviewEvent` |
| `internal/event/store_test.go` | Fixed ListEvents/ListAllEvents call site arity |

## Test Results

All tests pass:

- `go build ./...` -- clean compilation
- `go test ./internal/event/ -v` -- 40/40 pass (including 4 new review event tests)
- `go test ./internal/db/ -v` -- 27/27 pass (including migrator tests)

## Architecture Notes

The review_events table follows the same pattern as interspect_events:
- Dedicated store methods (not merged into unified Event type)
- Included in ListEvents/ListAllEvents UNION ALL for run-scoped event streams
- Replay input captured for deterministic run replay (PRD F1)
- Cursor-based pagination via `since` parameter
- No FK to runs (run_id is nullable, stored as NULLIF)
