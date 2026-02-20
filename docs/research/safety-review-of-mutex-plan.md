# Safety Review: Intercore Mutex Consolidation (F6)

**Reviewer:** Flux-drive Safety Reviewer
**Date:** 2026-02-18
**Plan under review:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-mutex-consolidation.md`
**Change risk classification:** Medium — new filesystem-based locking subsystem replacing inline bash patterns; touches hook infrastructure relied on by all Clavain sprint operations

---

## Threat Model Confirmation

This is a single-server, single-user development tool. There is no multi-tenancy, no network exposure, and no authentication boundary. The primary threat scenarios (established in the prior intercore security review at `infra/intercore/docs/research/review-intercore-security.md`) are:

- Malicious project repositories whose hook scripts or directory structure could influence `ic` behavior
- Symlink attacks on filesystem paths that the tool creates or accesses in `/tmp`
- An attacker who has already compromised the invoking shell's environment

All five concerns raised in the task are evaluated below under this threat model.

---

## Finding 1: /tmp Symlink Attack on Lock Directory Creation (MEDIUM)

**Severity:** Medium
**Exploitability:** Requires attacker to pre-stage a symlink before lock acquisition; race window is narrow
**Blast radius:** Lock directory creation could resolve to an arbitrary location the user can write to

### The Problem

The plan specifies `os.MkdirAll(m.BaseDir, 0755)` followed by `os.Mkdir(lockPath, 0755)` with `BaseDir = "/tmp/intercore/locks"`. Neither the base path creation nor the lock subdirectory creation checks for symlinks in any component of the path.

An attacker who can write to `/tmp` before the first `MkdirAll` runs can do:

```bash
mkdir /tmp/intercore
ln -s /home/mk /tmp/intercore/locks
# Now MkdirAll("/tmp/intercore/locks", 0755) is a no-op (dir exists via symlink)
# Mkdir("/tmp/intercore/locks/sprint-abc123", 0755) creates under /home/mk/
```

Consequences:
- Lock directories appear in paths the attacker chose, not `/tmp/intercore/locks/`
- The `owner.json` metadata file is written to that location
- A stale-lock `clean` operation could call `os.RemoveAll` on directories in the attacker-controlled location

This is a POSIX-level `/tmp` race (a classic sticky-bit attack) that is mitigated on Linux by the sticky bit (`/tmp` is `1777`), which prevents removing other users' directories, but does not prevent the symlink pre-staging attack described above because the symlink itself is created by the attacker, not the victim.

**On this specific server**, where `whoami` is `root` or `mk` and there are no other shell users with write access to `/tmp`, the practical exploitability is very low. However, the plan establishes a general-purpose lock manager and the bash helpers use the same `/tmp/intercore/locks` fallback path, so the attack surface applies in principle.

### Mitigation

The lock manager should verify that the resolved `BaseDir` is not a symlink before trusting it:

```go
func (m *Manager) ensureBaseDir() error {
    // Check whether /tmp/intercore exists as a symlink before creating locks
    if info, err := os.Lstat(m.BaseDir); err == nil {
        if info.Mode()&os.ModeSymlink != 0 {
            return fmt.Errorf("lock: base directory is a symlink: %s", m.BaseDir)
        }
    }
    return os.MkdirAll(m.BaseDir, 0700) // 0700, not 0755 — see Finding 2
}
```

Additionally, on Linux the lock base directory should be created as `/tmp/intercore-$UID/locks` where `$UID` is the running user's effective UID. This is the standard pattern used by `XDG_RUNTIME_DIR` and prevents the cross-user pre-staging attack entirely:

```go
const DefaultBaseDir = "/tmp/intercore-%d/locks" // formatted with os.Getuid()
```

With a UID-scoped directory, an attacker who is a different user cannot pre-create `/tmp/intercore-1000/` (owned by user 1000) without already having code execution as that user, at which point the attack model collapses.

---

## Finding 2: Lock Directory Permission 0755 Exposes Lock State to All Local Users (LOW-MEDIUM)

**Severity:** Low-Medium (depends on sensitivity of lock names and owner metadata)
**Exploitability:** Trivially readable by any user on the system
**Blast radius:** Information disclosure; lock state enumeration

### The Problem

The plan specifies `os.MkdirAll(m.BaseDir, 0755)` and `os.Mkdir(lockPath, 0755)`. With mode 0755, every user on the system can:

- `ls /tmp/intercore/locks/` — enumerate all active locks, their names, and scopes
- `cat /tmp/intercore/locks/sprint-abc123/owner.json` — read full owner metadata including PID, hostname, and session IDs

The `owner.json` structure in the plan is `{"pid": 12345, "host": "hostname", "session": "abc123", "created": 1708300000}`. The session ID is a Clavain/beads session token. Exposing it to any local user allows:

- Inference of what sprints and projects are in progress
- Session ID leakage (if session IDs are used for CSRF-like trust in any multi-agent protocol)
- PID leakage (which could assist in ptrace or signal attacks on the lock-holding process, though this requires privilege)

### Context

On this server, the primary user is `mk` or `root`, with `claude-user` as a secondary non-interactive user. The docs confirm there are no untrusted users. The practical risk is low. However, the `interlock` plugin (multi-agent file coordination) uses session IDs in its trust model, and `intermux` exposes agent activity. If a session ID from `owner.json` were ever used as a bearer token in any downstream protocol, 0755 permissions would matter.

### Mitigation

Change all `os.MkdirAll` and `os.Mkdir` calls in the lock manager to use mode `0700`:

```go
os.MkdirAll(m.BaseDir, 0700)
os.Mkdir(lockPath, 0700)
```

This restricts directory listing and file reading to the owner only. The lock acquire and release operations are still atomic under POSIX because `mkdir` atomicity is not affected by the permissions mode.

If `claude-user` (which runs as a different UID) needs to check or clean locks, the design should use the `ic lock` CLI (which runs as the same user via `sudo -u claude-user`) rather than direct filesystem reads.

---

## Finding 3: owner.json Metadata — Session ID and Hostname Leakage (LOW)

**Severity:** Low
**Exploitability:** Requires local filesystem read access (addressed by Finding 2 with 0700 mode)
**Blast radius:** Information disclosure of session tokens; no credential leakage

### The Problem

The `owner.json` format in the plan includes:

```json
{"pid": 12345, "host": "hostname", "session": "abc123", "created": 1708300000}
```

The `session` field appears to be a Clavain session identifier. The prior intercore security review confirmed no credentials are stored, and that remains true here — PIDs and hostnames are not secrets. However, the session ID deserves scrutiny.

In the bash wrapper, the owner string is constructed as:

```bash
--owner="$$:$(hostname -s 2>/dev/null || echo unknown)"
```

This uses `$$` (PID) and hostname, not a Clavain session token. The Go `Lock.Owner` field in the plan is labeled "PID:hostname or session ID", suggesting session IDs are optional. If the Clavain hooks pass a session ID as the owner (e.g., `$CLAUDE_SESSION_ID`), and that session ID is also used for sentinel scoping or dispatch authorization, then leaking it via a world-readable `owner.json` is a security concern.

### Assessment

The bash wrapper as written uses `$$:$(hostname -s)` — PID and hostname only. This is not sensitive. The concern is conditional on:
1. Mode 0755 being used (mitigated by Finding 2)
2. A future caller passing a session token as `--owner`

### Mitigation

- Apply 0700 permissions (Finding 2), making the question moot
- Add a comment in `owner.json` generation code and CLI help text clarifying that the `--owner` flag should not receive secret values
- Consider naming the field `label` instead of `owner` to discourage passing security-sensitive tokens

---

## Finding 4: Stale Lock Force-Removal — Missing Owner Verification on Clean (MEDIUM)

**Severity:** Medium
**Exploitability:** An attacker who can create directories under `/tmp/intercore/locks/` (before the sticky-bit issue is addressed) can cause legitimate locks to be forcibly removed
**Blast radius:** Premature lock release causes the protected critical section to execute concurrently, potentially corrupting sprint state in the beads database

### The Problem

The plan's `Clean(maxAge time.Duration)` method force-removes any lock directory older than `maxAge`. The `Release` method is described as "verify owner matches, rmdir" — but `Clean` has no such verification. It just removes stale lock directories by age.

The attack has two variants:

**Variant A — external directory injection:**
If an attacker can create `/tmp/intercore/locks/sprint-abc123/` before the legitimate process, the legitimate process's `Acquire` will find the directory already exists and spin-wait until stale timeout. After timeout, `Clean` will remove the attacker's directory, and the legitimate lock will be acquired. This is a denial-of-service against the lock, not a security bypass, because the directory must not contain a valid lock for the process to proceed.

**Variant B — timing the clean window:**
If an attacker can artificially age an active lock directory (using `touch -t`) and then trigger `ic lock clean`, the lock is removed while the holding process is still in its critical section. The next waiting process will then re-acquire the lock and enter the critical section concurrently with the still-running holder.

This matters because the critical section in `sprint_set_artifact` and `sprint_record_phase_completion` performs a read-modify-write on the beads JSON state. Concurrent execution of that section can corrupt `sprint_artifacts` or `phase_history`.

### Conditions for Variant B

- Attacker can modify the mtime of the lock directory (requires write access to `/tmp/intercore/locks/`)
- Attacker can invoke `ic lock clean` directly or trigger a hook that calls `intercore_lock_clean`

With 0755 permissions on the lock directory, the sticky bit protects the directory from deletion by other users, but they can still `touch` the directory's mtime if they have write permission (they don't under sticky bit, but if the directory has g+w, they could). With 0700 this is moot.

### The `Release` Ownership Check

The plan says `Release` verifies owner matches before `rmdir`. This is good but the verification has a TOCTOU window:

```
1. Read owner.json  [check passes]
2. [attacker replaces owner.json]
3. rmdir (removes legitimate lock held by different owner)
```

This race window is very narrow but exists. The verification should happen atomically or the owner.json should be renamed before removal rather than read-then-delete.

### Mitigation

1. **Scope the clean to the calling process's ownership.** `Clean` should by default only remove locks where `owner.json` PID matches a dead process:

```go
func (m *Manager) Clean(maxAge time.Duration) (int, error) {
    staleLocks, err := m.Stale(maxAge)
    // for each stale lock, read owner.json and check if PID is alive
    // only remove if kill(pid, 0) returns ESRCH (no such process)
    // always remove if owner.json is missing or unparseable
}
```

2. **The `--older-than=5s` default in the bash fallback is dangerously short.** A 5-second stale age is appropriate for a lock where the holder is doing a 100ms operation, but `ic lock clean --older-than=1s` in the integration test will delete locks held by any process that has been sleeping for 1 second. Change the clean default to `30s` or longer, and tie stale detection to PID liveness rather than age alone.

3. **Add PID-liveness check in the Go `Stale` function.** A lock is not truly stale if the owning PID is still alive, regardless of age. PID liveness check on Linux: `syscall.Kill(pid, 0)` returns `nil` if the process exists.

---

## Finding 5: Deployment Safety — Migration from Old Lock Paths (MEDIUM)

**Severity:** Medium
**Exploitability:** N/A — this is a deployment correctness issue, not an external attack
**Blast radius:** Double-acquisition of the same logical lock during the transition window causes the race condition that the entire feature is designed to prevent

### The Problem

The plan migrates 5 distinct lock patterns in `lib-sprint.sh` from inline `mkdir` patterns to `intercore_lock/intercore_unlock` wrappers. During the migration window (if any hook is sourcing the old `lib-sprint.sh` while another has been updated), two sprint operations can hold locks with different path conventions simultaneously:

- Old: `/tmp/sprint-lock-${sprint_id}` (via direct `mkdir`)
- New: `/tmp/intercore/locks/sprint-${sprint_id}/` (via `intercore_lock`)

Both the old and new locks would succeed independently because they use different directories. The mutual exclusion guarantee is lost until all callers are updated atomically.

### Rollout Risk

The plan's Task 3 migrates all 5 patterns in a single commit to `lib-sprint.sh`. This is the right approach — atomic file-level migration. However:

1. `lib-sprint.sh` is sourced at hook invocation time, not at session start. If one Claude Code session has an active hook running the old version while git pulls the new version, the old and new patterns will coexist during that hook's execution.

2. The fallback in `intercore_lock` (when `ic` is unavailable) falls back to the new `/tmp/intercore/locks/` path directly via `mkdir`. The old fallback in `lib-sprint.sh` uses `/tmp/sprint-lock-*`. These are different paths. A process using the old code and a process using the new fallback code will not see each other's locks.

### Dependency Ordering

The plan's dependency chain is:
- Task 0: Go lock package
- Task 1: CLI commands
- Task 2: Bash wrappers in `lib-intercore.sh`
- Task 3: Migration of `lib-sprint.sh`

Tasks 0-2 must be deployed (binary rebuilt and accessible as `ic`) before Task 3 is committed. If Task 3 is committed before `ic lock` is built and installed, `intercore_available` will return 0 (binary not found) and the bash wrapper falls back to the new lock path — but `intercore_available` first checks `ic health`, which requires a DB. If the DB is healthy but `ic lock` subcommand doesn't exist yet (old binary), `ic lock acquire` will exit non-zero and the fallback triggers.

**Specific gap:** `intercore_available` calls `ic health`. The plan explicitly states lock commands don't need a DB. But the bash wrapper calls `intercore_available` before deciding to use `ic lock`. This means if the DB is unhealthy or uninitialized, the lock wrapper falls back to the filesystem path — but uses the NEW path (`/tmp/intercore/locks/`), not the old `/tmp/sprint-lock-*` path. Any concurrent process still using the old path won't see the fallback's lock.

### Mitigation

**Pre-deploy checklist (must be satisfied before Task 3 is merged):**

1. Build and install the new `ic` binary: `go build -o ic ./cmd/ic && sudo mv ic /usr/local/bin/ic`
2. Verify `ic lock acquire test global && ic lock release test global` exits 0
3. Verify `ic health` exits 0 (ensures `intercore_available` will return true for the lock wrapper)
4. Run `bash test-integration.sh` and confirm all lock tests pass

**Rollback plan:**

Task 3 migration is reversible — git revert of `lib-sprint.sh` restores all inline `mkdir` patterns. The new lock directories in `/tmp/intercore/locks/` will be orphaned but harmless (they are in `/tmp` and will be cleaned on reboot). There is no irreversible data change.

**Transition window mitigation:**

Add a check in `lib-sprint.sh` sourcing to detect the old lock path and clean it up during the transition:

```bash
# Migration: clean up old-style sprint lock dirs if new lock manager is active
if intercore_available; then
    rm -rf /tmp/sprint-lock-* /tmp/sprint-claim-lock-* /tmp/sprint-advance-lock-* \
        /tmp/checkpoint-lock-* 2>/dev/null || true
