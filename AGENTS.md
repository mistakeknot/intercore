# AGENTS.md — intercore

## Overview

intercore is a Go CLI (`ic`) backed by a single SQLite WAL database that provides atomic state operations and throttle guards callable from bash hooks. It replaces ~15 scattered temp files in `/tmp/` used by the Clavain hook infrastructure.

**Location:** `infra/intercore/` (infrastructure, not a plugin — hooks depend on it)
**Database:** `.clavain/intercore.db` (project-relative, auto-discovered by walking up from CWD)

## Architecture

```
cmd/ic/main.go          CLI entry point, argument parsing, subcommand dispatch
internal/db/db.go       SQLite connection, migration, health check
internal/db/schema.sql  Embedded DDL (two tables: state, sentinels)
internal/db/disk.go     Disk space check (Linux syscall)
internal/state/         State CRUD with JSON validation and TTL
internal/sentinel/      Atomic throttle guards with CTE+RETURNING
lib-intercore.sh        Bash wrappers for hooks
test-integration.sh     End-to-end integration test
```

## CLI Commands

```
ic init                                    Create/migrate the database
ic health                                  Check DB readable, schema current, disk space
ic version                                 Print CLI and schema versions
ic sentinel check <name> <scope> --interval=<sec>   Atomic claim (exit 0=allowed, 1=throttled)
ic sentinel reset <name> <scope>           Clear a sentinel
ic sentinel list                           List all sentinels
ic sentinel prune --older-than=<dur>       Remove old sentinels
ic state set <key> <scope> [--ttl=<dur>]   Write JSON (stdin or @filepath)
ic state get <key> <scope>                 Read JSON (exit 0=found, 1=not found)
ic state delete <key> <scope>              Remove a state entry
ic state list <key>                        List scope_ids for a key
ic state prune                             Remove expired state entries
ic compat status                           Show legacy temp file vs DB coverage
ic compat check <key>                      Check if key has data in DB
```

### Exit Codes

| Code | Meaning | Example |
|------|---------|---------|
| 0 | Success / allowed / found | `ic state get` returns payload |
| 1 | Expected negative result | `ic sentinel check` → throttled |
| 2 | Unexpected error | Invalid JSON, DB corruption |
| 3 | Usage error | Missing required argument |

### Global Flags

- `--db=<path>` — Database path (default: `.clavain/intercore.db`, auto-discovered)
- `--timeout=<dur>` — SQLite busy timeout (default: 100ms)
- `--verbose` — Verbose output
- `--json` — JSON output

## Security

### Path Traversal Protection

The `--db` flag is validated by `validateDBPath()`:
- Must end in `.db` extension
- No `..` path components
- Must resolve to a path under CWD
- Parent directory must not be a symlink

### JSON Payload Validation

All payloads are validated before storage:
- Size: max 1MB
- Nesting depth: max 20 levels
- Key length: max 1000 chars
- String values: max 100KB each
- Array length: max 10,000 elements

## SQLite Patterns

### Connection Management
- `SetMaxOpenConns(1)` — single writer for WAL mode correctness
- PRAGMAs set explicitly after `sql.Open` (DSN `_pragma` unreliable with modernc driver)
- `busy_timeout` set to prevent immediate SQLITE_BUSY on contention

### Important: No CTE + UPDATE RETURNING
`modernc.org/sqlite` does NOT support `WITH claim AS (UPDATE ... RETURNING) SELECT ...`. Use direct `UPDATE ... RETURNING` with row counting instead. This is a known limitation.

### Transaction Isolation
| Operation | Isolation | Rationale |
|-----------|-----------|-----------|
| state set | Transaction (default) | Write with REPLACE |
| state get | No transaction | Read-only |
| sentinel check | Transaction (default) | Atomic claim + auto-prune |
| migrate | Transaction (default) | Schema DDL + version update |

### Migration Safety
- Pre-migration backup created automatically (`.backup-YYYYMMDD-HHMMSS`)
- Schema version read inside transaction to prevent TOCTOU
- `CREATE TABLE IF NOT EXISTS` makes migration idempotent

## Testing

```bash
go test ./...                    # Unit tests (including concurrency + race detection)
go test -race ./...              # Race detector
bash test-integration.sh         # Full CLI integration test (19 tests)
```

## Recovery Procedures

### DB Corruption
```bash
ic health                        # Diagnose
cp .clavain/intercore.db.backup-* .clavain/intercore.db  # Restore latest backup
ic health                        # Verify
```

### Schema Mismatch
```bash
ic version                       # Shows "schema: v<N>"
# If binary is too old: upgrade intercore binary
# If DB is too old: ic init (auto-migrates)
```

### Sentinel Stuck After Crash
```bash
ic sentinel reset <name> <scope_id>  # Clear the sentinel
```
