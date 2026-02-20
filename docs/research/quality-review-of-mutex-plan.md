# Quality Review: Intercore Mutex Consolidation (F6) Implementation Plan

**Reviewed:** `docs/plans/2026-02-18-intercore-mutex-consolidation.md`
**Date:** 2026-02-18
**Reviewer:** Flux-drive Quality & Style Reviewer

---

## Summary Verdict

The plan is architecturally sound and the overall design is appropriate. The three-layer approach (Go package, CLI wrappers, bash helpers) mirrors the existing sentinel and state modules well. However, there are a set of concrete issues across all four focus areas — Go naming, Bash style, test adequacy, and error handling — that should be addressed before implementation begins. None are blockers for the design, but several will cause friction or correctness problems at implementation time.

---

## 1. Go Naming Conventions and Idioms (`internal/lock/`)

### 1a. Package name collides with standard library

`package lock` clashes with any future intent to use `sync` sub-packages and is easily confused with the common conceptual term. More practically, if `internal/lock` is ever imported alongside `sync.Mutex` in the same file, the package-level name `lock` shadows common variable names.

More importantly, the existing codebase establishes a clear naming convention: packages are named after their domain noun in singular form — `sentinel`, `state`, `dispatch`, `phase`, `runtrack`. The plan's chosen name `lock` is slightly too generic; `lockfile` or `fslock` would be more specific and avoid confusion with in-memory locking primitives. The plan mentions "filesystem-level lock manager" so `fslock` aligns with that framing.

**Recommendation:** Rename to `internal/fslock/` with `package fslock`. All references in `cmd/ic/lock.go` update accordingly.

### 1b. `Manager` type is a weak abstraction — prefer `Store` to match codebase pattern

Every other internal package uses a `Store` type as the primary abstraction (`sentinel.Store`, `state.Store`, `phase.Store`, `runtrack.Store`) constructed by a `New()` function. Introducing `Manager` for a structurally equivalent type breaks the vocabulary of the codebase.

**Recommendation:** Rename `Manager` to `Store`, keeping `New(baseDir string) *Store`. If the `Manager` name feels intentional (to distinguish "no DB" from "DB-backed"), add a comment to `lock.go` explaining the deviation.

### 1c. `Lock` type name shadows the package name

With `package lock`, declaring `type Lock struct` means that inside the package, `Lock` refers to the struct, and the package itself is unavailable unqualified. With the `fslock` rename this is resolved, but it is worth noting here because it is a second reason to prefer the rename.

### 1d. `ErrTimeout` should live in an `errors.go` file

The existing pattern — seen in `internal/phase/errors.go` and `internal/runtrack/errors.go` — is to declare error sentinels in a separate `errors.go` file:

```go
// errors.go
package fslock

import "errors"

var (
    ErrTimeout    = errors.New("lock acquire timed out")
    ErrNotOwner   = errors.New("lock not held by caller")
    ErrNotFound   = errors.New("lock not found")
)
```

The plan embeds these inline in `lock.go`. For consistency with the rest of the codebase and for searchability, they should move to `errors.go`.

### 1e. `Acquire` owner-verification gap — missing `ErrNotOwner`

The plan describes `Release` as "verify owner matches, rmdir" but only names `ErrTimeout` as an error sentinel. Release-with-wrong-owner is a distinct failure mode from "lock not found" and callers (particularly the CLI) need to distinguish them to set correct exit codes (exit 1 vs exit 2). The plan should explicitly name `ErrNotOwner` and map it to exit code 2 (unexpected error) in the CLI table.

### 1f. `owner.json` metadata field `pid` type ambiguity

The metadata sketch uses `{"pid": 12345, "host": "hostname", "session": "abc123", "created": 1708300000}`. In Go, the owner struct should use `int` for PID and `int64` for the Unix timestamp. The plan does not show a Go struct for the metadata — it should be made explicit, since `json.Unmarshal` into an untyped `interface{}` is fragile and inconsistent with the codebase's typed-struct approach.

