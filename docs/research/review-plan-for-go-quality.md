# Go Quality Review: 2026-02-18-intercore-run-tracking.md

**Reviewed:** 2026-02-18
**Plan file:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-run-tracking.md`
**Codebase files examined:**
- `/root/projects/Interverse/infra/intercore/internal/phase/phase.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/store.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/store_test.go`
- `/root/projects/Interverse/infra/intercore/internal/phase/errors.go`
- `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch.go`
- `/root/projects/Interverse/infra/intercore/internal/dispatch/dispatch_test.go`

---

## Summary

The plan is architecturally sound and mostly follows existing conventions. Three issues
require fixes before implementation begins (P1-P2). Two issues are recommendations that
will improve long-term maintainability (P3). The test naming discrepancy is a minor
alignment issue only.

---

## P1 — Agent Status Constants Break Package Naming Convention

**Severity:** Must fix before implementation.

**Finding:**

The plan proposes these constants in `internal/phase/phase.go`:

```go
const (
    AgentActive    = "active"
    AgentCompleted = "completed"
    AgentFailed    = "failed"
)
```

The existing `phase.go` uses a `Status`-prefixed convention for all lifecycle state
constants:

```go
const (
    StatusActive    = "active"
    StatusCompleted = "completed"
    StatusCancelled = "cancelled"
    StatusFailed    = "failed"
)
```

Every status value in the package already uses the `Status` prefix: `StatusActive`,
`StatusCompleted`, `StatusCancelled`, `StatusFailed`. The `Agent`-prefixed constants
create a second, inconsistent vocabulary for what is semantically the same concept
(lifecycle status strings). Call-site code would have to distinguish `AgentCompleted`
from `StatusCompleted` despite them being identical string values, which is confusing.

Additionally, `AgentActive`, `AgentCompleted`, and `AgentFailed` are a strict subset of
the existing `StatusActive`, `StatusCompleted`, `StatusFailed` constants. There is no
reason to duplicate them.

**Fix:**

Do not introduce `AgentActive`, `AgentCompleted`, `AgentFailed`. The `run_agents` table
should be constrained to a subset of the existing `Status*` constants. In store methods
and CLI validation, reference `StatusActive`, `StatusCompleted`, `StatusFailed` directly.
If the agent lifecycle genuinely needs `StatusFailed` but not `StatusCancelled`, enforce
that at the validation layer (CLI or store), not by creating parallel constants.

If there is a future need for agent-specific states that do not overlap with run status
(e.g., `StatusTimeout`), introduce them at that point with the same `Status` prefix.

---

## P1 — `ErrNotFound` in the Phase Package Is Already Run-Scoped; Reusing It for Agents Is Misleading

**Severity:** Must fix before implementation — affects error handling contracts at call sites.

**Finding:**

`internal/phase/errors.go` declares:

```go
var ErrNotFound = errors.New("run not found")
```

The plan instructs `GetAgent` and `UpdateAgent` to return `ErrNotFound` on a missing
agent row. But `ErrNotFound`'s message is `"run not found"`, not `"agent not found"`.
Any caller (CLI command, integration test) that checks `errors.Is(err, phase.ErrNotFound)`
will receive a semantically ambiguous error — the same sentinel could mean either the run
or the agent was not found.

The dispatch package made the same design choice — its own `ErrNotFound` is
package-local:

```go
// internal/dispatch/dispatch.go
var ErrNotFound = errors.New("dispatch not found")
```

Because dispatch is a separate package, this does not conflict with `phase.ErrNotFound`.
But the new agent store methods live in the `phase` package and will share the existing
sentinel with run methods.

**Fix:**

Add a separate sentinel in `internal/phase/errors.go`:

```go
var ErrAgentNotFound = errors.New("agent not found")
```

Use `ErrAgentNotFound` in `GetAgent` and `UpdateAgent`. Update the plan's acceptance
criteria to test for `ErrAgentNotFound`, not `ErrNotFound`. This preserves
discriminability at CLI call sites that need to distinguish "no run" from "no agent".

There is no need for `ErrArtifactNotFound` unless `GetArtifact` is added later; `ListArtifacts`
returning an empty slice is not an error condition.

---

## P2 — `generateID`, `nullStr`, `nullInt64`, and `joinStrings` Are Duplicated Across Packages With No Centralization Plan

**Severity:** Fix now or document a conscious deferral — do not add a third copy.

**Finding:**

These four private helpers exist verbatim in both `internal/phase/store.go` and
`internal/dispatch/dispatch.go`:

| Helper | phase/store.go | dispatch/dispatch.go |
|---|---|---|
| `generateID()` | lines 27-38 | lines 79-90 |
| `nullStr()` | lines 297-302 | lines 317-322 |
| `nullInt64()` | lines 304-309 | lines 332-337 |
| `joinStrings()` | lines 318-327 | lines 339-348 |

The plan adds new store methods to the `phase` package. Because they live in the same
package as the existing phase helpers, the plan does not add a third copy — the new
methods will call the same package-level functions. On that narrow point, the plan is
correct.

However, the plan's Task 2 says "follow the same patterns as existing `Create`/`Get`/`List`
methods" without flagging that the dispatch package has already diverged and is carrying
dead weight. The MEMORY.md for this project records the `modernc.org/sqlite` quirks
but does not record this duplication. Future packages (if any are added) will copy a
fourth time unless this is addressed.

**Recommendation:**

The plan should add a note (or the implementation should create) an
`internal/sqlutil` package with these four helpers as exported functions. Both `phase`
and `dispatch` packages would then import it, eliminating the duplication. This is a
one-time cleanup that costs ~15 minutes and prevents the pattern from compounding.

If the team consciously accepts the duplication (e.g., to keep packages maximally
self-contained), record that decision in `infra/intercore/AGENTS.md` under Design
Decisions so future implementors do not copy again.

The plan as written does neither — it silently perpetuates the pattern by telling the
implementor to "follow existing patterns" in a codebase where the pattern is already
duplicated.

---

## P3 — Test File Naming: `agent_test.go` vs. `store_test.go` Convention

**Severity:** Minor alignment — does not affect correctness, but diverges from the established pattern.

**Finding:**

The existing phase package test files are:

```
internal/phase/phase_test.go    — tests for phase logic (NextPhase, ShouldSkip, etc.)
internal/phase/store_test.go    — tests for Store methods (Create, Get, UpdatePhase, etc.)
internal/phase/machine_test.go  — tests for the state machine
```

The plan proposes creating `internal/phase/agent_test.go` for the new `AddAgent`,
`GetAgent`, `UpdateAgent`, `ListAgents`, `AddArtifact`, `ListArtifacts`, and `Current`
store methods.

This is a reasonable organization choice, and there is no hard rule against it. But
`store_test.go` already contains all the store method tests and uses `setupTestStore(t)`,
which the plan correctly identifies as the helper to reuse. A separate `agent_test.go`
does not need its own helper setup — both files are in `package phase` and share the
same unexported symbols.

Two options are equally valid:

1. Add the new test functions to the existing `store_test.go` (keeps all store-layer
   tests co-located, minimizes file proliferation).
2. Create `agent_test.go` as a logical grouping for the new entity tests.

Option 2 is what the plan proposes, and it is defensible. However, the plan should
explicitly note that `setupTestStore` is available from `store_test.go` and does not
need to be redefined in `agent_test.go`. If the implementor follows the plan literally
without reading the existing test file, they may inadvertently duplicate `setupTestStore`,
which would cause a compile error (duplicate function in same package).

**Recommendation:**

Add this sentence to Task 3: "Do not redefine `setupTestStore` — it is already declared
in `store_test.go` and is available to all test files in the package."

---

## P3 — Test Function Naming: Plan Proposes Mixed Convention

**Severity:** Minor alignment issue.

**Finding:**

The plan proposes test names in the `TestStore_Method_Condition` form:

```
TestStore_AddAgent_Success
TestStore_AddAgent_BadRunID
TestStore_UpdateAgent_StatusChange
TestStore_UpdateAgent_NotFound
```

The existing `store_test.go` uses a simpler `TestStore_Method` or `TestStore_Verb_Noun`
form without a trailing condition suffix on most tests:

```
TestStore_CreateAndGet       — not TestStore_Create_Success
TestStore_Get_NotFound       — condition suffix used here
TestStore_UpdatePhase        — no suffix
TestStore_UpdatePhase_StaleDetection — condition suffix used here
TestStore_UpdatePhase_NotFound
TestStore_UpdateStatus
TestStore_UpdateStatus_NotFound
TestStore_ListActive
TestStore_ListByScopeID
TestStore_AddEventAndEvents
TestStore_Events_Ordered
TestStore_Events_Empty
```

The `dispatch_test.go` is even more terse: `TestCreateAndGet`, `TestGetNotFound`,
`TestUpdateStatus`, `TestUpdateStatusNotFound` — no `TestStore_` prefix at all.

The existing `phase` package tests do use `TestStore_` prefix consistently. The condition
suffix is used selectively — only when there are multiple meaningful outcomes (e.g.,
`_NotFound`, `_StaleDetection`). A generic `_Success` suffix appears nowhere in the
codebase.

**Fix:**

Rename the plan's proposed test functions to match the existing convention:

| Plan name | Corrected name |
|---|---|
| `TestStore_AddAgent_Success` | `TestStore_AddAgent` |
| `TestStore_AddAgent_BadRunID` | `TestStore_AddAgent_InvalidRun` |
| `TestStore_UpdateAgent_StatusChange` | `TestStore_UpdateAgent` |
| `TestStore_UpdateAgent_NotFound` | `TestStore_UpdateAgent_NotFound` (keep) |
| `TestStore_ListAgents_Empty` | `TestStore_ListAgents_Empty` (keep — meaningful condition) |
| `TestStore_ListAgents_Multiple` | `TestStore_ListAgents` |
| `TestStore_AddArtifact_Success` | `TestStore_AddArtifact` |
| `TestStore_ListArtifacts_FilterByPhase` | `TestStore_ListArtifacts_ByPhase` |
| `TestStore_ListArtifacts_NoFilter` | `TestStore_ListArtifacts` |

---

## P4 — `RunAgent` Type Name: Acceptable But Worth Scrutiny

**Severity:** Informational — no change required unless the team agrees.

**Finding:**

The plan proposes `RunAgent` as the type name for an agent record associated with a run.
The existing parallel type is `Run` (not `PhaseRun`). By analogy, you would expect
`Agent`, not `RunAgent`.

However, `RunAgent` is defensible here because:
1. The entity genuinely is run-scoped — it has no identity outside a run.
2. `Agent` alone is ambiguous in this codebase, which already has `AgentType` as a
   string field on `Dispatch`.
3. `RunArtifact` uses the same prefix consistently.

The 5-second rule: a reader seeing `phase.RunAgent` immediately understands "an agent
record attached to a run, in the phase package." The name passes.

No change recommended, but the plan should not also introduce `AgentActive` etc. — the
`Run`-prefix on the struct is enough disambiguation without also prefixing every constant.

---

## P4 — Task 7 Should Be Merged Into Task 2, Not Listed Separately

**Severity:** Process clarity issue — no code change required.

**Finding:**

Task 7 adds the `Current(ctx, projectDir string) (*Run, error)` store method to
`internal/phase/store.go`. Task 2 adds all other new store methods to the same file.
The plan's own dependency graph shows:

```
Task 1 (schema) → Task 2 (store) → Task 7 (current store method)
```

This implies Task 7 depends on Task 2 being complete first, yet they are both "add store
method to store.go." There is no technical reason to separate them. The note in Task 7
("This is listed as a separate task from Task 4 because the store method and CLI command
are independent concerns") is valid reasoning for separating the CLI command from the
store method, but not for separating two store methods.

The separation creates an unnecessary implementation checkpoint: an implementor would
finish Task 2, run unit tests, then open the same file again for Task 7. Merging them
into Task 2 reduces context switching without any correctness risk.

**Recommendation:** Merge Task 7 into Task 2. Update the dependency graph accordingly.
This reduces the total task count from 10 to 9 and makes the plan more honest about
what is actually one body of work.

---

## Summary Table

| ID | Area | Severity | Finding |
|---|---|---|---|
| P1-A | Naming | Must fix | `AgentActive/Completed/Failed` duplicates `StatusActive/Completed/Failed` — use existing constants |
| P1-B | Error handling | Must fix | `ErrNotFound` message is "run not found" — add `ErrAgentNotFound` for agent lookups |
| P2 | Helper duplication | Fix or document | `generateID`, `nullStr`, `nullInt64`, `joinStrings` duplicated across phase and dispatch — plan should address before adding more copies |
| P3-A | Test organization | Minor | Plan must note that `setupTestStore` already exists in `store_test.go`; do not redefine it |
| P3-B | Test naming | Minor | Drop `_Success` suffixes; align condition suffixes with existing codebase pattern |
| P4-A | Type naming | Informational | `RunAgent` is acceptable; consistent with `RunArtifact` |
| P4-B | Task structure | Process | Merge Task 7 into Task 2 — both are store method additions to the same file |

---

## Concrete Changes to the Plan

### In Task 2 (phase.go types section)

Replace:

```go
const (
    AgentActive    = "active"
    AgentCompleted = "completed"
    AgentFailed    = "failed"
)
```

With no new constants. Reference `StatusActive`, `StatusCompleted`, `StatusFailed` in
store methods and CLI validation.

### In errors.go (add one sentinel)

```go
var ErrAgentNotFound = errors.New("agent not found")
```

Update `GetAgent` and `UpdateAgent` to return `ErrAgentNotFound` instead of `ErrNotFound`.

### In Task 3 (test file note)

Add: "Do not redefine `setupTestStore` — it is declared in `store_test.go` and is
available to all test files in `package phase`."

Rename test functions per the table in the P3-B section above.

### In Task 7

Merge into Task 2. Remove Task 7 from the plan. Update the dependency graph.
