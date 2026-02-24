# Correctness Review: Action Store (phase_actions, schema v14)

**Reviewed files:**
- `/root/projects/Interverse/infra/intercore/internal/action/store.go`
- `/root/projects/Interverse/infra/intercore/internal/action/action.go`
- `/root/projects/Interverse/infra/intercore/internal/action/store_test.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/action.go`
- `/root/projects/Interverse/infra/intercore/cmd/ic/run.go` (lines 535–579, 263–296)

**Date:** 2026-02-21

---

## Invariants Under Review

Before scoring findings, these are the invariants the action store must satisfy:

1. **Atomicity of run+actions creation:** A run and its associated phase actions must both exist, or neither. A caller who receives exit 2 from `ic run create` must be able to assume neither is present.
2. **Batch-all-or-nothing:** `AddBatch` must leave zero partial writes on failure.
3. **Duplicate detection is sentinel-based:** Any code that calls `Add` or `AddBatch` and needs to distinguish a duplicate from a generic error must be able to use `errors.Is(err, ErrDuplicate)`.
4. **Resolved args remain valid JSON:** `ListForPhaseResolved` must return `Args` that are structurally valid JSON if the stored `Args` were valid JSON.
5. **Advance output is trustworthy:** When `ic run advance` emits an `actions` array, the consumer can act on it. Silent failure must not produce an empty `actions` array that looks like "no actions registered."
6. **No SQL injection via dynamic UPDATE:** `Update()` must not allow user-controlled strings to land in the SQL column list.
7. **Nil Args handled consistently:** A nil `Args` must not panic or produce a corrupt output anywhere in the read path.

---

## Findings

### CRITICAL — C1: Run-creation and action-batch registration are not atomic

**File:** `cmd/ic/run.go`, lines 263–295

**Code:**
```go
id, err := store.Create(ctx, run)   // line 263 — run committed to DB
if err != nil { ... return 2 }

// Register phase actions if --actions provided
if actionsJSON != "" {
    ...
    if err := aStore.AddBatch(ctx, id, batch); err != nil {
        fmt.Fprintf(os.Stderr, "ic: run create: register actions: %v\n", err)
        return 2   // EXIT 2 — but run is already in the DB!
    }
}
```

**Failure narrative:**

1. `store.Create(ctx, run)` succeeds. Run ID is committed to the `runs` table.
2. JSON parsing of `--actions` succeeds.
3. `aStore.AddBatch(ctx, id, batch)` hits a transient DB error (SQLITE_BUSY, disk full, or a FOREIGN KEY violation on a bad run ID race).
4. `cmdRunCreate` prints to stderr and returns exit 2.
5. Caller (lib-sprint.sh, automation) sees exit 2, assumes the whole operation failed, never learns the run ID, never calls `ic run cancel`.
6. The run is now an active orphan: `status=active`, no actions, no agents, invisible to the caller. It will show up in `ic run list --active` indefinitely.

**Invariant broken:** Invariant 1.

**Fix:** Wrap `store.Create` and `aStore.AddBatch` in a single transaction, or implement a compensating `store.Cancel/Delete` call in the error path. The cleanest approach is to pass the transaction handle into `AddBatch` via a `*sql.Tx`-accepting variant, since `phase.Store.Create` already uses a transaction internally. A simpler workaround: call `store.UpdateStatus(ctx, id, phase.StatusCancelled)` before returning on `AddBatch` failure, and emit the run ID on stderr so it can be cleaned up.

---

### HIGH — H1: `AddBatch` does not return `ErrDuplicate` sentinel on duplicate — inconsistency breaks programmatic callers

**File:** `internal/action/store.go`, lines 80–83

**Code (AddBatch):**
```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return fmt.Errorf("duplicate action for phase %s: %s", phase, a.Command)
}
```

**Code (Add):**
```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return 0, ErrDuplicate
}
```