**Recommendation:** Add to `lock.go`:

```go
type ownerMeta struct {
    PID     int    `json:"pid"`
    Host    string `json:"host"`
    Session string `json:"session"`
    Created int64  `json:"created"`
}
```

### 1g. Stale-lock detection uses directory mtime — document the POSIX hazard

The plan states: "check stale (stat mtime > maxAge), break if stale". On Linux, `os.Mkdir` sets the mtime of the new directory to the current time, which is correct. However, `os.WriteFile` (writing `owner.json` inside the lock dir) also updates the parent directory's mtime on some filesystems. If stale detection relies exclusively on directory mtime, a stale-but-recently-entered lock dir could be incorrectly classified as fresh. The safe approach is to use the `created` field from `owner.json` for staleness, only falling back to directory mtime if the metadata file is unreadable.

This is a correctness issue, not a style preference. The plan's "check stale (stat mtime > maxAge)" wording implies it uses directory mtime only. This should be revised.

### 1h. Spin loop should use `context.Context` for cancellation

`Acquire` accepts `maxWait time.Duration` but the function signature also receives a `ctx context.Context`. The spin loop (`sleep RetryWait, retry up to MaxWait/RetryWait times`) should select between the retry ticker and `ctx.Done()`. Without this, a cancelled context does not interrupt a spinning acquire, which can cause test hangs and is inconsistent with the Go convention that functions accepting `ctx` honour cancellation.

```go
func (s *Store) Acquire(ctx context.Context, name, scope, owner string, maxWait time.Duration) error {
    deadline := time.Now().Add(maxWait)
    for {
        if err := s.tryAcquire(name, scope, owner); err == nil {
            return nil
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(DefaultRetryWait):
        }
        if time.Now().After(deadline) {
            return ErrTimeout
        }
    }
}
```

The plan does not show a `ctx` parameter on `Acquire` at all, which is a gap given the codebase consistently passes `ctx` to all operations.

### 1i. `DefaultMaxWait` naming is misleading

The constant is described as `time.Second // 10 retries × 100ms` which means 1 second is the max wait. Naming it `DefaultMaxWait` is correct, but the comment "10 retries × 100ms" embeds an implementation assumption into the constant declaration. If `DefaultRetryWait` changes, the comment is wrong. Remove the derived comment or restructure as:

```go
DefaultRetryCount = 10
DefaultMaxWait    = DefaultRetryCount * DefaultRetryWait  // 1s
```

---

## 2. Bash Coding Style Consistency with `lib-intercore.sh`

### 2a. `intercore_available` is bypassed in the lock fallback path — but shouldn't be

Looking at `lib-intercore.sh`, every wrapper follows the same guard pattern:

```bash
intercore_sentinel_check() {
    if ! intercore_available; then return 0; fi
    ...
}
```

The proposed `intercore_lock` follows this for the primary path, but the fallback `mkdir` block is reachable even when `ic` is available but returns exit 2 (DB error). The sentinel wrappers handle this three-way distinction explicitly — see `intercore_sentinel_check_or_legacy`:

```bash
if [[ $rc -eq 0 ]]; then
    return 0  # allowed
elif [[ $rc -eq 1 ]]; then
    return 1  # throttled
fi
# Exit code 2+ = DB error — fall through to legacy path
```

The proposed `intercore_lock` does not handle exit code 2 separately — it uses `return $?` directly, which means a DB error on the lock path (exit 2) will be surfaced as a success-or-failure without falling through to the `mkdir` fallback. The plan states the lock commands "do NOT require a database connection" but `intercore_available` calls `ic health` which does require DB. If the DB is broken, `intercore_available` returns 1 and the fallback fires — but if `ic` is found and health passes yet `ic lock acquire` fails for another reason, the exit code leaks through as an unexpected value.

