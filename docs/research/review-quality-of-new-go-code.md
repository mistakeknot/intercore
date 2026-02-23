# Quality & Style Review: New Go Code in intercore

**Scope:** Five files reviewed.
- `internal/portfolio/topo.go`
- `internal/portfolio/topo_test.go`
- `internal/portfolio/deps_test.go`
- `cmd/ic/portfolio.go` (lines 240–380, `cmdPortfolioOrder` and `cmdPortfolioStatus`)
- `internal/phase/gate.go` (lines 262–316, `CheckUpstreamsAtPhase` switch case)

**Date:** 2026-02-20

---

## Overall Assessment

The new code is clean, idiomatic Go that integrates naturally with the existing codebase's conventions. Error wrapping, interface usage, and CLI exit code discipline are all consistent with surrounding code. There are no correctness blockers. The issues below are ordered by severity.

---

## Issues

### 1. Non-deterministic output in `TopologicalSort` — MEDIUM

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/topo.go`, lines 26-30

**Code:**
```go
for node, deg := range inDegree {
    if deg == 0 {
        queue = append(queue, node)
    }
}
```

Map iteration order in Go is randomised per run. The initial seed of zero-in-degree nodes into `queue` is therefore non-deterministic, which produces a different valid topological order on each invocation. For a CLI command (`ic portfolio order`) and a gate check, a stable output is important for:
- Reproducible shell scripts that parse line-by-line positional output.
- Diffing logs between runs.
- Unit tests that assert on a concrete order (which the current tests wisely avoid, but the next developer may not).

**Fix:** Sort the zero-in-degree seeds before seeding the queue, and sort each set of newly-eligible nodes before enqueuing them.

```go
// After collecting zero-in-degree nodes:
sort.Strings(queue)

// Inside the BFS loop, after decrementing in-degree:
var newlyReady []string
for _, next := range downstream[node] {
    inDegree[next]--
    if inDegree[next] == 0 {
        newlyReady = append(newlyReady, next)
    }
}
sort.Strings(newlyReady)
queue = append(queue, newlyReady...)
```

Add `"sort"` to the import. Also sort `downstream[node]` during construction if you want fully stable output regardless of input order — but seeding and enqueue-time sorts are the minimum requirement.

---

### 2. Unchecked error from `s.Add` in test setup — LOW

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps_test.go`, lines 95, 123-124, 143-144, 177-178, 193-194, 208-209

Several test functions call `s.Add(...)` as setup without checking the returned error:

```go
// TestRemoveDep (line 95)
s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")

// TestGetDownstream (lines 123-124)
s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")
s.Add(ctx, "portfolio1", "/proj/a", "/proj/c")

// TestAddDep_TransitiveCycle (lines 177-178)
s.Add(ctx, "portfolio1", "/proj/a", "/proj/b")
s.Add(ctx, "portfolio1", "/proj/b", "/proj/c")
```

If the setup `Add` fails silently, the test body will reach incorrect state and produce a misleading failure (or worse, a false pass). The project's own test style — visible in `TestAddDep` on line 49 — always checks setup calls.

**Fix:** Wrap all setup `Add` calls in a helper or check each one:

```go
mustAdd := func(up, down string) {
    t.Helper()
    if err := s.Add(ctx, "portfolio1", up, down); err != nil {
        t.Fatalf("setup Add(%s→%s): %v", up, down, err)
    }
}
mustAdd("/proj/a", "/proj/b")
mustAdd("/proj/b", "/proj/c")
```

This pattern also makes the test intent clearer by visually distinguishing setup from assertion.

---

### 3. Unused import suppression anti-pattern — LOW

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps_test.go`, line 234

```go
// Suppress unused import warning — time is used by the Dep struct in deps.go
var _ = time.Now
```

The `time` import is listed in the import block but is not used anywhere in the test file itself. The comment incorrectly attributes usage to `deps.go` — the test file's imports are independent of the non-test file. The right fix is to remove `"time"` from the test file's imports entirely. If `time.Time` or another `time` symbol is needed for test assertions in the future, add it back then.

```go
// Remove from import block:
"time"

// Remove entirely:
var _ = time.Now
```

---

### 4. `cmdPortfolioStatus` re-reads upstreams using phase index comparison — correctness note — LOW

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/portfolio.go`, lines 342-358

```go
childIdx := phase.ChainPhaseIndex(childChain, child.Phase)

for _, upstream := range upstreamMap[child.ProjectDir] {
    ...
    upIdx := phase.ChainPhaseIndex(upChain, upRun.Phase)
    if upIdx < childIdx {
        cs.Ready = false
        ...
    }
}
```

This logic compares a child's phase index against its upstream's phase index using each run's own chain. If an upstream run uses a different phase chain than the child (custom `--phases` on one but not the other), the indices are not comparable — index 2 in chain A is not the same phase position as index 2 in chain B.

The gate check in `gate.go` (`CheckUpstreamsAtPhase`, lines 298-303) correctly evaluates the upstream against a specific target phase (`rule.phase`), not against the child's current phase index. `cmdPortfolioStatus` should mirror that logic: check whether each upstream has passed the downstream's current phase by name, not by index.