`Add` returns the sentinel `ErrDuplicate`, which any caller can test with `errors.Is(err, ErrDuplicate)`. `AddBatch` returns a formatted string error wrapping no sentinel. Any code that does:

```go
if errors.Is(err, action.ErrDuplicate) { ... }
```

after `AddBatch` will silently skip the branch and treat a duplicate as a generic error (exit 2 in the CLI).

The CLI in `cmd/ic/run.go` (line 292) only checks `err != nil` for `AddBatch`, so the practical impact is limited today. But the inconsistency is a trap for future callers.

**Invariant broken:** Invariant 3.

**Fix:**
```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return fmt.Errorf("duplicate action for phase %s (%s): %w", phase, a.Command, ErrDuplicate)
}
```

Wrapping with `%w` preserves the sentinel through `errors.Is`.

---

### HIGH — H2: `resolveTemplateVars` substitutes artifact paths directly into a JSON string without JSON escaping — produces corrupt JSON when paths contain quotes or backslashes

**File:** `internal/action/store.go`, lines 187–214

**Code:**
```go
return path   // raw DB string, substituted into JSON array literally
```

```go
result = strings.ReplaceAll(result, "${run_id}", runID)
result = strings.ReplaceAll(result, "${project_dir}", projectDir)
```

**Failure narrative:**

The `Args` field stores a JSON array as a string, e.g. `["${artifact:plan}","${run_id}"]`. At resolve time, `${artifact:plan}` is replaced with the raw path from `run_artifacts.path`. If that path contains a double-quote, backslash, or newline — all legal filesystem characters — the resulting string is no longer valid JSON.

Example:
- Stored args: `["${artifact:plan}"]`
- Artifact path in DB: `docs/plans/clavain's "best" plan.md`
- Result after substitution: `["docs/plans/clavain's "best" plan.md"]`
- This is not parseable JSON.

The downstream consumer (`actionToMap`) calls `json.Unmarshal` on the resolved args. If parsing fails it falls back to using the raw string as a single string value:
```go
if err := json.Unmarshal([]byte(*a.Args), &parsed); err == nil {
    m["args"] = parsed
} else {
    m["args"] = *a.Args   // corrupt JSON emitted as a raw string
}
```

This means a consumer expecting `args` to be an array gets a scalar string containing garbage. A script doing `jq '.[].args[0]'` on the advance output will get `null` instead of the artifact path.

The same problem affects `${run_id}` (run IDs are controlled format, low risk) and `${project_dir}` (absolute paths can contain spaces and parentheses — low risk but not zero).

**Invariant broken:** Invariant 4.

**Fix:** After resolution, the replacement value must be JSON-encoded before substitution. The correct approach is to parse the args JSON array, substitute within individual string elements, then re-marshal:

```go
func resolveInArgsJSON(input, runID, projectDir string, lookupArtifact func(string) string) string {
    var arr []string
    if err := json.Unmarshal([]byte(input), &arr); err != nil {
        return input // not a JSON array, leave as-is
    }
    for i, s := range arr {
        s = templateVarRE.ReplaceAllStringFunc(s, func(match string) string {
            sub := templateVarRE.FindStringSubmatch(match)
            if len(sub) < 2 { return match }
            if p := lookupArtifact(sub[1]); p != "" { return p }
            return match
        })
        s = strings.ReplaceAll(s, "${run_id}", runID)
        s = strings.ReplaceAll(s, "${project_dir}", projectDir)
        arr[i] = s
    }
    b, err := json.Marshal(arr)
    if err != nil {
        return input
    }
    return string(b)
}
```

This correctly handles all special characters in paths.

---

### HIGH — H3: Advance command silently discards action-resolution errors

**File:** `cmd/ic/run.go`, line 539

**Code:**
```go
resolvedActions, _ = aStore.ListForPhaseResolved(ctx, id, result.ToPhase, run.ProjectDir)
```