**Recommendation:** Mirror the sentinel wrapper pattern explicitly:

```bash
intercore_lock() {
    local name="$1" scope="$2" timeout="${3:-1s}"
    if intercore_available; then
        local rc=0
        "$INTERCORE_BIN" lock acquire "$name" "$scope" \
            --timeout="$timeout" \
            --owner="$$:$(hostname -s 2>/dev/null || echo unknown)" >/dev/null || rc=$?
        if [[ $rc -eq 0 ]]; then return 0; fi   # acquired
        if [[ $rc -eq 1 ]]; then return 1; fi   # contention
        # rc=2+ (error) — fall through to mkdir fallback
    fi
    ...
}
```

### 2b. Fallback `mkdir -p "$(dirname "$lock_dir")"` is redundant and misleading

The fallback mkdir sequence is:

```bash
local lock_dir="/tmp/intercore/locks/${name}-${scope}"
mkdir -p "$(dirname "$lock_dir")" 2>/dev/null || true
mkdir "$lock_dir" 2>/dev/null
```

`dirname "/tmp/intercore/locks/sprint-abc"` is `/tmp/intercore/locks`, which is the base dir. This is fine, but existing `lib-intercore.sh` code does not use `dirname` for temp dirs — it uses a direct path. This creates a minor quoting hazard: if `name` or `scope` contains spaces (unlikely but possible), the dirname expansion is unquoted. The fallback should be:

```bash
local base_dir="/tmp/intercore/locks"
mkdir -p "$base_dir" 2>/dev/null || true
mkdir "${base_dir}/${name}-${scope}" 2>/dev/null
```

### 2c. Fallback stale-lock breaking is absent

The existing `lib-sprint.sh` inline pattern includes stale-lock breaking in the fallback:

```bash
if [[ $((now - lock_mtime)) -gt 5 ]]; then
    rmdir "$lock_dir" 2>/dev/null || ...
    mkdir "$lock_dir" 2>/dev/null || return 0
```

The proposed `intercore_lock` fallback spins up to `max_retries=10` times (total 1 second) but does not break stale locks. When `ic` is unavailable and a stale lock exists from a crashed process, the fallback will spin for 1 second and then fail. This degrades the safety property: the Go manager handles stale-breaking but the fallback does not.

This is a deliberate trade-off or an oversight — the plan should state which. If intentional, add a comment; if an oversight, add stale-breaking to the fallback using `touch -t` or mtime comparison (matching the existing lib-sprint.sh pattern).

### 2d. `intercore_lock_clean` uses `find` with `-mmin` instead of seconds-based duration

The fallback in `intercore_lock_clean` is:

```bash
find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d -mmin +1 -exec rmdir {} \; 2>/dev/null || true
```

`-mmin +1` is hardcoded regardless of the `$max_age` parameter passed in. If a caller passes `--older-than=5s` at the CLI level, the bash fallback ignores it and uses 1 minute. This inconsistency between the IC path and the fallback path is a silent correctness difference.

Additionally, `-mmin` uses minutes as the unit, while `max_age` is a duration string like `"5s"`. Parsing a duration string in bash to convert to `find -mmin` units requires arithmetic. The simplest consistent fix is to accept that the fallback always uses a conservative default and document this explicitly:

```bash
# Fallback: conservative 5-minute threshold (cannot parse duration strings in bash)
find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d -mmin +5 \
    -exec rmdir {} \; 2>/dev/null || true
```

### 2e. `intercore_unlock` suppresses stderr with `|| true` — inconsistent with codebase

Existing wrappers suppress stdout on operations that produce no useful output but preserve stderr for diagnostics (e.g., `intercore_dispatch_kill` uses `>/dev/null 2>&1 || true`). The plan's `intercore_unlock` also redirects stderr with `2>&1 || true`. This is consistent with the dispatch kill wrapper, so this is acceptable if unlock errors are truly unactionable. The plan should note this design decision explicitly — the comment "fail-safe: never block on unlock failure" is present, which is good.

