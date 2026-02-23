# Correctness Review: Portfolio Dependency Scheduling Gate Changes

**Reviewer:** Julik (Flux-drive Correctness Reviewer)
**Date:** 2026-02-20
**Scope:** `internal/portfolio/deps.go`, `internal/portfolio/topo.go`, `internal/phase/gate.go` (lines 114-331), `internal/phase/machine.go` (lines 46-50), `cmd/ic/run.go` (lines 454-464)

---

## Invariants Under Review

Before analyzing the code, I state the invariants that must remain true for this system to be correct:

1. **DAG invariant**: `project_deps` must always represent a directed acyclic graph scoped per `portfolio_run_id`. A cycle in that table means deadlock: no child can ever advance.
2. **Gate atomicity**: The decision to block or pass a gate must be based on a consistent snapshot of sibling state. A sibling that advances between the check and the gate record is acceptable; a sibling whose state is observed from two different points-in-time within the same evaluation is not.
3. **Child-run blocking**: A child run with upstream dependencies must never advance past a phase before all its upstreams have reached or passed that same phase index. The gate must be the enforcement point.
4. **Nil safety**: `ParentRunID`, `DepQuerier`, and `PortfolioQuerier` are all optional at different call sites. Every code path that dereferences them must guard against nil.
5. **Failed upstream semantics**: A failed or cancelled upstream should have a defined, documented policy — not accidental behavior from a missing branch.
6. **Duplicate-edge safety**: The schema has a `UNIQUE` constraint on `(portfolio_run_id, upstream_project, downstream_project)`. Application-layer logic must not silently swallow the resulting error as a cycle-detection failure.

---

## Finding 1 (CRITICAL): TOCTOU Race — Cycle Check and INSERT Are Not Atomic

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps.go`, lines 30-52

**The code:**

```go
func (s *DepStore) Add(ctx context.Context, portfolioRunID, upstream, downstream string) error {
    if upstream == downstream { ... }
    // Check for cycles
    reachable, err := s.HasPath(ctx, portfolioRunID, downstream, upstream)
    ...
    if reachable {
        return fmt.Errorf("add dep: cycle detected: ...")
    }
    _, err = s.db.ExecContext(ctx, `INSERT INTO project_deps ...`, ...)
    ...
}
```

**The race:**

Two concurrent callers both want to add edges that would together form a cycle:

```
Goroutine A: Add("p1", A, B)
Goroutine B: Add("p1", B, A)

T1: A calls HasPath(p1, B, A) — returns false (no edges yet)
T2: B calls HasPath(p1, A, B) — returns false (no edges yet)
T3: A executes INSERT A→B — succeeds
T4: B executes INSERT B→A — succeeds

