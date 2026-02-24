# Correctness Review — Intercore E5 Discovery Pipeline Changes

Reviewer: Julik (Flux-drive Correctness Reviewer)
Date: 2026-02-20
Diff: /tmp/qg-diff-1771620189.txt

---

## Invariants Established Before Review

These must hold across all write paths, retries, and partial failures:

1. **Discovery lifecycle is a one-way DAG.** Valid forward transitions: `new → scored`, `new/scored → promoted`, `new/scored → dismissed`. Dismissed and promoted discoveries cannot be re-scored or re-promoted. No backward transitions.
2. **Every state change has a corresponding event.** Submitting, scoring, promoting, dismissing, decaying, and deduping each emit exactly one `discovery_events` row in the same transaction. Events are never emitted without the corresponding state change, and vice versa.
3. **Cursor independence.** `phase_events`, `dispatch_events`, and `discovery_events` have separate AUTOINCREMENT sequences. Cursors track high-water marks per-table, not globally.
4. **Deduplication atomicity.** The read-similarity-scan + conditional-insert in `SubmitWithDedup` are in a single transaction to prevent TOCTOU.
5. **Score/tier consistency.** `confidence_tier` is always the value of `TierFromScore(relevance_score)` as of the most recent write. They are never updated independently.

---

## Changed Files Summary

| File | Nature | Lines |
|---|---|---|
| `cmd/ic/discovery.go` | NEW — CLI for 11 discovery subcommands | 642 |
| `cmd/ic/events.go` | Extended cursor load/save for third ID space | ~50 changed |
| `internal/event/store.go` | Third UNION ALL leg + MaxDiscoveryEventID | ~30 changed |
| `internal/event/store_test.go` | 90 new test lines for discovery event integration | 90 added |
| `internal/discovery/discovery.go` | Types, constants, helpers | pre-existing (full context) |
| `internal/discovery/store.go` | All CRUD + event emission | pre-existing (full context) |
| `internal/discovery/errors.go` | Sentinel errors | pre-existing (full context) |
| `internal/db/schema.sql` | v9 schema: 4 new tables | pre-existing (full context) |

---

## Analysis by Area

### 1. UNION ALL Query Correctness (internal/event/store.go)

**ListAllEvents** — correct. The three legs are:

```sql
SELECT id, run_id, 'phase' AS source, ...
FROM phase_events WHERE id > ?

UNION ALL

SELECT id, COALESCE(run_id, '') AS run_id, 'dispatch' AS source, ...
FROM dispatch_events WHERE id > ?

UNION ALL

SELECT id, COALESCE(discovery_id, '') AS run_id, 'discovery' AS source, ...
FROM discovery_events WHERE id > ?

ORDER BY created_at ASC, source ASC, id ASC
LIMIT ?
```

Parameter binding in `ListAllEvents`: `sincePhaseID, sinceDispatchID, sinceDiscoveryID, limit` — 4 parameters, 4 `?` placeholders. Correct.

**ListEvents (run-scoped) — ISSUE FOUND (C-01)**

```sql
-- phase leg
WHERE run_id = ? AND id > ?          -- binds: runID, sincePhaseID

-- dispatch leg
WHERE (run_id = ? OR ? = '') AND id > ?   -- binds: runID, runID, sinceDispatchID

-- discovery leg
WHERE id > ?                         -- binds: sinceDiscoveryID
```

Parameter binding count: 2 + 3 + 1 + 1(limit) = 7. Actual args: `runID, sincePhaseID, runID, runID, sinceDispatchID, sinceDiscoveryID, limit` = 7. Binding is correct.

However the discovery leg has no `run_id` predicate. `discovery_events` does not have a `run_id` column — it has `discovery_id`. There is no natural way to filter discovery events by run ID without a schema change. The consequence: `ic events tail <run_id>` returns all discovery events in the database, not just those associated with the named run. This leaks cross-run discovery noise into per-run consumers. See C-01 below for full analysis.

**Cursor update loop in cmdEventsTail** — correct. The loop advances `sincePhase`, `sinceDispatch`, and `sinceDiscovery` independently based on `e.Source`. Because the UNION ALL returns a single merged stream with the `source` column intact, the cursor is correctly routed per event. The high-water-mark update only fires after a successful `enc.Encode(e)`, preserving at-least-once semantics on output failure.

### 2. Cursor Backward Compatibility

**JSON format change: 3 fields → 4 fields**

