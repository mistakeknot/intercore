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

# List / kill / prune
ic dispatch list --active
ic dispatch kill <id>
ic dispatch prune --older-than=24h
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
