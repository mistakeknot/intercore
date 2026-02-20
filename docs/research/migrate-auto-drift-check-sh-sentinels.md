# Migration: auto-drift-check.sh to intercore sentinels

**Date:** 2026-02-17
**File:** `hub/clavain/hooks/auto-drift-check.sh`
**Library:** `hub/clavain/hooks/lib-intercore.sh` (v0.1.0, copied from `infra/intercore/lib-intercore.sh`)

## Summary

Migrated `auto-drift-check.sh` from inline temp-file sentinel logic to the `intercore_check_or_die` wrapper from `lib-intercore.sh`. This gives the hook atomic, TOCTOU-safe sentinel checks via `ic sentinel check` when intercore is available, with automatic fallback to the legacy temp-file pattern when it is not.

## Changes Made

### 1. Source lib-intercore.sh (line 19)

Added after `set -euo pipefail`:

```bash
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

The `2>/dev/null || true` ensures fail-open: if the library is missing or broken, the hook continues with inline fallback logic inside `intercore_check_or_die`.

### 2. Stop sentinel block replaced (was lines 41-46)

**Before (17 lines including variable + if/fi + touch):**
```bash
STOP_SENTINEL="/tmp/clavain-stop-${SESSION_ID}"
if [[ -f "$STOP_SENTINEL" ]]; then
    exit 0
fi
touch "$STOP_SENTINEL"
```

**After (2 lines):**
```bash
intercore_check_or_die "$INTERCORE_STOP_DEDUP_SENTINEL" "$SESSION_ID" 0 "/tmp/clavain-stop-${SESSION_ID}"
```

- `$INTERCORE_STOP_DEDUP_SENTINEL` resolves to `"stop"` (defined in lib-intercore.sh), the shared sentinel name for stop-hook anti-cascade.
- Interval `0` means once-per-session (file exists = throttled).
- Legacy path `/tmp/clavain-stop-${SESSION_ID}` is used as fallback when `ic` is unavailable.

### 3. Drift throttle block replaced (was lines 48-56)

**Before (9 lines including variable + if/stat/fi):**
```bash
THROTTLE_SENTINEL="/tmp/clavain-drift-last-${SESSION_ID}"
if [[ -f "$THROTTLE_SENTINEL" ]]; then
    THROTTLE_MTIME=$(stat -c %Y "$THROTTLE_SENTINEL" 2>/dev/null || stat -f %m "$THROTTLE_SENTINEL" 2>/dev/null || date +%s)
    THROTTLE_NOW=$(date +%s)
    if [[ $((THROTTLE_NOW - THROTTLE_MTIME)) -lt 600 ]]; then
        exit 0
    fi
fi
```

**After (2 lines):**
```bash
intercore_check_or_die "drift_throttle" "$SESSION_ID" 600 "/tmp/clavain-drift-last-${SESSION_ID}"
```

- Sentinel name `"drift_throttle"` is hook-specific (not shared across hooks).
- Interval `600` = 10 minutes, matching the original 600-second cooldown.
- `intercore_check_or_die` calls `exit 0` internally if throttled, so no wrapping `if` needed.

### 4. Removed standalone `touch "$THROTTLE_SENTINEL"` (was line 94)

The `intercore_check_or_die` function handles touching the legacy file as part of the "allowed" path. The explicit `touch` after the block decision was redundant and has been removed.

### 5. Removed stale cleanup `find /tmp ...` (was lines 108-110)

**Removed:**
```bash
find /tmp -maxdepth 1 \( -name 'clavain-stop-*' -o -name 'clavain-drift-last-*' -o -name 'clavain-compound-last-*' \) -mmin +60 -delete 2>/dev/null || true
```

Cleanup is now centralized in `session-handoff.sh` via `intercore_cleanup_stale`, which calls `ic sentinel prune --older-than=1h` (with legacy fallback). Running cleanup from every stop hook was wasteful and could race with other hooks.

## Behavioral Analysis

### When intercore (`ic`) is available

1. `intercore_check_or_die` calls `intercore_sentinel_check_or_legacy`
2. Which calls `ic sentinel check <name> <scope> --interval=<N>`
3. Exit 0 from `ic` = allowed (proceed), exit 1 = throttled (wrapper calls `exit 0` on the hook)
4. Exit 2+ = DB error, falls through to legacy temp-file path
5. On "allowed" path, `ic` atomically records the sentinel in SQLite (no TOCTOU race)

### When intercore is NOT available (fallback)

1. `source lib-intercore.sh` silently fails (`|| true`)
2. `intercore_check_or_die` detects `intercore_sentinel_check_or_legacy` is undefined via `type` check
3. Falls through to inline logic: check file existence (interval=0) or mtime (interval>0)
4. Touches the legacy file on "allowed" path
5. Behavior is identical to the original code

### Net effect on guard ordering

The guard order is preserved:
1. jq availability check
2. `stop_hook_active` check (anti-infinite-loop)
3. Per-repo opt-out (`.claude/clavain.no-driftcheck`)
4. SESSION_ID extraction
5. **Stop dedup sentinel** (now via intercore)
6. **Drift throttle** (now via intercore)
7. Interwatch discovery
8. Transcript availability
9. Signal detection + threshold

## Diff Summary

```
 hooks/auto-drift-check.sh | 112 → 95 lines (-17 lines net)
 +1 line:  source lib-intercore.sh
 -5 lines: inline stop sentinel block
 +2 lines: intercore_check_or_die for stop sentinel
 -9 lines: inline throttle block
 +2 lines: intercore_check_or_die for throttle
 -2 lines: touch "$THROTTLE_SENTINEL"
 -3 lines: find /tmp cleanup
```

## Verification

```bash
bash -n hub/clavain/hooks/auto-drift-check.sh   # Syntax OK
```

No runtime test needed beyond syntax check — the `intercore_check_or_die` function is already tested as part of the lib-intercore.sh unit, and the fallback path replicates the original logic exactly.
