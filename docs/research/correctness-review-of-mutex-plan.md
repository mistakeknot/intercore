# Correctness Review: Intercore Mutex Consolidation (F6)

**Plan file:** `docs/plans/2026-02-18-intercore-mutex-consolidation.md`
**Reviewed:** 2026-02-18
**Reviewer:** Julik (Flux-drive Correctness Reviewer)

---

## Invariants That Must Hold

Before drilling into failure modes, these are the correctness invariants the lock subsystem must preserve:

1. **Mutual exclusion:** At most one caller holds a named lock at any instant.
2. **Owner identity:** Only the process that acquired a lock may release it.
3. **Stale-lock bounded recovery:** A crashed holder must never block all future callers forever; stale break must happen within the configured age window.
4. **Fail-safe on timeout:** Functions that are currently fail-safe (`return 0` on lock timeout) must remain fail-safe after migration; functions that signal conflict (`return 1`) must continue to do so.
5. **No silent data corruption:** A lost-update race on `sprint_artifacts` or `phase_history` must not be introduced by the new wrappers.
6. **Fallback parity:** The bash fallback in `intercore_lock` must have semantically identical behavior (stale detection, retry count, fail behavior) to the Go path.
7. **intercore_available idempotency:** The binary-discovery and health check in `intercore_available` must not produce false positives that cause the caller to trust a broken Go path.

---

## Finding 1 — CRITICAL: mkdir + owner.json Write Is Not Atomic (TOCTOU on Lock Identity)

**Severity:** High (data corruption)
**Location:** Plan Task 0, `Acquire` logic, step 3

### What the plan says

```
2. os.Mkdir(lockPath, 0755)  — atomic acquire attempt
3. If mkdir succeeds: write owner.json inside, return nil
```

### The race

`mkdir` is atomic: exactly one caller wins the directory creation. That part is correct. However, between the successful `mkdir` and the completion of `os.WriteFile("owner.json", ...)`, the owner file does not exist yet. Any concurrent caller that enters the stale-break path and calls `rmdir(lockPath)` + re-`mkdir` during that window will:

1. See the directory (created by P1).
2. Try `stat(lockPath)` for mtime — gets the mtime of the freshly-created dir (less than `maxAge` seconds ago). The stale check passes: dir is young, so P2 backs off and waits.

That part is safe in the normal case. The problematic path is the **clean** command and the **List** command:

- `ic lock list` reads `owner.json` inside each lock directory. If called between `mkdir` and `WriteFile`, it reads a lock with no owner file. The plan's `List` implementation must handle this gracefully (empty owner.json → treat as incomplete acquire → skip or show as "unknown").
- `ic lock stale` uses `stat(lockDir).mtime` not `owner.json["created"]`. If `owner.json` never gets written (process killed after `mkdir` but before `WriteFile`), the directory exists with no owner metadata forever.

**Concrete failure sequence for the orphan case:**

```
t=0  P1: mkdir("/tmp/intercore/locks/sprint-abc123") → success
t=0  P1: [killed by SIGKILL before WriteFile]
t=5s Stale check: stat(lockDir).mtime = t=0, age = 5s, threshold = 5s
     → "not yet stale" by one second (off-by-one in boundary comparison)
t=6s Stale check fires: rmdir + re-mkdir by P2 → success
     P2 tries WriteFile → success
```

The orphan issue is manageable, but only if:
- `Clean` tolerates missing `owner.json` (currently not specified).
- `List` does not panic or crash on a missing owner file.

**Minimal fix:** The plan must explicitly state that `Acquire` and `List`/`Stale`/`Clean` tolerate a lock directory with no `owner.json`. The Go implementation should treat a missing or unreadable `owner.json` as "unknown owner, mtime is the age anchor." This is the right behavior and is cheap to implement; it just needs to be called out.

---

## Finding 2 — CRITICAL: Stale-Lock Breaking Is a TOCTOU Race (Two-Callers Can Both Succeed)

