# Quality Review: E8 Portfolio Orchestration (Go Code)

**Date:** 2026-02-20
**Scope:** `internal/portfolio/relay.go`, `internal/portfolio/dbpool.go`, `internal/portfolio/deps.go`, `internal/portfolio/deps_test.go`, `internal/phase/store.go` (portfolio additions), `internal/phase/gate.go` (PortfolioQuerier + children_at_phase), `cmd/ic/run.go` (+211 lines portfolio CLI)
**Project:** Go 1.22, `modernc.org/sqlite`, `SetMaxOpenConns(1)`, single-writer WAL

---

## Summary

The E8 portfolio additions are structurally sound and well-integrated with the existing intercore idioms. Interface design, transaction usage, and connection pooling are all correct. There are five concrete issues to address before this code is considered production-ready: four involve silently discarded errors that violate the project's own error-handling convention, one is a timer allocation bug that causes a small but real memory leak in the relay loop. Two secondary findings — a logic tautology in the gate check and a post-transaction query that may report stale data — are worth fixing for correctness reasons.

---

## 1. Errors Silently Discarded in Hot Paths

**Severity: High**

The project's established convention is explicit error handling with `%w` wrapping. The E8 additions break this in four places.

### 1a. relay.go — AddEvent and saveCursor return values dropped

`relay.go:124`, `relay.go:143`, `relay.go:155`, `relay.go:165`, `relay.go:237`:

```go
// relay.go line 124
r.store.AddEvent(ctx, &phase.PhaseEvent{ ... })  // error dropped

// relay.go line 155
r.saveCursor(ctx, child.ProjectDir, cursor)  // saveCursor itself drops the error from stateStore.Set

// relay.go line 165
r.stateStore.Set(ctx, "active-dispatch-count", r.portfolioID, ..., 0)  // error dropped
```

`saveCursor` is declared `func (r *Relay) saveCursor(...) {}` with no return value, silently eating any DB write failure. If the state table becomes unavailable, cursors silently fall back to zero on the next startup, replaying all events.

**Fix:** Log cursor and event write failures via `r.logw`. They are non-fatal (the relay should continue), but silent data loss is dangerous for the audit trail. At minimum:

```go
func (r *Relay) saveCursor(ctx context.Context, projectDir string, cursor int64) {
    if err := r.stateStore.Set(ctx, "relay-cursor", projectDir,
        json.RawMessage(strconv.Quote(strconv.FormatInt(cursor, 10))), 0); err != nil {
        fmt.Fprintf(r.logw, "[relay] cursor save %s: %v\n", projectDir, err)
    }
}
```

Apply the same pattern to both `AddEvent` calls inside `poll`.

### 1b. store.go:516 — RowsAffected error silently dropped in CancelPortfolio

```go
// store.go line 516
n, _ := result.RowsAffected()
```

Every other `RowsAffected` call in `store.go` checks the error (lines 153, 184, 221, 245, 544). This is the only exception. Under `modernc.org/sqlite` this call does not return errors in practice, but the inconsistency is a maintenance hazard — a future port or driver swap may silently hide a "portfolio not found" case.

**Fix:** Match the project convention:

```go
n, err := result.RowsAffected()
if err != nil {
    return fmt.Errorf("cancel portfolio: rows affected: %w", err)
}
```

### 1c. store.go:424,463 — json.Marshal errors silently dropped in CreatePortfolio

```go
// store.go line 424
b, _ := json.Marshal(portfolio.Phases)

// store.go line 463
b, _ := json.Marshal(child.Phases)
```

The single-project `Create` on line 57 handles this error correctly. The portfolio path suppresses it with `_`. A malformed phases slice would produce a silently truncated or invalid JSON string stored in the DB, causing a `parsePhasesJSON` error on every subsequent read.

**Fix:** Mirror the pattern from `Create`:

```go
b, err := json.Marshal(portfolio.Phases)
if err != nil {
    return "", nil, fmt.Errorf("create portfolio: marshal phases: %w", err)
}
```

### 1d. cmd/ic/run.go — GetChildren errors dropped in two places

```go
// run.go line 284
children, _ := store.GetChildren(ctx, args[0])

// run.go line 722
children, _ := store.GetChildren(ctx, args[0])
```

At line 284 (status display), dropping the error is tolerable — the command still shows the parent run. At line 722 (cancel audit trail), the error should at minimum be logged to stderr, because the cancel event loop silently emits zero child events when the query fails, making the audit trail appear clean when it is not.

**Fix for line 722:**

```go
children, err := store.GetChildren(ctx, args[0])
if err != nil {
    fmt.Fprintf(os.Stderr, "ic: run cancel: warning: get children: %v\n", err)
}
```

---

## 2. Timer Leak in Relay Loop

**Severity: Medium**

`relay.go:74`:

```go
case <-time.After(r.interval):
```

`time.After` allocates a new `time.Timer` on every iteration. The timer's channel is not referenced after the case fires, so the timer is not garbage-collected until it expires — one timer per poll cycle (default 2s) accumulates until the GC runs. In a long-lived relay, this is a steady-state leak that produces measurable heap pressure.

