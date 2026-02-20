# Architecture Review: 2026-02-18-intercore-run-tracking Plan

**Reviewed:** 2026-02-18
**Plan file:** `/root/projects/Interverse/docs/plans/2026-02-18-intercore-run-tracking.md`
**Codebase root:** `/root/projects/Interverse/infra/intercore/`

---

## Executive Summary

The plan is sound in its DB schema choices and correctly reuses established conventions. Two structural issues deserve attention before implementation begins: the placement of `RunAgent`/`RunArtifact` types inside the `phase` package creates a semantic boundary violation, and `main.go` is already 1726 lines and will breach sustainable reading size with three new command groups added inline. The Task 7 over-separation is a minor sequencing annoyance, not a structural problem. Schema naming is defensible. No unnecessary abstractions are proposed. The plan otherwise lands the right tradeoffs for a CLI-only v1 tool.

---

## P0 — Must Fix (Boundary Violation)

### P0-1: `RunAgent` and `RunArtifact` belong in a new package, not `internal/phase`

**Finding:** The plan places `RunAgent`, `RunArtifact`, their status constants, and their store methods inside `internal/phase`. The `phase` package's stated purpose is the run lifecycle state machine — phase constants, transition logic, complexity-based skip, optimistic concurrency on `UpdatePhase`, and the `PhaseEvent` audit log. Its domain noun is `Run` (the sprint lifecycle object). Agents and artifacts are participants and outputs of a run, not aspects of its phase progression.

**The precedent set by `internal/dispatch` is directly applicable.** Dispatch is also run-adjacent — it tracks individual agent invocations that happen during a run. It got its own package (`internal/dispatch`) with its own `Store`, its own `ErrNotFound`, its own `idChars`/`idLen`/`generateID` copy, and its own type `Dispatch`. The plan's own architecture note says "agents and artifacts are run-scoped (not standalone entities)" as the justification for keeping them in `phase`. But `dispatches` are equally run-scoped (they reference `project_dir` and are filtered by `scope_id` which is used to group run-related dispatches). The distinction offered does not hold.

**Concrete coupling created by the plan as written:**

- `internal/phase/phase.go` gains `AgentActive`, `AgentCompleted`, `AgentFailed` constants. These conflict by name with `dispatch.StatusCompleted` / `dispatch.StatusFailed`. Any consumer reading both packages will import two packages defining `"completed"` with identical string values under different namespaces — confusing, not dangerous, but a naming debt signal.
- `internal/phase/store.go` gains `AddAgent`, `UpdateAgent`, `GetAgent`, `ListAgents`, `AddArtifact`, `ListArtifacts`. This file is currently 328 lines and cleanly scoped to run/phase_event CRUD. Post-plan it will be ~550 lines mixing two distinct entity types in one store, with no structural boundary between them.
- `internal/phase/agent_test.go` (new file) partially mitigates the readability problem but does not fix the boundary: the test still imports `phase` for `RunAgent` — callers will experience the full `phase` namespace even when they only care about agents.

**Smallest viable fix:** Create `internal/runtrack` (or `internal/agent` — see naming note below) as a new package following the same structure as `internal/dispatch`:

```
internal/runtrack/
    runtrack.go     — RunAgent, RunArtifact types + status constants
    store.go        — Store{db *sql.DB}, New(), AddAgent, ListAgents, AddArtifact, ListArtifacts
    store_test.go   — tests for the store
```

