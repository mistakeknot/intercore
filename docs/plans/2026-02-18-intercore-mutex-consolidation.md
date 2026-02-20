# Intercore Mutex Consolidation (F6) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Bead:** iv-x4dk
**Phase:** executing (as of 2026-02-19T01:15:49Z)

**Goal:** Consolidate scattered `mkdir`-based locks under `/tmp/intercore/locks/` with owner metadata, expose `ic lock list/stale/clean` CLI commands, and provide bash helpers `intercore_lock`/`intercore_unlock` in `lib-intercore.sh`.

**Architecture:** This is a **filesystem-level lock manager**, not a SQLite-based one. The `mkdir` pattern is used for process-level mutual exclusion of bash read-modify-write operations (serialize concurrent bead state updates). SQLite sentinels serve a different purpose (time-based throttle guards). Both coexist.

The lock manager has three layers:
1. **Go package** (`internal/lock/`) — lock/unlock/list/stale/clean operations on `/tmp/intercore/locks/`
2. **CLI commands** (`ic lock acquire/release/list/stale/clean`) — thin CLI wrappers
3. **Bash helpers** (`lib-intercore.sh`) — `intercore_lock`/`intercore_unlock` that replace inline `mkdir` patterns

**Tech Stack:** Go 1.22, bash, POSIX filesystem operations (no SQLite for locks — filesystem atomicity is the point)

**Key Conventions (from existing codebase):**
- 8-char alphanumeric IDs via `crypto/rand`
- Exit codes: 0=success/acquired, 1=contention/not found, 2=error, 3=usage
- `--flag=value` manual arg parsing in CLI
- Integration tests in `test-integration.sh`

---

## What Already Exists

Currently, 5 distinct `mkdir`-based lock patterns exist in `hub/clavain/hooks/lib-sprint.sh`:

| Lock dir pattern | Purpose | Stale timeout |
|---|---|---|
| `/tmp/sprint-lock-${sprint_id}` | Artifact update + phase history serialization | 5s |
| `/tmp/sprint-advance-lock-${sprint_id}` | Phase advance serialization | 5s |
| `/tmp/sprint-claim-lock-${sprint_id}` | Session claim serialization | none (single attempt) |
| `/tmp/checkpoint-lock-<encoded-path>` | Checkpoint write serialization | 1s (10 retries) |

All use the same spin-wait pattern: `while ! mkdir ... ; do sleep 0.1; retries++; done` with stale-lock breaking after 5s and fail-safe on timeout.

## What This Plan Delivers

1. **Structured lock directory:** `/tmp/intercore/locks/<name>/<scope>/` with owner metadata file inside
2. **`ic lock` CLI commands:** acquire, release, list, stale, clean
3. **Bash wrappers:** `intercore_lock`/`intercore_unlock` that replace the inline `mkdir` pattern
4. **Migration of lib-sprint.sh** to use the new helpers (backward-compatible fallback)

---

## Review Fixes Applied

The following findings from the flux-drive review (fd-architecture, fd-correctness, fd-quality, fd-safety) have been incorporated:

