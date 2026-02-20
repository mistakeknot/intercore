# Plan: E4.1 Kernel Interspect Events + E4.2 Durable Cursors
**Phase:** planned (as of 2026-02-20T06:37:40Z)

**Beads:** iv-3sns (E4.1), iv-shra (E4.2)
**Sprint:** iv-kdon
**Scope:** `infra/intercore/` — Go files only, no bash/hook changes

## Context

E4 (Interspect kernel event integration) needs two foundational pieces:
1. A new `interspect_events` table so corrections become first-class kernel events
2. Durable cursor registration so long-lived consumers (like Interspect) don't lose their position after 24h idle

Both are independent and can be built in parallel.

---

## Task 1: Kernel Interspect Events (iv-3sns)

### 1a. Schema migration (v6 → v7)

**File: `internal/db/schema.sql`** — append new table:

```sql
-- v7: interspect evidence events
CREATE TABLE IF NOT EXISTS interspect_events (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          TEXT,
    agent_name      TEXT NOT NULL,
    event_type      TEXT NOT NULL,
    override_reason TEXT,
    context_json    TEXT,
    session_id      TEXT,
    project_dir     TEXT,
    created_at      INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX IF NOT EXISTS idx_interspect_events_agent ON interspect_events(agent_name);
CREATE INDEX IF NOT EXISTS idx_interspect_events_created ON interspect_events(created_at);
CREATE INDEX IF NOT EXISTS idx_interspect_events_run ON interspect_events(run_id) WHERE run_id IS NOT NULL;
```

**File: `internal/db/db.go`** — bump `currentSchemaVersion` and `maxSchemaVersion` to 7. No ALTER TABLE needed — v7 is a new table, `CREATE TABLE IF NOT EXISTS` in schema.sql handles it.

### 1b. Event store methods

**File: `internal/event/event.go`** — add source constant:

```go
const SourceInterspect = "interspect"
```

**File: `internal/event/store.go`** — add two methods:

```go
// AddInterspectEvent records a human correction or agent dispatch signal.
func (s *Store) AddInterspectEvent(ctx context.Context, runID, agentName, eventType, overrideReason, contextJSON, sessionID, projectDir string) (int64, error)

// ListInterspectEvents returns interspect events, optionally filtered by agent name.
func (s *Store) ListInterspectEvents(ctx context.Context, agentName string, since int64, limit int) ([]InterspectEvent, error)

// MaxInterspectEventID returns the highest interspect_events.id (for cursor tracking).
func (s *Store) MaxInterspectEventID(ctx context.Context) (int64, error)
```

New struct (same file or event.go):

```go
type InterspectEvent struct {
    ID             int64     `json:"id"`
    RunID          string    `json:"run_id,omitempty"`
    AgentName      string    `json:"agent_name"`
    EventType      string    `json:"event_type"`
    OverrideReason string    `json:"override_reason,omitempty"`
    ContextJSON    string    `json:"context_json,omitempty"`
    SessionID      string    `json:"session_id,omitempty"`
    ProjectDir     string    `json:"project_dir,omitempty"`
    Timestamp      time.Time `json:"timestamp"`
}
```