fi
```

This should run once at source time, inside the `_SPRINT_LOADED` guard, to eliminate any stale old-format locks at the moment the new lock manager takes over.

---

## Finding 6: intercore_available DB Health Check Blocks Lock Subsystem When DB is Broken (LOW-MEDIUM)

**Severity:** Low-Medium
**Exploitability:** Not an external attack; a self-inflicted availability problem
**Blast radius:** If the intercore DB is corrupt or uninitialized, ALL lock operations fall back to direct filesystem mkdir, silently dropping the owner metadata and stale-breaking logic

### The Problem

The plan states: "The lock commands do NOT require a database connection." The Go `ic lock` subcommands operate purely on the filesystem. However, the bash wrapper `intercore_lock` calls `intercore_available` before deciding to use `ic lock`. `intercore_available` calls `ic health`, which checks DB readiness.

If the DB is broken:
1. `intercore_available` returns 1
2. `intercore_lock` falls back to direct `mkdir`
3. The new `/tmp/intercore/locks/` path is used for the fallback
4. No `owner.json` is written
5. Stale detection relies only on `mtime` from the parent process call, not the Go lock manager's logic

This defeats the primary benefit of the new lock manager (structured ownership metadata and PID-liveness-based stale detection) in the most likely failure scenario — a freshly set-up machine where `ic init` hasn't been run yet.

### Mitigation

The bash wrapper should bypass `intercore_available` for lock operations and instead check only for binary existence:

```bash
intercore_lock() {
    local name="$1" scope="$2" timeout="${3:-1s}"
    # Lock commands don't need a healthy DB — check binary existence only
    local bin
    bin=$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)
    if [[ -n "$bin" ]]; then
        "$bin" lock acquire "$name" "$scope" --timeout="$timeout" \
            --owner="$$:$(hostname -s 2>/dev/null || echo unknown)" >/dev/null
        return $?
    fi
    # Fallback: direct mkdir when ic binary is not installed at all
    ...
}
```

Alternatively, add a new `intercore_lock_available` check that only tests binary existence, not DB health:

```bash
intercore_lock_available() {
    [[ -n "$(command -v ic 2>/dev/null || command -v intercore 2>/dev/null)" ]]
}
```

---

## Finding 7: Bash Fallback find Command in intercore_lock_clean — Unconstrained rmdir (LOW)

**Severity:** Low
**Exploitability:** Requires intentional misuse; not externally triggerable without shell access
**Blast radius:** `rmdir` on non-empty directories silently fails, so the safety floor is present; but `find` with `-exec rmdir` visits all subdirectories

### The Problem

The `intercore_lock_clean` fallback in the plan uses:

```bash
find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d -mmin +1 -exec rmdir {} \; 2>/dev/null || true
```

Issues:
1. `-mmin +1` means "older than 1 minute" — this is significantly more aggressive than the 5-second stale timeout used by the Go lock manager and the inline bash patterns. A 1-minute-old lock is likely still held by a valid operation that stalled (e.g., a slow `bd set-state` call). The fallback clean is more aggressive than the primary clean.

2. `find /tmp/intercore/locks` will fail with an error if the directory doesn't exist yet. The `2>/dev/null || true` suppresses this, but the behavior is silent. The directory might not exist because `intercore_lock` was never called (no session has run yet), or because `ic` created it under a different path. Silent suppression hides misconfiguration.

3. This contradicts the plan note that `intercore_lock_clean` accepts a `max_age` parameter. The fallback hardcodes `-mmin +1` and ignores the `$max_age` parameter entirely.

### Mitigation

Fix the fallback to honor the `max_age` parameter and convert it to minutes for `find`:

```bash
intercore_lock_clean() {
    local max_age="${1:-5s}"
    if intercore_available; then
        "$INTERCORE_BIN" lock clean --older-than="$max_age" >/dev/null 2>&1 || true
        return 0
    fi
    # Fallback: convert max_age to minutes for find
    local age_minutes=1  # default: 1 minute as a conservative floor
    if [[ "$max_age" =~ ^([0-9]+)s$ ]]; then
        # Round up seconds to minutes
        age_minutes=$(( (${BASH_REMATCH[1]} + 59) / 60 ))
        [[ $age_minutes -lt 1 ]] && age_minutes=1
    fi
    [[ -d /tmp/intercore/locks ]] || return 0
    find /tmp/intercore/locks -mindepth 1 -maxdepth 1 -type d -mmin "+${age_minutes}" \
        -exec rmdir {} \; 2>/dev/null || true
}
```

---

## Finding 8: Integration Test Stale Lock Detection Uses touch -t (LOW — Test Quality)

**Severity:** Low (test infrastructure only)
**Exploitability:** N/A
**Blast radius:** Tests may pass on platforms where `touch -t` is not supported or where mtime precision is low

### The Problem

The integration test in Task 4 uses:

```bash
touch -t 202601010000 /tmp/intercore/locks/staletest-global
```

`touch -t` requires a specific timestamp format and is not POSIX-guaranteed to modify the directory's mtime in a way that the Go `os.Stat(lockPath).ModTime()` comparison will respect. On some Linux filesystems with coarse mtime resolution, this may produce false passes.

More importantly, the test modifies the mtime of the lock directory *externally* rather than testing the Go `Stale` function's actual logic. If the Go implementation uses `atime` instead of `mtime`, the test would pass even if the logic is wrong.

### Mitigation

The Go unit test `TestStaleBreaking` is the right place to test this behavior. The integration test should instead test the end-to-end behavior by waiting for the actual stale timeout (with a very short `--older-than=200ms` for test speed), or by using a dedicated test helper that sets the lock's creation time via the `owner.json` `created` field:

```bash
# Better: test with a short stale window and real time passage
ic lock acquire staletest global
sleep 0.3
ic lock stale --older-than=200ms | grep -q "staletest"
```

This requires the `--older-than` flag to accept milliseconds, which should be added to the Go implementation.

---

## Deployment Readiness Assessment

### Go/No-Go Summary

| Issue | Severity | Block ship? | Effort |
|---|---|---|---|
| /tmp symlink pre-staging attack | Medium | No (low practical risk on this server) | 1 hour (UID-scoped dir + symlink check) |
| 0755 permissions expose lock state | Low-Medium | No (no untrusted users on this server) | 5 min (change to 0700) |
| owner.json session ID leakage | Low | No | 5 min (document, fix via Finding 2) |
| Clean has no PID-liveness check | Medium | Recommend fix before deploy | 2 hours (add kill(0) check) |
| Deployment transition window | Medium | Yes — pre-deploy checklist required | Documentation only |
| intercore_available blocks lock subsystem | Low-Medium | No | 30 min (split check function) |
| find fallback ignores max_age parameter | Low | No | 15 min |
| Integration test uses touch -t | Low | No | 30 min |

### Pre-Deploy Checklist

All of these must pass before Task 3 (lib-sprint.sh migration) is committed:

1. `go build -o ic ./cmd/ic && ./ic lock acquire test global --timeout=1s && ./ic lock release test global` exits 0
2. `bash test-integration.sh` passes all lock tests (including the new ones from Task 4)
3. `ic health` exits 0 (confirms `intercore_available` will not fall back)
4. No running sprint hooks hold `/tmp/sprint-lock-*` locks at migration time (check with `ls /tmp/sprint-lock-* 2>/dev/null`)
5. Install new binary before merging Task 3: `sudo cp ic /usr/local/bin/ic`

### Rollback Plan

The migration is fully reversible:
- `git revert` of `lib-sprint.sh` restores all inline `mkdir` patterns
- Old `/tmp/sprint-lock-*` paths work regardless of the new lock manager
- New `/tmp/intercore/locks/` directories in `/tmp` are harmless and cleaned on reboot
- No DB schema changes — the lock manager is filesystem-only

**Irreversible operations:** None. This is a pure filesystem-level change with no database migration.

### Post-Deploy Verification

After deploying Task 3:

1. Trigger a sprint operation: `sprint_set_artifact` should call `intercore_lock sprint <id>` instead of `mkdir /tmp/sprint-lock-<id>`
2. Verify lock directory appears: `ls /tmp/intercore/locks/` should show `sprint-<id>/` with `owner.json` inside
3. Verify old path not used: `ls /tmp/sprint-lock-* 2>/dev/null` should return empty
4. Run `ic lock list` and confirm active locks show correct owner metadata

---

## Prioritized Mitigations

### Implement Before Merging Task 3

1. **Change 0755 to 0700** in all `os.MkdirAll` and `os.Mkdir` calls in the lock manager. 5-minute change. Eliminates Finding 2 and makes Finding 3 moot.

2. **Add PID-liveness check to `Clean` and `Stale`** (Finding 4). Before force-removing a lock, check `syscall.Kill(pid, 0)` returns `ESRCH`. This prevents clean from evicting locks held by running processes regardless of their age. If `owner.json` is missing or the PID field is unparseable, treat as dead and remove.

3. **Fix the bash fallback to honor the `max_age` parameter** in `intercore_lock_clean` (Finding 7). 15-minute fix.

4. **Add the transition-window cleanup** to `lib-sprint.sh` (Finding 5). One-liner `rm -rf /tmp/sprint-lock-*` inside the `_SPRINT_LOADED` guard when `intercore_available` returns true.

### Implement Soon After

5. **Split `intercore_available` for lock commands** (Finding 6). Add `intercore_lock_available` that only checks binary existence, not DB health. This ensures the full lock manager (with `owner.json`) is used even when the DB is unhealthy.

6. **Use UID-scoped base directory** (Finding 1). Change `DefaultBaseDir` to `/tmp/intercore-%d/locks` formatted with `os.Getuid()`. Eliminates the cross-user `/tmp` pre-staging attack entirely.

### Document as Known Residual Risk

7. The TOCTOU window in `Release` between reading `owner.json` and calling `rmdir` is not exploitable in practice (single-user server, narrow race). Document it as a known limitation.

8. The `command -v ic` PATH injection risk (inherited from the prior review, Finding 5 of `review-intercore-security.md`) applies equally to `ic lock`. The mitigation (search fixed paths first) remains low priority given the threat model.

---

## Summary of Confirmed Safe Aspects

The following concerns from the task prompt have been evaluated and are acceptable as designed:

- **Symlink attack on individual lock directories:** With 0700 on the base dir (recommended), other users cannot create directories within it. The sticky bit on `/tmp` prevents deletion, but not creation in a 0755 base dir — hence the 0700 recommendation.
- **owner.json information content:** PID and hostname are not secrets. The bash wrapper uses `$$:hostname`, not session tokens. No credentials are present.
- **Stale lock force-removal by the owning process:** The `Clean` function called from `intercore_lock_clean` is only invoked from the Clavain hook infrastructure itself, not by external callers. The primary risk is the missing PID-liveness check (Finding 4), not external manipulation.

---

**Review complete:** 2026-02-18
**Reviewer note:** The plan is well-structured and the three-layer design (Go package, CLI, bash wrappers) is architecturally sound. The most actionable improvements are the 0700 permission fix and the PID-liveness check for stale cleaning — both are low-effort and materially reduce risk.