This is a logic gap rather than a crash bug, but it will silently mislead the operator in mixed-chain portfolios.

**Suggested fix:** Look up the child's current phase name and check whether the upstream has passed it:

```go
childPhase := child.Phase
for _, upstream := range upstreamMap[child.ProjectDir] {
    upRun, ok := childByProject[upstream]
    if !ok {
        continue
    }
    if upRun.Status == phase.StatusCompleted {
        continue
    }
    upChain := phase.ResolveChain(upRun)
    targetIdx := phase.ChainPhaseIndex(upChain, childPhase)
    if targetIdx < 0 {
        // upstream chain doesn't include this phase — treat as ahead
        continue
    }
    upIdx := phase.ChainPhaseIndex(upChain, upRun.Phase)
    if upIdx < targetIdx {
        cs.Ready = false
        cs.BlockedBy = append(cs.BlockedBy, fmt.Sprintf("%s (at %s)", upstream, upRun.Phase))
    }
}
```

---

### 5. `json.NewEncoder(os.Stdout).Encode(...)` leaks encoder — informational

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/portfolio.go`, lines 270, 365

```go
json.NewEncoder(os.Stdout).Encode(order)
json.NewEncoder(os.Stdout).Encode(statuses)
```

This pattern appears throughout `cmd/ic/` (it is the project convention), so this is not a new regression. The encoder is never stored, which means if `Encode` is called more than once in a future refactor, a new encoder would be created each time (losing any accumulated state). For a one-shot CLI command this is harmless, but it is worth noting the convention is inconsistent with `encoding/json` best practice. No action required unless the calling pattern changes.

---

## Strengths

**`internal/portfolio/topo.go`**
- Kahn's algorithm is the correct choice: O(V+E), detects cycles cheaply, well understood.
- The cycle error message includes both counts (`%d nodes, %d sorted`), making debugging straightforward.
- The function signature `([]Dep) ([]string, error)` matches the "accept concrete types at boundaries" approach the codebase uses elsewhere.
- Zero allocation for the empty-input case.

**`internal/portfolio/topo_test.go`**
- Tests cover the meaningful topological cases: linear chain, diamond (multiple paths to one node), disjoint forest, empty input, and cycle detection.
- Assertions use index comparison rather than exact order, which is correct for a non-deterministic but order-preserving sort — though fixing finding #1 above would make the tests both more robust and simpler to reason about.
- The cycle test checks the error string for `"cycle"` rather than an exact message, which is appropriately loose.

**`internal/portfolio/deps_test.go`**
- `TestHasPath` uses a table-driven pattern with named fields — exactly right for this kind of multi-case reachability test.
- `tempDB` helper is clean: uses `t.TempDir()`, registers `t.Cleanup`, and sets `MaxOpenConns(1)` per project convention.
- Covers self-loop, direct cycle, transitive cycle, and false-positive (diamond shape) — strong coverage for the add-dep guard path.

**`internal/phase/gate.go` — `CheckUpstreamsAtPhase` case**
- The `nil` guard for `dq` and `pq` at the top of the case (`cond.Result = GateFail`, `allPass = false`) matches how `CheckChildrenAtPhase` handles a missing querier — consistent with the existing switch structure.
- Uses `qerr` as the error variable name to avoid shadowing the outer `err` — a subtle but correct choice.
- The `behindDetails` slice builds a human-readable detail string that mirrors the `CheckChildrenAtPhase` format.
- The `continue` when the phase is not in the upstream's chain (`targetIdx < 0`) is correctly permissive — an upstream running a shorter chain is not a blocker.

**`cmd/ic/portfolio.go` — `cmdPortfolioOrder` and `cmdPortfolioStatus`**
- Exit code discipline: `3` for usage error, `2` for runtime error — consistent with all other commands in the file.
- Error messages include the command prefix (`"ic: portfolio order: ..."`) consistently.
- `childStatus` struct is defined locally within the function, keeping the type scoped to where it is used — appropriate given no other consumer exists yet.
- The tabular plain-text output uses `%-40s %-20s %-12s %-6s` column formatting, consistent with other `ic` list commands.

---

## Summary Table

| Finding | File | Severity | Action |
|---------|------|----------|--------|
| Non-deterministic topo sort output | `topo.go:26-30` | Medium | Sort seeds and newly-ready nodes before enqueue |
| Unchecked setup `Add` calls in tests | `deps_test.go:95,123-124,143-144,177-178,193-194,208-209` | Low | Add `mustAdd` helper or inline error check |
| Unused import suppressed with `var _ = time.Now` | `deps_test.go:234` | Low | Remove `"time"` import and the suppression line |
| Phase index comparison breaks with mixed chains in `cmdPortfolioStatus` | `portfolio.go:342-358` | Low | Evaluate upstream against child's phase name, not index |
| Inline `json.NewEncoder` (existing pattern) | `portfolio.go:270,365` | Informational | No action now; note for future refactor |
