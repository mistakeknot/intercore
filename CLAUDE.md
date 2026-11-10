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

## CLI Reference

Full `ic` command reference is in [agents/cli-reference.md](agents/cli-reference.md); package notes are in [docs/agent-cli-reference.md](docs/agent-cli-reference.md). Key commands: `ic run`, `ic dispatch`, `ic gate`, `ic events`, `ic lock`, `ic coordination`, `ic cost`, `ic discovery`.

## Claude-Specific Settings

- Go version is whatever `go.mod` says; storage is SQLite (`modernc.org/sqlite`, pure Go, no CGO)
- Always use `SetMaxOpenConns(1)` when opening the database
- PRAGMAs must be set explicitly after `sql.Open` (DSN _pragma is unreliable)
- CTE wrapping `UPDATE ... RETURNING` is **not supported** by modernc.org/sqlite — use direct `UPDATE ... RETURNING` with row counting instead

## Design Decisions (Do Not Re-Ask)

- The CLI is the primary contract: bash hooks shell out to `ic`. `pkg/` is also importable by other modules (Clavain and Autarch use it); `internal/` is not. See [COMPATIBILITY.md](COMPATIBILITY.md)
- `PRAGMA user_version` only (no `schema_version` table)
- `--db` flag validates path traversal: `.db` extension, no `..`, under CWD
- Pre-migration backup created automatically (timestamped)
- Sentinel auto-prune runs synchronously in same transaction (not goroutine)
- TTL computation in Go (`time.Now().Unix()`) not SQL (`unixepoch()`) to avoid float promotion
