# Architecture Review: Intercore Mutex Consolidation (F6)

**Plan reviewed:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-mutex-consolidation.md`
**Date:** 2026-02-18
**Codebase:** `/root/projects/Interverse/infra/intercore/`

---

## Summary Verdict

The plan is structurally sound and well-scoped. The three-layer architecture (Go package / CLI commands / bash wrappers) mirrors every existing subsystem in intercore exactly. The decision to use filesystem-level atomicity rather than SQLite is correct for this use case. There are four issues worth addressing before implementation begins, ranging from one must-fix boundary violation to three design clarifications that affect correctness under failure conditions.

---

## 1. Boundaries and Coupling

### The split between `internal/lock/` and the DB is well-reasoned

The plan explicitly justifies why locks live on the filesystem rather than in SQLite: `mkdir` atomicity is the point, and the lock manager must work even when the DB is broken or uninitialized. This reasoning is correct. Sentinels answer "was this allowed in the last N seconds?" — a time-bounded throttle stored durably. Locks answer "is this operation in progress right now?" — a presence signal that must be created and destroyed within a single process lifetime and must not survive crashes in a way that blocks future runs. These are genuinely different contracts, and keeping them in separate mechanisms avoids forcing SQLite's transaction model onto a problem it would complicate unnecessarily.

### Must-fix: `intercore_available()` gate breaks lock independence

The plan states that `ic lock` "works even when the DB is broken or uninitialized," but the bash wrapper `intercore_lock()` routes through `intercore_available()`, which calls `ic health`, which opens the database. If the DB is unhealthy, `intercore_available()` returns 1, and the function falls through to the direct-`mkdir` fallback path. This means the bash-layer contract ("works when DB is broken") is only partially honored — the CLI contract holds, but the wrapper negates it.

The problem is structural: `intercore_available()` is the wrong gate for lock operations. It was designed for DB-backed commands (sentinel, state, dispatch, run). The lock command has no DB dependency at all.

The fix is small. Add a separate availability check in `lib-intercore.sh` that tests only whether the binary is present, without running the DB health check:

```bash
intercore_bin_available() {
    # Returns 0 if the ic binary is present (no DB check — for filesystem-only commands).
    [[ -n "$INTERCORE_BIN" ]] && return 0
    INTERCORE_BIN=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)
    [[ -n "$INTERCORE_BIN" ]]
}
```

Then `intercore_lock()` and `intercore_unlock()` use `intercore_bin_available` instead of `intercore_available`. This preserves the documented guarantee without requiring a second copy of the binary-discovery logic.

### The fallback lock directory matches the primary path — good

The fallback in `intercore_lock()` creates `/tmp/intercore/locks/${name}-${scope}` directly, which is identical to the path the Go lock manager would create. This means `ic lock clean` and `ic lock list` will see fallback-created locks correctly. This is intentional and correct — it is worth calling out explicitly in the plan's acceptance criteria, since it is the property that makes the fallback safe to use.

### Cross-module sourcing order is already handled

`lib-sprint.sh` line 12 already sources `lib-intercore.sh` before any sprint functions are defined:
```bash
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```
Task 3's "Prerequisite" block proposes adding a second source directive near the top. This would create a double-source. The existing guard `[[ -n "${_SPRINT_LOADED:-}" ]]` applies to `lib-sprint.sh`, not to `lib-intercore.sh`, so the second source call would not re-execute the body but it is still unnecessary and confusing. The prerequisite should be removed from Task 3 — the existing source is sufficient.

### Scope creep check

The plan touches four files: `internal/lock/lock.go` (new), `cmd/ic/lock.go` (new), `cmd/ic/main.go` (switch case + usage), `lib-intercore.sh` (new functions + version bump), and `hub/clavain/hooks/lib-sprint.sh` (migration). All five are necessary for the stated goal. No unnecessary files are touched.

---

## 2. Pattern Analysis

### The three-layer structure matches existing subsystems exactly

Every existing intercore subsystem follows this pattern:
- `internal/<name>/` — pure Go package with a `Store` or `Manager` type
- `cmd/ic/<name>.go` — thin CLI wrappers with no business logic
- `lib-intercore.sh` — bash wrappers delegating to the binary with fallbacks

The plan replicates this pattern faithfully. The `Manager` type in `internal/lock/` with `NewManager(baseDir string) *Manager` and method receivers mirrors how `sentinel.Store` and `state.Store` are constructed.

One point of divergence: existing internal packages take a `*sql.DB` as their construction argument (passed via `d.SqlDB()`). The lock manager takes a `baseDir string`. This is correct — there is no DB — but `cmdLock` in `lock.go` will need to call `lock.NewManager(lock.DefaultBaseDir)` directly rather than calling `openDB()`. The plan acknowledges this at Task 1, but the acceptance criteria for Task 1 does not explicitly verify that `cmdLock` does not call `openDB()`. Make that explicit in the acceptance criteria to prevent accidental DB coupling during implementation.

### Naming consistency

The plan introduces the term "scope" for the second lock dimension. Existing intercore commands use `scope_id` (sentinels) and `scope` (state). The lock commands use `scope` as the positional argument label. This is acceptable — the lock concept does not naturally have an "ID" connotation — but the AGENTS.md update in Task 5 should document the distinction explicitly to prevent future confusion.

The lock directory structure `<name>-<scope>` concatenated with a hyphen has a collision risk if either `name` or `scope` contains a hyphen. For example, `sprint-claim`/`abc123` and `sprint`/`claim-abc123` would both produce `sprint-claim-abc123`. In the migration table, the plan uses names like `sprint-claim` and `sprint-advance`, both of which contain hyphens, making this a real concern rather than a theoretical one.

The fix is to use a separator that is unlikely to appear in valid identifiers, such as a double-underscore or a URL-encoded delimiter. Alternatively, use a subdirectory per name: `<baseDir>/<name>/<scope>/`. Either approach eliminates the ambiguity. The directory-per-name approach also makes `ic lock list` output easier to parse and makes `rmdir` on the parent a safe "clean all locks for name X" operation.

### Exit-code convention is correctly applied

The plan uses exit 0 (acquired/released), 1 (contention/not found), 2 (error), 3 (usage) — exactly matching the documented convention in `AGENTS.md`. This is important because bash callers in `lib-sprint.sh` pattern-match on exit codes.

### No anti-patterns introduced

The plan does not add new global state to `main.go`, does not reach into other packages' internals, and does not create circular imports. The new `internal/lock/` package has no imports from any other internal package — it is a pure filesystem package.

---

## 3. Simplicity and YAGNI

### `Manager.BaseDir` field is appropriate, not premature

The `Manager` struct exposes `BaseDir` as a configurable field rather than hardcoding `/tmp/intercore/locks`. This is justified by three concrete needs: the fallback bash code writes to the same path and needs it to match, the integration tests need to write to a temp dir, and `ic lock` should respect an environment variable override if a different `/tmp` is mounted (common in containers). This is not speculative flexibility — it has immediate uses.

### `--timeout` defaulting at the CLI vs. at the Go layer — resolve this clearly

The plan sets `DefaultMaxWait = 1 * time.Second` in the Go package and `--timeout=1s` as the CLI default. This duplication is fine as long as both are documented, but the bash wrapper `intercore_lock()` passes the timeout from its own `$3` argument: `"$INTERCORE_BIN" lock acquire "$name" "$scope" --timeout="$timeout"`. The bash default is `"${3:-1s}"`. Three places express the same value (1 second). If the Go package's `DefaultMaxWait` changes, the bash default does not update automatically. The Go default should be the single source of truth: the CLI should pass no `--timeout` flag when none is given, and the Go package applies `DefaultMaxWait`. The bash wrapper should pass `--timeout` only when the caller explicitly provides a non-default value. This is how existing wrappers handle optional arguments (see `intercore_dispatch_wait()` in `lib-intercore.sh` lines 204-211).

### The stale-breaking spin-wait in `Acquire` duplicates the inline bash logic being removed

The plan replaces five inline spin-wait blocks in `lib-sprint.sh` with a Go implementation of the same algorithm. This is the correct consolidation. The only concern is that the Go implementation's stale-age threshold (5 seconds, matching `DefaultStaleAge`) needs to match what the bash migration passes as `--timeout`. Currently `lib-sprint.sh` functions use 5-second stale timeouts and 1-second spin timeouts (10 retries at 100ms). The plan's migration table passes `"1s"` as the timeout, which covers the spin wait, but the stale-breaking threshold is controlled separately by `DefaultStaleAge`. These are independent parameters and the plan correctly treats them as such, but the integration test in Task 4 uses `touch -t 202601010000` to age a lock, which produces a lock that is over one year old — any `--older-than` value would match it. Using `touch -d "10 seconds ago"` would be a more precise and less surprising test.

### The fallback `intercore_lock_clean()` uses `find ... -mmin +1`

The fallback in `intercore_lock_clean()` deletes lock directories older than 1 minute (`-mmin +1`). The primary path uses `--older-than=5s` by default. This is a 12x difference in timeout between the primary and fallback paths. The fallback should use a threshold proportional to `max_age`, or at minimum match the primary at 5 seconds. The `find` command can use `-newer` with a reference file, or `-mmin` with fractional values (`-mmin +0.1` for 6 seconds). The mismatch creates a window where a genuinely stale lock from a crashed process would not be cleaned by the fallback for a full minute, potentially blocking all sprint operations during that window.

### `ic lock release` ownership verification is the right call — but needs a safe failure mode

The plan specifies that `Release` verifies the owner matches before removing the lock. This is correct for detecting bugs where the wrong caller tries to release. However, the plan does not specify what the release error output looks like for the `intercore_unlock()` bash wrapper, which swallows all output with `>/dev/null 2>&1 || true`. If release fails due to owner mismatch, the lock will not be removed, and the bash caller will silently continue. This could leave stale locks on the filesystem.

The recommendation is to document a specific behavior: `Release` should succeed (exit 0) if the lock directory does not exist at all (idempotent cleanup), but should exit 2 with a message to stderr if the directory exists but the owner does not match. The bash wrapper's `|| true` is appropriate for the "directory already gone" case, but the owner-mismatch case warrants the error surfacing. The simplest fix is to change `intercore_unlock()` to suppress stdout but preserve stderr:

```bash
"$INTERCORE_BIN" lock release "$name" "$scope" >/dev/null || true
# (stderr not suppressed — owner mismatch messages reach the terminal)
```

---

## 4. Task Sequencing and Integration Risk

### Tasks 0 and 1 are prerequisite to Tasks 2 and 3 — the plan correctly sequences them

The critical integration risk is that Task 3 (migrating `lib-sprint.sh`) replaces battle-tested inline lock code with calls to `intercore_lock`. If the Go implementation has a bug, all sprint locking fails simultaneously across all five lock points. The plan mitigates this correctly by:
1. Having unit tests in Task 0 before any migration
2. Having integration tests in Task 4 (though this comes after Task 3 in the plan)

Recommendation: Move the integration test for basic lock acquire/release/list into Task 1's acceptance criteria, so Task 3 cannot begin until the CLI is verified end-to-end. The full integration test section in Task 4 can remain for the contention and stale tests.

### `lib-sprint.sh` migration: trap-based unlock is mentioned but not specified

Task 3 mentions "Add `intercore_unlock <name> <scope>` before every return path (and in a trap for safety)." The existing code does not use traps for lock cleanup — it uses explicit `rmdir` before every `return` path. Adding trap-based cleanup is a behavioral change, not just a refactor, and it interacts with bash's trap inheritance rules when functions are called from subshells. The existing pattern (explicit unlock before each return) is safer in this codebase, which is careful about `set -e` not being set (see `lib-intercore.sh` line 5: "Do NOT use `set -e` here"). Traps should be removed from the plan unless there is a specific crash scenario they address that cannot be handled by the existing explicit-cleanup pattern.

### The `sprint_advance` lock calls `sprint_record_phase_completion` while holding the lock

In `lib-sprint.sh`, `sprint_advance()` acquires `sprint-advance-lock` then calls `sprint_record_phase_completion()`, which acquires `sprint-lock`. These are different locks (different names/scopes), so there is no deadlock risk. The plan maps them correctly: `sprint-advance`/`${sprint_id}` and `sprint`/`${sprint_id}`. But after migration, if `sprint_record_phase_completion` is called while holding the outer lock, and `sprint_record_phase_completion` tries to call `intercore_lock "sprint" "$sprint_id"`, the spin-wait timeout for the inner lock must be shorter than the outer lock's stale-breaking threshold. With both at 1s timeout and 5s stale threshold, this is fine — the inner lock will time out long before the outer lock goes stale. Just verify this remains true if timeouts are ever adjusted.

---

## 5. Findings Classified by Priority

### Must Fix

**F1 — `intercore_available()` gate defeats lock's DB-independence guarantee**
File: `/root/projects/Interverse/infra/intercore/lib-intercore.sh`
Add `intercore_bin_available()` (binary check only, no DB health call) and use it in `intercore_lock()` and `intercore_unlock()`. This is a one-function addition.

### Should Fix Before Implementation

**F2 — Lock directory name collision for hyphenated names**
File: `/root/projects/Interverse/infra/intercore/internal/lock/lock.go`
Change the directory naming from `<name>-<scope>` to `<name>/<scope>` (subdirectory per name) to eliminate the collision. `sprint-claim`/`abc123` vs `sprint`/`claim-abc123` are currently indistinguishable on disk.

**F3 — Fallback `intercore_lock_clean()` uses 60-second threshold vs. 5-second primary**
File: `/root/projects/Interverse/infra/intercore/lib-intercore.sh`
Change `find ... -mmin +1` to a threshold that approximates the `max_age` parameter (at minimum `+0.1` to match the 5-second default, or use `-newer` with a reference file).

### Cleanup / Low Risk

**F4 — Double-source directive in Task 3 prerequisite**
`lib-sprint.sh` already sources `lib-intercore.sh` at line 12. Remove the proposed second source directive from Task 3.

**F5 — Timeout default expressed in three places; unify at Go package**
Remove hardcoded `"1s"` bash default from `intercore_lock()` and let the Go package's `DefaultMaxWait` be the single source. Pass `--timeout` only when the caller provides an explicit override.

**F6 — Remove trap-based cleanup from Task 3 specification**
Traps interact poorly with bash's function call model when `set -e` is absent. The existing explicit-unlock-before-return pattern is safer and is already what the current code does.

**F7 — Integration test uses year-2026-old timestamp; use relative aging**
In Task 4, replace `touch -t 202601010000` with `touch -d "10 seconds ago"` for the stale lock test to make intent clear and make the test date-independent.

---

## 6. What the Plan Gets Correct

The following design choices are sound and should not be revisited:

- Filesystem-level locking is the right mechanism for process mutual exclusion; SQLite is not.
- `ic lock` operating without `openDB()` is the right dependency isolation.
- The `Manager` type with configurable `BaseDir` enables testing without mocking.
- Exit codes 0/1/2/3 match the documented convention throughout.
- The fallback path in `intercore_lock()` writes to the same directory as the Go manager, so `ic lock list` and `ic lock clean` can observe fallback-created locks.
- The migration table correctly preserves the semantics of each existing lock (fail-safe vs. fail-conflict) by mapping them to the correct `|| return <code>` pattern.
- Keeping the lock package in `internal/` (not exported) is correct — only the CLI layer should be the public interface for bash callers.
- The plan does not attempt to consolidate sentinel and lock mechanisms; they serve different purposes and should remain separate.