The `_` discards any error from action resolution. If the query fails (SQLITE_BUSY, context cancellation, schema mismatch), `resolvedActions` is nil. The JSON output will have no `actions` key, which is indistinguishable from "no actions registered for this phase."

**Failure narrative:**

1. `ic run advance` succeeds, `result.Advanced = true`.
2. DB becomes briefly busy (another process holds the write lock).
3. `ListForPhaseResolved` returns `nil, SQLITE_BUSY_TIMEOUT`.
4. `out["actions"]` is omitted from the JSON output.
5. `sprint_next_step()` in lib-sprint.sh reads no actions, falls through to the static routing table.
6. The sprint proceeds without running the registered flux-drive or interpeer command.
7. No error is logged anywhere.

The advance itself succeeded. The run advanced. But the caller silently received a lie: it thinks there are no actions for this phase.

**Invariant broken:** Invariant 5.

**Fix:**
```go
resolvedActions, err = aStore.ListForPhaseResolved(ctx, id, result.ToPhase, run.ProjectDir)
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run advance: warning: could not resolve actions: %v\n", err)
    // Do not fail the advance — it already happened. But signal the partial result.
    if flagJSON {
        out["actions_error"] = err.Error()
    }
}
```

The advance should not be rolled back (it already committed), but the degraded output must be visible to the caller.

---

### MEDIUM — M1: `UNIQUE constraint` error detection is fragile string matching

**File:** `internal/action/store.go`, lines 45, 81

**Code:**
```go
if strings.Contains(err.Error(), "UNIQUE constraint") {
    return 0, ErrDuplicate
}
```

This pattern is present throughout the codebase (also in `internal/discovery/store.go`, `internal/runtrack/store.go`). For the `modernc.org/sqlite` driver on this project, the error message is currently `"UNIQUE constraint failed: phase_actions.run_id, phase_actions.phase, phase_actions.command"`. The substring `"UNIQUE constraint"` is reliable for this driver.

However:

1. The error message is not part of the SQLite C library's public API. It has changed before across SQLite versions.
2. If the driver is ever replaced (e.g., `mattn/go-sqlite3`), the error message format changes. The `mattn` driver returns `"UNIQUE constraint failed: ..."` (same), but the `modernc` driver's behavior has subtly differed in the past on translated errors.
3. The `FOREIGN KEY constraint` check on line 48 is also string-matched. A combined constraint violation message could fail to match.

The more robust approach is to use the SQLite error code directly:

```go
import "modernc.org/sqlite"

var sqliteErr *sqlite.Error
if errors.As(err, &sqliteErr) && sqliteErr.Code() == 2067 { // SQLITE_CONSTRAINT_UNIQUE
    return 0, ErrDuplicate
}
```

SQLite's extended result codes are stable across versions. Code 2067 (`SQLITE_CONSTRAINT_UNIQUE`) will not change.

**Invariant broken:** Invariant 3 (reliability, not current correctness).

---

### MEDIUM — M2: `AddBatch` has no test for partial-failure atomicity

**File:** `internal/action/store_test.go`

`TestAddBatch` (line 186) only tests the happy path: two actions inserted, both readable. There is no test that:

- Inserts the first action successfully
- Fails on the second action (e.g., duplicate or FK violation)
- Verifies the first action was rolled back