**Severity:** High (mutual exclusion violated)
**Location:** Plan Task 0, `Acquire` logic, step 4

### What the plan says

```
4. If mkdir fails (EEXIST): check stale (stat mtime > maxAge), break if stale, else spin-wait
```

### The race

Two processes P2 and P3 are both spinning on a lock held by dead P1. Both reach the stale-break threshold at approximately the same time:

```
t=5s  P2: stat(lockDir) → mtime=old → stale
t=5s  P3: stat(lockDir) → mtime=old → stale
t=5s  P2: rmdir(lockDir) → success (lockDir removed)
t=5s  P3: rmdir(lockDir) → ENOENT (already gone)  ← P3 must handle this
t=5s  P2: mkdir(lockDir) → success → P2 holds lock
t=5s  P3: mkdir(lockDir) → EEXIST ← P3 must now spin, not also acquire
```

This interleaving is actually **safe as written** for the stale-break case, because after P2's `rmdir` succeeds and P3's `rmdir` fails with ENOENT, P3 immediately retries `mkdir` and loses to P2. No double-acquisition.

**However**, the existing `sprint_advance` code in `lib-sprint.sh` (line 514) uses `rm -rf` as a fallback when `rmdir` fails:

```bash
rmdir "$lock_dir" 2>/dev/null || rm -rf "$lock_dir" 2>/dev/null || true
mkdir "$lock_dir" 2>/dev/null || return 1
break
```

This `rm -rf` would remove `owner.json` even after P2 has written it — meaning P3 could then succeed at `mkdir` and believe it holds the lock, while P2 also believes it holds the lock. **Two holders, simultaneous.** Invariant 1 violated.

The new Go `Clean` function must similarly avoid the `rm -rf` escalation. `rmdir` on a non-empty directory (one with `owner.json`) will fail with ENOTEMPTY — and that is the correct behavior, because a fresh live owner already holds the lock. The plan does not call this out explicitly. The Go implementation must use `os.Remove(ownerFile)` followed by `os.Remove(lockDir)`, not `os.RemoveAll`.

**Migration risk:** Task 3 removes the `rm -rf` fallback from `lib-sprint.sh`. This is correct behavior, but the plan does not flag this as a safety-critical deletion. It should be explicit.

---

## Finding 3 — HIGH: Release Does Not Verify Owner Before Removing

**Severity:** High (lock theft by unrelated caller)
**Location:** Plan Task 0, `Release` function; Task 2, `intercore_unlock`

### What the plan says

```go
(m *Manager) Release(name, scope string) error  — verify owner matches, rmdir
```

The plan mentions owner verification in the function signature description but gives no implementation detail. The bash `intercore_unlock` in Task 2 does not verify owner at all:

```bash
intercore_unlock() {
    ...
    if intercore_available; then
        "$INTERCORE_BIN" lock release "$name" "$scope" >/dev/null 2>&1 || true
        return 0
    fi
    # Fallback: direct rmdir
    rmdir "/tmp/intercore/locks/${name}-${scope}" 2>/dev/null || true
    return 0
}
```

The bash fallback has **no owner check whatsoever**. Any process that knows the lock name and scope can call `intercore_unlock` and release a lock it does not hold.

### Concrete failure sequence

```
P1: acquires sprint-abc123 lock
P2: is spinning on sprint-abc123, times out, returns 0 (fail-safe)
P2: in its cleanup path, calls intercore_unlock "sprint" "abc123"
    → fallback rmdir succeeds (P2 never had the lock)
P3: acquires sprint-abc123 lock (gets it)
P1 and P3 now both believe they hold the lock
```

This race is directly enabled by `intercore_unlock` returning 0 always ("fail-safe: never block on unlock failure") combined with the fallback having no owner knowledge.

