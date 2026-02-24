# Code Quality Review: Phase Action Package

**Date:** 2026-02-21
**Scope:** `internal/action/store.go`, `internal/action/action.go`, `cmd/ic/action.go`, `lib-intercore.sh` (Phase Action wrappers section)
**Comparators:** `internal/lane/store.go`, `internal/runtrack/store.go`, `cmd/ic/run.go`, `cmd/ic/discovery.go`

---

## Summary

The action package is well-structured and idiomatic for this codebase. It integrates cleanly with existing patterns. Four concrete issues are worth fixing before this stabilizes: two correctness issues (error comparison style and `rows.Err()` omission in `AddBatch`), one missing bash wrapper, and one minor inconsistency in FK error handling. Everything else is either project-consistent or minor style.

---

## Go: `internal/action/store.go` + `internal/action/action.go`

### Finding 1 — Sentinel errors are in-package but not in a separate errors.go file (informational, no action required)

Other packages (`runtrack`, `discovery`) place sentinel errors in a dedicated `errors.go`:

```
internal/runtrack/errors.go   → ErrAgentNotFound, ErrRunNotFound, ...
internal/discovery/errors.go  → ErrNotFound, ErrDuplicate, ...
```

The action package defines its sentinels at the top of `store.go`. This is not wrong; both patterns appear in the codebase. Given that the action package has only two sentinels and no separate types file needed, keeping them in `store.go` is fine. If the package grows, extract to `errors.go` then.

---

### Finding 2 — FK constraint check is inline string match rather than a named helper (LOW — consistency issue)

**Location:** `internal/action/store.go:48`

```go
if strings.Contains(err.Error(), "FOREIGN KEY constraint") {
    return 0, fmt.Errorf("run not found: %s", a.RunID)
}
```

`runtrack/store.go` uses a named helper that matches the exact canonical SQLite message:

```go
// runtrack/store.go:392-394
func isFKViolation(err error) bool {
    return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
```

The action package's check matches `"FOREIGN KEY constraint"` which is a prefix-match and will work, but it's inconsistent. More importantly, the action package returns a plain non-wrapped error for this case:

```go
return 0, fmt.Errorf("run not found: %s", a.RunID)
```

This loses the original DB error from the chain. Compare to runtrack which returns a typed sentinel:

```go
return "", ErrRunNotFound   // callers can errors.Is() against it
```

The action package should adopt the same pattern. The FK case in `Add()` should return `ErrNotFound` or a new `ErrRunNotFound` sentinel rather than a formatted string, and the check should use a helper or the full string `"FOREIGN KEY constraint failed"`.

**Suggested fix:**

Add a local helper (or import a shared one if one is created):

```go
// in store.go or a future errors.go
func isFKViolation(err error) bool {
    return err != nil && strings.Contains(err.Error(), "FOREIGN KEY constraint failed")
}
```

Define a sentinel:

```go
var ErrRunNotFound = errors.New("run not found")
```

Then in `Add()`:

```go
if isFKViolation(err) {
    return 0, ErrRunNotFound
}
```

---

### Finding 3 — `AddBatch` does not wrap the duplicate error as a sentinel (LOW — inconsistency)

**Location:** `internal/action/store.go:81-83`

```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return fmt.Errorf("duplicate action for phase %s: %s", phase, a.Command)
}
```

The single-item `Add()` at line 45 returns `ErrDuplicate` for the same condition. `AddBatch` returns a formatted string instead. This means callers cannot use `errors.Is(err, ErrDuplicate)` when using the batch path — they get a different error shape with no consistent detection strategy.

**Suggested fix:**

```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return fmt.Errorf("phase %s command %s: %w", phase, a.Command, ErrDuplicate)
}
```

Using `%w` here preserves the sentinel while adding context. Callers can still `errors.Is(err, ErrDuplicate)`.

---

### Finding 4 — `resolveTemplateVars` is a method on `*Store` but does not need receiver access

**Location:** `internal/action/store.go:188`

```go
func (s *Store) resolveTemplateVars(ctx context.Context, runID, projectDir, input string) string {
```