Old format (before this diff, from `saveCursor`):
```json
{"phase":N,"dispatch":N,"interspect":0}
```

New format:
```json
{"phase":N,"dispatch":N,"interspect":0,"discovery":N}
```

`loadCursor` uses `json.Unmarshal` into a struct with `json:"discovery"`. When an old cursor (3 fields) is loaded, Go's JSON decoder sets `Discovery` to its zero value (0), which is safe — the consumer will read all discovery events from the beginning. This is the correct at-least-once behavior: on first upgrade with an existing cursor, the consumer replays all discovery events once, then the cursor advances normally. No data loss, no skipping. Backward compatibility is safe.

**cmdEventsCursorRegister** now initializes with `{"phase":0,"dispatch":0,"interspect":0,"discovery":0}`. Old consumers that registered before E5 will get `discovery=0` from old cursor data via loadCursor's zero-default behavior. Correct.

**Cursor restore guard:**
```go
if consumer != "" && sincePhase == 0 && sinceDispatch == 0 && sinceDiscovery == 0 {
    sincePhase, sinceDispatch, sinceDiscovery = loadCursor(...)
}
```
The guard only skips cursor loading if all three are zero. This is intentional: the `--since-X=N` flags allow overriding individual cursors. However `--since-discovery=0` cannot override the stored cursor (it looks like "no override" to the guard). See C-06.

### 3. Transaction Safety (internal/discovery/store.go)

Every mutating method (`Submit`, `SubmitWithDedup`, `Score`, `Promote`, `Dismiss`, `RecordFeedback`, `Decay`, `Rollback`) uses `BeginTx` + `defer tx.Rollback()` + explicit `tx.Commit()`. If `Commit` is not reached (panic, error return), the transaction rolls back cleanly. This is the correct pattern for this codebase.

**Atomicity of discovery + event pair:** Each operation writes the state change and emits the event in the same transaction. If the state update succeeds but the event INSERT fails, the entire transaction rolls back — no orphaned state without an event, and no event without a state change. Invariant 2 holds for all methods except one:

**Decay has a split-read-then-write pattern (C-02):** The `Decay` function reads eligible discoveries outside a transaction, then opens a separate transaction to apply updates. Between the read and the write, an intervening `Score` or `Promote` call can change a row's status, causing the subsequent Decay transaction to update a promoted discovery's score. This violates Invariant 1 (promoted discoveries should be stable). See C-02 for full details.

**SubmitWithDedup atomicity:** The comment at line 69 states "Scan + insert happen in a single transaction to prevent TOCTOU." This is correct — the full similarity scan and conditional insert are within the same `BeginTx`/`Commit` envelope. The single-connection constraint (`SetMaxOpenConns(1)`) ensures SQLite serializes all writes, so two concurrent `SubmitWithDedup` calls for the same source cannot both read "no match" and both insert. This invariant holds.

### 4. SQL Parameter Binding Verification

**Submit** (lines 40–43):
```sql
INSERT INTO discoveries (id, source, source_id, title, summary, url, raw_metadata, embedding, relevance_score, confidence_tier, status, discovered_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
```
12 `?` → args: `id, source, sourceID, title, summary, url, rawMetadata, embedding, score, tier, StatusNew, now` = 12. Correct.

**Score** UPDATE (line 272):
```sql
UPDATE discoveries SET relevance_score = ?, confidence_tier = ?, status = ? WHERE id = ?
```
4 `?` → args: `score, tier, StatusScored, id` = 4. Correct.

**Promote** UPDATE (line 329):
```sql
UPDATE discoveries SET status = ?, bead_id = ?, promoted_at = ? WHERE id = ?
```
4 `?` → args: `StatusPromoted, beadID, now, id` = 4. Correct.

**Rollback** UPDATE (lines 610–612):
```sql
UPDATE discoveries SET status = 'dismissed', reviewed_at = ?
WHERE source = ? AND discovered_at >= ? AND status NOT IN ('promoted', 'dismissed')
RETURNING id
```
3 `?` → args: `now, source, sinceTimestamp` = 3. Correct.

**ListEvents** (lines 58–62):
```
runID, sincePhaseID,            -- 2
runID, runID, sinceDispatchID,  -- 3
sinceDiscoveryID,               -- 1
limit,                          -- 1
```
Total: 7. Placeholders in SQL: 2 + 3 + 1 + 1 = 7. Correct.

