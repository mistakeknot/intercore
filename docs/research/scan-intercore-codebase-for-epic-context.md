# Intercore Codebase Scan — Epic Context

**Date:** 2026-02-19
**Purpose:** Quick orientation snapshot — what exists, what's in progress, what's missing.

---

## What's Already Built

### Go Source Structure

**`cmd/ic/`** (CLI entry points):
- `main.go` — arg parsing, shared helpers, global flags (`--db`, `--timeout`, `--verbose`, `--json`)
- `dispatch.go` — `ic dispatch spawn/status/list/poll/wait/kill/prune`
- `run.go` — `ic run create/status/advance/phase/current/agent/artifact/...`
- `gate.go` — `ic gate check/override/rules`
- `lock.go` — `ic lock acquire/release/list/stale/clean`
- `events.go` — `ic events tail/cursor`

**`internal/`** packages:
- `db/` — SQLite WAL connection (single writer, explicit PRAGMAs), schema migration (v5), disk check
- `state/` — scoped key-value store with TTL and JSON validation
- `sentinel/` — atomic throttle guards via `UPDATE ... RETURNING` (no CTE wrapping — modernc limitation)
- `dispatch/` — agent process lifecycle: `dispatch.go` (CRUD, ID gen), `spawn.go` (fork dispatch.sh, prompt hash), `collect.go` (liveness poll, verdict/summary parsing)
- `phase/` — run lifecycle state machine: `phase.go` (types, constants, transition table), `store.go` (CRUD + optimistic concurrency), `machine.go` (Advance + gate evaluation), `gate.go` (GateCondition, GateEvidence, gateRules table, RuntrackQuerier/VerdictQuerier interfaces, EvaluateGate), `errors.go`
- `lock/` — filesystem-based mutex (POSIX mkdir atomicity), stale-break, PID liveness check
- `event/` — event bus: `event.go` (types, source constants), `store.go` (AddPhaseEvent, AddDispatchEvent, ListEvents dual-cursor), `notifier.go` (Subscribe/Notify), `handler_log.go` (stderr logging), `handler_hook.go` (fires `.clavain/hooks/on-event.sh` async), `handler_spawn.go` (SpawnHandler — implemented but NOT wired into CLI)
- `runtrack/` — agent and artifact tracking within runs (`run_agents`, `run_artifacts` tables)

**Supporting files:**
- `lib-intercore.sh` — bash wrappers for all IC subsystems (v0.6.0)
- `test-integration.sh` — end-to-end CLI integration tests (~93 tests)
- `AGENTS.md` — comprehensive operational reference (up-to-date with v5 schema)
- `PHILOSOPHY.md` — north star, doctrine, decision filters
- `docs/product/intercore-vision.md` — v1.5 vision doc (the authoritative scope reference)

### Schema Version

Schema is at v5 (5 migrations: state+sentinels → dispatches → runs → run_agents/artifacts → dispatch_events).

### Test Coverage

17 test files across 9 packages (~130 unit tests + ~93 integration tests):
- `db_test.go` — migration, health check
- `state_test.go` — CRUD, TTL, validation
- `sentinel_test.go` — atomic claim, throttle, prune
- `dispatch_test.go`, `spawn_test.go`, `collect_test.go` — full dispatch lifecycle
- `lock_test.go` — 8 tests, race-detector safe
- `phase_test.go`, `store_test.go`, `machine_test.go`, `gate_test.go` — phase chain, gate evaluation
- `runtrack/store_test.go` — agent/artifact CRUD
- `event/store_test.go`, `notifier_test.go`, `handler_*.go` tests — event bus handlers

---

## Open Work Items (Beads)

`bd list --status=open` returned no intercore-specific beads from the Interverse scope. The active plans visible in `docs/plans/` represent recent executed or in-flight work:

- `2026-02-18-intercore-policy-engine.md` — gate evaluation (appears fully implemented: `gate.go` exists, `ic gate check/override/rules` are in `cmd/ic/gate.go`, wrappers in `lib-intercore.sh`)
- `2026-02-18-intercore-hook-adapter.md` — replacing `/tmp/clavain-*` sentinels in Clavain hooks with IC calls (plan exists; migration research docs exist for 5 hook files)
- `2026-02-18-intercore-run-tracking.md` — agent/artifact tracking (implemented: `runtrack/` package, `run_agents`, `run_artifacts` tables)
- `2026-02-18-intercore-mutex-consolidation.md` — filesystem lock subsystem (implemented: `internal/lock/`)
- `2026-02-17-intercore-state-database.md` — original state DB plan (fully implemented)

---

## What's In Progress / Partially Done