Without this test, the `defer tx.Rollback()` behavior on the error path is unverified. The implementation is correct (Go's `database/sql` rolls back a transaction when you return before `Commit()`), but it is not proven by the test suite.

A test should:
```go
func TestAddBatchRollbackOnDuplicate(t *testing.T) {
    // Pre-insert one action to cause a duplicate
    s.Add(ctx, &Action{RunID: "test-run-1", Phase: "planned", Command: "/conflict"})

    batch := map[string]*Action{
        "executing": {Command: "/clavain:work"},   // would succeed
        "planned":   {Command: "/conflict"},        // will fail (duplicate)
    }
    err := s.AddBatch(ctx, "test-run-1", batch)
    if err == nil {
        t.Fatal("expected error")
    }

    // The "executing" row must not have been written
    actions, _ := s.ListForPhase(ctx, "test-run-1", "executing")
    if len(actions) != 0 {
        t.Fatalf("partial batch was not rolled back: %d rows written", len(actions))
    }
}
```

---

### MEDIUM — M3: `ic run action add` does not validate that `--args` is valid JSON before storing

**File:** `cmd/ic/action.go`, lines 85–87

```go
if argsJSON != "" {
    a.Args = &argsJSON
}
```

The CLI accepts any string as `--args` and stores it verbatim. If a user passes `--args='not json'`, the DB stores `not json`. When `actionToMap` later calls `json.Unmarshal` on it, the parse fails and it falls back to emitting the raw string. Downstream consumers expecting an array will receive a string.

The `ic run create --actions` path (line 277) does validate the outer JSON envelope (`json.Unmarshal` of the full actions map), but does not validate the inner `args` value.

**Fix:** Add JSON validation at the CLI layer:
```go
if argsJSON != "" {
    if !json.Valid([]byte(argsJSON)) {
        fmt.Fprintf(os.Stderr, "ic: run action add: --args must be valid JSON\n")
        return 3
    }
    a.Args = &argsJSON
}
```

---

### LOW — L1: `Update()` args slice building is correct but brittle

**File:** `internal/action/store.go`, lines 155–160

```go
sets = append(sets, "updated_at = ?")
args = append(args, now, runID, phase, command)
```

The single `append(args, now, runID, phase, command)` call appends four values at once. This is Go-correct: variadic `append` accepts multiple elements. The positional correspondence to `WHERE run_id = ? AND phase = ? AND command = ?` (plus `updated_at = ?` in SET) is correct.

However, any future edit that inserts a new field into the WHERE clause without updating this line will silently produce wrong parameter bindings. The pattern used in other stores (appending WHERE args one at a time) is more robust. This is not a current bug but a maintenance trap.

---

### LOW — L2: `resolveTemplateVars` calls `templateVarRE.FindStringSubmatch` inside `ReplaceAllStringFunc` — double regex execution per match

**File:** `internal/action/store.go`, lines 190–196

```go
result := templateVarRE.ReplaceAllStringFunc(input, func(match string) string {
    sub := templateVarRE.FindStringSubmatch(match)
    ...
    artType := sub[1]
```

`ReplaceAllStringFunc` already passes the full match to the callback. The callback then re-runs the regex on the already-matched string to extract the capture group. This is correct but wasteful. The standard pattern is `ReplaceAllFunc` with `[]byte` and using the submatch overload, or simply `ReplaceAllStringFunc` with a manual `strings.TrimPrefix/TrimSuffix` on `"${artifact:"` and `"}"`:

```go
artType := strings.TrimSuffix(strings.TrimPrefix(match, "${artifact:"), "}")
```

This is a performance nit, not a correctness issue. The args string is short and this runs once per advance.

---

### LOW — L3: Portfolio runs use `ProjectDir = ""` — `resolveTemplateVars` silently substitutes empty string for `${project_dir}`

**File:** `cmd/ic/run.go`, line 539

For portfolio runs, `run.ProjectDir` is `""` (documented in AGENTS.md: "A portfolio run is identified by `project_dir = ""`"). If phase actions on a portfolio run contain `${project_dir}`, the variable is silently replaced with an empty string, producing args like `["/file.md"]` when the intent was `["/root/projects/foo/file.md"]`.

Portfolio runs do not typically have phase actions registered on them (actions belong to child runs), but the code path allows it. No guard exists.

---

### INFORMATIONAL — I1: `actionToMap` correctly handles nil Args (no bug)

This was explicitly asked about. The implementation:
```go
func actionToMap(a *action.Action) map[string]interface{} {
    m := map[string]interface{}{...}
    if a.Args != nil {
        var parsed interface{}
        if err := json.Unmarshal([]byte(*a.Args), &parsed); err == nil {
            m["args"] = parsed
        } else {
            m["args"] = *a.Args
        }
    }
    return m
}
```

When `Args` is nil, the `"args"` key is simply absent from the returned map. This is correct and consistent with how JSON serialization of optional fields works. The JSON output from `ic run advance` will omit `"args"` for actions without arguments, which is fine for consumers using `jq '.actions[].args // []'`.

---

### INFORMATIONAL — I2: `AddBatch` rollback on error is mechanically correct

The `defer tx.Rollback()` pattern in `AddBatch` is correct for Go's `database/sql`. After a successful `tx.Commit()`, the deferred `Rollback()` is a no-op (returns `sql.ErrTxDone`, which is ignored). After an error return from inside the loop, `Rollback()` fires before the function returns, undoing all inserts in the batch. This is the standard pattern and it works correctly.

What is absent is a **test proving this behavior** (see M2).

---

### INFORMATIONAL — I3: Dynamic SQL in `Update()` has no injection risk

The `sets` slice is built exclusively from hardcoded string literals (`"args = ?"`, `"mode = ?"`, `"priority = ?"`, `"updated_at = ?"`). No user-supplied string ever enters the SQL column name. The `fmt.Sprintf` for the query only joins those hardcoded literals. This is safe.

---

## Summary Table

| ID | Severity | Description | File | Line |
|----|----------|-------------|------|------|
| C1 | CRITICAL | Run+actions creation not atomic: orphaned run on batch failure | run.go | 263–295 |
| H1 | HIGH | `AddBatch` does not wrap `ErrDuplicate` sentinel | store.go | 81–83 |
| H2 | HIGH | JSON corruption: artifact paths substituted without JSON escaping | store.go | 190–204 |
| H3 | HIGH | Advance silently discards action-resolution error | run.go | 539 |
| M1 | MEDIUM | UNIQUE constraint detection via fragile string matching | store.go | 45, 81 |
| M2 | MEDIUM | No test for partial-batch rollback atomicity | store_test.go | — |
| M3 | MEDIUM | CLI does not validate `--args` as JSON before storage | action.go | 85–87 |
| L1 | LOW | Brittle multi-value `append` in `Update()` WHERE args | store.go | 156 |
| L2 | LOW | Double regex execution per match in `resolveTemplateVars` | store.go | 190–196 |
| L3 | LOW | `${project_dir}` resolves to empty string for portfolio runs | run.go | 539 |
| I1 | INFO | `actionToMap` nil Args handling is correct | action.go | 277 |
| I2 | INFO | `AddBatch` deferred Rollback is mechanically correct | store.go | 63 |
| I3 | INFO | `Update()` dynamic SQL has no injection risk | store.go | 135–159 |

---

## Priority Order for Fixes

**Fix immediately (before first production use of phase actions):**

1. **C1** — Wrap run creation and action batch in one transaction. An orphaned run is silent data corruption that will confuse `ic run list --active` and block future runs.
2. **H2** — JSON escape artifact paths before substitution. A path with a quote in it will silently corrupt the args that the consuming agent receives.
3. **H3** — Surface action-resolution errors in the advance output. Silent degradation here defeats the entire purpose of the actions feature.

**Fix before any programmatic consumer beyond lib-sprint.sh:**

4. **H1** — Wrap the `ErrDuplicate` sentinel from `AddBatch` so `errors.Is` works.
5. **M3** — Validate `--args` JSON at the CLI layer.

**Fix in a cleanup pass:**

6. **M1** — Switch from string-matching to SQLite error codes.
7. **M2** — Add partial-rollback test for `AddBatch`.
8. **L1**, **L2**, **L3** — Maintenance improvements, no current correctness impact.
