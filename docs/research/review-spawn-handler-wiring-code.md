# Quality Review: Spawn Handler Wiring

**Files reviewed:**
- `/root/projects/Interverse/infra/intercore/internal/event/handler_spawn.go`
- `/root/projects/Interverse/infra/intercore/internal/runtrack/store.go` (new `UpdateAgentDispatch` method)
- `/root/projects/Interverse/infra/intercore/cmd/ic/run.go` (`cmdRunAdvance`, lines 237-280)
- `/root/projects/Interverse/infra/intercore/internal/runtrack/store_test.go` (new `UpdateAgentDispatch` tests)

**Language:** Go 1.22, `modernc.org/sqlite`
**Verdict:** Solid addition. One correctness gap, two naming nits, one missing test case, one silent-error pattern inherited from the existing codebase.

---

## 1. handler_spawn.go — AgentSpawnerFunc adapter

### Overall

Clean. The func-type adapter pattern (`AgentSpawnerFunc`) is idiomatic Go and consistent with the `http.HandlerFunc` convention. The guard for `nil` logw defaulting to `os.Stderr` is appropriate.

### Findings

**1.1 Spawn failures are logged but not counted — no observable failure signal (medium)**

In `NewSpawnHandler`, per-agent spawn errors are logged but the handler returns `nil` regardless:

```go
if err := spawner.SpawnByAgentID(ctx, id); err != nil {
    fmt.Fprintf(logw, "[event] auto-spawn: agent %s failed: %v\n", id, err)
    continue
}
```

This is a deliberate design choice — one agent failing should not block others — but it means the caller gets no signal that any spawns failed. If all agents in a batch fail, the advance operation returns success (exit 0) with only stderr output. For a CLI tool where the operator may redirect stderr to a log file and be looking at the return code, this is invisible.

A common mitigation is to return a sentinel error after the loop if any failures occurred:

```go
var firstErr error
for _, id := range agentIDs {
    if err := spawner.SpawnByAgentID(ctx, id); err != nil {
        fmt.Fprintf(logw, "[event] auto-spawn: agent %s failed: %v\n", id, err)
        if firstErr == nil {
            firstErr = fmt.Errorf("auto-spawn: at least one agent failed (first: %w)", err)
        }
        continue
    }
    fmt.Fprintf(logw, "[event] auto-spawn: agent %s started\n", id)
}
return firstErr
```

Whether to do this depends on whether a partial-spawn outcome should be treated as a hard failure. Given that `cmdRunAdvance` already returns exit 0 when `result.Advanced == true`, propagating even a partial error would change behavior in a visible way. This is flagged for a conscious decision rather than a required fix, but it should be documented in the handler's comment.

**1.2 `logw` parameter position inconsistency (minor)**

The `NewSpawnHandler` signature is `(querier, spawner, logw)`. The analogous `NewLogHandler` signature in `handler_log.go` (by extension pattern) likely follows `(writer, ...)`. If this project adopts `io.Writer` as the first or last argument consistently, `logw` should match. Not a blocking issue but worth checking against the other handler constructors for consistency.

**1.3 `AgentSpawnerFunc` comment is accurate and sufficient**

The doc comment on `AgentSpawnerFunc` is precise. No change needed.

---

## 2. runtrack/store.go — UpdateAgentDispatch

### Overall

The implementation is structurally identical to `UpdateAgent`, which is the right model to follow. Error prefix is consistent with the module's naming style (`"agent update dispatch: %w"`).

### Findings

**2.1 Error prefix style inconsistency — verb-noun ordering (minor)**

All other error prefixes in `store.go` use a noun-object pattern:

```
"agent add: %w"
"agent update: %w"
"agent get: %w"
"agent list: %w"
"artifact add: %w"
```

`UpdateAgentDispatch` uses `"agent update dispatch: %w"`. This is fine but slightly breaks from the terse two-segment style. `"agent dispatch update: %w"` would be more consistent with how the entity comes first. Alternatively, keep `"agent update: %w"` since callers can distinguish by the operation context. Low priority.

**2.2 `updated_at` is correctly updated**

The `SET dispatch_id = ?, updated_at = ?` correctly timestamps the record. This matches `UpdateAgent`. Good.

**2.3 Rows-affected check is correct**

Using `RowsAffected()` to detect not-found is the established pattern in this file. The double `fmt.Errorf` for the two possible errors from `ExecContext` and `RowsAffected` are both properly wrapped with `%w`. No issue.

**2.4 Empty dispatchID not validated (minor, by-design)**