**ListAllEvents** (line 96):
`sincePhaseID, sinceDispatchID, sinceDiscoveryID, limit` = 4. Placeholders: 4. Correct.

No parameter binding errors found.

### 5. Lifecycle State Machine

State transitions enforced in code:
- `Score`: blocks if `status == dismissed || status == promoted`
- `Promote`: blocks if `status == dismissed`; idempotent if `status == promoted`
- `Dismiss`: no idempotency guard (C-04)
- `Rollback`: UPDATE's `WHERE` clause excludes `promoted` and `dismissed` rows — correct, will not double-dismiss via rollback path

The `StatusProposed` constant is defined in `discovery.go` but no code path transitions to it. It appears to be planned for a future state ("proposed for bead inclusion" prior to full promotion). It is not a bug — it does not corrupt the current state machine.

### 6. Dedup Logic

`SubmitWithDedup` scans `WHERE source = ? AND embedding IS NOT NULL` — all rows for the source, including dismissed and promoted ones. A similarity hit against a dismissed record returns the dismissed ID to the caller. The caller (CLI) prints the ID and exits 0. If the caller then attempts `ic discovery score <id>`, it will receive `ErrLifecycle`. This is confusing but not corrupting. See I-04 for the improvement.

The similarity scan exits on the first hit above threshold:
```go
if sim >= dedupThreshold {
    existingID = eid
    found = true
    break
}
```
This returns the first match, not the best match. For near-duplicate detection this is typically acceptable, but users should be aware that with multiple near-duplicates the one with the lowest ROWID wins.

### 7. Schema and Migration

The `discovery_events.from_status` and `to_status` columns have `NOT NULL DEFAULT ''`. This means the decay batch event (which uses `discovery_id = ''`, `from_status` and `to_status` both implicitly defaulting) will insert correctly. However `discovery_id = ''` is a non-standard use of a column that conceptually should reference a discovery. The decay event intentionally uses an empty string to represent a batch operation with no single discovery ID. This is workable but means the `discovery_id` column is not uniformly a valid ID, which could confuse consumers.

Migration: v9 tables use `CREATE TABLE IF NOT EXISTS`, which is idempotent. The db.go migration code applies the full `schemaDDL` on every migration run. For the v8→v9 transition, the new tables will be created. For databases already at v9, the tables already exist so no error occurs. This is safe. The missing explicit `if currentVersion >= 8` guard (I-01) is a documentation gap, not a correctness issue.

### 8. Test Coverage

**New unit tests in store_test.go:**
- `TestListAllEvents_IncludesDiscovery` — verifies discovery events appear in unified stream. Passes.
- `TestMaxDiscoveryEventID` — verifies the new Max function. Passes.
- `TestMaxEventIDs_EmptyTables` extended to cover discovery. Passes.

**Missing tests:**
- No test for `ListEvents` (run-scoped) with discovery events — this is where C-01 would be caught.
- No test for `Dismiss` idempotency (double-dismiss should be a no-op; currently emits a second event).
- No test for `Decay` concurrent modification (Decay reads a row, Score modifies it, Decay writes stale data).
- No test for `SubmitWithDedup` hitting a dismissed record.

The discovery store_test.go has good coverage for the happy paths and key error paths (not-found, lifecycle blocks, gate blocks, dedup). The concurrency/TOCTOU scenarios are not covered by the unit tests.

---

## Findings Summary

### C-01 (MEDIUM) — Discovery leg ignores run-ID filter in ListEvents

File: `/root/projects/Interverse/infra/intercore/internal/event/store.go`, lines 52–56.

`ListEvents` is called when a consumer tails events for a specific run (`ic events tail <run_id>`). The phase and dispatch legs correctly filter by `run_id`. The discovery leg has no equivalent filter because `discovery_events` has no `run_id` column. Result: every consumer watching a specific run receives all discovery events in the database, not just discovery events relevant to that run.

Interleaving that produces incorrect behavior:
1. Consumer A watches `run-001` with `--consumer=reactor`.
2. Background scan submits 50 discoveries unrelated to `run-001`.
3. Consumer A's next poll via `ListEvents(ctx, "run-001", ...)` returns all 50 discovery events.
4. Consumer A's logic (e.g., "advance phase if relevant discovery found") fires spuriously.

Smallest fix: Add a comment to `ListEvents` docstring stating that discovery events are returned regardless of `runID` (since they lack a run-scoped FK), so callers that need run-scoped filtering must do it in their own logic. Alternatively, add `run_id` to the `discovery_events` schema (v10 migration) and populate it optionally when a discovery is submitted within a run context.