1. **P0 (architecture + correctness):** Added `intercore_lock_available()` — a binary-only check that skips DB health. Lock commands don't need the DB, so `intercore_available()` (which runs `ic health`) would silently force fallback on DB issues.
2. **P0 (correctness):** Changed stale-lock breaking to use `os.Remove(ownerFile)` + `os.Remove(lockDir)` — never `os.RemoveAll`. Two processes racing to break the same stale lock could destroy a live lock with `rm -rf`.
3. **P0 (architecture):** Changed lock directory layout from `<name>-<scope>` to `<name>/<scope>` subdirectories. Hyphens in names (e.g., `sprint-claim`) caused ambiguous paths.
4. **P1 (correctness):** Added `--owner` flag to `ic lock release` and owner verification. Without it, any caller could free any lock (lock theft).
5. **P1 (quality):** Added `context.Context` to all Manager methods (codebase convention).
6. **P1 (quality):** Stale detection reads `owner.json` created timestamp instead of dir mtime (dir mtime can be updated by any entry touch).
7. **P1 (safety):** Changed all permissions from 0755 to 0700 (lock state shouldn't be world-readable).
8. **P1 (safety):** Added PID-liveness check (`syscall.Kill(pid, 0)`) in `Clean` — only removes locks whose owning PID is dead.
9. **P1 (quality):** Bash fallback uses 3-way exit code split (0=acquired, 1=contention, 2+=fallthrough to legacy) matching `intercore_sentinel_check_or_legacy` pattern.
10. **P2 (quality):** Added concurrent goroutine race test. Integration test uses relative time offset and cleanup trap.
11. **P2 (quality):** Removed redundant `source lib-intercore.sh` from Task 3 — lib-sprint.sh already sources it at line 12.

---

## Task 0: Create lock package with core operations

**New file:** `internal/lock/lock.go`

```go
package lock

const (
    DefaultBaseDir   = "/tmp/intercore/locks"
    DefaultStaleAge  = 5 * time.Second
    DefaultMaxWait   = time.Second       // 10 retries × 100ms
    DefaultRetryWait = 100 * time.Millisecond
)

type Lock struct {
    Name    string
    Scope   string
    Owner   string    // PID:hostname or session ID
    Created time.Time
}

type Manager struct {
    BaseDir string
}
```

Core functions:
- `NewManager(baseDir string) *Manager` — defaults to `/tmp/intercore/locks`
- `(m *Manager) Acquire(ctx context.Context, name, scope, owner string, maxWait time.Duration) error` — mkdir + write owner metadata, spin-wait with stale breaking
- `(m *Manager) Release(ctx context.Context, name, scope, owner string) error` — verify owner matches, remove owner.json then rmdir
- `(m *Manager) List(ctx context.Context) ([]Lock, error)` — scan lock dirs, read metadata
- `(m *Manager) Stale(ctx context.Context, maxAge time.Duration) ([]Lock, error)` — locks older than maxAge (by owner.json created, not dir mtime)
- `(m *Manager) Clean(ctx context.Context, maxAge time.Duration) (int, error)` — remove stale locks (PID-dead check first), return count

Error sentinels: `ErrTimeout`, `ErrNotOwner`, `ErrNotFound`

Lock directory structure: `<baseDir>/<name>/<scope>/owner.json`
Owner metadata: `{"pid": 12345, "host": "hostname", "session": "abc123", "created": 1708300000}`

The `Acquire` logic:
1. `os.MkdirAll(filepath.Join(m.BaseDir, name), 0700)` — ensure name dir exists
2. `os.Mkdir(lockPath, 0700)` — atomic acquire attempt (lockPath = `<baseDir>/<name>/<scope>`)
3. If mkdir succeeds: write `owner.json` inside, return nil
4. If mkdir fails (EEXIST): read `owner.json` created timestamp for stale check (NOT dir mtime). If created > maxAge ago, break stale lock using `os.Remove(ownerFile)` + `os.Remove(lockDir)` (**never `os.RemoveAll`** — prevents destroying a concurrently re-acquired lock). Then retry mkdir.
5. Spin: sleep RetryWait, retry up to MaxWait/RetryWait times
6. If timeout: return `ErrTimeout`

The `Release` logic:
1. Read `owner.json`, verify owner matches the caller's identity
2. If mismatch: return `ErrNotOwner`
3. `os.Remove(ownerFile)` then `os.Remove(lockDir)`

The `Clean` logic:
1. For each stale lock (created > maxAge): check if PID is alive via `syscall.Kill(pid, 0)`
2. If PID returns `ESRCH` (no such process): remove lock. If PID is alive: skip (process may be stalled, not dead).

**New file:** `internal/lock/lock_test.go`

Unit tests:
- `TestAcquireRelease` — basic lock/unlock cycle
- `TestAcquireContention` — second acquire blocks, first releases, second succeeds
- `TestStaleBreaking` — acquire a lock, backdate owner.json created, second acquire breaks it
- `TestReleaseOwnerVerification` — release with wrong owner returns ErrNotOwner
- `TestList` — acquire 2 locks, list returns both with metadata
- `TestClean` — acquire lock, age it, clean removes it (use current PID for dead-PID simulation)
- `TestConcurrentAcquire` — goroutine race test: N goroutines compete for same lock, exactly 1 holds at a time

**Acceptance:** All unit tests pass. Lock directory created under `/tmp/intercore/locks/`. Owner metadata written and readable.

---

## Task 1: Add `ic lock` CLI commands

**File:** `cmd/ic/main.go`

Add case to main switch:
```go
case "lock":
    exitCode = cmdLock(ctx, subArgs)
```

Add to `printUsage()`:
```
  lock acquire <name> <scope> [--timeout=<dur>] [--owner=<s>]  Acquire a lock
  lock release <name> <scope>                    Release a lock
  lock list                                      List active locks
  lock stale [--older-than=<dur>]                List stale locks
  lock clean [--older-than=<dur>]                Remove stale locks
```

**New file:** `cmd/ic/lock.go`

```go
func cmdLock(ctx context.Context, args []string) int {
    // Parse subcommand: acquire, release, list, stale, clean
    // No DB needed — lock manager uses filesystem only
}
```

Commands:
- `ic lock acquire <name> <scope>` — exit 0=acquired, 1=timeout/contention, 2=error
  - `--timeout=1s` (default 1s)
  - `--owner=<string>` (default: `$PID:$HOSTNAME`)
- `ic lock release <name> <scope> [--owner=<s>]` — exit 0=released, 1=not found/not owner, 2=error
  - `--owner=<string>` (required for owner verification; default: `$PID:$HOSTNAME`)
- `ic lock list` — tab-separated: name, scope, owner, age. Exit 0.
- `ic lock stale` — like list but only stale locks. `--older-than=5s` (default)
- `ic lock clean` — remove stale locks, print count. `--older-than=5s` (default)

**Note:** The lock commands do NOT require a database connection (no `openDB` call). They operate purely on the filesystem. This means `ic lock` works even when the DB is broken or uninitialized.

**Acceptance:** `ic lock acquire test global && ic lock list && ic lock release test global` works end-to-end. Exit codes match convention.

---

## Task 2: Add bash wrappers to lib-intercore.sh

**File:** `infra/intercore/lib-intercore.sh`

Add after the dispatch wrappers section:

```bash
# --- Lock wrappers ---

# intercore_lock_available — Check if ic binary exists (no DB health check).
# Lock commands are filesystem-only — they work even when the DB is broken.
# This avoids the intercore_available() DB health check that would silently
# force all lock operations into the dumber bash fallback.
intercore_lock_available() {
    if [[ -n "$INTERCORE_BIN" ]]; then return 0; fi
    INTERCORE_BIN=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)
    [[ -n "$INTERCORE_BIN" ]]
}

# intercore_lock — Acquire a named lock with spin-wait.
# Args: $1=name, $2=scope, $3=timeout (optional, default "1s")
# Returns: 0 if acquired, 1 if timeout/contention
# Uses 3-way exit code split: 0=acquired, 1=contention, 2+=fallthrough to fallback
intercore_lock() {
    local name="$1" scope="$2" timeout="${3:-1s}"
    local _owner="$$:$(hostname -s 2>/dev/null || echo unknown)"
    if intercore_lock_available; then
        local rc=0
        "$INTERCORE_BIN" lock acquire "$name" "$scope" --timeout="$timeout" \
            --owner="$_owner" >/dev/null || rc=$?
        if [[ $rc -eq 0 ]]; then return 0; fi   # acquired
        if [[ $rc -eq 1 ]]; then return 1; fi   # contention
        # Exit 2+ = binary error — fall through to legacy
    fi
    # Fallback: direct mkdir (legacy pattern, no owner metadata)
    local lock_dir="/tmp/intercore/locks/${name}/${scope}"
    mkdir -p "$(dirname "$lock_dir")" 2>/dev/null || true
    local retries=0 max_retries=10
    while ! mkdir "$lock_dir" 2>/dev/null; do
        retries=$((retries + 1))
        [[ $retries -gt $max_retries ]] && return 1
        sleep 0.1
    done
    return 0
}

# intercore_unlock — Release a named lock.
# Args: $1=name, $2=scope
# Returns: 0 always (fail-safe: never block on unlock failure)
intercore_unlock() {
    local name="$1" scope="$2"
    local _owner="$$:$(hostname -s 2>/dev/null || echo unknown)"
    if intercore_lock_available; then
        "$INTERCORE_BIN" lock release "$name" "$scope" --owner="$_owner" >/dev/null 2>&1 || true
        return 0
    fi
    # Fallback: rm owner.json + rmdir (no owner check in fallback)
    rm -f "/tmp/intercore/locks/${name}/${scope}/owner.json" 2>/dev/null || true
    rmdir "/tmp/intercore/locks/${name}/${scope}" 2>/dev/null || true
    return 0
}

# intercore_lock_clean — Remove stale locks.
# Args: $1=max_age (optional, default "5s")
# Returns: 0 always (fail-safe)
intercore_lock_clean() {
    local max_age="${1:-5s}"
    if intercore_lock_available; then
        "$INTERCORE_BIN" lock clean --older-than="$max_age" >/dev/null 2>&1 || true
        return 0
    fi
    # Fallback: find + rm stale lock dirs (5 second threshold via -mmin)
    find /tmp/intercore/locks -mindepth 2 -maxdepth 2 -type d -not -newermt '5 seconds ago' \
        -exec sh -c 'rm -f "$1/owner.json" && rmdir "$1"' _ {} \; 2>/dev/null || true
    return 0
}
```

Bump `INTERCORE_WRAPPER_VERSION` to `"0.4.0"`.

**Acceptance:** `source lib-intercore.sh && intercore_lock test global && intercore_unlock test global` works. Fallback path works when `ic` is not available.

---

## Task 3: Migrate lib-sprint.sh to use intercore_lock/intercore_unlock

**File:** `hub/clavain/hooks/lib-sprint.sh`

Replace all 5 inline `mkdir` lock patterns with calls to `intercore_lock`/`intercore_unlock`. The migration preserves fail-safe behavior: if `intercore_lock` returns 1, the function returns 0 (or 1 for sprint_claim/sprint_advance, matching current behavior).

**Migration table:**

| Function | Old lock dir | New name/scope | Fail behavior |
|---|---|---|---|
| `sprint_set_artifact` | `/tmp/sprint-lock-${sprint_id}` | `sprint`/`${sprint_id}` | return 0 (fail-safe) |
| `sprint_record_phase_completion` | `/tmp/sprint-lock-${sprint_id}` | `sprint`/`${sprint_id}` | return 0 (fail-safe) |
| `sprint_claim` | `/tmp/sprint-claim-lock-${sprint_id}` | `sprint-claim`/`${sprint_id}` | return 1 (conflict) |
| `sprint_advance` | `/tmp/sprint-advance-lock-${sprint_id}` | `sprint-advance`/`${sprint_id}` | return 1 (conflict) |
| `checkpoint_write` | `/tmp/checkpoint-lock-<encoded>` | `checkpoint`/`<encoded>` | return 0 (fail-safe) |

For each function:
1. Replace the `local lock_dir=...` and `while ! mkdir ...` block with `intercore_lock <name> <scope> || return <fail_code>`
2. Add `intercore_unlock <name> <scope>` before every return path (and in a trap for safety)
3. Remove the stale-lock-breaking inline code (handled by the Go lock manager)

**Example transformation for `sprint_set_artifact`:**

Before:
```bash
local lock_dir="/tmp/sprint-lock-${sprint_id}"
local retries=0
while ! mkdir "$lock_dir" 2>/dev/null; do
    retries=$((retries + 1))
    [[ $retries -gt 10 ]] && {
        local lock_mtime now
        lock_mtime=$(stat -c %Y "$lock_dir" 2>/dev/null || ...)
        now=$(date +%s)
        if [[ $((now - lock_mtime)) -gt 5 ]]; then
            rmdir "$lock_dir" 2>/dev/null || ...
            mkdir "$lock_dir" 2>/dev/null || return 0
            break
        fi
        return 0
    }
    sleep 0.1
done
# ... critical section ...
rmdir "$lock_dir" 2>/dev/null || true
```

After:
```bash
intercore_lock "sprint" "$sprint_id" "1s" || return 0
# ... critical section ...
intercore_unlock "sprint" "$sprint_id"
```

**Prerequisite:** lib-sprint.sh already sources lib-intercore.sh at line 12 — do NOT add a second source (it would reset `INTERCORE_BIN` since there's no double-source guard). The existing source line is sufficient.

**Acceptance:** All sprint functions work identically. `ls /tmp/intercore/locks/` shows organized lock dirs instead of scattered `/tmp/sprint-*` patterns. No remaining `mkdir` lock patterns in lib-sprint.sh.

---

## Task 4: Integration tests

**File:** `infra/intercore/test-integration.sh`

Add a new test section `# --- Lock tests ---`:

```bash
# Cleanup trap for lock test dir
_lock_cleanup() { rm -rf /tmp/intercore/locks/test-* 2>/dev/null || true; }
trap _lock_cleanup EXIT

# Lock acquire/release basic cycle
ic lock acquire testlock global --owner="test:host"
assert_exit 0 "lock acquire"
ic lock list | grep -q "testlock"
assert_exit 0 "lock list shows acquired lock"
ic lock release testlock global --owner="test:host"
assert_exit 0 "lock release"

# Owner verification — wrong owner cannot release
ic lock acquire ownertest scope1 --owner="alice:host"
ic lock release ownertest scope1 --owner="bob:host"
assert_exit 1 "lock release with wrong owner fails"
ic lock release ownertest scope1 --owner="alice:host"
assert_exit 0 "lock release with correct owner succeeds"

# Lock contention (acquire twice without release)
ic lock acquire contention scope1 --owner="test:host"
ic lock acquire contention scope1 --timeout=200ms --owner="test2:host"
assert_exit 1 "lock acquire times out on contention"
ic lock release contention scope1 --owner="test:host"

# Stale lock detection (use relative offset via owner.json backdating)
ic lock acquire staletest global --owner="test:host"
# Backdate the owner.json created field to 10 seconds ago
local stale_dir="/tmp/intercore/locks/staletest/global"
local backdate=$(($(date +%s) - 10))
echo "{\"pid\":99999,\"host\":\"test\",\"session\":\"test\",\"created\":${backdate}}" > "$stale_dir/owner.json"
ic lock stale --older-than=1s | grep -q "staletest"
assert_exit 0 "stale lists old lock"

# Clean removes stale locks (PID 99999 should not exist)
ic lock clean --older-than=1s
assert_exit 0 "lock clean succeeds"
ic lock list | grep -q "staletest"
assert_exit 1 "stale lock removed by clean"
```

**Acceptance:** `bash test-integration.sh` passes with the new lock tests.

---

## Task 5: Update AGENTS.md and CLAUDE.md

**File:** `infra/intercore/AGENTS.md`

Add `internal/lock/` to the architecture diagram. Add `ic lock` commands to the CLI commands section.

**File:** `infra/intercore/CLAUDE.md`

Add lock quick reference:
```bash
# Lock management (filesystem-based, no DB required)
ic lock acquire <name> <scope> [--timeout=1s]  # Acquire mutex
ic lock release <name> <scope>                  # Release mutex
ic lock list                                    # Show active locks
ic lock stale [--older-than=5s]                 # Show stale locks
ic lock clean [--older-than=5s]                 # Remove stale locks
```

**File:** `infra/intercore/lib-intercore.sh` header comment — already updated in Task 2.

**Acceptance:** Documentation reflects the new lock subsystem.
