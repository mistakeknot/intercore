# Architecture Review: Intercore Policy Engine Plan
## Plan: /root/projects/Interverse/docs/plans/2026-02-18-intercore-policy-engine.md
## Reviewer: Flux Architecture & Design Reviewer
## Date: 2026-02-18

---

## Summary

The plan replaces a gate stub in `internal/phase/machine.go` with real database-backed
evaluation. The proposed implementation adds new query methods to `phase.Store` that
reach directly into `run_artifacts`, `run_agents`, and `dispatches` — tables owned by
`runtrack` and `dispatch` packages. This creates a cross-package dependency hidden
inside a method receiver, not surfaced in the import graph. Three of the four questions
posed by the task have clear answers; all three point in the same direction: the coupling
is real, the fix is achievable without a full package extraction, and the `gateRules` map
abstraction is appropriate for this stage.

---

## 1. Does phase.Store querying runtrack tables violate package boundaries?

**Yes, and it is the most significant structural problem in the plan.**

The existing package structure enforces clean horizontal ownership:

```
internal/runtrack/   owns: run_agents, run_artifacts
internal/dispatch/   owns: dispatches
internal/phase/      owns: runs, phase_events
```

The CLI layer in `cmd/ic/run.go` is already the place where both `phase` and `runtrack`
are composed — it imports both packages and operates against them using a shared
`*sql.DB`. That composition point is correct by the existing conventions.

The plan's proposed `CountArtifacts`, `CountActiveAgents`, and `HasVerdict` methods
on `phase.Store` cross that boundary by querying tables the phase package does not own.
The violation is not just conceptual — it has practical consequences:

**Index coupling.** The plan explicitly notes it "uses existing indexes" from runtrack
(`idx_run_artifacts_phase`, `idx_run_agents_status`) and dispatch
(`idx_dispatches_scope`). If runtrack or dispatch schema changes (column renamed, index
dropped, FK semantics change), `phase.Store` silently breaks. The breakage would not be
detectable at compile time.

**Ownership confusion for `HasVerdict`.** The `HasVerdict` method has a particularly
tangled path: it calls `s.Get()` to retrieve the run's `scope_id`, then branches on
whether `scope_id` is nil to decide whether to query `dispatches` or `run_artifacts`.
This means `phase.Store` is now implementing dispatch/verdict policy logic. The branching
fallback (`type = 'review'` artifact check when no scope_id exists) is especially
fragile — it encodes an assumption about review artifact naming conventions that belongs
in `runtrack` or `dispatch`, not `phase`.

**The `run_agents` query treats run_agents as a phase-level concept.** Agents in
`runtrack` exist independently of phase; the `executing → review` gate treats agent
completion as a phase prerequisite. The concept "all agents for this run are done" is
`runtrack`'s business, not `phase`'s.

**The violation cannot be detected by Go's import checker** because all packages share
the same `*sql.DB` handle. The database is a shared mutable resource that bypasses Go's
type system. This makes the boundary violation invisible to tooling and reviewers who
inspect import graphs.

### What the plan gets right

The plan correctly observes that "making evaluateGate a Store method follows the
established pattern where Store owns all DB interactions." This is true of the tables
the phase package does own (`runs`, `phase_events`). The reasoning does not transfer to
tables owned by other packages.

---

## 2. Should gate evaluation be its own package (internal/gate)?

**Not yet, and extracting it now would be premature.**

A separate `internal/gate` package would be the right long-term shape if gate evaluation
ever grows to: (a) be independently testable without the full phase/runtrack context,
(b) be shared by more than one consumer, or (c) accumulate enough logic that it
outgrows a single file. None of those conditions currently hold.

The plan adds one new file (`gate.go`), one public method (`EvaluateGate`), and one
private method (`evaluateGate`). That is proportional to the problem. Extracting a
separate package for this scope would require defining an interface that `phase.Store`
and the gate package communicate through — adding indirection with no current second
consumer.

The right move is to solve the boundary violation within the existing package structure,
not to add a layer on top of it.