Result: A→B and B→A both exist in project_deps. The DAG invariant is violated.
```

The cycle check (a read across multiple SQL queries) and the INSERT are two separate database round trips with no transaction holding a write lock across them. SQLite's default isolation level is "serializable per statement," not "serializable per multi-statement group." Two concurrent processes using WAL mode can both read the same pre-insert snapshot and both pass their cycle checks before either commit lands.

**This is not a theoretical race.** The `ic portfolio dep add` command is a CLI tool. Multiple agents or orchestrator retries can invoke it concurrently for the same portfolio run. The UNIQUE constraint on the schema prevents duplicate edges but does NOT prevent the B→A case when A→B already exists — those are two distinct rows and both inserts will succeed.

**Severity:** Critical. A cycle in `project_deps` causes permanent deadlock: no child with an upstream dependency on any node in the cycle can ever pass `CheckUpstreamsAtPhase`, and the portfolio run stalls with no error and no visible cause unless someone runs `ic portfolio order` (which would return a cycle-detected error).

**Fix:** Wrap the entire `HasPath` + `INSERT` in an explicit `BEGIN IMMEDIATE` transaction:

```go
func (s *DepStore) Add(ctx context.Context, portfolioRunID, upstream, downstream string) error {
    if upstream == downstream {
        return fmt.Errorf("add dep: upstream and downstream cannot be the same project")
    }
    tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
    if err != nil {
        return fmt.Errorf("add dep: begin tx: %w", err)
    }
    defer tx.Rollback()

    reachable, err := hasPathTx(ctx, tx, portfolioRunID, downstream, upstream)
    if err != nil {
        return fmt.Errorf("add dep: cycle check: %w", err)
    }
    if reachable {
        return fmt.Errorf("add dep: cycle detected: adding %s → %s would create a cycle", upstream, downstream)
    }
    _, err = tx.ExecContext(ctx, `
        INSERT INTO project_deps (portfolio_run_id, upstream_project, downstream_project, created_at)
        VALUES (?, ?, ?, ?)`,
        portfolioRunID, upstream, downstream, time.Now().Unix(),
    )
    if err != nil {
        return fmt.Errorf("add dep: %w", err)
    }
    return tx.Commit()
}
```

`hasPathTx` is a copy of `HasPath` that accepts a `*sql.Tx` instead of using `s.db`. With SQLite and `SetMaxOpenConns(1)`, `BEGIN IMMEDIATE` is sufficient: only one write transaction can be open at a time, so the read-then-write is serialized against other writers.

The topology-based cycle check in `TopologicalSort` (topo.go) is a secondary defense for the `ic portfolio order` command; it does not protect the `Add` path. Do not rely on it for correctness of the write path.

---

## Finding 2 (HIGH): `CheckUpstreamsAtPhase` Skips Failed Upstreams — Policy Is Wrong

**File:** `/root/projects/Interverse/infra/intercore/internal/phase/gate.go`, lines 289-306

**The code:**

```go
for _, upstream := range upstreams {
    upstreamRun, ok := siblingByProject[upstream]
    if !ok {
        continue // upstream project has no child run — not blocking
    }
    if upstreamRun.Status == StatusCompleted {
        continue // completed upstreams don't block
    }
    upstreamChain := ResolveChain(upstreamRun)
    targetIdx := ChainPhaseIndex(upstreamChain, rule.phase)
    if targetIdx < 0 {
        continue
    }
    upstreamIdx := ChainPhaseIndex(upstreamChain, upstreamRun.Phase)
    if upstreamIdx < targetIdx {
        behind++
    }
}
```

**The problem:** `StatusFailed` and `StatusCancelled` are both terminal states but neither is `StatusCompleted`. The code treats them as non-blocking — it falls through into the phase-index comparison. A failed upstream is in its terminal phase (the phase it was at when it failed). `upstreamIdx` will typically be less than `targetIdx`, so `behind++` fires and the downstream run is blocked forever by a failed upstream it cannot unblock.

Compare this with `CheckChildrenAtPhase` at line 238:

```go
if child.Status == StatusCompleted || child.Status == StatusCancelled {
    continue // completed/cancelled children don't block
}
```

The two checks have asymmetric policy: `CheckChildrenAtPhase` treats cancelled as non-blocking, `CheckUpstreamsAtPhase` does not treat failed or cancelled as non-blocking. This inconsistency looks like an oversight, not an intentional decision (there's no comment explaining why a failed upstream should permanently block a downstream).

**Scenario:**
- Upstream run `/proj/api` fails at phase `executing` due to a build error.
- Downstream run `/proj/frontend` is at phase `plan`, gated on upstream reaching `executing`.
- `upstreamIdx` (at `executing`) >= `targetIdx` (at `executing`) — in this case the downstream actually passes. But if the upstream fails at `plan`, the downstream is blocked at `brainstorm` forever: `upstreamIdx=1 < targetIdx=3`.
- The portfolio stalls silently. No alert. No path forward except `ic gate override`.

**Severity:** High. A single child failure that happens before its required phase propagates as a permanent invisible block on all downstream children.

**Fix:** Decide the policy explicitly and encode it. The most defensible options are:

Option A — Failed upstream is non-blocking (let the portfolio proceed, the portfolio gate will catch overall failure):
```go
if upstreamRun.Status == StatusCompleted ||
    upstreamRun.Status == StatusFailed ||
    upstreamRun.Status == StatusCancelled {
    continue
}
```

Option B — Failed upstream blocks with a clear error message:
```go
if upstreamRun.Status == StatusFailed {
    behind++
    behindDetails = append(behindDetails, fmt.Sprintf("%s FAILED", upstream))
    continue
}
if upstreamRun.Status == StatusCancelled {
    continue
}
```

Option A is more consistent with how `CheckChildrenAtPhase` handles cancelled children. Option B makes failure explicit in gate evidence rather than silently infinite. Either is acceptable — the current behavior of treating failed runs as "active but behind" is neither.

---

## Finding 3 (HIGH): `siblingByProject` Map Last-Write-Wins on Duplicate `project_dir`

**File:** `/root/projects/Interverse/infra/intercore/internal/phase/gate.go`, lines 283-286

**The code:**

```go
siblingByProject := make(map[string]*Run)
for _, s := range siblings {
    siblingByProject[s.ProjectDir] = s
}
```

**The problem:** If two child runs share the same `ProjectDir` (which the schema may or may not prevent — `project_dir` has no UNIQUE constraint scoped to `parent_run_id`), the map silently keeps the last one scanned. The upstream dependency check then uses whatever run happened to be last in the `GetChildren` result ordering.

Depending on which run is in the map, the gate can pass when it should block, or block when it should pass. This is a silent correctness failure with no error returned.

**Severity:** High if duplicate `project_dir` per portfolio is possible; Low if the schema or application prevents it. I could not find a UNIQUE constraint on `(parent_run_id, project_dir)` in the runs table from the reviewed schema. If such a constraint is absent, this map build is silently wrong.

**Fix (defensive):**

```go
for _, s := range siblings {
    if existing, dup := siblingByProject[s.ProjectDir]; dup {
        return "", "", nil, fmt.Errorf(
            "gate check: duplicate child run for project %q: %s and %s",
            s.ProjectDir, existing.ID, s.ID,
        )
    }
    siblingByProject[s.ProjectDir] = s
}
```

Alternatively, add a `UNIQUE(parent_run_id, project_dir)` constraint to the `runs` table. Both together is better than either alone.

---

## Finding 4 (MEDIUM): `portfolio status` Compares Indices Across Heterogeneous Chains

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/portfolio.go`, lines 343-358