This method performs one DB lookup (for artifact paths). The DB reference is accessed via `s.db`. However, the function's _role_ is a resolver, not a store operation. Making it a method creates a subtle ambiguity about whether it's part of the public API (it isn't — lowercase) or a core store concern.

This is a very minor point and does not warrant a change on its own. The current shape is not wrong. If `resolveTemplateVars` is ever tested in isolation, converting it to a package-level function taking `*sql.DB` would improve testability. No action required now.

---

### Finding 5 — `Update()` silently ignores `RowsAffected` error (LOW — existing pattern, tolerated)

**Location:** `internal/action/store.go:164`

```go
n, _ := result.RowsAffected()
```

The `_` discards any error from `RowsAffected()`. This pattern also appears in `lane/store.go:178` and `runtrack/store.go:82`, so it is an accepted project convention. For `modernc.org/sqlite`, `RowsAffected()` does not return meaningful errors for `UPDATE`. Consistent with existing code — no change needed.

---

### Finding 6 — `interface{}` instead of `any` in `Update()` (VERY LOW — project style check)

**Location:** `internal/action/store.go:136`

```go
var args []interface{}
```

`runtrack/store.go` uses `[]interface{}` in the same way (lines 238, 286), so this is project-consistent. Go 1.18+ `any` is an alias; the codebase has not migrated. No change needed.

---

## Go: `cmd/ic/action.go`

### Finding 7 — Error comparison uses `==` instead of `errors.Is` (MEDIUM — correctness risk)

**Location:** `cmd/ic/action.go:92`, `:216`, `:257`

```go
if err == action.ErrDuplicate {   // line 92
if err == action.ErrNotFound {    // line 216
if err == action.ErrNotFound {    // line 257
```

The rest of the codebase has migrated to `errors.Is` for sentinel comparison:

- `cmd/ic/discovery.go:632-644` — all four sentinel checks use `errors.Is`
- `cmd/ic/gate.go:89,93` — both use `errors.Is`
- `cmd/ic/lock.go:78,120,126` — all use `errors.Is`

The exceptions that still use `==` are in `run.go` (lines 1296, 1394, 1606), but those are against runtrack sentinels and appear to be pre-migration holdovers. The action package is new code and should adopt `errors.Is` from the start to be future-proof: if the error is ever wrapped in a chain, `==` will silently fail to match.

**Suggested fix** (three locations):

```go
// line 92
if errors.Is(err, action.ErrDuplicate) {
    return 1
}

// line 216
if errors.Is(err, action.ErrNotFound) {
    return 1
}

// line 257
if errors.Is(err, action.ErrNotFound) {
    return 1
}
```

Add `"errors"` to the imports if not already present (it is — `store.go` already imports it).

---

### Finding 8 — `--priority` parsing uses `fmt.Sscanf` instead of `strconv.Atoi`

**Location:** `cmd/ic/action.go:47` and `:203`

```go
fmt.Sscanf(strings.TrimPrefix(arg, "--priority="), "%d", &priority)
```

Every integer flag in `run.go` uses `strconv.Atoi` or `strconv.ParseInt` with explicit error handling (lines 88, 100, 108, 116, 369, 1078, 1102). `fmt.Sscanf` silently ignores parse errors — if a caller passes `--priority=abc`, the variable stays at its zero value with no user-visible error message.

**Suggested fix** (both callsites in `cmdRunActionAdd` and `cmdRunActionUpdate`):

```go
case strings.HasPrefix(arg, "--priority="):
    val := strings.TrimPrefix(arg, "--priority=")
    p, err := strconv.Atoi(val)
    if err != nil {
        fmt.Fprintf(os.Stderr, "ic: run action add: --priority: not an integer: %s\n", val)
        return 3
    }
    priority = p
```

This is consistent with `run.go:369` which handles the same flag type identically.

---

### Finding 9 — `cmdRunActionUpdate` success path prints to stdout unconditionally (minor inconsistency)

**Location:** `cmd/ic/action.go:220-222`

```go
fmt.Printf("Updated: %s → %s\n", phase, command)
return 0
```

The `update` subcommand does not check `flagJSON` before printing. When `--json` is passed, callers expect clean JSON output only. Compare `cmdRunActionDelete` which also only prints a human line — but delete producing no JSON output on success is somewhat common (similar to `lane close`). For `update` specifically, returning a structured JSON acknowledgement or at least suppressing the human line when `--json` is set would be more consistent with the rest of the CLI.

The `add` subcommand correctly branches on `flagJSON` (lines 98-101). The `update` subcommand should do the same:

```go
if flagJSON {
    json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"updated": true})
} else {
    fmt.Printf("Updated: %s → %s\n", phase, command)
}
```

---

### Finding 10 — `actionToMap` does not include `created_at` / `updated_at`

**Location:** `cmd/ic/action.go:267-283`

```go
func actionToMap(a *action.Action) map[string]interface{} {
    m := map[string]interface{}{
        "id":          a.ID,
        "run_id":      a.RunID,
        "phase":       a.Phase,
        "action_type": a.ActionType,
        "command":     a.Command,
        "mode":        a.Mode,
        "priority":    a.Priority,
    }
    ...
}
```

The action struct has `CreatedAt` and `UpdatedAt` fields (populated from the DB scan) but they are not included in the JSON map. Consumers relying on `--json` output to correlate timing or sort actions cannot access this data. Other list commands in the codebase (e.g., `run artifact list`, `run agent list`) include timestamp fields in their JSON output.

**Suggested fix:**

```go
m["created_at"] = a.CreatedAt
m["updated_at"] = a.UpdatedAt
```

---

## Bash: `lib-intercore.sh` Phase Action Wrappers

**Location:** Lines 507-552

### Finding 11 — `intercore_run_action_update` wrapper is missing (MEDIUM — gap in parity)

The CLI exposes `ic run action update` (implemented in `cmd/ic/action.go:cmdRunActionUpdate`). The bash wrapper file provides `intercore_run_action_add`, `intercore_run_action_list`, and `intercore_run_action_delete`, but there is no `intercore_run_action_update` wrapper. Any shell hook or sprint script that needs to modify an existing action's args, mode, or priority has no library function to call — callers would have to invoke `ic` directly, bypassing the `intercore_available` guard and the `--db` forwarding.

**Suggested addition** (insert after `intercore_run_action_list`, before `intercore_run_action_delete`):

```bash
# intercore_run_action_update — Update args, mode, or priority for a phase action.
# Args: $1=run_id, $2=phase, $3=command, $4=args_json (optional), $5=mode (optional)
# Returns: 0 on success, 1 on not found
intercore_run_action_update() {
    local run_id="$1" phase="$2" command="$3" args_json="${4:-}" mode="${5:-}"
    if ! intercore_available; then return 1; fi
    local cmd_args=(run action update "$run_id" --phase="$phase" --command="$command")
    [[ -n "$args_json" ]] && cmd_args+=(--args="$args_json")
    [[ -n "$mode" ]] && cmd_args+=(--mode="$mode")
    "$INTERCORE_BIN" "${cmd_args[@]}" ${INTERCORE_DB:+--db="$INTERCORE_DB"} 2>/dev/null
}
```

---

### Finding 12 — `intercore_run_action_add` hard-codes `--json` in the command args array (style inconsistency)

**Location:** `lib-intercore.sh:516`

```bash
local cmd_args=(--json run action add "$run_id" --phase="$phase" --command="$command" --mode="$mode")
```

`intercore_run_action_list` also hard-codes `--json` (line 528):

```bash
local cmd_args=(--json run action list "$run_id")
```

This is intentional and consistent: wrappers that return data always request JSON for structured output. The pattern is correct; callers of these wrappers pipe the JSON to `jq`. This is consistent with how `intercore_run_advance` uses `--json` (line 550) and `intercore_run_tokens` (line 446). No change needed — documenting here for completeness.

---

### Finding 13 — `intercore_run_advance` wrapper position is at the end of the Phase Action section (cosmetic)

**Location:** `lib-intercore.sh:543-552`

`intercore_run_advance` is placed in the "Phase Action wrappers" section header comment block, but it was a pre-existing wrapper that existed before the action subcommands were added (it was absent in the codebase as a gap, then added as part of the phantom wrapper fix). Its placement after `intercore_run_action_delete` is fine functionally, but it doesn't belong under the "Phase Action" label — it's a run lifecycle operation. This is cosmetic and low-priority.

---

## Summary Table

| # | File | Severity | Finding |
|---|------|----------|---------|
| 2 | `internal/action/store.go` | LOW | FK error returned as formatted string, not sentinel; helper missing |
| 3 | `internal/action/store.go` | LOW | `AddBatch` duplicate case returns plain error instead of `%w ErrDuplicate` |
| 7 | `cmd/ic/action.go` | MEDIUM | `==` used for sentinel comparison instead of `errors.Is` (3 sites) |
| 8 | `cmd/ic/action.go` | LOW | `fmt.Sscanf` for `--priority` silently discards parse errors; use `strconv.Atoi` |
| 9 | `cmd/ic/action.go` | LOW | `update` success path ignores `flagJSON`; prints human text even with `--json` |
| 10 | `cmd/ic/action.go` | LOW | `actionToMap` omits `created_at`/`updated_at` fields |
| 11 | `lib-intercore.sh` | MEDIUM | `intercore_run_action_update` wrapper missing — no parity with `ic run action update` |

Findings 1, 4, 5, 6, 12, 13 require no action.

---

## Recommended Fix Order

1. **Finding 7** — `errors.Is` for sentinel comparison. New code; easy, no risk, aligns with codebase direction.
2. **Finding 11** — Add missing `intercore_run_action_update` bash wrapper. Closes a parity gap before scripts depend on the absence.
3. **Finding 8** — `strconv.Atoi` for `--priority`. Consistent with all other CLI flag parsing.
4. **Finding 3** — Wrap `ErrDuplicate` with `%w` in `AddBatch` for consistent detection.
5. **Finding 2** — Add `isFKViolation` helper and `ErrRunNotFound` sentinel to close the FK handling gap.
6. **Finding 10** — Add timestamps to `actionToMap` (additive, no breakage).
7. **Finding 9** — `flagJSON` branch in `cmdRunActionUpdate` (polish).