### SpawnHandler — Implemented but Not Wired

`internal/event/handler_spawn.go` exists with a complete `NewSpawnHandler` implementation — queries `ListPendingAgentIDs`, calls `SpawnByAgentID` for each. The `runtrack.Store` implements `ListPendingAgentIDs`. However, `dispatch.Store` does NOT implement `SpawnByAgentID` — only a mock exists in the test file. The handler is **not registered in the Notifier** in any CLI command. Research docs (`review-spawn-handler-wiring-code.md`, `review-spawn-handler-correctness.md`) exist, indicating wiring work is pending.

### Hook Adapter Migration — Research Done, Not Executed

Five migration research docs exist mapping specific Clavain hook temp files to IC sentinel/state calls (`migrate-auto-compound-sh-sentinels.md`, `migrate-auto-drift-check-sh-sentinels.md`, `migrate-auto-publish-sh-sentinel.md`, `migrate-lib-sprint-sh-cache-invalidation.md`, `migrate-session-handoff-sh-sentinels.md`). The plan (`2026-02-18-intercore-hook-adapter.md`) exists. The actual hook file changes in `hub/clavain/hooks/` appear not yet executed.

---

## What's Missing Relative to Vision Scope

The vision doc (`intercore-vision.md` v1.5) defines a roadmap organized as an "Autonomy Ladder":

| Level | Name | Status |
|-------|------|--------|
| 0 | Record | **Done** — runs, phases, dispatches, events, state all persisted |
| 1 | Enforce | **Done** — gates with artifact_exists, agents_complete, verdict_exists; hard/soft tiers; override with audit |
| 2 | React | **Partially done** — event bus exists, HookHandler wired, SpawnHandler scaffolded but not wired; no OS-level event reactor consuming `ic events tail -f` exists yet |
| 3 | Adapt | **Not started** — Interspect integration, structured evidence correlation, routing adjustment feedback loop |
| 4 | Orchestrate | **Not started** — portfolio-level cross-project runs, resource scheduling, token budgets |

### Specific Vision Features Not Yet Built

**From the vision doc's kernel subsystem inventory:**

1. **Token tracking per dispatch** — `dispatches` table has no token columns yet. Vision: "records how many tokens each agent consumed."
2. **Token aggregation per run / budget events** — not in schema.
3. **Discovery subsystem** — "Scored discoveries with confidence-tiered autonomy gates" — no `discoveries` table, no `ic discovery` command.
4. **Rollback primitive** — `SkipPhase(run_id, phase_id, reason, actor)` as an explicit kernel primitive. Currently skip is part of `Advance()` complexity logic, not a separately callable command.
5. **Portfolio / cross-project runs** — no multi-project run grouping, no composite gate evaluation.
6. **Configurable phase chains** — vision says "phase chains are configurable — supplied as an ordered array at `ic run create` time." Currently the 8-phase Clavain chain is hardcoded in `phase.go` constants. No `--phases` flag on `ic run create`.
7. **Artifact content hash** — vision specifies SHA256 hashing; `run_artifacts` table has `path` and `type` but no `content_hash` or `dispatch_id` columns.
8. **Dispatch metadata for cost/quality tradeoffs** — model, sandbox mode, timeout are spawn flags captured at spawn time, but no structured cost reporting (tokens_in, tokens_out, cost_usd).
9. **SpawnHandler wired end-to-end** — `dispatch.Store.SpawnByAgentID` not implemented; handler not registered in `ic run advance`.
10. **Event reactor (Level 2)** — no OS-level `ic events tail -f --consumer=clavain-reactor` process exists; reactions are not automated yet.

### Planned but Unexecuted

- `2026-02-18-intercore-hook-adapter.md` — Clavain hook migration from temp files to IC. The research is done; the bash hook changes are not.
- `docs/plans/2026-02-18-intercore-mutex-consolidation.md` — may already be done (lock subsystem exists); worth verifying against plan.

---

## Summary

**What exists:** A solid Level 0 (Record) + Level 1 (Enforce) kernel. Six subsystems are fully built and tested: state, sentinels, dispatch, lock, phase+gates, event bus, run tracking. ~130 unit tests + ~93 integration tests. Schema at v5.

**What's in progress:** SpawnHandler wiring (interfaces defined, dispatch.Store impl missing), hook adapter migration from temp files (research done, execution pending).

**What's missing:** Everything from Level 2 onward — automated event reactions (SpawnHandler wired end-to-end, OS reactor), token tracking/budgets, discovery subsystem, rollback primitive, portfolio/cross-project runs, configurable phase chains, artifact content hashing. The vision doc is ambitious; the codebase is approximately at the 40% mark of the full scope.