**Correct fix:** The Go `Release` command must read `owner.json`, compare `owner` field to `--owner` argument, and exit 1 if they differ. The bash `intercore_lock` wrapper must pass `--owner="$$:$(hostname -s)"` on acquire, and `intercore_unlock` must pass the same owner string on release so the Go side can verify it. The fallback bash path is inherently unverifiable (it has no stored owner info), which means the fallback is safe only when the fallback acquire also stores no owner.

The plan currently makes the bash fallback acquire with no owner metadata, making the owner-based release check impossible on the fallback path. The simplest safe approach for the fallback: the session (script invocation) that acquired must be the only one that calls unlock. Document this clearly as a limitation of the fallback.

---

## Finding 4 — HIGH: intercore_available Health Check Gates Lock Acquire — Wrong Dependency

**Severity:** High (silent fallback regression under DB corruption)
**Location:** Task 2, `intercore_lock` wrapper

### What the plan says

```bash
intercore_lock() {
    ...
    if intercore_available; then
        "$INTERCORE_BIN" lock acquire ...
        return $?
    fi
    # Fallback: direct mkdir
```

`intercore_available` calls `ic health` which connects to the SQLite DB. If the DB is missing, corrupt, or `ic init` has not been run, `intercore_available` returns 1 and the fallback runs.

**The lock subsystem is explicitly designed to work without a DB** (Task 1, "The lock commands do NOT require a database connection"). Yet the bash wrapper gatekeeps lock operations behind a DB health check. If the DB is broken, the wrapper silently falls back to a dumber implementation with no owner tracking, no stale detection (the fallback has a fixed 10-retry spin with no mtime check), and different stale age behavior.

**This means:** A DB failure degrades the lock manager silently from a structured system to a raw-mkdir system. Worse, if `ic` binary exists but `ic health` fails (DB schema mismatch after upgrade), `ic lock acquire` would succeed perfectly (no DB needed), but the bash wrapper never tries it.

**Correct fix:** Introduce a separate `intercore_lock_available` function that checks only whether the `ic` binary is present and functional for lock operations, without requiring the DB:

```bash
intercore_lock_available() {
    local bin
    bin=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)
    [[ -z "$bin" ]] && return 1
    # Lock commands don't need DB — a quick version check is sufficient
    "$bin" version >/dev/null 2>&1
}
```

Or: pass `INTERCORE_BIN` resolution through the same check as `intercore_available` but skip the `health` gate specifically for lock operations. Either way, the wrapper must not be held hostage to DB availability when the lock operations do not use the DB.

---

## Finding 5 — MEDIUM: Fallback Bash Stale Detection Is Missing (Fixed-Timeout Only)

**Severity:** Medium (fallback does not match Go behavior)
**Location:** Task 2, `intercore_lock` fallback path

### What the plan says

```bash
# Fallback: direct mkdir (legacy pattern)
local retries=0 max_retries=10
while ! mkdir "$lock_dir" 2>/dev/null; do
    retries=$((retries + 1))
    [[ $retries -gt $max_retries ]] && return 1
    sleep 0.1
done
```

This fallback spins for 1 second and gives up with `return 1`. But the original `lib-sprint.sh` patterns use `return 0` (fail-safe) for `sprint_set_artifact` and `checkpoint_write`, and `return 1` for `sprint_claim` and `sprint_advance`. The fallback does not mirror this distinction — it always returns 1 on contention regardless of the lock purpose.

More importantly: the fallback has **no stale detection**. The Go `Acquire` breaks stale locks after `maxAge`. If the Go path dies and the fallback runs, a stale lock from a crashed process will block every fallback caller for the full 1-second spin window, then return 1, which for fail-safe callers means silently dropping the update. For critical callers like `sprint_advance` this means phase transitions are silently skipped under DB-unavailability + concurrent crash.

The existing `lib-sprint.sh` inline patterns (lines 223-243, 502-521) have stale detection. The fallback path in the new wrapper removes it.

**Correct fix:** Add mtime-based stale detection to the fallback:

```bash
while ! mkdir "$lock_dir" 2>/dev/null; do
    retries=$((retries + 1))
    if [[ $retries -gt $max_retries ]]; then
        # Check stale before giving up
        local mtime now
        mtime=$(stat -c %Y "$lock_dir" 2>/dev/null || echo 0)
        now=$(date +%s)
        if [[ $((now - mtime)) -gt 5 ]]; then
            rmdir "$lock_dir" 2>/dev/null && mkdir "$lock_dir" 2>/dev/null && return 0
        fi
        return 1
    fi
    sleep 0.1
done
```

---

## Finding 6 — MEDIUM: sprint_advance Calls sprint_record_phase_completion While Holding the Advance Lock — Nested Lock, Deadlock Risk

**Severity:** Medium (deadlock under migration)
**Location:** `lib-sprint.sh` line 542; Task 3 migration table

### Current code in sprint_advance (line 541-542):

```bash
bd set-state "$sprint_id" "phase=$next_phase" 2>/dev/null || true
sprint_record_phase_completion "$sprint_id" "$next_phase"
rmdir "$lock_dir" 2>/dev/null || true
```

`sprint_record_phase_completion` acquires `sprint`/`${sprint_id}` (via the plan's naming). `sprint_advance` holds `sprint-advance`/`${sprint_id}`. These are **different lock names**, so no deadlock in the plan's naming scheme.

However, after migration Task 3:
- `sprint_advance` acquires `sprint-advance`/`${sprint_id}`
- Inside the critical section, it calls `sprint_record_phase_completion`
- Which tries to acquire `sprint`/`${sprint_id}`

If another process holds `sprint`/`${sprint_id}` (locked by a concurrent `sprint_set_artifact`) and is waiting to acquire `sprint-advance`/`${sprint_id}` (never, in practice, but conceivable), this is a lock-ordering issue. More practically: `sprint_advance` does not release its lock before calling `sprint_record_phase_completion`. The plan shows `intercore_unlock "sprint" "$sprint_id"` only before the `rmdir` at the end, but the nested `sprint_record_phase_completion` call is still inside the `sprint-advance` lock. This means:

```
P1 holds sprint-advance/abc123
P1 calls sprint_record_phase_completion → tries to acquire sprint/abc123
P2 holds sprint/abc123 (in sprint_set_artifact)
P2 is not waiting for sprint-advance/abc123
→ No deadlock, P1 waits up to 1s for sprint/abc123 to be released by P2
→ If P2 is slow (bd state write takes >1s), sprint_record_phase_completion returns 0 (fail-safe on timeout)
→ Phase is advanced but phase_history is not updated
→ phase and phase_history are now inconsistent
```

This is not a new bug — it exists today — but the migration must not make it worse, and the plan does not acknowledge it. The correct fix is to call `sprint_record_phase_completion` *after* releasing the advance lock, not while holding it. The current code already does this wrong (see line 541-544 in `lib-sprint.sh`): `sprint_record_phase_completion` is called at line 542, `rmdir` at line 544. The migration must fix this ordering:

```bash
# Correct order after migration:
bd set-state "$sprint_id" "phase=$next_phase" 2>/dev/null || true
intercore_unlock "sprint-advance" "$sprint_id"  # Release BEFORE nested lock
sprint_record_phase_completion "$sprint_id" "$next_phase"
# No unlock needed here — already done above
```

The plan's migration table does not call this ordering fix out.

---

## Finding 7 — MEDIUM: ic lock release Without --owner Argument Creates Wrong Exit Code Contract

**Severity:** Medium (bash callers silently succeed on wrong-owner release)
**Location:** Task 1, `ic lock release` CLI command

The plan says:

```
ic lock release <name> <scope>  — exit 0=released, 1=not found, 2=error
```

There is no `--owner` argument specified for `ic lock release`. If owner verification is to happen (as stated in the `Release` function description), the CLI must accept and require an `--owner` flag. Without it, the CLI has no identity to verify against, and releases any lock with matching name+scope unconditionally.

Additionally, if the lock dir exists but `owner.json` is missing (crash during acquire, Finding 1), should `release` return 0 ("I cleaned it up"), 1 ("not found"), or 2 ("error")? The plan is silent on this boundary case.

**Correct fix:** Add `--owner=<string>` to `ic lock release`. If owner does not match, exit 1 with a message on stderr. The bash wrapper passes the same `$$:$(hostname -s)` string used at acquire time.

---

## Finding 8 — MEDIUM: intercore_lock_clean Fallback Uses find -mmin +1 (60s), Not 5s

**Severity:** Medium (fallback stale window is 12x longer than Go path)
**Location:** Task 2, `intercore_lock_clean` fallback

```bash
find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d -mmin +1 -exec rmdir {} \; 2>/dev/null || true
```

`-mmin +1` means older than 60 seconds. The Go `Clean` default is 5 seconds (`DefaultStaleAge`). The bash `intercore_lock_clean` passes `max_age="${1:-5s}"` to the Go path, but the fallback uses a hardcoded 60-second window. Any code calling `intercore_lock_clean "5s"` on the fallback path will silently leave 5-59-second-old stale locks in place. This behavioral divergence between paths is a silent correctness gap.

**Correct fix:** Parse the `max_age` argument in the fallback. `5s` → compute `mmin` value (`5/60` rounds to 0, use `-newer` with a temp file instead):

```bash
# For sub-minute thresholds use find -newer with a reference file
local tmp_ref
tmp_ref=$(mktemp /tmp/intercore-ref-XXXXXX)
touch -d "-${max_age}" "$tmp_ref" 2>/dev/null || true
find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d ! -newer "$tmp_ref" \
    -exec rmdir {} \; 2>/dev/null || true
rm -f "$tmp_ref" 2>/dev/null || true
```

Or: accept that the fallback has a coarser granularity and document it explicitly.

---

## Finding 9 — LOW: Migration Task 3 Uses "trap for safety" but Trap in Sourced Library Is Dangerous

**Severity:** Low (signal handling regression in calling hook)
**Location:** Task 3 description

The plan says:

> Add `intercore_unlock <name> <scope>` before every return path (and in a trap for safety)

Setting a `trap` inside a function that is part of a sourced library will replace the calling hook's trap handler. In Clavain hooks, `trap` is used by session-handoff.sh and other scripts for cleanup. A `trap ERR` or `trap EXIT` set inside a sprint function will shadow the hook's trap, leaving the hook without its own cleanup on error.

**Correct fix:** Do not use `trap` inside the library functions. Instead, use the pattern the existing code uses: explicit `rmdir` (now `intercore_unlock`) before every `return` path. This is what the "Before" example in Task 3 already shows — the inline code does `rmdir` before each return. Keep that discipline; drop the trap suggestion.

---

## Finding 10 — LOW: intercore_available Is Called and Caches INTERCORE_BIN, But Lock Commands Bypass DB Health Check

**Severity:** Low (inconsistent availability semantics)
**Location:** Task 2, `intercore_lock` and `intercore_unlock`

`intercore_available` caches the binary path in `INTERCORE_BIN` and sets it to empty string if the health check fails. Once set, `if [[ -n "$INTERCORE_BIN" ]]` short-circuits on every subsequent call, returning 0 without re-checking health.

If the DB health check fails at session start (setting `INTERCORE_BIN=""`), and then `ic` becomes available mid-session (e.g., someone runs `ic init`), the cached empty string means `intercore_available` keeps returning 1 for the entire session. Lock operations stay in fallback mode permanently. This is a known limitation of the sentinel path and is accepted there, but for lock operations it means once the DB is broken, you can never escape the fallback even if you fix the DB — until the process exits and re-sources the library.

This is low severity because the lock subsystem is designed to function without a DB. But it conflicts with the goal of seamlessly using the Go lock manager when available.

---

## Finding 11 — LOW: Integration Test Uses touch -t to Age Lock Dir, But mtime Precision Is Filesystem-Dependent

**Severity:** Low (flaky test)
**Location:** Task 4 integration test

```bash
touch -t 202601010000 /tmp/intercore/locks/staletest-global
ic lock stale --older-than=1s | grep -q "staletest"
```

`touch -t` with a date in the past works on Linux with ext4/tmpfs (sub-second mtime), but the `--older-than=1s` comparison in Go uses `time.Since(info.ModTime()) > maxAge`. On a system where `tmpfs` mtime resolution is 1-second, this test may be flaky if the lock was created less than 1s before `touch` runs. The lock dir is created by `ic lock acquire staletest global`, which gets mtime=now. Then `touch -t 202601010000` sets it to 2026-01-01 00:00. The comparison `age > 1s` will be true (age is ~48 days), so this specific test will not be flaky. However, the test for clean-removes-stale should also verify that the lock directory is actually gone from the filesystem, not just absent from `ic lock list` (which reads metadata and could theoretically skip corrupt dirs).

This is minor but worth noting so future tests don't depend on `--older-than=200ms` with wall-clock timing.

---

## Finding 12 — OBSERVATION: sprint_claim Fallback After Failed mkdir Has a Write-Without-Lock Window

**Severity:** Low (pre-existing, not introduced by this plan)
**Location:** `lib-sprint.sh` `sprint_claim` lines 310-318 (not in migration scope)

The plan migrates `sprint_claim` to use `intercore_lock "sprint-claim" "$sprint_id"`. The existing code on failed `mkdir` does:

```bash
if ! mkdir "$claim_lock" 2>/dev/null; then
    sleep 0.3
    current_claim=$(bd state "$sprint_id" active_session ...)
    if [[ "$current_claim" == "$session_id" ]]; then
        return 0  # We already own it
    fi
    echo "Sprint $sprint_id is being claimed by another session" >&2
    return 1
fi
```

This reads `active_session` without holding any lock — a plain check-then-act on a value another process is currently writing under the lock. The migration replaces this with `intercore_lock "sprint-claim" "$sprint_id" || return 1`. The new behavior: if the lock is busy, spin-wait up to timeout, then return 1. This is semantically correct — it eliminates the lockless read. The plan's migration is an improvement here.

---

## Summary of Findings

| # | Severity | Finding | Impact |
|---|----------|---------|--------|
| 1 | Critical | mkdir+owner.json non-atomic: missing owner.json not handled by List/Clean/Stale | Orphan locks never cleaned |
| 2 | Critical | Stale-break TOCTOU + `rm -rf` in existing code must not appear in Go `Clean` | Mutual exclusion violation if `rm -rf` used |
| 3 | High | `intercore_unlock` bash fallback has no owner verification | Lock theft by unrelated caller |
| 4 | High | `intercore_available` (DB health) gates lock ops that don't need DB | Silent fallback when DB broken, even if `ic` works fine |
| 5 | Medium | Fallback bash has no stale detection, always returns 1 on timeout | Fail-safe callers silently drop updates under concurrent crash |
| 6 | Medium | `sprint_advance` calls `sprint_record_phase_completion` while holding lock | phase/phase_history inconsistency on contention |
| 7 | Medium | `ic lock release` has no `--owner` flag in plan | Owner verification impossible at CLI level |
| 8 | Medium | `intercore_lock_clean` fallback uses `-mmin +1` (60s) vs 5s Go default | Stale locks linger 12x longer in fallback mode |
| 9 | Low | "trap for safety" suggestion in Task 3 will clobber hook trap handlers | Signal/exit cleanup broken in calling hooks |
| 10 | Low | `INTERCORE_BIN=""` cache never re-checked in same session | Lock ops stuck in fallback after transient DB failure |
| 11 | Low | Integration test timing could be flaky with sub-second `--older-than` values | Flaky CI |
| 12 | Low | `sprint_claim` fallback lockless read (pre-existing, improved by migration) | Eliminated by new plan — net positive |

---

## Required Changes Before Implementation

### Must-fix before writing any code

**F1 — Specify missing-owner.json handling in Acquire, List, Stale, Clean:**
Add to Task 0 spec: "A lock directory with no `owner.json` (crash during acquire) is treated as a stale lock anchored to the directory's mtime."

**F2 — Prohibit rm -rf in Go Clean:**
Task 0 spec must explicitly state: "Use `os.Remove(ownerFile)` then `os.Remove(lockDir)`. Never `os.RemoveAll`. If `os.Remove(lockDir)` fails with ENOTEMPTY, another process already re-acquired; return without error."

**F3 — Add --owner to ic lock release:**
Task 1 must add `--owner=<string>` to `release` subcommand. Task 2 `intercore_unlock` must pass the owner string. The bash fallback documents that it cannot verify ownership.

**F4 — Separate lock availability check from DB health check:**
Task 2 must not call `intercore_available` for lock operations. Replace with `intercore_lock_available` that checks only binary presence + `ic version`.

### Should-fix before Task 3 migration

**F5 — Add stale detection to bash fallback:**
Task 2 fallback in `intercore_lock` must mirror the 5-second stale-break behavior of the Go path.

**F6 — Fix lock ordering in sprint_advance migration:**
Task 3 migration of `sprint_advance` must call `intercore_unlock "sprint-advance"` before calling `sprint_record_phase_completion`, not after it.

**F7 — Add --owner to ic lock release CLI:**
Already covered in F3.

**F8 — Fix intercore_lock_clean fallback timeout:**
Use `touch`+`find -newer` to respect sub-minute `max_age` values.

### Should-fix or document before ship

**F9 — Remove trap suggestion from Task 3:**
Replace "in a trap for safety" with "before every return path" — which the plan's own example already shows correctly. Drop the trap language.

---

## Migration Safety Assessment

The lib-sprint.sh migration (Task 3) is **safe to proceed** for `sprint_set_artifact`, `sprint_record_phase_completion`, and `checkpoint_write` (fail-safe paths) **provided F5 is addressed** (fallback stale detection).

The migration of `sprint_claim` and `sprint_advance` (conflict-signaling paths) is safe in the Go path but degrades in the fallback path (no stale detection → always returns 1 on contention instead of sometimes breaking stale locks). If the DB is unavailable, the fallback will be more conservative (more contention returns) than the current inline code. This is acceptable in production; it is worth documenting.

The most important gate before shipping Task 3: run `go test -race ./...` on the Go lock package. The mkdir-based locking logic in Go has no shared mutable state (filesystem operations are the synchronization point), so the race detector will not catch POSIX-level races, but it will catch any shared struct fields that get concurrent access in the Manager.

An integration test exercising two concurrent `ic lock acquire` processes (using `&` + `wait` in the test script) is essential and not currently in the Task 4 plan. The contention test shown uses sequential calls (`ic lock acquire contention scope1` then `ic lock acquire contention scope1 --timeout=200ms`), which does not test true concurrent acquire.

---

## Correctness Contract for the Bash Fallback (Explicit)

The plan should state this clearly in comments and AGENTS.md:

The bash fallback in `intercore_lock`/`intercore_unlock` provides:
- **Mutual exclusion** via `mkdir` atomicity (correct)
- **No owner verification** (any caller can unlock any lock by name+scope)
- **No stale detection** unless F5 is fixed (currently: fixed-timeout only)
- **No audit trail** (no owner.json equivalent)

The bash fallback does NOT provide the same safety guarantees as the Go path. It is a degraded-mode compatibility shim for environments where `ic` is absent or the binary is broken. It should never be the primary code path in production.
