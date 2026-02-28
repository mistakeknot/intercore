# Task 4: CLI Command — `ic situation snapshot`

## Summary

Created `cmd/ic/situation.go` and registered the `situation` subcommand in `main.go`. The command provides a JSON snapshot of system state (runs, dispatches, events, queue) via the existing `observation.Collector`.

## Constructor Verification

Before writing code, verified all constructor signatures from source:

| Package | Constructor | Signature | File |
|---------|------------|-----------|------|
| `phase` | `New(db)` | `func New(db *sql.DB) *Store` | `internal/phase/store.go:25` |
| `dispatch` | `New(db, recorder)` | `func New(db *sql.DB, recorder DispatchEventRecorder) *Store` | `internal/dispatch/dispatch.go:101` |
| `event` | `NewStore(db)` | `func NewStore(db *sql.DB) *Store` | `internal/event/store.go:18` |
| `scheduler` | `NewStore(db)` | `func NewStore(db *sql.DB) *Store` | `internal/scheduler/store.go:17` |
| `observation` | `NewCollector(p, d, e, s)` | `func NewCollector(p PhaseQuerier, d DispatchQuerier, e EventQuerier, s SchedulerQuerier) *Collector` | `internal/observation/observation.go:105` |

Note: `phase` uses `New()` while `event` and `scheduler` use `NewStore()`. `dispatch.New()` takes a second `DispatchEventRecorder` arg (nil for read-only).

## Files Created/Modified

### Created: `cmd/ic/situation.go`

- `cmdSituation(ctx, args) int` — routes to `snapshot` subcommand
- `cmdSituationSnapshot(ctx, args) int` — parses `--run=<id>` and `--events=<n>` flags, opens DB, creates stores, runs `Collector.Collect()`, outputs JSON to stdout
- Follows existing CLI patterns: hand-rolled arg parser, exit codes 0/2/3, `slog.Error` for diagnostics

### Modified: `cmd/ic/main.go`

Added `case "situation"` to the switch block at line 135 (after `publish`, before `default`).

## Verification

### Build
```
go build -o ic ./cmd/ic  # success, no errors
```

### Smoke test
```
$ ./ic init --db=test-situation.db
initialized test-situation.db (schema v23)

$ ./ic situation snapshot --db=test-situation.db
{
  "timestamp": "2026-02-28T09:01:25.711105021Z",
  "runs": [],
  "dispatches": {
    "active": 0,
    "total": 0,
    "agents": []
  },
  "recent_events": [],
  "queue": {
    "pending": 0,
    "running": 0,
    "retrying": 0
  }
}
```

### Full test suite
```
go test ./... -count=1  # all 28 packages pass
```

## Design Notes

- The `dispatch.New(db, nil)` call passes nil for the event recorder since `situation snapshot` is read-only
- The `Collector` handles nil stores gracefully (skips queries), making it safe even if the DB has no data
- Default event limit is 20, matching the `Collector.Collect` default
- The command supports positional run ID (`ic situation snapshot <run-id>`) or `--run=<id>` flag