The established Go idiom for a polling loop with cancellation is `time.NewTicker`:

```go
ticker := time.NewTicker(r.interval)
defer ticker.Stop()

for {
    if err := r.poll(ctx, cursors); err != nil {
        fmt.Fprintf(r.logw, "[relay] poll error: %v\n", err)
    }
    select {
    case <-ctx.Done():
        return ctx.Err()
    case <-ticker.C:
    }
}
```

This is a single allocation, stopped deterministically on context cancellation.

---

## 3. Logic Tautology in gate.go children_at_phase Check

**Severity: Medium**

`gate.go:226`:

```go
if IsTerminalStatus(child.Status) && child.Status != StatusActive {
    continue // completed/cancelled children don't block
}
```

`IsTerminalStatus` returns true only for `completed`, `cancelled`, and `failed` — none of which is `StatusActive`. The `&& child.Status != StatusActive` clause is always true when the first clause is true, and the condition could never be false via that path. This is a tautology that survives because it does not cause incorrect behavior, but it will confuse maintainers into thinking `StatusActive` could be terminal.

The intended logic appears to be: "skip children that are terminal OR completed". Given the three terminal statuses, the correct and clear form is:

```go
if IsTerminalStatus(child.Status) {
    continue
}
```

If the intent was specifically to not skip `StatusFailed` children (treating them as still-blocking), the condition should be explicit:

```go
if child.Status == StatusCompleted || child.Status == StatusCancelled {
    continue
}
```

Pick the semantically correct version and remove the tautology.

---

## 4. Post-Transaction GetChildren Reads Stale Data in Cancel

**Severity: Medium**

`cmd/ic/run.go:711–732`:

```go
if err := store.CancelPortfolio(ctx, args[0]); err != nil { ... }

// Record cancel events for portfolio and children
store.AddEvent(ctx, &phase.PhaseEvent{ RunID: args[0], ... })
children, _ := store.GetChildren(ctx, args[0])  // reads AFTER tx commits
for _, c := range children {
    store.AddEvent(ctx, &phase.PhaseEvent{
        RunID:     c.ID,
        FromPhase: c.Phase,  // phase from a post-cancel read
        ...
    })
}
```

`CancelPortfolio` atomically transitions all active children to `cancelled` inside its own transaction. The subsequent `GetChildren` query returns children that are now in `cancelled` status. The `FromPhase` field in the child cancel event records the current (post-cancel) phase correctly, but this pattern means:

1. The cancel event audit for children is emitted outside the transaction that performed the state change. If the process crashes between `CancelPortfolio` returning and the `AddEvent` loop, the DB is in a consistent state (children are cancelled) but the audit trail is missing the cancel events for children.
2. `children, _` discards the query error (covered in Finding 1d), compounding the problem.

**Recommended fix:** Move child cancel event recording into `CancelPortfolio` itself (inside the same transaction), or use a separate "record rollback events in one batch" helper that is transaction-scoped. This matches the existing pattern in `Rollback` (`machine.go`) which records events inside `RollbackPhase`.

---

## 5. DBPool DSN Encodes busy_timeout Inconsistently

**Severity: Low**

`dbpool.go:47`:

```go
dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=busy_timeout%%3D%d", dbPath, p.busyTimeout.Milliseconds())
```

CLAUDE.md documents that DSN `_pragma` is unreliable with the modernc driver: "PRAGMAs must be set explicitly after `sql.Open`". The DBPool uses the DSN pragma approach for child DBs, which is inconsistent with the project's established pattern. The verification probe on line 56 (`SELECT 1`) will silently succeed even if the pragma was ignored, meaning busy_timeout may be 0 for all child connections.

**Fix:** After `sql.Open`, set the PRAGMA explicitly:

```go
db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
if err != nil { ... }
db.SetMaxOpenConns(1)

_, err = db.ExecContext(context.Background(),
    fmt.Sprintf("PRAGMA busy_timeout = %d", p.busyTimeout.Milliseconds()))
if err != nil {
    db.Close()
    return nil, fmt.Errorf("dbpool: set busy_timeout %s: %w", dbPath, err)
}
```

---

## 6. Cursor Encoding Round-Trip Is Unnecessarily Fragile

**Severity: Low**

`relay.go:166` and `relay.go:238`:

```go
json.RawMessage(strconv.Quote(strconv.Itoa(totalActive)))
json.RawMessage(strconv.Quote(strconv.FormatInt(cursor, 10)))
```

And the corresponding decode in `loadCursors`:

```go
var s string
if err := json.Unmarshal(payload, &s); err != nil { continue }
val, err := strconv.ParseInt(s, 10, 64)
```

The encode produces `"\"42\""` (a JSON string containing a decimal number). This works but is unusual — most callers of `state.Set` store a JSON number directly (`json.RawMessage("42")`), which would simplify decode to a single `json.Unmarshal` into `int64`. The current approach requires a two-step: unmarshal to string, parse string to int. It also means the state value is a string, not a number, which breaks schema consistency if any external consumer reads this key expecting a number.