The `Store` in `runtrack` calls `phase.New(db).Get(ctx, runID)` for run validation — no circular dependency, since `runtrack` → `phase` (one direction only). Alternatively, skip the cross-package validation in the store and let the FK constraint in SQLite surface the error at the DB layer (which is what the plan's `TestStore_AddAgent_BadRunID` test already relies on).

**Naming choice for the new package:** `runtrack` is descriptive. `agent` is short but collides with the conceptual "agent" used everywhere in the broader ecosystem. `artifact` alone is too narrow. Either `runtrack` or keeping agent + artifact in separate packages (`internal/agent`, `internal/artifact`) are both valid. The single-package `runtrack` is the lower-friction path given the tight FK relationship between the two tables.

**If the team rejects splitting:** At minimum, split `internal/phase/store.go` into `store.go` (run/event CRUD, as now) and `agent_store.go` (agent/artifact CRUD) within the same package. This does not fix the semantic boundary problem but keeps each file's responsibility readable.

---

## P1 — Should Fix Before Merge (Maintainability Risk)

### P1-1: main.go will cross 2100 lines — split the run command group into a separate file

**Finding:** `cmd/ic/main.go` is 1726 lines today. The planned additions are:

- `cmdRunCurrent` — ~30 lines
- `cmdRunAgent` dispatcher + `cmdRunAgentAdd`, `cmdRunAgentList`, `cmdRunAgentUpdate` — ~120 lines
- `cmdRunArtifact` dispatcher + `cmdRunArtifactAdd`, `cmdRunArtifactList` — ~80 lines
- `agentToMap`, `artifactToMap`, `printAgent`, `printArtifact` output helpers — ~60 lines
- Updated `printUsage` — ~15 lines

Estimated post-plan size: ~2030 lines. The file already contains five distinct command groups (`sentinel`, `state`, `dispatch`, `run`, `compat`) plus shared infrastructure (`openDB`, `resolveDBPath`, `validateDBPath`, flag parsing, `printUsage`). Each group averages ~250–350 lines. This is not a bug, but it creates a practical problem: finding and modifying any single command requires navigating a 2000-line file with no file-level orientation.

**The existing dispatch command group is the natural split target.** The `run` command group (lines 1190–1726) and the `dispatch` command group (lines 736–1188) are the two largest, each self-contained. Splitting them into separate files within the same `main` package resolves the navigation problem without any interface changes.

**Smallest viable change:** After implementing the plan, move the run command group (all `cmdRun*` functions + `runToMap`, `eventToMap`, `printRun`, the new `agentToMap`, `artifactToMap`, `printAgent`, `printArtifact`) into `cmd/ic/run.go`. The dispatch command group (`cmdDispatch*` + `dispatchToMap`, `printDispatch`) can similarly move to `cmd/ic/dispatch.go`. `main.go` retains: argument parsing, `main()`, `printUsage()`, `cmdInit`, `cmdVersion`, `cmdHealth`, `cmdCompat`, `cmdSentinel*`, `cmdState*`, and the shared helpers (`openDB`, `resolveDBPath`, `validateDBPath`, `boolStr`). This lands `main.go` at ~750 lines — readable, with clear file-level intent.

**This split requires zero interface changes** because Go allows any number of `.go` files in the same package directory. All functions remain in `package main`, all globals (`flagDB`, `flagJSON`, etc.) remain accessible. The only mechanical risk is ensuring `openDB` and flag variables stay in `main.go` (not in one of the split files).

**Recommended sequencing:** Do the file split as a standalone commit immediately before or after Task 4-6, not interleaved with them. A pure file-move commit with no logic changes is trivially reviewable.

---

## P2 — Structural Observations (Addressable in This PR or Deferred)

### P2-1: Task 7 over-separation is a minor friction point, not a real structural problem

**Finding:** The plan separates `Store.Current()` (Task 7) from `cmdRunCurrent` (Task 4) with an explicit note that they "could be merged into Task 4 if preferred." The reason given is that store methods and CLI commands are "independent concerns."

This is technically correct but produces a misleading dependency graph: Task 4 is listed as needing both Task 2 (agent store methods) and Task 7 (current store method), but the current store method has nothing to do with agents or artifacts — it is a `runs` table query. It belongs with the other run store methods (Create, Get, List, ListActive) in Task 2.

**Fix:** Merge Task 7 into Task 2. `Store.Current()` is a standard run query, follows the exact same pattern as `Store.ListActive()`, and its test belongs in `internal/phase/store_test.go` alongside the existing run store tests. This collapses 10 tasks to 9 and removes a confusing dependency arrow from the graph.

### P2-2: `run_agents` table name is correct; conceptual collision with `dispatches` is manageable but worth documenting

**Finding:** The question raised is whether `run_agents` could collide conceptually with `dispatches`. They represent different things:

- `dispatches`: a tracked process invocation — has a PID, prompt file, output file, model, token counts, exit code. It is the operating system artifact of running an agent.
- `run_agents`: a lightweight membership record — which agents participated in this run, under what label, at what status. It is the orchestration artifact.

A `dispatch_id` foreign key on `run_agents` (proposed in the schema) correctly models the optional link between them. The tables are complementary, not redundant.

**Risk:** The column `agent_type` appears in both `run_agents` (TEXT DEFAULT 'claude') and `dispatches` (TEXT, stores "codex" etc.). These defaults are inconsistent — "claude" vs. "codex" — and the values are not validated. If `run_agents.dispatch_id` references a dispatch record, a consumer could independently check `dispatches.agent_type` and `run_agents.agent_type` and get different strings. This is a data integrity gap that the schema should address: either enforce consistency via a CHECK constraint or document that `run_agents.agent_type` is a free-form label independent of the dispatch record.

**Fix (minimal):** Add a comment in the schema DDL clarifying that `run_agents.agent_type` is a display label, not required to match `dispatches.agent_type`. No schema change needed, but the ambiguity should not persist undocumented.

### P2-3: `generateID` and helper functions are duplicated across packages — acceptable for now

**Finding:** `generateID`, `idChars`, `idLen`, `joinStrings`, `nullStr`, `nullInt64` are copied verbatim between `internal/dispatch/dispatch.go` and `internal/phase/store.go`. Adding a new `internal/runtrack` package (per P0-1) would require a third copy.

**Analysis:** This is intentional duplication — each package is self-contained with no dependency on shared internal utilities. The AGENTS.md for the codebase does not mention a shared `internal/util` or `internal/ids` package. Given the project's explicit CLI-only scope and the small size of these helpers, extraction is not warranted yet. The duplication becomes worth addressing only when a fourth or fifth package copies the same code.

**Recommendation:** Accept the duplication. If a `runtrack` package is created per P0-1, copy the helpers there as done in `dispatch`. Do not extract to a shared package in this PR.

---

## P3 — YAGNI and Simplicity Checks

### P3-1: `UpdateAgent` by ID alone is the right scope — do not add a bulk status-update path preemptively

**Finding:** The plan specifies `UpdateAgent(ctx, id, status string)` — update a single agent by its ID. This is the correct minimal interface. No broader batch update, no filter-by-run-and-status bulk operation. Accepting the plan as-is here.

### P3-2: `ListArtifacts` optional phase filter via `*string` is appropriate

**Finding:** The plan uses `phase *string` as a nullable filter argument. This matches the pattern used by `List(ctx, scopeID *string)` for runs and dispatches — consistent, idiomatic for this codebase. Accepting as-is.

### P3-3: No `GetArtifact` method is proposed — correct

**Finding:** The plan does not include a `GetArtifact(ctx, id)` method, and none of the planned CLI commands require one. The artifact use case is write-once and list — consistent with file output tracking semantics. This is correct YAGNI.

### P3-4: `ic run current` query inlines SQL in the CLI handler (Task 4) rather than using a store method

**Finding:** Task 4's implementation note shows the SQL inline: `SELECT id FROM runs WHERE status = 'active' AND project_dir = ? ORDER BY created_at DESC LIMIT 1`. This only makes sense if Task 7 is skipped and `Current()` is embedded directly in `cmdRunCurrent`. But the plan also defines Task 7 as a proper `Store.Current()` method. These two descriptions are inconsistent — the CLI command should use the store method, not inline SQL. The inline SQL note in Task 4 appears to be a planning artifact left over from an earlier version before Task 7 was extracted.

**Fix:** Remove the SQL snippet from Task 4's description. Task 4 should read: "implement `cmdRunCurrent` calling `store.Current(ctx, projectDir)`". This is purely a plan document clarification, not a code change.

---

## P4 — Minor / Cosmetic

### P4-1: Test file naming for new agent tests

**Finding:** The plan creates `internal/phase/agent_test.go`. If the P0-1 recommendation is accepted and a `runtrack` package is created, this file moves to `internal/runtrack/store_test.go`. If P0-1 is rejected and everything stays in `phase`, the filename `agent_test.go` is acceptable — it parallels `store_test.go` and `machine_test.go` as a clear scope indicator.

### P4-2: `intercore_run_phase` bash wrapper (Task 8) is listed but not needed for scripting

**Finding:** Task 8 adds `intercore_run_phase <run_id>` as a bash wrapper. This is a thin wrapper around `ic run phase <run_id>`, which already prints a single line. The existing `ic run phase` command is already simple enough to call directly in scripts without a wrapper. This is minor over-wrapping but has no architectural consequence. Include it for consistency with the other wrappers or omit it — either is fine.

### P4-3: `lib-intercore.sh` version bump from `v0.2.x` to `v0.3.0` signals minor addition correctly

**Finding:** The plan bumps `lib-intercore.sh` to `v0.3.0`. This is appropriate for a minor feature addition (new command wrappers, no breaking changes). No issue.

---

## Summary Table

| ID | Priority | Description | Action |
|----|----------|-------------|--------|
| P0-1 | Must Fix | `RunAgent`/`RunArtifact` placed in `phase` package violates boundary — creates new package `internal/runtrack` | Create new package before Task 2 |
| P1-1 | Should Fix | `main.go` will hit ~2030 lines — split `cmd/ic/run.go` and `cmd/ic/dispatch.go` | File-move commit alongside or after Tasks 4-6 |
| P2-1 | Low | Task 7 (Current store method) should merge into Task 2 — it is a run query, not agent-adjacent | Merge tasks |
| P2-2 | Low | `run_agents.agent_type` default 'claude' vs `dispatches.agent_type` 'codex' inconsistency needs a comment | Add DDL comment |
| P2-3 | Accept | `generateID` + helpers duplicated — intentional, do not extract yet | No action |
| P3-1 | Accept | `UpdateAgent` scope is correct | No action |
| P3-2 | Accept | `*string` filter pattern for `ListArtifacts` is consistent | No action |
| P3-3 | Accept | No `GetArtifact` is correct YAGNI | No action |
| P3-4 | Clarify | Task 4 inline SQL conflicts with Task 7 store method — remove SQL from Task 4 doc | Plan doc edit |
| P4-1 | Cosmetic | Test file location depends on P0-1 decision | Resolve after P0-1 |
| P4-2 | Cosmetic | `intercore_run_phase` wrapper is redundant but harmless | Optional |
| P4-3 | Accept | Version bump to v0.3.0 is correct | No action |

---

## Recommended Implementation Order (revised)

```
Task 1 (schema v4)
  → NEW: create internal/runtrack package (P0-1 fix, replaces Task 2 placement)
  → Task 3 (unit tests — now in runtrack package)
  → Task 7-merged-into-2: Store.Current() added to internal/phase/store.go
  → Tasks 4, 5, 6 in parallel (CLI commands, using runtrack store)
  → FILE SPLIT: cmd/ic/run.go + cmd/ic/dispatch.go extracted from main.go (P1-1)
  → Task 8 (bash wrappers)
  → Task 9 (integration tests)
  → Task 10 (docs)
```
