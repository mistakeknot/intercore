# Migration Guide: Temp Files → intercore

## Overview

intercore replaces scattered `/tmp/` files with a single SQLite database. Migration uses a **read-fallback** strategy: write to DB only, consumers try DB first and fall back to legacy files.

## Phase 1: Read-Fallback (v1 launch)

### Pre-migration cleanup

Before enabling read-fallback, remove stale legacy files to prevent ghost-gating:

```bash
rm -f /tmp/clavain-dispatch-*.json /tmp/clavain-stop-* \
      /tmp/clavain-compound-last-* /tmp/clavain-drift-last-*
```

### Migration window constraint

All hooks producing a given key must be migrated before any consumer switches to read-fallback for that key. Otherwise, old hooks write to `/tmp/` while new consumers read from DB, causing state divergence.

### Hook migration pattern

**Before (temp file sentinel):**
```bash
STOP_SENTINEL="/tmp/clavain-stop-${SESSION_ID}"
if [[ -d "$STOP_SENTINEL" ]]; then exit 0; fi
mkdir "$STOP_SENTINEL" 2>/dev/null || exit 0
```

**After (with read-fallback):**
```bash
source lib-intercore.sh
if intercore_available; then
    intercore_sentinel_check stop "$SESSION_ID" 0 || exit 0
else
    # Legacy fallback — uses mkdir (atomic), NOT touch (symlink-vulnerable)
    STOP_SENTINEL="/tmp/clavain-stop-${SESSION_ID}"
    if [[ -d "$STOP_SENTINEL" ]]; then exit 0; fi
    mkdir "$STOP_SENTINEL" 2>/dev/null || exit 0
fi
```

### Security: Why `mkdir` instead of `touch`

`touch` creates a regular file and is vulnerable to symlink attacks (TOCTOU: attacker races to create a symlink at the path before `touch` runs). `mkdir` with `O_EXCL` semantics is atomic and fails safely if the target already exists or is a symlink.

## Temp File → intercore Mapping

| Legacy Pattern | intercore Key | Type |
|---------------|--------------|------|
| `/tmp/clavain-dispatch-*.json` | `dispatch` | state (JSON payload) |
| `/tmp/clavain-stop-*` | `stop` | sentinel (interval=0) |
| `/tmp/clavain-compound-last-*` | `compound_throttle` | sentinel (interval=300) |
| `/tmp/clavain-drift-last-*` | `drift_throttle` | sentinel (interval=600) |
| `/tmp/clavain-handoff-*` | `handoff` | state (JSON payload) |
| `/tmp/clavain-autopub*.lock` | `autopub` | sentinel (interval=0) |
| `/tmp/clavain-catalog-remind-*.lock` | `catalog_remind` | sentinel (interval=0) |
| `/tmp/clavain-discovery-brief-*.cache` | `discovery_brief` | state (JSON with TTL) |

Patterns that stay as-is (mutex/lock files):
- `mkdir`-based locks → no change (intercore doesn't replace filesystem mutexes)

## Phase 2: Validation (4 weeks post-launch)

```bash
ic compat status     # Shows which consumers still fall back to legacy
ic compat check dispatch  # Test if dispatch reads from DB
```

Verify: all active hooks use `lib-intercore.sh` wrappers.

## Phase 3: Legacy Removal

1. Remove fallback logic from all consumers
2. Remove temp file writes from hooks
3. Run cleanup: `rm -f /tmp/clavain-dispatch-*.json /tmp/clavain-stop-* /tmp/clavain-compound-last-* /tmp/clavain-drift-last-* /tmp/clavain-handoff-* /tmp/clavain-autopub*.lock /tmp/clavain-catalog-remind-*.lock /tmp/clavain-discovery-brief-*.cache`