`UpdateAgentDispatch(ctx, agentID, "")` will succeed and write an empty string into `dispatch_id`. Given the column appears to be nullable in `AddAgent` (it accepts `nil`), storing an empty string differs semantically from `NULL`. Whether this matters depends on downstream queries. The existing `GetAgent` returns `dispatchID` as a `*string` via `nullStr`, which will return a non-nil pointer to `""` for an empty string, not `nil`. If callers use `agent.DispatchID != nil` to test existence of a dispatch link, an empty-string DispatchID would be a latent bug.

If `dispatchID` is always meant to be a real ID, add a guard:

```go
if dispatchID == "" {
    return fmt.Errorf("agent update dispatch: dispatchID must not be empty")
}
```

This is a narrow correctness risk, present only if `UpdateAgentDispatch` is ever called with an empty string, which the current wiring in `run.go` (line 279) does not do.

---

## 3. cmd/ic/run.go — Closure adapter in cmdRunAdvance

### Overall

The closure is correctly capturing `run`, `rtStore`, `dStore`, and `ctx` from the enclosing function. The pattern is appropriate for wiring dependencies that are local to a command invocation.

### Findings

**3.1 `ctx` captured in closure is correct but should be documented (minor)**

The closure passed to `AgentSpawnerFunc` captures `ctx` from `cmdRunAdvance`'s outer scope:

```go
spawner := event.AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
    agent, err := rtStore.GetAgent(ctx, agentID)
```

Note that the closure parameter `ctx` shadows the outer `ctx`. This is correct Go — the inner `ctx` parameter is the one passed at call time via `SpawnByAgentID`. This is fine and expected. No change needed, but it may confuse readers who see the closure uses `ctx` expecting the outer one. A brief comment would help:

```go
// spawner is called with a fresh context from the notifier at event time.
spawner := event.AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
```

**3.2 Dispatch config re-use silently ignores Get error (correctness gap — medium)**

At line 253:

```go
if agent.DispatchID != nil {
    prior, err := dStore.Get(ctx, *agent.DispatchID)
    if err == nil && prior.PromptFile != nil {
```

The `err` from `dStore.Get` is only consumed in the `if err == nil` guard. If the lookup fails for a reason other than not-found (database error, schema mismatch, etc.), the code silently falls through to the convention-based prompt path. The intent appears to be "if we can't read the prior dispatch, use the convention path," which is a reasonable fallback, but a DB error should at least be logged:

```go
if agent.DispatchID != nil {
    prior, err := dStore.Get(ctx, *agent.DispatchID)
    if err != nil {
        fmt.Fprintf(os.Stderr, "[spawn] could not read prior dispatch %s: %v\n", *agent.DispatchID, err)
    } else if prior.PromptFile != nil {
        opts.PromptFile = *prior.PromptFile
        // ...
    }
}
```

Without this, a persistent DB error would cause all spawns to fall back to the convention path silently, which could produce confusing behavior when the `.ic/prompts/` directory doesn't exist either.

**3.3 Empty PromptFile error message includes agentID but not agent.Name (minor)**

At line 270:

```go
return fmt.Errorf("spawn: agent %s has no prompt file and no name for convention lookup", agentID)
```

When `agent.Name` is nil and `opts.PromptFile` is empty, this error is returned. Including the agent type would help the operator diagnose the issue:

```go
return fmt.Errorf("spawn: agent %s (type=%s) has no prompt file and no name for convention lookup", agentID, agent.AgentType)
```

**3.4 `dispatch.Spawn` error is returned unwrapped (minor)**

At line 274-276:

```go
result, err := dispatch.Spawn(ctx, dStore, opts)
if err != nil {
    return err
}
```

All other errors in this closure are wrapped with context. The `dispatch.Spawn` error is returned bare, which breaks the `%w` chain convention used elsewhere in the file. Prefer:

```go
if err != nil {
    return fmt.Errorf("spawn: %w", err)
}
```

**3.5 Notifier subscription order is functional but the comment describes intent well**

The three subscriptions (`"log"`, `"hook"`, `"spawn"`) are added in a sensible order. The comments are accurate. No issue.

**3.6 `dispatchRecorder` closure captures `ctx` from outer scope (intentional, noted)**

Unlike the spawner, `dispatchRecorder` at line 223 captures the outer `ctx` directly (not through a parameter). This means all dispatch events during a run advance share the same context, which is correct for this use case. Consistent with how `phaseCallback` is written.

---

## 4. runtrack/store_test.go — UpdateAgentDispatch tests

### Overall