**The code:**

```go
childChain := phase.ResolveChain(child)
childIdx := phase.ChainPhaseIndex(childChain, child.Phase)

for _, upstream := range upstreamMap[child.ProjectDir] {
    upRun, ok := childByProject[upstream]
    ...
    upChain := phase.ResolveChain(upRun)
    upIdx := phase.ChainPhaseIndex(upChain, upRun.Phase)
    if upIdx < childIdx {
        cs.Ready = false
    }
}
```

**The problem:** Index comparison across two independently resolved phase chains is only meaningful if both chains have the same phase ordering. If a child uses a custom chain `["plan","execute","done"]` (3 phases) and its upstream uses the default chain (9 phases), then `childIdx=1` ("execute") vs `upIdx=3` ("planned") is a nonsensical numeric comparison. "planned" (index 3 in the default chain) does NOT mean the upstream is "ahead of" the downstream at "execute" (index 1 in the custom chain).

This is also present in `CheckUpstreamsAtPhase` in gate.go (lines 297-303), where the same cross-chain index comparison is performed. In the gate case, `targetIdx` is looked up from `upstreamChain` (line 298), which is correct for the upstream — but this means the gate is asking "is the upstream at or past `rule.phase` in its own chain?" which is the right question. However, the `portfolio status` display logic asks "is the upstream's absolute chain index less than the child's absolute chain index?" which has no coherent meaning across different chains.

**The gate logic in gate.go is less wrong** because it uses `ChainPhaseIndex(upstreamChain, rule.phase)` — it checks whether the upstream has reached a specific named phase. The `portfolio status` display command is more wrong because it uses raw indices cross-chain.

**Fix for `portfolio status`:** Compare named phases, not indices:

```go
// Instead of index comparison, check if upstream has reached the same named phase
childPhase := child.Phase
upChain := phase.ResolveChain(upRun)
if !phase.ChainContains(upChain, childPhase) {
    // upstream chain doesn't have this phase at all — can't compare
    continue
}
upChainPhaseIdx := phase.ChainPhaseIndex(upChain, childPhase)
upCurrentIdx := phase.ChainPhaseIndex(upChain, upRun.Phase)
if upCurrentIdx < upChainPhaseIdx {
    cs.Ready = false
    ...
}
```

Or document clearly that this is a known limitation when runs use heterogeneous chains.

---

## Finding 5 (MEDIUM): `CheckUpstreamsAtPhase` Uses `rule.phase` (Target Phase) Not the Current Child's Phase

**File:** `/root/projects/Interverse/infra/intercore/internal/phase/gate.go`, lines 296-303

This is more subtle than it looks. When `CheckUpstreamsAtPhase` is injected (line 145):

```go
rules = append(rules, gateRule{check: CheckUpstreamsAtPhase, phase: to})
```

`rule.phase` is set to `to` — the phase the child is trying to advance **to**. Inside the check:

```go
targetIdx := ChainPhaseIndex(upstreamChain, rule.phase)  // upstream's index of "to"
upstreamIdx := ChainPhaseIndex(upstreamChain, upstreamRun.Phase)  // upstream's current index
if upstreamIdx < targetIdx {
    behind++
}
```

