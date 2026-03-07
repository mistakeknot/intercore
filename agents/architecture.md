# Architecture

Security model, SQLite patterns, and schema upgrade procedures.

## Security

### Path Traversal Protection

The `--db` flag is validated: must end in `.db`, no `..` components, must resolve under CWD, parent directory must not be a symlink.

### JSON Payload Validation

Max 1MB size, 20 levels nesting, 1000-char keys, 100KB string values, 10000-element arrays.

### Lock Input Validation

Name and scope components reject `/`, `\`, `..`, empty, `.`. Resolved path must remain under `BaseDir`.

## SQLite Patterns

- `SetMaxOpenConns(1)` -- single writer for WAL mode correctness
- PRAGMAs set explicitly after `sql.Open` (DSN `_pragma` unreliable with modernc driver)
- `busy_timeout` set to prevent immediate `SQLITE_BUSY`
- `modernc.org/sqlite` does NOT support CTE + UPDATE RETURNING -- use direct `UPDATE ... RETURNING`
- Transaction isolation varies by operation (see `internal/` package docs for specifics)
- Pre-migration backup created automatically (`.backup-YYYYMMDD-HHMMSS`)
- Schema version inside transaction prevents TOCTOU
- `CREATE TABLE IF NOT EXISTS` makes migration idempotent

### Deployment: Schema Upgrade

```bash
go build -o /home/mk/go/bin/ic ./cmd/ic   # Rebuild (schema is //go:embed'd)
ic init                                     # Migrate live DB (creates backup)
ic version                                  # Verify schema version
```