Two tests were added: `TestStore_UpdateAgentDispatch` (happy path: set a dispatch ID) and `TestStore_UpdateAgentDispatch_NotFound` (error sentinel). Both follow the file's conventions exactly and use the established `setupTestStore` / `createHelperRun` helpers correctly.

### Findings

**4.1 Missing test: overwrite existing dispatch ID**

`TestStore_UpdateAgentDispatch` sets a dispatch ID on an agent that starts with none. There is no test for overwriting an existing dispatch ID (i.e., calling `UpdateAgentDispatch` twice). This is relevant because the spawn adapter always calls it after a new dispatch is created:

```go
return rtStore.UpdateAgentDispatch(ctx, agentID, result.ID)
```

If an agent is re-spawned, the second call must succeed and produce the new ID. A test case would be:

```go
func TestStore_UpdateAgentDispatch_Overwrite(t *testing.T) {
    store, d := setupTestStore(t)
    ctx := context.Background()
    createHelperRun(t, d, "testrun1")

    id, _ := store.AddAgent(ctx, &Agent{RunID: "testrun1", AgentType: "codex"})

    if err := store.UpdateAgentDispatch(ctx, id, "dispatch-first"); err != nil {
        t.Fatalf("first UpdateAgentDispatch: %v", err)
    }
    if err := store.UpdateAgentDispatch(ctx, id, "dispatch-second"); err != nil {
        t.Fatalf("second UpdateAgentDispatch: %v", err)
    }

    got, _ := store.GetAgent(ctx, id)
    if got.DispatchID == nil || *got.DispatchID != "dispatch-second" {
        t.Errorf("DispatchID = %v, want %q", got.DispatchID, "dispatch-second")
    }
}
```

**4.2 `GetAgent` return values silently ignored in tests (inherited pattern)**

In several existing tests (and the new ones follow the same pattern), the `GetAgent` call after `UpdateAgentDispatch` ignores the error:

```go
got, _ = store.GetAgent(ctx, id)
```

This is inherited from the existing test style in the file. If `GetAgent` fails, `got` will be nil and the subsequent field access (`got.DispatchID`) will panic. The fix is to use `require` semantics:

```go
got, err = store.GetAgent(ctx, id)
if err != nil {
    t.Fatalf("GetAgent after UpdateAgentDispatch: %v", err)
}
```

The existing tests have this same pattern, so fixing it here without fixing it everywhere is inconsistent. Either fix all occurrences or accept the pattern. Flagged as low priority.

**4.3 Test names are consistent with the rest of the file**

`TestStore_UpdateAgentDispatch` and `TestStore_UpdateAgentDispatch_NotFound` follow the `TestStore_<Method>` and `TestStore_<Method>_<Condition>` naming convention used throughout `store_test.go`. No change needed.

---

## Summary of Findings by Priority

| # | File | Severity | Finding |
|---|------|----------|---------|
| 3.2 | run.go | Medium | Silent discard of `dStore.Get` error on prior dispatch lookup |
| 1.1 | handler_spawn.go | Medium | No signal returned when all spawns fail (design decision needed) |
| 2.4 | store.go | Low-Medium | Empty dispatchID would store `""` instead of NULL; silent semantic difference |
| 3.4 | run.go | Low | `dispatch.Spawn` error returned unwrapped, breaks `%w` chain |
| 4.1 | store_test.go | Low | Missing test for overwriting an existing dispatch ID |
| 3.3 | run.go | Low | Error message omits agent type when no prompt file is found |
| 1.2 | handler_spawn.go | Low | `logw` parameter position — check against other handler constructors |
| 2.1 | store.go | Low | Error prefix `"agent update dispatch"` vs project pattern `"agent X: %w"` |
| 4.2 | store_test.go | Low | Ignored `GetAgent` error in post-update assertions (inherited pattern) |
| 3.1 | run.go | Low | Closure parameter `ctx` shadows outer `ctx` — add comment for clarity |

---

## What Is Working Well

- `AgentSpawnerFunc` is the correct Go pattern for adapting a function to an interface without introducing unnecessary types or indirection.
- `UpdateAgentDispatch` mirrors `UpdateAgent` structurally, giving the store consistent patterns for all mutation operations.
- Error wrapping with `%w` is consistent across `UpdateAgentDispatch` and the new closure, with the one exception noted in 3.4.
- Test coverage covers the two most important cases (success path and not-found sentinel). The setup helpers and helper run creation follow established conventions.
- The closure correctly captures `run.ProjectDir` (an immutable snapshot from before the phase advance) rather than querying it again inside the spawner, avoiding a race.
- Subscription naming (`"log"`, `"hook"`, `"spawn"`) is clear and follows the existing notifier pattern.