This says: "the upstream is behind if it hasn't yet reached the phase the *downstream* is trying to advance to."

The semantic is: "before you advance to phase X, your upstreams must already be at phase X." This is a strict in-step requirement. It means if A is at `brainstorm` and B depends on A, B cannot advance to `plan` until A is already at `plan`.

This is a legitimate policy but it needs to be explicit in the doc comment, because it is stronger than "upstream must be ahead of downstream." Consider whether the intended policy is:
- Strict in-step: upstream at >= `to` before downstream reaches `to` (current behavior)
- Loose lead: upstream at >= `from` before downstream reaches `to`

If the intent is that upstreams only need to have **started** the phase before the downstream enters it (loose lead), the current implementation is too strict and will cause unnecessary blocking in pipelines where projects have similar but not identical timelines.

No code change needed if strict in-step is intentional — but add a doc comment to `CheckUpstreamsAtPhase` and the rule injection site explaining this policy precisely.

---

## Finding 6 (MEDIUM): `HasPath` DFS — `visited` Set Applied After Reachability Check

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/deps.go`, lines 66-82

**The code:**

```go
for len(stack) > 0 {
    node := stack[len(stack)-1]
    stack = stack[:len(stack)-1]
    if node == to {
        return true, nil
    }
    if visited[node] {
        continue
    }
    visited[node] = true
    downstream, err := s.GetDownstream(ctx, portfolioRunID, node)
    ...
    stack = append(stack, downstream...)
}
```

**The subtle issue:** The reachability check `if node == to` fires before `if visited[node]`. This means if `to` appears multiple times in the graph (via multiple incoming edges), it will be found correctly on the first occurrence. That is correct behavior.

However, the initial frontier is seeded from `GetDownstream(ctx, portfolioRunID, from)` without marking `from` as visited. If there is a cycle already in the database (which cannot happen if `Add` is working correctly, but see Finding 1), the DFS could loop indefinitely — `from` would be pushed back onto the stack through a cycle and the graph traversal would not terminate until the context is cancelled.

**With Finding 1 fixed (transactions preventing cycles), this is not a reachability issue but only a concern for correctness during database corruption recovery.** The defensively correct fix is to mark `from` as visited before starting the DFS:

```go
func (s *DepStore) HasPath(ctx context.Context, portfolioRunID, from, to string) (bool, error) {
    visited := map[string]bool{from: true}  // prevent re-entering from
    initial, err := s.GetDownstream(ctx, portfolioRunID, from)
    ...
}
```

This has no effect on a valid DAG. On a corrupted DAG (cycle exists), it limits infinite loops to cycles not involving `from` itself — a partial defense.

Additionally: the DFS does O(n) SQL queries where n is the number of nodes reachable from `from`. For a large graph, this could be slow and could exhaust the context deadline. An in-memory adjacency-list representation loaded once (similar to how `TopologicalSort` works) would be more efficient. For the current scale (small portfolios), this is acceptable but worth noting.

---

## Finding 7 (LOW): `TopologicalSort` Does Not Include Isolated Nodes

**File:** `/root/projects/Interverse/infra/intercore/internal/portfolio/topo.go`

**The code:** Only nodes that appear in at least one edge are included in `inDegree` and thus in the sort output. A portfolio project that has no declared dependencies (no edges in either direction) will not appear in the topological order output from `ic portfolio order`.

This is not a correctness bug for the gate system (gates work per-run and don't call `TopologicalSort`). But the `ic portfolio order` and `ic portfolio status` commands will produce output that omits independent projects, which may be confusing to operators.

**Fix:** Either document this limitation, or populate `inDegree` from the child runs table:

```go
func TopologicalSortWithNodes(deps []Dep, allProjects []string) ([]string, error) {
    // seed inDegree with all known projects at 0 before processing edges
    for _, p := range allProjects {
        if _, ok := inDegree[p]; !ok {
            inDegree[p] = 0
        }
    }
    // ... rest of Kahn's algorithm unchanged
}
```

---

## Finding 8 (LOW): `CheckUpstreamsAtPhase` Error Path Dereferences `run.ParentRunID` Without Guard

**File:** `/root/projects/Interverse/infra/intercore/internal/phase/gate.go`, lines 262-269

**The code:**

```go
case CheckUpstreamsAtPhase:
    if dq == nil || pq == nil {
        cond.Result = GateFail
        cond.Detail = "no dep/portfolio querier provided"
        allPass = false
        break
    }
    upstreams, qerr := dq.GetUpstream(ctx, *run.ParentRunID, run.ProjectDir)
