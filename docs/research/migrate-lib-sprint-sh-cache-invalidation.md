# Migration Analysis: lib-sprint.sh Cache Invalidation → intercore

**Date:** 2026-02-17
**File:** `hub/clavain/hooks/lib-sprint.sh`
**Library:** `hub/clavain/hooks/lib-intercore.sh` (v0.1.0)

## Current State

`lib-sprint.sh` is the sprint state library for Clavain. It manages sprint beads (type=epic beads with sprint=true state), including creation, phase tracking, artifact recording, and cache invalidation. All functions are fail-safe (return 0 on error) except `sprint_claim()` which returns 1 on conflict.

### Current cache invalidation logic (line 882)

The `sprint_invalidate_caches()` function performs a simple glob-based file deletion:

```bash
sprint_invalidate_caches() {
    rm -f /tmp/clavain-discovery-brief-*.cache 2>/dev/null || true
}
```

This is called automatically by `sprint_record_phase_completion()` (line 291) whenever a sprint phase completes. The discovery brief caches are regenerated on-demand by the discovery system when needed.

### Key distinction: cache vs. sentinel

Discovery brief caches are **cached data** — they store precomputed results that can be regenerated at any time. They are NOT throttle sentinels (which guard against repeated execution). This distinction is important because:

- **Sentinels** use `intercore sentinel set/check` — they track "when did X last happen?"
- **Caches** use `intercore state set/delete` — they store arbitrary key-value data

For invalidation, we need `state delete` (remove all scopes of a key), not `sentinel reset`.

### File structure context

The file has a double-source guard at line 8-9:
```bash
[[ -n "${_SPRINT_LOADED:-}" ]] && return 0
_SPRINT_LOADED=1
```

After this guard, three libraries are sourced:
1. `lib.sh` — interphase phase primitives (via Clavain shim)
2. `lib-gates.sh` — gates for advance_phase (via shim → interphase)

A jq dependency check at line 25 stubs out all functions (including `sprint_invalidate_caches`) if jq is missing, since the library is JSON-heavy.

## Migration Plan

### Change 1: Source lib-intercore.sh (after double-source guard)

**Location:** After line 9 (`_SPRINT_LOADED=1`), before `SPRINT_LIB_PROJECT_DIR`.

```bash
# Source intercore state primitives (cache invalidation, sentinel checks)
source "${BASH_SOURCE[0]%/*}/lib-intercore.sh" 2>/dev/null || true
```

**Rationale:**
- Must be after the double-source guard so it only loads once
- Uses `${BASH_SOURCE[0]%/*}` (parent directory of current script) — same idiom used elsewhere in the hooks directory
- `|| true` ensures fail-open: if lib-intercore.sh is missing, the fallback `rm -f` path in the function still works
- Placed before the jq check because lib-intercore.sh does not depend on jq (intercore CLI is a Go binary)

### Change 2: Replace sprint_invalidate_caches function body

**Before:**
```bash
sprint_invalidate_caches() {
    rm -f /tmp/clavain-discovery-brief-*.cache 2>/dev/null || true
}
```

**After:**
```bash
sprint_invalidate_caches() {
    if type intercore_state_delete_all &>/dev/null; then
        intercore_state_delete_all "discovery_brief" "/tmp/clavain-discovery-brief-*.cache"
    else
        rm -f /tmp/clavain-discovery-brief-*.cache 2>/dev/null || true
    fi
}
```

**Rationale:**
- `type ... &>/dev/null` checks if the function is available (lib-intercore.sh loaded and intercore binary found)
- `intercore_state_delete_all` iterates all scopes for the key `discovery_brief` and deletes them via `intercore state delete`
- The second argument (`"/tmp/clavain-discovery-brief-*.cache"`) is the legacy glob pattern — `intercore_state_delete_all` uses it as a fallback if the intercore binary is unavailable (see lib-intercore.sh lines 150-163)
- The outer `else` branch is a second layer of fallback: if lib-intercore.sh itself failed to load (function not defined), we still do the legacy `rm -f`
- This is a **two-layer graceful degradation**: intercore → lib-intercore fallback → raw rm

### What does NOT change

1. **The jq-missing stub** (line 36): `sprint_invalidate_caches() { return 0; }` — this is correct. If jq is missing, the entire sprint library is non-functional, and there's nothing to invalidate.
2. **The call site** (line 291): `sprint_invalidate_caches` — no change needed, the function signature is identical.
3. **No cleanup ownership**: Unlike sentinels, discovery caches don't need periodic cleanup. They are invalidated on-demand when sprint phases complete, and stale caches simply get overwritten on next generation.

## Verification

### Syntax check
```bash
bash -n hub/clavain/hooks/lib-sprint.sh
```
Result: **PASS** (no syntax errors)

### Behavioral verification matrix

| Scenario | intercore available | lib-intercore loaded | Result |
|----------|-------------------|---------------------|--------|
| Normal (intercore installed) | Yes | Yes | `intercore state delete` for each scope |
| intercore binary missing | No | Yes | `rm -f /tmp/clavain-discovery-brief-*.cache` (lib-intercore fallback) |
| lib-intercore.sh missing | N/A | No | `rm -f /tmp/clavain-discovery-brief-*.cache` (outer else) |
| jq missing | N/A | N/A | `return 0` (stub, no-op) |

### Risk assessment

**Risk: LOW** — Cache invalidation is idempotent and non-critical. Even if it fails completely, the only effect is that stale discovery briefs persist until the next session, which is harmless (they'll be regenerated).

## State Key Design

- **Key name:** `discovery_brief`
- **Scope pattern:** Typically per-project or per-session (set by the discovery system when writing caches)
- **Value:** The cached discovery brief content
- **Lifecycle:** Set by discovery system → deleted by `sprint_invalidate_caches` on phase completion → regenerated on next discovery query

## Summary of Changes Made

Two modifications to `hub/clavain/hooks/lib-sprint.sh`:

1. **Line 11-12 (new):** Added `source "${BASH_SOURCE[0]%/*}/lib-intercore.sh"` after the double-source guard
2. **Lines 882-888 (replaced):** `sprint_invalidate_caches()` now checks for `intercore_state_delete_all` availability and uses it with graceful fallback to legacy `rm -f`

Both changes maintain the library's fail-safe contract: errors are suppressed, and the function always returns 0.
