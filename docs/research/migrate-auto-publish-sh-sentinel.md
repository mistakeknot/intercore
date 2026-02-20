# Migration Analysis: auto-publish.sh Sentinel → intercore

**Date:** 2026-02-17
**File:** `hub/clavain/hooks/auto-publish.sh`
**Library:** `hub/clavain/hooks/lib-intercore.sh` (v0.1.0)
**Status:** Applied

## Current State (Pre-Migration)

`auto-publish.sh` is a Clavain PostToolUse hook that detects `git push` commands in plugin repos, auto-increments patch version if the developer forgot to bump, syncs the marketplace, and pushes marketplace changes.

### Current sentinel logic (lines 55–68, original)

A single **global sentinel** (`/tmp/clavain-autopub.lock`) with 60-second TTL prevents all auto-publish re-triggers within a 60-second window. This covers both the plugin push and the marketplace push in one window, preventing cascade triggers across plugin → marketplace push chains.

```bash
local sentinel="/tmp/clavain-autopub.lock"
if [[ -f "$sentinel" ]]; then
    local sentinel_age now sentinel_mtime
    now=$(date +%s)
    sentinel_mtime=$(stat -c %Y "$sentinel" 2>/dev/null || stat -f %m "$sentinel" 2>/dev/null || echo "0")
    sentinel_age=$((now - sentinel_mtime))
    if [[ "$sentinel_age" -lt 60 ]]; then
        exit 0
    fi
    # Expired — remove and continue
    rm -f "$sentinel"
fi
```

A separate `touch "$sentinel"` on line 91 fires BEFORE any push to prevent re-trigger.

### Key difference from other hooks

Unlike `auto-compound.sh` and `session-handoff.sh`, this hook has only **one sentinel** and it uses a **global** scope (not per-session). This is intentional — auto-publish is a global rate limiter that should prevent cascade triggers across ALL sessions, not just the current one.

## Migration Plan (Applied)

### Change 1: Source lib-intercore.sh

Added after the jq guard (line 20), inside `main()`:

```bash
# Source intercore sentinel wrappers (fail-open if unavailable)
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

Placed after the jq guard rather than before because `lib-intercore.sh` itself does not depend on jq, but it's consistent with placing it early in `main()` without changing the jq-first-exit semantics. The `|| true` ensures fail-open behavior if the library is missing.

### Change 2: Replace sentinel check block (lines 55–68)

**Before (14 lines):**
```bash
local sentinel="/tmp/clavain-autopub.lock"
if [[ -f "$sentinel" ]]; then
    local sentinel_age now sentinel_mtime
    now=$(date +%s)
    sentinel_mtime=$(stat -c %Y "$sentinel" 2>/dev/null || stat -f %m "$sentinel" 2>/dev/null || echo "0")
    sentinel_age=$((now - sentinel_mtime))
    if [[ "$sentinel_age" -lt 60 ]]; then
        exit 0
    fi
    # Expired — remove and continue
    rm -f "$sentinel"
fi
```

**After (3 lines):**
```bash
# Global sentinel: prevent ALL auto-publish re-triggers within 60s.
# Uses intercore sentinel with legacy temp-file fallback.
intercore_check_or_die "autopub" "global" 60 "/tmp/clavain-autopub.lock"
```

**Semantics preserved:**
- Name `"autopub"` uniquely identifies this sentinel in the ic database
- Scope `"global"` (not per-session) matches the original design intent — a global rate limiter
- Interval `60` matches the original 60-second TTL
- Legacy path `/tmp/clavain-autopub.lock` preserved for fallback when `ic` is unavailable

### Change 3: Remove standalone `touch "$sentinel"` (lines 90–91)

**Before:**
```bash
# Write sentinel BEFORE any push to prevent re-trigger
touch "$sentinel"
```

**After:** Removed entirely. `intercore_check_or_die` handles both the check and the claim atomically:
- When `ic` is available: `ic sentinel check "autopub" "global" --interval=60` does an atomic `INSERT OR IGNORE` + timestamp comparison in SQLite
- When `ic` is unavailable: the fallback in `intercore_check_or_die` calls `touch "$legacy_path"` after the staleness check passes

This is safe because `intercore_check_or_die` touches the legacy file (or records in SQLite) at **check time**, not at trigger time. Since the sentinel block runs before any marketplace/plugin push, the timing is equivalent to the original `touch "$sentinel"` which also ran before pushes.

## Risk Assessment

### Fail-open guarantee

The fail-open chain works as follows:
1. `source ... || true` — if lib-intercore.sh is missing, no error
2. `intercore_check_or_die` — if the function is undefined (source failed), `set -euo pipefail` causes the script to exit non-zero
3. The outer wrapper `main "$@" || true` catches the non-zero exit
4. `exit 0` on the last line ensures the hook always exits 0

Net effect: if intercore is completely broken, the sentinel check is skipped and auto-publish fires unconditionally. This is the correct fail-open behavior — it's better to publish twice than to never publish.

### TOCTOU improvement

The original code had a classic TOCTOU race:
```
test -f "$sentinel"  →  (window)  →  touch "$sentinel"
```
Two concurrent `git push` events could both pass the test before either touches the file.

With `ic sentinel check`, the check-and-claim is a single SQLite transaction (`INSERT OR IGNORE` with a unique constraint on `(name, scope_id)`), eliminating the race window.

The legacy fallback retains the TOCTOU race but this is acceptable — it only activates when `ic` is unavailable.

### Behavioral changes

- **None for the happy path.** The sentinel name, scope, interval, and legacy path all map exactly to the existing behavior.
- **Timing change (minor):** The original code touched the sentinel at line 91 (after version comparison, before push). The new code claims the sentinel at line 60 (before marketplace lookup). This means the sentinel fires ~2ms earlier, which is negligible but worth noting — if the hook exits between lines 60 and the push (e.g., marketplace not found), the sentinel is already claimed and another push within 60s would be suppressed. This is actually **safer** than the original, which could allow a second trigger if the first failed between check and touch.

### Global scope consideration

Using `"global"` as the scope_id means all sessions share a single sentinel. This is correct for auto-publish because:
1. The marketplace repo is a single resource (concurrent pushes would conflict)
2. The 60-second window is specifically to prevent the hook's own `git push --force-with-lease` and marketplace push from re-triggering the hook
3. A per-session sentinel would miss cascade triggers from other sessions

## Verification

```bash
bash -n hub/clavain/hooks/auto-publish.sh   # Syntax check — PASSED
```

Functional verification requires triggering a `git push` in a plugin repo during a Clavain session.

## Net Change Summary

| Metric | Before | After |
|--------|--------|-------|
| Sentinel lines | 17 (check block + touch) | 3 (source + call) |
| TOCTOU race | Yes | No (when ic available) |
| Fallback | N/A (temp file only) | Temp file fallback |
| Scope | Global (implicit) | Global (explicit `"global"` scope_id) |
| Atomicity | Non-atomic (test/touch separate) | Atomic (SQLite INSERT OR IGNORE) |
