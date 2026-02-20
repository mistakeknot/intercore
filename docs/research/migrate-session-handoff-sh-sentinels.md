# Migration Analysis: session-handoff.sh to intercore sentinels

**Date:** 2026-02-17
**File:** `hub/clavain/hooks/session-handoff.sh`
**Status:** Applied

## Summary

Migrated `session-handoff.sh` from raw temp-file sentinel checks to `intercore_check_or_die` wrappers from `lib-intercore.sh`. The wrapper tries `ic sentinel check` (SQLite-backed, atomic) and falls back to the legacy temp-file pattern when `ic` is unavailable, preserving fail-open semantics.

## Changes Made

### 1. Source lib-intercore.sh (line 18-19)

```bash
# shellcheck source=hooks/lib-intercore.sh
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

Added immediately after `set -euo pipefail`. The `|| true` ensures the hook continues even if the library is missing — `intercore_check_or_die` has an inline fallback for this case.

### 2. Stop sentinel (lines 37-41, was lines 34-40)

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
intercore_check_or_die "$INTERCORE_STOP_DEDUP_SENTINEL" "$SESSION_ID" 0 "/tmp/clavain-stop-${SESSION_ID}"
```

Uses the shared `INTERCORE_STOP_DEDUP_SENTINEL` variable (value: `"stop"`) defined in `lib-intercore.sh`. This is the cross-hook anti-cascade sentinel — all Stop hooks share the same sentinel name so only one fires per stop cycle. The interval `0` means once-per-scope (session).

### 3. Handoff sentinel (lines 43-44, was lines 42-46 + line 86)

**Before:**
```bash
SENTINEL="/tmp/clavain-handoff-${SESSION_ID}"
if [[ -f "$SENTINEL" ]]; then
    exit 0
fi
# ... later at line 86:
touch "$SENTINEL"
```

**After:**
```bash
intercore_check_or_die "handoff" "$SESSION_ID" 0 "/tmp/clavain-handoff-${SESSION_ID}"
```

The `intercore_check_or_die` wrapper writes the sentinel atomically at check time (both in the DB path and the legacy `touch` fallback path), so the separate `touch "$SENTINEL"` on old line 86 is no longer needed and was removed. This also closes the TOCTOU window that existed in the original code between the file check and the `touch` 40+ lines later.

### 4. Stale cleanup (lines 134-139, was line 140)

**Before:**
```bash
find /tmp -maxdepth 1 -name 'clavain-stop-*' -mmin +60 -delete 2>/dev/null || true
```

**After:**
```bash
if type intercore_cleanup_stale &>/dev/null; then
    intercore_cleanup_stale
else
    find /tmp -maxdepth 1 -name 'clavain-stop-*' -mmin +60 -delete 2>/dev/null || true
fi
```

`intercore_cleanup_stale` calls `ic sentinel prune --older-than=1h` when available, which cleans up all stale sentinels (not just `clavain-stop-*` but also `clavain-drift-last-*` and `clavain-compound-last-*`). Falls back to the original `find` command when the wrapper is unavailable.

## Behavioral Analysis

### Fail-open preserved

Every path is fail-open:
- `source lib-intercore.sh` fails → `|| true` → `intercore_check_or_die` uses inline fallback
- `ic` binary not found → `intercore_available` returns 1 → legacy temp file path
- `ic` DB broken → `intercore_available` returns 1 → legacy temp file path
- `ic sentinel check` returns exit 2+ (DB error) → falls through to legacy temp file
- Legacy temp file path = same behavior as the original code

### TOCTOU improvement

The original code had a significant TOCTOU gap for the handoff sentinel: it checked the file at line 44 but didn't `touch` it until line 86 (after signal analysis). If signal analysis took time, a concurrent hook invocation could slip through. The new `intercore_check_or_die` writes the sentinel atomically at check time, closing this window.

The stop sentinel was already tight (check then immediate touch), but now benefits from SQLite's atomic upsert when `ic` is available.

### No functional changes to signal detection or prompt

Lines 46-132 (signal detection, handoff path construction, prompt generation, JSON output) are completely untouched. The migration only affects sentinel management and cleanup.

## Verification

```bash
bash -n hub/clavain/hooks/session-handoff.sh  # SYNTAX OK
```

## Risk Assessment

**Low risk.** The wrapper functions have inline fallbacks that replicate the exact original behavior. The only net change in behavior is:
1. When `ic` is available: sentinels use SQLite (more robust, atomic, no temp file clutter)
2. When `ic` is unavailable: identical to the original code
3. Handoff sentinel TOCTOU window closed (improvement)
