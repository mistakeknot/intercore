# Intercore Code Quality Review

Date: 2026-02-17
Reviewer: Flux-drive Quality & Style Reviewer
Target: intercore v0.1.0 (Go 1.22, SQLite WAL, ~2340 lines)

## Executive Summary

Intercore is a well-structured Go CLI with strong adherence to Go idioms, solid error handling, and appropriate use of table-driven tests. The code demonstrates infrastructure-grade discipline with WAL safety, path validation, and atomic operations. A few minor issues were identified around unused variables, error propagation opportunities, and shell library patterns.

**Overall Assessment: Production-Ready with Minor Improvements Recommended**

---

## Universal Review

### File Organization
**PASS** - Clear layering:
- `cmd/ic/main.go` - CLI entry point, argument parsing, command dispatch
- `internal/db/` - Database layer (connection, migration, health)
- `internal/state/` - State store operations
- `internal/sentinel/` - Sentinel/throttle operations
- `lib-intercore.sh` - Bash wrapper library for hooks

No ad-hoc structure or misplaced responsibilities.

### Naming Consistency
**PASS** - Strong adherence to Go conventions:
- Package names: `db`, `state`, `sentinel` (short, lowercase, no underscores)
- Exported types: `DB`, `Store`, `Sentinel` (UpperCamelCase)
- Sentinel errors: `ErrNotFound`, `ErrSchemaVersionTooNew`, `ErrNotMigrated`
- Unexported: `schemaDDL`, `maxPayloadSize`, `flagDB` (lowerCamelCase)
- Constants grouped with `const` blocks
- Package-level vars declared with `var` blocks

**5-Second Naming Rule Compliance:**
- `Open(path string, busyTimeout time.Duration) (*DB, error)` - clear
- `ValidatePayload(data []byte) error` - clear
- `Check(ctx context.Context, name, scopeID string, intervalSec int) (bool, error)` - clear

### Error Handling Patterns
**PASS with MINOR IMPROVEMENTS**

**Strong points:**
- Consistent `fmt.Errorf("operation: %w", err)` wrapping with context
- Sentinel errors properly defined and exported
- Error checks immediately after operations
- No discarded errors in production paths

**Minor issues:**
1. **db/db.go:74** - Auto-prune error logged to stderr but not wrapped in chain:
   ```go
   if _, err := tx.ExecContext(ctx, "DELETE FROM sentinels WHERE unixepoch() - last_fired > 604800"); err != nil {
       fmt.Fprintf(os.Stderr, "ic: auto-prune: %v\n", err)
   }
   ```
   This is acceptable for non-critical cleanup, but consider wrapping with context for debugging.

2. **main.go:621-623** - `cmdCompatStatus` ignores errors from `filepath.Glob` and `store.List`:
   ```go
   matches, _ := filepath.Glob(pattern)
   ids, _ := store.List(ctx, key)
   ```
   **RECOMMENDATION:** Log or wrap these errors for debugging, even in fallback mode.

### API Design Consistency
**PASS** - Clean, predictable interfaces:
- All store operations accept `context.Context` as first param
- All store constructors follow `New(db *sql.DB) *Store`
- All methods return `(result, error)` tuple
- Boolean operations return `(bool, error)` not `error` with nil check
- All CLI commands return `int` exit codes (0=success, 1=negative, 2=error, 3=usage)