Simpler, consistent alternative:

```go
// encode
payload, _ := json.Marshal(cursor)
r.stateStore.Set(ctx, "relay-cursor", projectDir, json.RawMessage(payload), 0)

// decode
var val int64
if err := json.Unmarshal(payload, &val); err != nil { continue }
cursors[child.ProjectDir] = val
```

---

## 7. Missing Test Coverage for New Store Methods

**Severity: Low**

The `internal/phase/store_test.go` file has 25 test functions covering the pre-existing Store API thoroughly. The four new portfolio methods added in E8 have zero unit tests:

- `CreatePortfolio` — transactional batch insert, unique ID generation for parent and children
- `CancelPortfolio` — cross-row transaction, status guard
- `IsPortfolio` — trivial but used as a branching condition in the CLI
- `UpdateMaxDispatches` — single-row update

The `internal/portfolio/deps_test.go` tests are well-structured and serve as a good template. The same pattern (in-process SQLite with `t.TempDir()`) should be applied for the store methods. At minimum, `CreatePortfolio` and `CancelPortfolio` need table-driven tests covering:

- Portfolio with N children round-trips correctly
- `CancelPortfolio` on a non-existent or already-cancelled run returns an error
- `CancelPortfolio` only cancels active children (not already-completed ones)
- `json.Marshal` failure path for phases (once Finding 1c is fixed)

The relay itself (`relay.go`) has no tests. Given its polling loop and multi-DB state, integration testing is appropriate. A minimal test that constructs two in-process SQLite DBs (one for the portfolio, one for a child) and verifies cursor advance and event relay would catch the cursor encoding and AddEvent error issues.

---

## 8. Naming: EventChildCompleted Mapped from EventCancel Is Misleading

**Severity: Low**

`relay.go:119–121`:

```go
eventType := EventChildAdvanced
if evt.EventType == phase.EventCancel {
    eventType = EventChildCompleted
}
```

A cancelled child maps to `EventChildCompleted`. Cancel and completion are semantically different — a cancelled child was stopped, not finished. From a portfolio consumer's perspective, seeing `child_completed` for a cancelled run will produce incorrect downstream decisions (e.g., advancing the portfolio phase when it should block).

**Fix:** Add a distinct `EventChildCancelled = "child_cancelled"` constant and emit it for cancel events. Consumers that want to unblock on either completion or cancellation can check both; consumers that should block on cancellation will correctly remain blocked.

---

## Positive Observations

These are notable strengths in the E8 implementation that should be preserved:

- **DBPool mutex discipline** — `sync.Mutex` wrapping `handles` map with absolute path enforcement and atomic open+verify is correct. The `Close` pattern (lock held, then re-initialize map) prevents use-after-close races.
- **PortfolioQuerier interface** — keeping the portfolio query interface in `gate.go` alongside the other querier interfaces is consistent with the "accept interfaces" pattern. `phase.Store` satisfies it without a wrapper.
- **CreatePortfolio transaction** — parent + all children inserted atomically with a single `defer tx.Rollback()` guard is the correct pattern. Failure leaves no partial state.
- **deps.go error context** — every error in `DepStore` is wrapped with its operation name (`"add dep: %w"`, `"list deps scan: %w"`, etc.), matching project conventions precisely.
- **children_at_phase gate injection** — detecting portfolio runs by `run.ProjectDir == ""` and injecting the check per-transition without modifying `gateRules` is clean. Custom chain support for portfolio phase matching (via `ChainPhaseIndex`) is correct.
- **deps_test.go helper pattern** — `tempDB` with `t.Cleanup` and in-process SQLite is the right approach and matches the rest of the test suite.

---

## Action Summary

| # | File | Issue | Severity |
|---|------|-------|----------|
| 1a | `internal/portfolio/relay.go` | AddEvent and saveCursor errors silently dropped | High |
| 1b | `internal/phase/store.go:516` | RowsAffected error dropped in CancelPortfolio | High |
| 1c | `internal/phase/store.go:424,463` | json.Marshal errors dropped in CreatePortfolio | High |
| 1d | `cmd/ic/run.go:284,722` | GetChildren errors dropped | High |
| 2 | `internal/portfolio/relay.go:74` | time.After leak, use time.NewTicker | Medium |
| 3 | `internal/phase/gate.go:226` | Tautology in children_at_phase terminal check | Medium |
| 4 | `cmd/ic/run.go:711–732` | Cancel audit events recorded outside transaction | Medium |
| 5 | `internal/portfolio/dbpool.go:47` | DSN pragma unreliable, set explicitly after Open | Low |
| 6 | `internal/portfolio/relay.go:166,238` | Cursor encoding round-trip unnecessarily complex | Low |
| 7 | `internal/phase/store_test.go` | CreatePortfolio/CancelPortfolio have no tests | Low |
| 8 | `internal/portfolio/relay.go:121` | EventCancel mapped to EventChildCompleted (wrong) | Low |