**Design decisions:**
- `InterspectEvent` is a separate struct from `Event` because its fields are fundamentally different (agent_name, override_reason, context_json don't map to from_state/to_state)
- The `ListAllEvents`/`ListEvents` UNION queries are NOT extended to include interspect events — those queries power `ic events tail` which is phase+dispatch focused. Interspect events get their own query path via `ic interspect query` (E4.6, future)
- The cursor system DOES need to track interspect event IDs for future consumer use — add `MaxInterspectEventID` now

### 1c. CLI: `ic interspect record`

**File: `cmd/ic/interspect.go`** (new file) — add `cmdInterspect` dispatcher:

```go
func cmdInterspect(ctx context.Context, args []string) int
// Subcommands: "record"

func cmdInterspectRecord(ctx context.Context, args []string) int
// Flags:
//   --run=<id>           optional run ID
//   --agent=<name>       required agent name
//   --type=<type>        required: "correction" or "agent_dispatch"
//   --reason=<reason>    optional: "agent_wrong", "deprioritized", "already_fixed"
//   --context=<json>     optional context JSON
//   --session=<id>       optional Claude session ID
//   --project=<dir>      optional project directory
```

**File: `cmd/ic/main.go`** — add `case "interspect"` to the main dispatcher, routing to `cmdInterspect`.

### 1d. Tests

**File: `internal/event/store_test.go`** — add table-driven tests:
- `TestAddInterspectEvent`: insert and verify fields
- `TestListInterspectEvents`: filter by agent, since cursor, limit
- `TestListInterspectEventsEmpty`: no results returns empty slice
- `TestMaxInterspectEventID`: returns highest ID, 0 when empty

**File: `cmd/ic/interspect_test.go`** (new file) — integration test:
- `TestInterspectRecord`: runs `cmdInterspectRecord` with valid flags, verifies event in DB
- `TestInterspectRecordMissingAgent`: validates required flags

### Files Changed/Created

| File | Action | ~Lines |
|------|--------|--------|
| `internal/db/schema.sql` | Edit (append table) | +12 |
| `internal/db/db.go` | Edit (bump version) | +2 |
| `internal/event/event.go` | Edit (add constant + struct) | +15 |
| `internal/event/store.go` | Edit (add 3 methods) | +60 |
| `internal/event/store_test.go` | Edit (add tests) | +80 |
| `cmd/ic/interspect.go` | Create | +90 |
| `cmd/ic/main.go` | Edit (add case) | +2 |

---

## Task 2: Durable Cursor Registration (iv-shra)

### 2a. CLI: `ic events cursor register`

**File: `cmd/ic/events.go`** — add `case "register"` to `cmdEventsCursor`:

```go
func cmdEventsCursorRegister(ctx context.Context, args []string) int
// Usage: ic events cursor register <consumer> [--durable]
// Creates cursor entry at position {phase:0, dispatch:0, interspect:0}
// --durable uses TTL=0 (no expiration), else uses default 24h TTL
```

Also update `saveCursor` to accept a TTL parameter (currently hardcoded to 24h). The `--durable` flag on `ic events tail --consumer=<name>` should also be supported to override the 24h default.

### 2b. Cursor schema extension for interspect

Update `loadCursor`/`saveCursor` to include a third field `interspect` for tracking the interspect_events cursor position:

```go
var cursor struct {
    Phase      int64 `json:"phase"`
    Dispatch   int64 `json:"dispatch"`
    Interspect int64 `json:"interspect"`
}
```

This is backward-compatible — old cursors without the `interspect` field will decode to 0 (start from beginning).

### 2c. Update cursor list output

Enhance `cmdEventsCursorList` to show whether each cursor is durable. Query `state` table for `expires_at IS NULL` to determine durability.

### 2d. Tests

**Add to existing `cmd/ic/events_test.go` or `internal/event/store_test.go`:**
- `TestCursorRegister`: register a consumer, verify cursor exists with position 0,0,0
- `TestCursorRegisterDurable`: register with --durable, verify TTL=0 (no expiration)
- `TestCursorBackwardCompat`: load cursor without `interspect` field, verify defaults to 0

### Files Changed

| File | Action | ~Lines |
|------|--------|--------|
| `cmd/ic/events.go` | Edit (add register, update cursor helpers) | +50 |
| `internal/event/store_test.go` or test file | Edit (add tests) | +40 |

---

## Files NOT Modified

- `internal/db/schema.sql` — only E4.1 modifies it (new table, no ALTER)
- `internal/budget/` — untouched
- `internal/dispatch/` — untouched
- `internal/phase/` — untouched
- `hub/clavain/` — no hook changes (that's E4.3-E4.6)

## Verification

```bash
cd /root/projects/Interverse/infra/intercore
go test ./internal/event/... -v -count=1
go test ./cmd/ic/... -v -count=1        # if integration tests exist
go test ./... -count=1                   # full suite, no regressions
go vet ./...
```

## Parallel Execution Strategy

E4.1 and E4.2 are independent — they can be dispatched to separate Codex agents. The only shared touchpoint is:
- E4.2's cursor extension adds the `interspect` field to cursor JSON, which E4.1's `MaxInterspectEventID` feeds into
- Resolution: E4.2 adds the field to cursor helpers; E4.1 adds `MaxInterspectEventID` to the store. No merge conflict since they modify different functions in different files.