### C-02 (MEDIUM) — Decay reads snapshot outside transaction, writes in separate transaction

File: `/root/projects/Interverse/infra/intercore/internal/discovery/store.go`, lines 467–528.

The `SELECT id, relevance_score` query runs outside any transaction (line 471: `s.db.QueryContext`). The subsequent UPDATE loop runs inside a separate `BeginTx` (line 499). Between these two calls, any concurrent write that changes a discovery's status can cause Decay to update a promoted discovery's score.

With `SetMaxOpenConns(1)`, SQLite serializes write transactions, but does not prevent a read outside a transaction from seeing a snapshot that is stale by the time the write transaction starts. The window is small but real: in a long-running decay operation with many rows, the SELECT → BeginTx gap is on the order of milliseconds to seconds.

Fix: Move the SELECT inside the transaction:
```go
tx, err := s.db.BeginTx(ctx, nil)
if err != nil { ... }
defer tx.Rollback()

rows, err := tx.QueryContext(ctx, `SELECT id, relevance_score FROM discoveries ...`, cutoff)
```

This ensures the rows read and the rows updated are from the same consistent snapshot.

### C-03 (LOW) — Interspect cursor always written as zero in saveCursor

File: `/root/projects/Interverse/infra/intercore/cmd/ic/events.go`, line 322.

```go
payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":0,"discovery":%d}`, phaseID, dispatchID, discoveryID)
```

This is a pre-existing issue (predates E5), but the E5 change adds a fourth cursor field following the correct pattern. The `interspect` field is correctly read from storage at line 309 but not returned by `loadCursor` and not written by `saveCursor`. Any consumer relying on `interspect` event cursor durability will replay from the beginning on every restart. Since `SourceInterspect` events are not currently in the UNION ALL query (they come from a separate `ListInterspectEvents` path), this does not affect the discovery pipeline. But it is a latent correctness bug in the cursor system.

### C-04 (LOW) — Dismiss is not idempotent: double-dismiss emits duplicate event

File: `/root/projects/Interverse/infra/intercore/internal/discovery/store.go`, lines 352–386.

`Dismiss` does not check whether `status == dismissed` before proceeding. A second `Dismiss` call on an already-dismissed discovery will:
- Execute `UPDATE discoveries SET status = 'dismissed', reviewed_at = ?` (no-op for status, but updates the timestamp)
- Insert a second `discovery.dismissed` event into `discovery_events`

Compare to `Promote` which explicitly returns `nil` when `status == promoted`. The asymmetry is a bug. Consumers reading the event stream will see two `dismissed` events for one discovery, which can trigger double-processing.

Fix:
```go
if status == StatusDismissed {
    return nil
}
```

### C-05 (INFO) — Rollback emits dismissed events with empty from_status

File: `/root/projects/Interverse/infra/intercore/internal/discovery/store.go`, lines 633–636.

The `Rollback` function emits `EventDismissed` for each affected discovery with `from_status = ''`. The prior status (which could be `new`, `scored`, etc.) is not captured. The audit trail loses the information of what state was bypassed by the bulk rollback.

No data corruption — the discoveries themselves are correctly updated to `dismissed`. This is purely an observability/auditability gap.

### C-06 (INFO) — --since-discovery=0 cannot override stored cursor

File: `/root/projects/Interverse/infra/intercore/cmd/ic/events.go`, lines 115–117.

A user who passes `--since-discovery=0` explicitly expects to replay from the beginning, but the guard `sinceDiscovery == 0` causes the stored cursor to be loaded anyway. The only way to force replay is `ic events cursor reset <consumer>`. This is consistent with the existing behavior for phase and dispatch cursors; it is not a regression. Documenting this limitation in the CLI help would prevent user confusion.

---

## Improvements

**I-01** — Add explicit `if currentVersion >= 8` guard in db.go migration path for self-documenting upgrade clarity.

**I-02** — Confirm that `idx_discovery_events_discovery` covers the Rollback per-ID event loop (it does — `discovery_id = ?` uses this index).

**I-03** — Add `0.0 <= score <= 1.0` validation in CLI score commands to reject malformed input before it reaches the store.

**I-04** — Filter `SubmitWithDedup` scan to `status NOT IN ('dismissed')` to avoid returning dismissed IDs as dedup matches.
