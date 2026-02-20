# Migration Analysis: auto-compound.sh Sentinel → intercore

**Date:** 2026-02-17
**File:** `hub/clavain/hooks/auto-compound.sh`
**Library:** `hub/clavain/hooks/lib-intercore.sh` (v0.1.0)

## Current State

`auto-compound.sh` is a Clavain stop hook that detects compoundable signals (git commits, debugging resolutions, investigation language, bead closures, etc.) and triggers Claude to run `/clavain:compound` when total signal weight >= 3.

### Current sentinel logic (lines 48–63)

1. **Stop dedup sentinel** (`/tmp/clavain-stop-${SESSION_ID}`): If file exists, exit. Otherwise, `touch` it immediately — prevents other stop hooks from cascading.
2. **Compound throttle** (`/tmp/clavain-compound-last-${SESSION_ID}`): `stat` the file's mtime, compare to current time. If < 300 seconds have passed, exit. Refreshed with `touch` after a successful trigger (line 94).

### Current cleanup (line 107)

A `find /tmp` line deletes stale sentinels (>1 hour) from `/tmp`. This runs on every successful trigger, which is redundant since `session-handoff.sh` now owns cleanup via `intercore_cleanup_stale`.

## Migration Plan

### Change 1: Source lib-intercore.sh

Add after `set -euo pipefail` (line 24):

```bash
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

The `|| true` ensures fail-open behavior if the library is missing. `${BASH_SOURCE[0]%/*}` resolves to the hooks directory without requiring a `cd` or subshell.

### Change 2: Replace stop sentinel block (lines 48–53)

**Before:**
```bash
STOP_SENTINEL="/tmp/clavain-stop-${SESSION_ID}"
if [[ -f "$STOP_SENTINEL" ]]; then
    exit 0
fi
touch "$STOP_SENTINEL"
```

**After:**
```bash
# Claim stop sentinel FIRST (before throttle check) to prevent other hooks
# from analyzing the transcript, even if this hook exits due to throttle.
intercore_check_or_die "$INTERCORE_STOP_DEDUP_SENTINEL" "$SESSION_ID" 0 "/tmp/clavain-stop-${SESSION_ID}"
```

**Semantics preserved:** `intercore_check_or_die` with interval=0 means "once per session" — if already claimed, exits the script (`exit 0`). If unclaimed, claims it (via `ic sentinel check` or `touch` fallback) and returns 0 to continue. The legacy file path `/tmp/clavain-stop-${SESSION_ID}` is kept for fallback if `ic` is unavailable.

The `$INTERCORE_STOP_DEDUP_SENTINEL` variable is defined in `lib-intercore.sh` as `"stop"` — this is the shared sentinel name used by all stop hooks for anti-cascade.

### Change 3: Replace compound throttle block (lines 56–63)

**Before:**
```bash
THROTTLE_SENTINEL="/tmp/clavain-compound-last-${SESSION_ID}"
if [[ -f "$THROTTLE_SENTINEL" ]]; then
    THROTTLE_MTIME=$(stat -c %Y "$THROTTLE_SENTINEL" 2>/dev/null || stat -f %m "$THROTTLE_SENTINEL" 2>/dev/null || date +%s)
    THROTTLE_NOW=$(date +%s)
    if [[ $((THROTTLE_NOW - THROTTLE_MTIME)) -lt 300 ]]; then
        exit 0
    fi
fi
```

**After:**
```bash
# THEN check throttle (5-min cooldown specific to this hook)
intercore_check_or_die "compound_throttle" "$SESSION_ID" 300 "/tmp/clavain-compound-last-${SESSION_ID}"
```

**Semantics preserved:** `intercore_check_or_die` with interval=300 means "at most once per 5 minutes." The `ic sentinel check` command atomically checks and claims. Fallback uses the same `stat -c %Y` / `stat -f %m` pattern.

### Change 4: Remove standalone `touch "$THROTTLE_SENTINEL"` (line 94)

**Before (line 94):**
```bash
touch "$THROTTLE_SENTINEL"
```

**After:** Removed entirely. The `intercore_check_or_die` call already touches the legacy file and/or records the sentinel in the ic database when it allows passage. There is no need for a second touch at trigger time.

Note: The `$THROTTLE_SENTINEL` variable is no longer defined after the migration, so this line would be a bug if left in place.

### Change 5: Remove stale cleanup `find /tmp` line (line 107)

**Before:**
```bash
find /tmp -maxdepth 1 \( -name 'clavain-stop-*' -o -name 'clavain-drift-last-*' -o -name 'clavain-compound-last-*' \) -mmin +60 -delete 2>/dev/null || true
```

**After:** Removed entirely. Cleanup is now centralized in `session-handoff.sh` via `intercore_cleanup_stale`, which calls `ic sentinel prune --older-than=1h` (with the same `find /tmp` as fallback).

## Risk Assessment

### Fail-open guarantee
- If `lib-intercore.sh` fails to source (`|| true` swallows the error), then `intercore_check_or_die` is undefined. Under `set -euo pipefail`, calling an undefined function causes the script to exit with non-zero status.
- Hook contract: exit 0 = no block, non-zero exit = also no block (Claude Code treats any non-JSON/non-zero as "proceed"). So fail-open is preserved either way.
- The `source ... || true` means sourcing failure won't exit the script; the undefined function call later will, but with fail-open semantics.

### TOCTOU improvement
- `ic sentinel check` uses SQLite with `INSERT OR IGNORE` + timestamp comparison, which is atomic. This eliminates the TOCTOU race between `test -f` and `touch` in the legacy path.
- Legacy fallback still has the TOCTOU race but that is acceptable for backward compatibility.

### Behavioral changes
- **None for the happy path.** The sentinel names (`stop`, `compound_throttle`) and intervals (0, 300) map exactly to the existing behavior.
- **Minor improvement:** If `ic` is available, the stop sentinel is now stored in SQLite rather than `/tmp`, making it survive `/tmp` cleanup by other processes (unlikely but possible).

## Verification

After making changes, run:
```bash
bash -n hub/clavain/hooks/auto-compound.sh
```

This checks syntax only. Functional verification requires a Clavain session with signal weight >= 3.