```

The rule is only injected when `run.ParentRunID != nil && *run.ParentRunID != ""` (line 144). So by the time `CheckUpstreamsAtPhase` executes, `run.ParentRunID` should be non-nil and non-empty.

However, `gateRules` is a package-level map. If someone adds `CheckUpstreamsAtPhase` to `gateRules` as a static rule for a specific transition (forgetting that it was designed to be dynamically injected only for child runs), the dereference at line 269 would panic for any run without a parent.

**Fix:** Add an explicit nil guard inside the `CheckUpstreamsAtPhase` case:

```go
if run.ParentRunID == nil || *run.ParentRunID == "" {
    cond.Result = GateFail
    cond.Detail = "upstreams_at_phase check requires a child run with parent_run_id"
    allPass = false
    break
}
upstreams, qerr := dq.GetUpstream(ctx, *run.ParentRunID, run.ProjectDir)
```

This is defensive programming against future misuse of the static `gateRules` table and has no runtime cost on the happy path.

---

## Finding 9 (LOW): `portfolio dep remove` Does Not Normalize Paths

**File:** `/root/projects/Interverse/infra/intercore/cmd/ic/portfolio.go`, lines 146-181

`cmdPortfolioDepAdd` normalizes upstream/downstream paths via `filepath.Abs` (lines 80-89). `cmdPortfolioDepRemove` does not:

```go
// add:
upstream, err := filepath.Abs(upstream)
...
downstream, err = filepath.Abs(downstream)

// remove:
// (no normalization — paths are used as-is from flags)
```

If the user runs `ic portfolio dep add ./proj/a ./proj/b` from `/root/projects` (stored as `/root/projects/proj/a`), then `ic portfolio dep remove ./proj/a ./proj/b` from the same directory, the remove will find the row. But if the remove is run from a different working directory, the relative paths resolve differently and the `DELETE` will find zero rows, returning a spurious "not found" error.

**Fix:** Apply the same `filepath.Abs` normalization in `cmdPortfolioDepRemove` before calling `depStore.Remove`.

---

## Summary Table

| # | Severity | Location | Issue |
|---|----------|----------|-------|
| 1 | CRITICAL | `portfolio/deps.go:Add` | TOCTOU race between cycle check and INSERT — use `BEGIN IMMEDIATE` transaction |
| 2 | HIGH | `phase/gate.go:CheckUpstreamsAtPhase` | Failed/cancelled upstreams block downstream forever — define policy explicitly |
| 3 | HIGH | `phase/gate.go:CheckUpstreamsAtPhase` | `siblingByProject` map silently last-write-wins on duplicate `project_dir` |
| 4 | MEDIUM | `cmd/ic/portfolio.go:cmdPortfolioStatus` | Cross-chain index comparison is meaningless for heterogeneous phase chains |
| 5 | MEDIUM | `phase/gate.go:CheckUpstreamsAtPhase` | "Strict in-step" upstream policy is undocumented; may be unintentionally strict |
| 6 | MEDIUM | `portfolio/deps.go:HasPath` | DFS does not mark `from` as visited — cycles in corrupted DB cause infinite loop |
| 7 | LOW | `portfolio/topo.go:TopologicalSort` | Isolated nodes (no edges) are omitted from `order` output |
| 8 | LOW | `phase/gate.go:CheckUpstreamsAtPhase` | Missing nil guard on `run.ParentRunID` inside the case body — future panic risk |
| 9 | LOW | `cmd/ic/portfolio.go:cmdPortfolioDepRemove` | Path normalization missing — `filepath.Abs` applied in `add` but not `remove` |

---

## Recommended Action Order

1. **Fix Finding 1 immediately.** The TOCTOU race in `Add()` can corrupt the DAG. Wrap in `BEGIN IMMEDIATE`. Since SQLite with `SetMaxOpenConns(1)` is the design constraint, this fix is straightforward and has no performance cost for the expected call rate.

2. **Fix Finding 2 before enabling portfolio scheduling in production.** Decide the policy: failed upstreams should either block explicitly with a clear error message in gate evidence, or they should be treated as non-blocking. The current behavior (blocking forever with no indication why) is a 3 AM incident waiting to happen.

3. **Fix Finding 3 by adding a schema-level UNIQUE constraint** on `(parent_run_id, project_dir)` in the `runs` table, or add the duplicate-detection error in the gate evaluation path.

4. Findings 4-9 are addressable in a follow-up cleanup pass without urgency.