### Complexity Budget
**PASS** - No premature abstractions:
- Single-file packages for `state` and `sentinel` (appropriate for 200 lines each)
- No interface bloat - `*sql.DB` passed directly
- Custom argument parser justified (Go's `flag` stops at first non-flag arg)
- JSON validation function is complex but necessary for payload limits

### Dependency Discipline
**PASS**
- Standard library only (encoding/json, database/sql, time, context, path/filepath, strings, io)
- Single external dependency: `modernc.org/sqlite` (required for pure-Go SQLite)
- No test-only dependencies (uses `sql` directly with temp DBs)

### Test Strategy
**PASS** - Strong table-driven and concurrent testing:
- Table-driven tests in `sentinel_test.go:40-76` and `state_test.go:250-263`
- Concurrent race test in `sentinel_test.go:147-175` (atomic.Int32 for correctness)
- WAL safety verified via `db_test.go:99-145` (concurrent migration test)
- Cleanup with `t.Cleanup()` for temp databases

---

## Go-Specific Review

### Error Handling

**PASS** - Idiomatic error wrapping:
- All errors wrapped with `%w` for chain-preserving propagation
- Context preserved in all wraps: `fmt.Errorf("state get: %w", err)`
- Sentinel errors defined at package level and checked with `errors.Is()`

**Example from state/state.go:72-75:**
```go
if errors.Is(err, sql.ErrNoRows) {
    return nil, ErrNotFound
}
return nil, fmt.Errorf("state get: %w", err)
```

**ISSUE: db/db.go:189 - Missing checkDiskSpace function:**
Referenced in `db.go:164` but not present in `db.go`. Found in `disk.go:8-18`. This is correct modular design, but the function is unexported and platform-specific (uses `syscall.Statfs_t`, Linux-only).

**RECOMMENDATION:** Add build tags or fallback for non-Linux platforms:
```go
// +build linux

package db
```

### Naming (5-Second Rule)

**PASS** - All public APIs are self-documenting:
- `db.Open(path, timeout)` - clear
- `store.Set(ctx, key, scopeID, payload, ttl)` - clear
- `sentinel.Check(ctx, name, scopeID, intervalSec)` - returns `(allowed bool, error)` - clear

**MINOR: Unexported vars with "flag" prefix are unconventional:**
```go
var (
    flagDB      string
    flagTimeout time.Duration
    flagVerbose bool
    flagJSON    bool
)
```
These are package-level globals for CLI flags. The `flag` prefix is clear but unconventional (usually `_` suffix or no prefix). This is acceptable for a main package.

### File Organization

**ISSUE: main.go is 697 lines:**
Go community convention suggests splitting at 300-500 lines. Possible splits:
- `cmd/ic/main.go` - main, arg parsing, resolveDBPath, openDB
- `cmd/ic/commands.go` - all `cmd*` functions
- `cmd/ic/compat.go` - compat subcommand and legacyPatterns map

**RECOMMENDATION:** Split `main.go` into 3 files when adding new commands.

### "Accept Interfaces, Return Structs"

**PASS** - All constructors return concrete types:
- `db.Open(...) (*DB, error)`
- `state.New(*sql.DB) *Store`
- `sentinel.New(*sql.DB) *Store`

Store packages accept `*sql.DB` directly (no interface bloat). This is appropriate for infrastructure code where mocking is not a primary concern.

### Import Grouping

**PASS** - All files follow stdlib/external/internal grouping:
```go
import (
    "context"
    "database/sql"
    "encoding/json"
    // blank line
    _ "modernc.org/sqlite"
    // blank line
    "github.com/mistakeknot/interverse/infra/intercore/internal/db"
)
```

### Testing Approach

**PASS** - Excellent test coverage and patterns:

1. **Table-driven tests** where appropriate:
   - `sentinel_test.go:40-76` - TestSentinelCheck with 2 cases
   - `state_test.go:250-263` - TestValidatePayload_Valid with 6 cases

2. **Race detector compatibility:**
   - `sentinel_test.go:147-175` uses `sync/atomic.Int32` for concurrent test
   - All tests compatible with `go test -race`

3. **Transactional safety verified:**
   - `sentinel_test.go:147-175` - 10 concurrent goroutines, exactly 1 allowed
   - `db_test.go:99-145` - 5 sequential migrations, version is 1

4. **Edge cases covered:**
   - TTL truncation test (state_test.go:101-124)
   - Symlink rejection test (db_test.go:42-56)
   - Schema version too new (db_test.go:204-224)

**MINOR: db_test.go:99 comment is misleading:**
```go
// Test that sequential migration from different connections is safe.
// Real-world scenario: two `ic init` commands run back-to-back.
// With SetMaxOpenConns(1), each connection serializes, so concurrent
// open is the bottleneck (not the migration itself).
```
This test runs sequentially (for i := 0; i < n; i++), not concurrently. The comment suggests concurrency but the implementation is serial.

**RECOMMENDATION:** Either:
1. Rename to `TestMigrate_Sequential`
2. Actually run concurrent migrations with goroutines + WaitGroup

---

## Shell-Specific Review

### File: lib-intercore.sh

**STRICT MODE: PASS (with justification)**
Line 2-3 explicitly documents why `set -e` is NOT used:
```bash
# This file is SOURCED by hooks. Do NOT use set -e here — it would exit
# the parent shell on any failure.
```
This is correct. Sourced libraries must not use `set -e` or they will exit the parent shell.

**QUOTING: PASS**
All variable expansions are properly quoted:
- Line 14: `INTERCORE_BIN=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)`
- Line 30: `printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id"`
- Line 42: `"$INTERCORE_BIN" sentinel check "$name" "$scope_id" --interval="$interval"`

**INJECTION SAFETY: PASS**
No `eval`, no unsafe command construction, all inputs are passed as arguments (not expanded).

**RETURN VALUE PATTERNS: ISSUE**
All wrapper functions return 0 on failure:
```bash
intercore_state_set() {
    local key="$1" scope_id="$2" json="$3"
    if ! intercore_available; then return 0; fi
    printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id" || return 0
}
```

**ISSUE:** This silently succeeds when intercore is unavailable or when `state set` fails. The `|| return 0` on line 30 means failures are hidden.

**RECOMMENDATION:** Document the fail-safe behavior in comments:
```bash
intercore_state_set() {
    # Fail-safe: returns 0 if intercore unavailable or write fails.
    # Hooks should not block on DB failures.
    local key="$1" scope_id="$2" json="$3"
    if ! intercore_available; then return 0; fi
    printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id" || return 0
}
```

**PORTABILITY: PASS**
- Shebang is missing (correct for sourced library)
- `shellcheck shell=bash` directive present (line 5)
- Uses `[[ ]]` (Bash-specific) not `[ ]` (POSIX) - acceptable per directive

**CLEANUP/TRAPS: N/A**
This is a library, not a script. No temp files created, no cleanup needed.

---

## Specific Findings

### 1. Unused Variables (Dead Code)

**ISSUE: cmd/ic/main.go:24-25 - flagVerbose and flagJSON declared but never read:**
```go
var (
    flagDB      string
    flagTimeout time.Duration
    flagVerbose bool // NEVER READ
    flagJSON    bool // NEVER READ
)
```

**IMPACT:** Misleading to users (appears to be a feature but is not implemented).

**RECOMMENDATION:**
1. Remove the flags and related parsing code (lines 50-53) until implementation is ready.
2. OR: Implement verbose logging and JSON output for at least one command (e.g., `health`, `version`).

**FILES:**
- `cmd/ic/main.go:24-25` (declaration)
- `cmd/ic/main.go:50-53` (parsing)

---

### 2. JSON Validation Function Structure

**FILE: internal/state/state.go:139-195 - validateDepth**

**ASSESSMENT: PASS (complex but necessary)**

The function has multiple responsibilities:
1. Check nesting depth (max 20 levels)
2. Check string length (max 100KB)
3. Check array length (max 10,000 elements)

**OBSERVATION:** Lines 176-178 are dead code:
```go
if len(v) > maxKeyLength {
    // Could be a key or a value — we limit both for simplicity
}
```
This check has no effect (no return or error). The comment suggests it's intentional but the code does nothing.

**RECOMMENDATION:**
1. Remove lines 176-178 (dead code).
2. OR: Return an error if `len(v) > maxKeyLength` and add a test case for it.

**MINOR: inArray flag is set but never unset correctly:**
Line 169 sets `inArray = false` when `]` is encountered, but what if an array is nested inside an object? The logic assumes a flat structure.

**EXAMPLE:**
```json
{
  "data": [1, 2, 3],
  "other": "value"
}
```
When `]` is hit, `inArray` is set to false. But if there's a nested array:
```json
{
  "arrays": [[1, 2], [3, 4]],
  "value": 1
}
```
The inner `]` would set `inArray = false` prematurely.

**RECOMMENDATION:** Use a stack to track array nesting depth, not a boolean flag.

---

### 3. Error Propagation Opportunities

**FILE: cmd/ic/main.go:621-659 - cmdCompatStatus and cmdCompatCheck**

**OBSERVATION:** Errors from `filepath.Glob` and `store.List` are ignored:
```go
matches, _ := filepath.Glob(pattern)
ids, _ := store.List(ctx, key)
```

**JUSTIFICATION:** This is a compatibility/diagnostic command. Failures are not critical.

**RECOMMENDATION:** Log errors to stderr for debugging:
```go
matches, err := filepath.Glob(pattern)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: compat: glob %s: %v\n", pattern, err)
}
```

---

### 4. Bash Library Fail-Safe Behavior

**FILE: lib-intercore.sh**

**OBSERVATION:** All wrappers return 0 on failure (lines 30, 42):
```bash
intercore_state_set() {
    # ...
    printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id" || return 0
}
```

**JUSTIFICATION:** Bash hooks should not block on DB failures (defensive design).

**ISSUE:** This behavior is not documented. Users calling these functions may not realize failures are hidden.

**RECOMMENDATION:** Add function-level comments:
```bash
# intercore_state_set writes a state entry to the DB.
# Fail-safe: returns 0 if intercore unavailable or write fails.
# Hooks should not block on DB failures.
intercore_state_set() {
    local key="$1" scope_id="$2" json="$3"
    if ! intercore_available; then return 0; fi
    printf '%s\n' "$json" | "$INTERCORE_BIN" state set "$key" "$scope_id" || return 0
}
```

---

### 5. Test Coverage Gaps

**MISSING: Integration test for auto-prune in sentinel.Check**

The auto-prune logic in `sentinel/sentinel.go:72-75` is only covered by indirect tests. No explicit test verifies that stale sentinels are deleted during a `Check` call.

**RECOMMENDATION:** Add a test:
```go
func TestSentinelCheck_AutoPrune(t *testing.T) {
    db := setupTestDB(t)
    store := New(db)
    ctx := context.Background()

    // Insert a stale sentinel (> 7 days old)
    _, err := db.Exec("INSERT INTO sentinels (name, scope_id, last_fired) VALUES (?, ?, ?)",
        "stale", "s1", time.Now().Unix()-604801)
    if err != nil {
        t.Fatal(err)
    }

    // Trigger auto-prune via Check
    store.Check(ctx, "fresh", "s2", 0)

    // Verify stale sentinel was deleted
    var count int
    db.QueryRow("SELECT COUNT(*) FROM sentinels WHERE name = 'stale'").Scan(&count)
    if count != 0 {
        t.Errorf("auto-prune failed: stale sentinel still exists")
    }
}
```

---

### 6. Platform-Specific Code Without Build Tags

**FILE: internal/db/disk.go**

**OBSERVATION:** Uses `syscall.Statfs_t`, which is Linux-specific:
```go
func checkDiskSpace(dir string, minBytes uint64) error {
    var stat syscall.Statfs_t
    if err := syscall.Statfs(dir, &stat); err != nil {
        return fmt.Errorf("cannot check disk space: %w", err)
    }
    // ...
}
```

**IMPACT:** Build will fail on Windows and macOS without fallback.

**RECOMMENDATION:** Add build tags:
```go
// +build linux

package db

import (
    "fmt"
    "syscall"
)

func checkDiskSpace(dir string, minBytes uint64) error {
    var stat syscall.Statfs_t
    // ...
}
```

And a stub for other platforms:
```go
// +build !linux

package db

import "fmt"

func checkDiskSpace(dir string, minBytes uint64) error {
    // Disk space check not implemented on this platform
    return nil
}
```

---

## What Was NOT Flagged (Correctly)

### Pure Style Preferences
- Variable naming (`flagDB` vs `dbFlag`) - project choice is clear
- Line breaks in SQL queries (some multi-line, some single-line) - both are readable

### Missing Patterns Not Used by Project
- No logging framework (uses `fmt.Fprintf(os.Stderr, ...)` directly) - appropriate for CLI
- No context cancellation checks in store methods - acceptable for fast DB ops
- No metrics/instrumentation - not needed for v0.1.0

### Tooling Recommendations
- No linter config files (`.golangci.yml`, etc.) - project may use defaults
- No pre-commit hooks - not a code quality issue

---

## Recommendations Summary

### CRITICAL (Fix Before v1.0)
1. **Remove or implement `--verbose` and `--json` flags** (main.go:24-25, 50-53)
2. **Add build tags for platform-specific disk.go** (Linux-only syscall.Statfs_t)

### IMPORTANT (Fix in Next Release)
3. **Document fail-safe behavior in lib-intercore.sh** (all wrapper functions)
4. **Fix inArray nesting bug in validateDepth** (state.go:139-195) - use stack not boolean
5. **Remove or implement dead code** (state.go:176-178 - maxKeyLength check)

### NICE-TO-HAVE (Consider for Future)
6. **Split main.go into smaller files** (697 lines → 3 files)
7. **Log ignored errors in compat commands** (main.go:621-659)
8. **Add auto-prune integration test** (sentinel_test.go)
9. **Rename or fix TestMigrate_Concurrent** (runs sequentially, not concurrently)

---

## Conclusion

Intercore demonstrates strong Go idioms, infrastructure-grade error handling, and solid testing practices. The code is production-ready with a few minor cleanup items. The custom argument parser is well-justified, the WAL safety is correctly implemented, and the test suite covers critical concurrency and transactional correctness.

**Key Strengths:**
- Idiomatic error wrapping with `%w`
- Strong table-driven and concurrent testing
- Correct WAL safety (SetMaxOpenConns(1), explicit PRAGMAs)
- No dependency bloat
- Clean API design (context-first, consistent return tuples)

**Key Areas for Improvement:**
- Remove or implement unused CLI flags (--verbose, --json)
- Add platform-specific build tags for disk.go
- Document fail-safe shell library behavior
- Fix inArray nesting bug in JSON validation

**Overall Grade: A- (Production-Ready with Minor Fixes Recommended)**
