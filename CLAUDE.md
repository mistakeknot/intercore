# CLAUDE.md

> **Documentation is in AGENTS.md** - This file contains Claude-specific settings only.
> For project documentation, architecture, and conventions, see [AGENTS.md](./AGENTS.md).

## Quick Commands

```bash
go build -o ic ./cmd/ic    # Build the binary
go test ./...               # Run all tests
go test -race ./...         # Run tests with race detector
bash test-integration.sh    # Run integration tests (builds ic, creates temp DB)
```

## Dispatch Quick Reference

```bash
# Spawn a codex agent
ic dispatch spawn --prompt-file=<f> --project=<dir> --name=<label> --output=<path>

# Check status / poll liveness / wait for completion
ic dispatch status <id> --json
ic dispatch poll <id>
ic dispatch wait <id> --timeout=5m

# Token tracking
ic dispatch tokens <id> --set --in=1000 --out=500 --cache=200

# List / kill / prune
ic dispatch list --active
ic dispatch kill <id>
ic dispatch prune --older-than=24h
```

## Run Quick Reference

```bash
# Create and advance a run
ic run create --project=. --goal="Implement feature X" --complexity=3
ic run create --project=. --goal="Quick fix" --phases='["plan","execute","done"]'
ic run create --project=. --goal="Big feature" --token-budget=500000 --budget-warn-pct=80
ic run advance <id>              # Advance to next phase
ic run phase <id>                # Print current phase
ic run current --project=.       # Print active run ID for project

# Manage runs
ic run status <id> --json        # Full details
ic run list --active             # Active runs
ic run events <id>               # Audit trail
ic run cancel <id>               # Cancel
ic run set <id> --complexity=1   # Adjust settings

# Skip, tokens, budget
ic run skip <id> <phase> --reason="Not needed"  # Pre-skip a phase
ic run tokens <id> --json        # Token aggregation across dispatches
ic run budget <id> --json        # Check budget thresholds (exit 1=exceeded)

# Track agents and artifacts within a run
ic run agent add <run> --type=claude --name=brainstorm-agent
ic run agent list <run>
ic run agent update <id> --status=completed
ic run artifact add <run> --phase=brainstorm --path=docs/brainstorms/x.md
ic run artifact list <run> --phase=brainstorm
```

## Lock Quick Reference

```bash
# Acquire / release a named lock
ic lock acquire <name> <scope> --timeout=1s --owner=$$:$(hostname -s)
ic lock release <name> <scope> --owner=$$:$(hostname -s)

# Inspect / clean
ic lock list
ic lock stale --older-than=5s
ic lock clean --older-than=5s
```

Lock commands are **filesystem-only** (no SQLite) — they work even when the DB is broken.

## Gate Quick Reference

```bash
ic gate check <run_id> [--priority=N]      # Dry-run gate eval (exit 0=pass, 1=fail)
ic gate override <run_id> --reason=<text>   # Force-advance past failed gate
ic gate rules [--phase=<p>]                 # Display gate rules table
```

## Event Bus Quick Reference

```bash
# Tail events for a run (one-shot)
ic events tail <run_id>
ic events tail --all                        # All runs

# Tail with consumer cursor (at-least-once delivery)
ic events tail <run_id> --consumer=my-hook
ic events tail --all --consumer=my-hook -f  # Follow mode with polling

# Cursor management
ic events cursor list                       # Show all consumer cursors
ic events cursor reset <consumer>           # Reset cursor to replay from start

# Options: --since-phase=N --since-dispatch=N --limit=100 --poll-interval=500ms
```

## Claude-Specific Settings

- Project uses Go 1.22 with SQLite (`modernc.org/sqlite`, pure Go, no CGO)
- Always use `SetMaxOpenConns(1)` when opening the database
- PRAGMAs must be set explicitly after `sql.Open` (DSN _pragma is unreliable)
- CTE wrapping `UPDATE ... RETURNING` is **not supported** by modernc.org/sqlite — use direct `UPDATE ... RETURNING` with row counting instead

## Design Decisions (Do Not Re-Ask)

- CLI only (no Go library API in v1) — bash hooks shell out to `ic`
- `PRAGMA user_version` only (no `schema_version` table)
- `--db` flag validates path traversal: `.db` extension, no `..`, under CWD
- Pre-migration backup created automatically (timestamped)
- Sentinel auto-prune runs synchronously in same transaction (not goroutine)
- TTL computation in Go (`time.Now().Unix()`) not SQL (`unixepoch()`) to avoid float promotion
