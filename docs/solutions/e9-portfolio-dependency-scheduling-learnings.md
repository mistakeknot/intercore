---
title: E9 Portfolio Dependency Scheduling Learnings
category: correctness
severity: high
bead: iv-wp62
date: 2026-02-20
tags: [portfolio, gate-evaluation, cycle-detection, toctou, sqlite, intercore]
---

# E9: Portfolio Dependency Scheduling Learnings

Sprint `iv-wp62` — making portfolio dependency graph participate in gate evaluation.

## What Was Built

- **Cycle detection** in `DepStore.Add()` via DFS reachability check (HasPath)
- **Gate integration** — new `CheckUpstreamsAtPhase` gate type blocks downstream child runs when upstream dependencies haven't reached the required phase
- **Topological sort** — Kahn's algorithm for dependency-respecting execution order
- **Portfolio status** — CLI command showing per-child readiness with blocked-by details
- **Integration tests** — full E9 section covering deps, cycles, gates, topo order

## Patterns Discovered

### 1. TOCTOU Race in Check-Then-Insert with SQLite

**Problem:** `HasPath()` (reads the graph) and `INSERT` (writes to it) were separate DB operations. Two concurrent `Add()` calls could both pass the cycle check, then both INSERT, creating a cycle.

**Fix:** Wrap both operations in a `BeginTx` transaction. Introduced a `queryCtx` interface to let the DFS code work with either `*sql.DB` or `*sql.Tx`:

```go
type queryCtx interface {
    QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
    ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
}
```

**Lesson:** Any read-then-write pattern in SQLite needs a transaction, even with `SetMaxOpenConns(1)`. Go's connection pool serializes at the connection level, not the logical operation level.

### 2. Terminal Status Exhaustiveness

**Problem:** `CheckUpstreamsAtPhase` only exempted `StatusCompleted` upstream runs. If an upstream was cancelled or failed, it would block downstream runs forever.

**Fix:** Exempt all terminal states: `StatusCompleted`, `StatusCancelled`, `StatusFailed`.

**Lesson:** When checking "is this run still blocking?", the correct question is "is this run in a terminal state?" not "is this run completed?". The existing `CheckChildrenAtPhase` gate already did this correctly — consistency across gates matters.

### 3. Map Collision in Lookup Construction

**Problem:** `siblingByProject` map used last-write-wins on duplicate `project_dir`. While the schema doesn't prevent this (no UNIQUE constraint on `(parent_run_id, project_dir)`), silent data loss is worse than a loud failure.

**Fix:** First-write-wins with `if _, exists := m[key]; !exists` guard. Chose defensive code over schema migration since the duplicate shouldn't occur in practice (creation deduplicates).

**Lesson:** When building lookup maps from query results, always consider the duplicate key case explicitly — even if "it shouldn't happen."

### 4. Interface Injection for Gate Extension

**Pattern:** Adding a new gate check type (`CheckUpstreamsAtPhase`) required threading a new `DepQuerier` interface through `Advance()` → `EvaluateGate()`. This is the 4th querier interface (after `RuntrackQuerier`, `VerdictQuerier`, `PortfolioQuerier`).

**Concern:** The `Advance()` signature now has 9 parameters. If a 5th gate type needs a new querier, consider grouping queriers into a `GateContext` struct.

### 5. Integration Test Gotcha — Multi-Line CLI Output

**Problem:** `ic run create --projects=A,B` outputs the portfolio run ID on line 1 and child run IDs on subsequent lines. Capturing with `$(...)` gets all lines, which breaks when passed as a single argument.

**Fix:** Pipe through `| head -1` to capture only the portfolio run ID.

**Lesson:** Always check if a CLI command outputs more than one line before using `$(...)` capture.

### 6. Gate Interaction — Artifact Gates Fire Alongside Upstream Gates

**Problem:** Integration test expected upstream gate to pass, but the artifact gate (which fires for every child run) also needed to pass. The test didn't add artifacts for the child runs.

**Fix:** Add artifacts before gate check so the artifact gate passes and the upstream gate can be verified independently.

**Lesson:** Gate evaluation runs ALL applicable rules, not just the one being tested. Integration tests must satisfy all gates, not just the one under test.

## Complexity Calibration

Estimated: C3 (moderate). Actual: C3. The scope was well-bounded (6 plan tasks), but the correctness review caught 3 real issues that added ~30% effort. Gate interaction in integration tests was the biggest surprise.