### 2f. Version bump target is inconsistent with file header

The file header of `lib-intercore.sh` says:

```bash
# Version: 0.1.0 (source: infra/intercore/lib-intercore.sh)
```

The actual variable is:

```bash
INTERCORE_WRAPPER_VERSION="0.3.0"
```

The plan says "Bump `INTERCORE_WRAPPER_VERSION` to `"0.4.0"`" — correct. But the file header comment at line 2 ("Version: 0.1.0") would also need updating. This is a minor drift already present in the file; the implementation task should include fixing both.

### 2g. `lib-sprint.sh` prerequisite sourcing uses `BASH_SOURCE[0]` pattern inconsistently

The plan proposes adding:

```bash
_sprint_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${_sprint_dir}/lib-intercore.sh" 2>/dev/null || true
```

But `lib-sprint.sh` already sources `lib-intercore.sh` on line 12:

```bash
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

The plan adds a redundant source. The implementation should reuse the existing source, which uses the `%/*` suffix trim pattern rather than cd+dirname. Adding a second source of the same file would double-execute the initialization code. The double-sourcing guard (`_SPRINT_LOADED=1`) applies to `lib-sprint.sh` itself, not to `lib-intercore.sh`. `lib-intercore.sh` has no double-source guard, so this would re-execute the `intercore_available` probe and reset `INTERCORE_BIN`.

**Recommendation:** Remove the proposed prerequisite block from the migration plan — the source is already in place.

---

## 3. Test Approach Adequacy

### 3a. Unit tests are well-scoped but missing a concurrency test

The proposed unit tests (`TestAcquireRelease`, `TestAcquireContention`, `TestStaleBreaking`, `TestList`, `TestClean`) cover the primary paths. However, the sentinel package includes `TestSentinelCheck_Concurrent` with `sync.WaitGroup` and `atomic.Int32` to verify exactly-once semantics under 10 concurrent goroutines. The lock package's `Acquire` is specifically designed for mutual exclusion — a concurrent test is the most important test it could have, and the plan omits it.

**Recommendation:** Add `TestAcquireConcurrent` — spawn 10 goroutines all acquiring the same lock, assert exactly one succeeds immediately and the others eventually succeed in sequence (or time out). This test should run with `-race`.

### 3b. `TestAcquireContention` test design needs clarification

The plan says "second acquire blocks, first releases, second succeeds." In a test context, this requires the first goroutine to hold the lock while the second is spinning. This is a timing-sensitive test. The safe approach is to:

1. Acquire in goroutine 1
2. In goroutine 2, begin acquire with a timeout long enough to succeed after release
3. Signal goroutine 1 to release
4. Assert goroutine 2 succeeded

The plan does not detail the goroutine coordination mechanism. Without it, an implementer may write a racy test that relies on sleep timing. Given the codebase uses `sync.WaitGroup` and channels in concurrent tests, this should be explicit in the plan.

### 3c. `TestStaleBreaking` — mtime manipulation approach needs `os.Chtimes`

The plan says "acquire a lock, set mtime to past." In Go, this requires `os.Chtimes(lockPath, pastTime, pastTime)`. The plan does not specify whether it is modifying the lock directory's mtime or the `owner.json` file's mtime. If the implementation checks directory mtime (as the plan implies), the test must use `os.Chtimes` on the directory, not the file. This should be stated.

### 3d. Integration test: `touch -t 202601010000` is fragile and will break in CI

The proposed integration test ages a lock with:

```bash
touch -t 202601010000 /tmp/intercore/locks/staletest-global
```

This hardcodes a past date (January 1, 2026). By the time this runs (or when tests run in the future), this date will be in the past — which is the intent — but `touch -t` format is `[[CC]YY]MMDDhhmm[.ss]` which does not include seconds by default. The existing integration tests do not use `touch -t` for time manipulation; they use `time.Sleep` (e.g., `sleep 2` for the TTL test). For consistency and reliability, use `touch -d "10 minutes ago"` (GNU coreutils) or simply pre-create the directory via the `ic` CLI and then manipulate mtime.

More importantly, `touch -t 202601010000` sets the mtime to a fixed past date. If the stale detection threshold is `--older-than=1s`, any date in the past works — but the choice of a calendar date rather than a relative offset is fragile if the test is ever run offline or with a frozen clock.

**Recommendation:** Replace with:

```bash
touch -d "10 seconds ago" /tmp/intercore/locks/staletest-global
```

Or, if `touch -d` portability is a concern (it is not on Linux), use a Python one-liner:

```bash
python3 -c "import os, time; os.utime('/tmp/intercore/locks/staletest-global', (time.time()-10, time.time()-10))"
```

### 3e. Integration test cleanup gap — stale test state leaks between runs

The integration test does not clean up `/tmp/intercore/locks/` between test runs. If a test fails mid-run and leaves a lock directory, the next run will fail on the "lock acquire" step because the lock already exists. The existing test setup uses `TEST_DIR=$(mktemp -d)` with a `trap cleanup EXIT`, but the lock tests operate on `/tmp/intercore/locks/` (not inside `TEST_DIR`).

**Recommendation:** At the start of the lock test section and in the `cleanup()` function, add:

```bash
# In cleanup():
rm -rf /tmp/intercore/locks/testlock-global \
       /tmp/intercore/locks/contention-scope1 \
       /tmp/intercore/locks/staletest-global
```

Or, cleaner: have the lock tests use a custom `--base-dir` flag pointing inside `TEST_DIR`, which is already cleaned up by the trap. This requires adding `--base-dir` as a flag to `ic lock` commands, which may be worth the investment for testability.

### 3f. No test for the bash wrapper fallback path

The existing integration test has an explicit "Legacy Fallback Path" section that sets `INTERCORE_BIN=""` and `PATH="/usr/bin:/bin"` to verify the bash fallback works without `ic`. The plan adds new bash wrappers but does not include a corresponding fallback test section in `test-integration.sh`. The fallback `mkdir`-based lock path should be tested with the same pattern.

---

## 4. Error Handling Patterns

### 4a. `Release` owner-verification failure silently degrades to `rmdir`

The plan says Release "verify owner matches, rmdir." If the owner metadata cannot be read (e.g., `owner.json` is absent or malformed), the correct behavior is not specified. The sentinel package treats missing rows as a no-op for Reset — but for a lock, releasing a lock you don't own is a correctness failure, not a no-op.

The plan should specify the error handling policy: if the caller is the wrong owner, should Release return `ErrNotOwner` (exit 2) or silently succeed (exit 0)? Given that the bash fallback uses `rmdir` without owner verification, a pragmatic policy might be: if metadata is unreadable, allow the release (fail-open for resilience). But this should be explicit.

### 4b. CLI error message format deviates from established pattern

The existing CLI error format is consistently `ic: <subcommand> <action>: <detail>`:

```go
fmt.Fprintf(os.Stderr, "ic: sentinel check: %v\n", err)
fmt.Fprintf(os.Stderr, "ic: state set: %v\n", err)
```

The plan's proposed `cmd/ic/lock.go` does not show error message format, but the implementation should follow this pattern:

```go
fmt.Fprintf(os.Stderr, "ic: lock acquire: %v\n", err)
fmt.Fprintf(os.Stderr, "ic: lock release: %v\n", err)
```

This is important for the bash wrappers, which rely on error messages appearing on stderr to distinguish error conditions from expected negatives (exit 1).

### 4c. `ErrTimeout` propagation in the CLI — exit code mapping

The plan says `ic lock acquire` exits 1 on "timeout/contention." From the Go side, `Acquire` returns `ErrTimeout` on timeout and presumably `ErrNotFound` or nothing on contention (since contention is handled by spinning, not returning). The CLI layer must map:

- `nil` → exit 0 (acquired)
- `ErrTimeout` → exit 1 (timeout = contention not resolved)
- any other error → exit 2 (unexpected)

This mapping is not shown in the plan. The sentinel CLI uses `if allowed { return 0 } else { return 1 }` — the lock CLI needs an analogous clear mapping documented.

### 4d. `os.MkdirAll` permissions on the base dir

The plan specifies `os.MkdirAll(m.BaseDir, 0755)`. The existing codebase uses `os.MkdirAll(dir, 0755)` in `cmdInit` for the `.clavain/` directory. This is consistent. However, `/tmp/intercore/locks/` is world-writable by default when created with 0755, which means other users on the system could interfere with locks. The plan's architecture note mentions this is for process-level mutual exclusion — if multiple users share the machine (unlikely in this deployment but worth noting), 0700 would be more appropriate for the `locks/` directory.

This is a low-severity concern given the deployment context (single-user server), but the plan should acknowledge it.

### 4e. `Clean` return count type — consistency with `Prune`

The plan specifies `Clean(maxAge time.Duration) (int, error)`. The existing `Prune` functions return `(int64, error)` to match `sql.Result.RowsAffected()`. For the filesystem-based `Clean`, returning `int` is natural since it is a counter, not a `RowsAffected` value. This is fine, but the CLI printing pattern should match the existing `"N pruned\n"` format used by `sentinel prune` and `state prune`:

```go
fmt.Printf("%d cleaned\n", count)
```

The plan says "print count" but does not specify the format string. Consistent output format matters for bash parsing in scripts.

---

## Minor Points

- The plan version is dated `2026-02-19T01:09:17Z` in the bead header but the filename is `2026-02-18`. The UTC date rolls over; this is fine but may cause confusion.
- The migration table for `sprint_claim` shows "none (single attempt)" as the stale timeout but the migration assigns it to `intercore_lock` with a default `1s` timeout. This changes the behavior from single-attempt to spin-wait. If `sprint_claim` semantics require single-attempt (no retry), the migration should pass `--timeout=0` or use a dedicated flag. The plan should explicitly address this behavior change.
- `ic lock list` output format is described as "tab-separated: name, scope, owner, age." Adding an "age" column requires computing the duration since `created`. The `age` value should be a human-readable string (e.g., `"3s"`, `"5m"`) consistent with how `ic dispatch list` formats timestamps. The plan does not specify this format.

---

## Priority Summary

| Finding | Severity | Task |
|---------|----------|------|
| `Acquire` missing `ctx.Context` parameter | High | Task 0 |
| Stale detection uses dir mtime only (not `created` field) | High | Task 0 |
| Missing `ErrNotOwner` error sentinel | High | Task 0, 1 |
| `intercore_lock` does not handle exit code 2 fallthrough | High | Task 2 |
| `lib-sprint.sh` already sources `lib-intercore.sh` — redundant add | High | Task 3 |
| `touch -t` hardcoded date in integration test | Medium | Task 4 |
| Integration test leaks lock dirs between runs | Medium | Task 4 |
| Missing concurrent acquire unit test | Medium | Task 0 |
| `Manager` should be `Store` (vocabulary consistency) | Medium | Task 0 |
| Package should be `fslock` not `lock` (naming clarity) | Medium | Task 0 |
| Error sentinels in `errors.go` (not inline) | Low | Task 0 |
| `DefaultMaxWait` comment embeds derived assumption | Low | Task 0 |
| Fallback missing stale-lock breaking | Low | Task 2 |
| Fallback `find -mmin` ignores `max_age` param | Low | Task 2 |
| File header version comment stale (0.1.0 vs 0.3.0) | Low | Task 2 |
| `sprint_claim` behavior change (no retry → 1s retry) | Low | Task 3 |
| No fallback path test for new bash wrappers | Low | Task 4 |