**Smallest viable change:** Keep the types and the `gateRules` map in `internal/phase`
(the plan's `gate.go` file). Move the three cross-package queries out of `phase.Store`
and instead pass the query results into `evaluateGate` as parameters. The CLI layer
already has access to both stores; it can gather the counts and pass them in. This
resolves the coupling without extracting a package.

An alternative that preserves the Store method pattern without cross-package queries:
add a `GateInputs` struct that the CLI populates by calling `runtrack.Store` and
`dispatch.Store` before calling `phase.Store.EvaluateGate`. The `EvaluateGate` method
then receives `GateInputs` rather than querying the DB itself. This keeps gate logic in
`phase` but makes the data dependency explicit and verifiable.

```go
// GateInputs carries pre-fetched counts for gate evaluation.
// The caller assembles these by querying runtrack and dispatch stores.
type GateInputs struct {
    ArtifactCounts map[string]int // phase -> count
    ActiveAgents   int
    HasVerdict     bool
}

func (s *Store) EvaluateGate(ctx context.Context, runID string, cfg GateConfig, inputs GateInputs) (*GateCheckResult, error)
```

The CLI's `cmdGateCheck` would:
1. Open DB
2. Create `phase.Store`, `runtrack.Store` (and optionally `dispatch.Store`)
3. Gather `GateInputs` by calling the appropriate store methods
4. Pass `GateInputs` to `phase.Store.EvaluateGate`

This is one additional step in the CLI handler, adds zero new packages, and makes
every cross-domain data dependency traceable through function parameters rather than
hidden in SQL queries.

---

## 3. Is the gateRules map the right abstraction?

**Yes, for the current requirements. The map should stay as-is.**

The `gateRules` map keyed on `[2]string{from, to}` is:
- A single source of truth for all transition requirements
- Readable without understanding execution flow
- Straightforwardly testable (iterate the map, verify each entry)
- Sufficient for the current rule set (seven transitions, three check types)

The plan correctly notes that adding a new check is one line in this table. That is
the right tradeoff at this scope.

**What to avoid.** The plan should not add a configuration layer (DB-stored rules,
YAML/TOML rules files) at this point. The AGENTS.md documents that "Policy config is a
Go map, not a DB table." This is an explicit design decision. The rules are not runtime
data — they encode structural invariants of the phase lifecycle. Storing them in a DB
would make the gate logic operationally fragile and non-auditable.

**One concern with the current map:** The check string constants (`"artifact_exists"`,
`"agents_complete"`, `"verdict_exists"`) are untyped. If a new rule entry mistyped one
of these strings, the gate would silently pass (the `default:` case in the switch does
nothing — it just appends a condition with no result set). Define these as typed
constants:

```go
const (
    CheckArtifactExists = "artifact_exists"
    CheckAgentsComplete = "agents_complete"
    CheckVerdictExists  = "verdict_exists"
)
```

This is a minor hardening, not a required change.

---

## 4. Does adding gate.go to internal/phase create unnecessary coupling?

**The file placement is fine. The method placement is the problem.**

Adding `gate.go` as a new file in `internal/phase` is the right call. The gate types
(`GateCondition`, `GateEvidence`, `GateCheckResult`), the `gateRules` map, and the
`EventOverride` constant all belong in `phase` because they describe phase transition
policy. The `GateConfig` and gate result constants already live there.

The coupling problem is not the file but the three query methods proposed for
`store.go`:
- `CountArtifacts` — belongs on `runtrack.Store`
- `CountActiveAgents` — belongs on `runtrack.Store`
- `HasVerdict` — belongs on `dispatch.Store` (or a purpose-built query helper)

These methods are not wrong in isolation — they are correct implementations of their
queries. The problem is their receiver. Attaching them to `phase.Store` embeds the
cross-package dependency into the domain layer rather than surfacing it at the
composition point (the CLI).

The `TestAdvance_GateTiers` test change proposed in Task 3 confirms this. The test
requires instantiating a `runtrack.Store` to create fixtures that satisfy the phase
gate. That is the test telling you the dependency direction is wrong: phase tests should
not need runtrack to function.

---

## 5. Additional Issues Not Covered by the Four Questions

### HasVerdict fallback is a semantic risk

The `HasVerdict` fallback path (when `scope_id` is nil, check `run_artifacts` for
`type = 'review'`) silently changes the meaning of "has verdict" depending on run
configuration. A run without a `scope_id` would pass the verdict gate by having any
artifact of type `review`, even if no actual verdict was rendered by a dispatch.

This is a correctness concern, not just a style concern. The `review` artifact type is
never defined as a gate-passing criterion in the `gateRules` table — it exists as an
implementation fallback. This creates two paths to gate pass with different semantics
and no visible differentiation in the gate evidence.

Recommended fix: remove the fallback entirely. If a run has no `scope_id`, the verdict
gate should fail explicitly with `"no scope_id: cannot check verdict"` in the condition
detail. Override (via `ic gate override`) is the right path for unusual run
configurations.

### ic gate override writes a phase_event then calls UpdatePhase separately

The plan's `ic gate override` description:
1. Writes a phase_event with `EventOverride`
2. Calls `store.UpdatePhase`

These are two separate DB operations. If the process crashes between them, the event
record exists but the phase did not advance. The correct order is: advance first, then
record the event — matching the existing pattern in `Advance()` (line 119-138 of
`machine.go`). Or better: make `OverrideAdvance` a method on Store that wraps both
operations in a transaction, following the same pattern as the existing `UpdatePhase`
with its optimistic concurrency check.

### Test isolation: setupMachineTest returning *sql.DB couples test layers

Task 3 proposes changing `setupMachineTest` to return `(*Store, *sql.DB, context.Context)`
so tests can create runtrack fixtures. Returning a raw `*sql.DB` from a test helper for
`internal/phase` means phase tests are operationally coupled to the raw schema rather
than to a typed store abstraction.

If this coupling cannot be avoided (because the gate methods are on `phase.Store`), it
is a symptom of the package boundary violation. With the `GateInputs` approach described
above, phase tests would never need `runtrack.Store` or `*sql.DB` directly — they would
construct `GateInputs` inline with known values and call `EvaluateGate` with them.

---

## Decision Summary

| Finding | Severity | Recommendation |
|---------|----------|----------------|
| phase.Store querying runtrack/dispatch tables | Must fix | Use GateInputs struct; gather counts in CLI handler |
| HasVerdict fallback: dual semantics | Must fix | Remove fallback; fail gate explicitly when no scope_id |
| gate override event-before-advance ordering | Must fix | Advance first, event second (or single transaction) |
| Untyped check strings in gateRule | Optional | Define string constants for the three check types |
| internal/gate extraction | Premature | Defer until a second consumer exists |
| gateRules map as config mechanism | Correct | Keep as-is; do not move to DB |
| gate.go placement in internal/phase | Correct | File placement is right |

---

## Recommended Minimal Change to the Plan

Replace Tasks 1 and 2 as follows:

**Task 1 (revised):** Add `CountArtifacts` and `CountActiveAgents` to `runtrack.Store`,
not `phase.Store`. Add `HasVerdict` (without fallback) to `dispatch.Store` or as a
package-level query function in `dispatch`. These are two-line additions to existing
store files with clean ownership.

**Task 2 (revised):** Change `evaluateGate` to accept `GateInputs` rather than querying
the DB. The private `evaluateGate` and public `EvaluateGate` methods stay on
`phase.Store` but take a `GateInputs` parameter. The `Advance()` function signature
changes to accept a `GateInputs` (gathered by the CLI before calling `Advance`), or
`Advance` itself gathers the inputs by accepting a `runtrack.Store` parameter alongside
the existing `phase.Store`. The second option (passing the `runtrack.Store` to
`Advance`) is slightly simpler because it keeps the `Advance` call site in `cmd/ic/run.go`
unchanged in structure.

```go
// In cmd/ic/run.go (cmdRunAdvance):
phaseStore := phase.New(d.SqlDB())
rtStore := runtrack.New(d.SqlDB())
result, err := phase.Advance(ctx, phaseStore, rtStore, id, cfg)
```

```go
// In internal/phase/machine.go:
func Advance(ctx context.Context, store *Store, rt RuntrackQuerier, runID string, cfg GateConfig) (*AdvanceResult, error)
```

Where `RuntrackQuerier` is a minimal interface defined in `internal/phase`:

```go
type RuntrackQuerier interface {
    CountArtifacts(ctx context.Context, runID, phase string) (int, error)
    CountActiveAgents(ctx context.Context, runID string) (int, error)
}
```

For the verdict check, either accept a third `DispatchQuerier` interface parameter or
add `HasVerdict` to `RuntrackQuerier` (since `run_agents` and `run_artifacts` share the
runtrack package). Given `HasVerdict` queries `dispatches`, a second interface is
cleaner:

```go
type VerdictQuerier interface {
    HasVerdict(ctx context.Context, scopeID string) (bool, error)
}
```

This approach:
- Keeps all gate logic and types in `internal/phase` (correct ownership)
- Keeps all DB queries in the package that owns their tables (correct boundary)
- Makes every cross-package data dependency visible in function signatures
- Keeps `phase` tests free of `runtrack.Store` dependencies
- Requires no new packages

The total diff versus the original plan: add two interface definitions to `phase/gate.go`,
change `Advance` and `EvaluateGate` signatures to accept interface parameters, move the
three query implementations to their rightful packages. The CLI handler changes are
minimal — it already holds `*sql.DB` and can instantiate both stores.
