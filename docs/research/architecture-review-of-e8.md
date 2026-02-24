# Architecture Review: E8 Portfolio Orchestration

**Date:** 2026-02-20
**Scope:** Cross-project portfolio runs added to the intercore kernel
**Files reviewed:**
- `internal/portfolio/relay.go`
- `internal/portfolio/dbpool.go`
- `internal/portfolio/deps.go`
- `internal/phase/gate.go` (portfolio additions)
- `internal/phase/store.go` (CreatePortfolio, CancelPortfolio, GetChildren, UpdateMaxDispatches)
- `cmd/ic/run.go` (portfolio create path)
- `cmd/ic/portfolio.go` (relay and dep commands)
- `cmd/ic/dispatch.go` (dispatch limit check)
- `internal/db/schema.sql` (v10 additions)

---

## Summary Verdict

The E8 portfolio design is structurally sound at the macro level: the package boundary is cleanly drawn, the cross-DB polling pattern is appropriate for the SQLite-based kernel, and the gate extension uses the established interface injection pattern correctly. There are no circular imports, no god-module growth, and no layer inversions.

The primary concerns are in the middle layer: the dispatch limit check is a four-level nested read that degrades silently and can be bypassed by running `ic dispatch spawn` without `--scope-id`; the relay writes raw SQL against child databases that have no schema-version guard; and the portfolio run's identity convention (empty `project_dir`) is an implicit sentinel that is not enforced by any type or schema constraint.

---

## 1. Module Boundaries and Coupling

### What was added

A new `internal/portfolio` package was created with three files: `relay.go`, `dbpool.go`, and `deps.go`. A new `cmd/ic/portfolio.go` file adds CLI surface. The `internal/phase` package received four new store methods and one new gate check. The `cmd/ic/dispatch.go` file received a dispatch-limit pre-check.

### Dependency graph

```
cmd/ic/portfolio.go
  └── internal/portfolio   (new)
        ├── internal/phase  (Store, PhaseEvent, IsTerminalStatus, EventCancel)
        └── internal/state  (Store.Get, Store.Set)

cmd/ic/run.go              (existing, extended)
  └── internal/portfolio   (NOT imported — portfolio mode is handled inline)

cmd/ic/dispatch.go         (existing, extended)
  └── internal/phase       (Store.Get — new read path)
  └── internal/state       (Store.Get — new read path)

internal/phase/gate.go     (existing, extended)
  └── PortfolioQuerier     (new interface, no external import)

internal/portfolio/relay.go
  └── phase.Store          (concrete type, not interface)
```

The boundary between `portfolio` and `phase` is well-drawn in the gate direction: `gate.go` defines `PortfolioQuerier` as a narrow interface (`GetChildren` only), and `phase.Store` satisfies it without the gate knowing about the portfolio package. This is the correct direction for a lower-layer defining an interface that an upper layer satisfies.

The relay direction is the inverse: `relay.go` holds a `*phase.Store` concrete field, not an interface. This is acceptable for a single-purpose daemon that owns the full wiring, but it means the relay cannot be tested without a real SQLite database. A `PhaseChildQuerier` interface (just `GetChildren` and `AddEvent`) would allow unit testing with a stub and would cost two lines.

### AddEvent coupling

`relay.go` calls `r.store.AddEvent(ctx, &phase.PhaseEvent{...})` directly. `AddEvent` is a new method on `*phase.Store` added in E8. The relay is the only caller. This is appropriate — the relay exists solely to write portfolio events — but `AddEvent` on `phase.Store` is a weaker abstraction point than the existing `Advance` path. It bypasses the state machine entirely and writes raw events to `phase_events`. This is intentional (the relay is recording observations, not driving transitions), but it should be documented explicitly in the method, since other future callers of `AddEvent` might not know that it skips gate evaluation, the optimistic-concurrency check, and all callbacks.

### Deps store location

`DepStore` (in `internal/portfolio/deps.go`) stores project dependency edges in the `project_deps` table, which lives in the portfolio's own DB. The `project_deps` table references `runs(id)` via a foreign key. This is correct: dependency edges are portfolio metadata and belong in the portfolio DB, not spread across child DBs.

---

## 2. Cross-DB Polling Pattern

### Design choice is sound

The relay opens read-only SQLite connections to child project databases and polls `phase_events` using a per-project cursor stored in the portfolio's `state` table. For a CLI kernel backed by SQLite WAL files, polling is the right approach. SQLite does not support cross-database triggers, and a push-based event bus would require either a network service or a shared memory segment — both of which are out of scope for this kernel layer. The at-cursor-boundary polling with `LIMIT 100` is safe and resumable.

### Schema-version blindness

`queryChildEvents` issues a raw `SELECT id, from_phase, to_phase, event_type FROM phase_events WHERE id > ?` against each child DB. There is no check that the child's schema version matches the relay's expectations. If a child project is running an older version of intercore that has a different `phase_events` schema, the query will silently succeed but return stale or misinterpreted data. At minimum the relay should read `PRAGMA user_version` from each child DB and fail loudly (or skip that child with a warning) if the version is below the minimum required for E8 (v10).

### DBPool has no eviction or TTL

`DBPool` in `dbpool.go` opens one `*sql.DB` per project directory and never closes it until `pool.Close()` is called at relay shutdown. For a long-running relay supervising many child projects, this is a file-descriptor leak proportional to the number of projects. If a child project's DB file is rotated, moved, or replaced (e.g., during a restore from backup), the pooled handle will silently continue reading the old inode. An eviction policy (e.g., close and reopen after N poll cycles, or on connection error) should be added. The simplest fix is to close and reopen on any query error from the pool.

### Cursor storage uses project directory as scope_id

Cursors are persisted as `state.Set(ctx, "relay-cursor", child.ProjectDir, ...)`. The `state` table uses `(key, scope_id)` as a composite key. Using `child.ProjectDir` (an absolute path string) as the scope is correct, but the `state` table has a 1MB value-size limit and a max key length of 1000 characters. On most systems this is fine, but deeply nested project paths could approach the limit. The cursor value is a JSON-quoted integer (`"\"42\""`), which is a double-encoding (JSON string containing a quoted integer string). This is functional but brittle — a single `strconv.FormatInt(cursor, 10)` value stored as a JSON number would be cleaner and consistent with how the state table is used elsewhere.

### Dispatch count write on every poll cycle

At the end of each `poll()` call, `r.stateStore.Set(ctx, "active-dispatch-count", r.portfolioID, ...)` is called unconditionally. This means every poll cycle — even when no events occurred and `totalActive` is unchanged — performs a write to the portfolio DB. This is a minor concern for write amplification but could matter if the relay is running at a short interval with many children.

---

## 3. Portfolio Gate Check (children_at_phase)

### Pattern is correct

The `CheckChildrenAtPhase` gate check in `gate.go` follows the established interface-injection pattern: it receives a `PortfolioQuerier` and calls `GetChildren`. The check is injected only when `run.ProjectDir == ""` (the portfolio-run sentinel), which means non-portfolio runs never hit this code path even if `pq` is wired.

### Portfolio identity is an implicit sentinel

A portfolio run is identified by `project_dir = ""` in the `runs` table. This convention is established in `CreatePortfolio` (line 441: `portfolioID, "", portfolio.Goal, ...`) and checked in `gate.go` (`isPortfolio := run.ProjectDir == ""`). There is no column, flag, or schema-level constraint that enforces this. Any code that creates a run with an empty `project_dir` outside `CreatePortfolio` would be silently treated as a portfolio run by the gate. A Boolean `is_portfolio` column with `NOT NULL DEFAULT 0` in the schema, set to `1` only by `CreatePortfolio`, would make this explicit and queryable. The current approach works but will become fragile if the sentinel is replicated in new code paths.

### pq nil-safety in cmdRunAdvance

In `cmd/ic/run.go` line 453-457, `phase.Advance` is called with `store` as the `pq` argument (the `*phase.Store` implements `PortfolioQuerier`). This means every `ic run advance` call — for both portfolio and regular runs — passes a non-nil `pq`. For regular runs, the `isPortfolio` check prevents `CheckChildrenAtPhase` from being injected into the rule set, so the pq value is never used. This is safe but means the gate receives a legitimate `GetChildren` implementation even for runs that will never need it. There is no defensive concern here, but it is worth noting that the nil-safety contract documented in machine.go (`// pq may be nil for non-portfolio runs`) is not exercised in practice.

### Gate does not auto-advance the portfolio

The relay records informational events; it does not drive portfolio phase transitions. A portfolio run must be advanced externally via `ic run advance <portfolio-id>`, at which point the `children_at_phase` gate check will fire. This is the correct design for the kernel layer (mechanism not policy), but it means nothing automatically advances the portfolio when all children catch up. Operators or Clavain scripts must poll the portfolio run and advance it. This should be documented in AGENTS.md as an explicit operational requirement.

---

## 4. Design Patterns

### Established patterns correctly followed

| Pattern | Location | Status |
|---------|----------|--------|
| Interface injection for cross-package queries | `PortfolioQuerier` in gate.go | Correct |
| Narrow interface per use-case | `PortfolioQuerier` has exactly one method | Correct |
| Single SQLite connection per DB (`SetMaxOpenConns(1)`) | `dbpool.go:52` | Correct |
| Read-only mode for cross-DB access | `?mode=ro` in DSN | Correct |
| Optimistic concurrency on phase transitions | `UpdatePhase WHERE phase = ?` | Unchanged, correct |
| Transaction wrapping multi-row portfolio creation | `CreatePortfolio` uses `BeginTx` | Correct |
| CancelPortfolio cascades in single transaction | `UPDATE runs WHERE parent_run_id = ?` | Correct |

### Anti-pattern: four-level nested guard for dispatch limit

The dispatch limit check in `cmd/ic/dispatch.go` is:

```go
if opts.ScopeID != "" {
    if run, err := phaseStore.Get(ctx, opts.ScopeID); err == nil && run.ParentRunID != nil {
        if parent, err := phaseStore.Get(ctx, *run.ParentRunID); err == nil && parent.MaxDispatches > 0 {
            if payload, err := stateStore.Get(ctx, "active-dispatch-count", *run.ParentRunID); err == nil {
                if count, err := strconv.Atoi(countStr); err == nil && count >= parent.MaxDispatches {
                    // block
                }
            }
        }
    }
}
```

This is four levels of error-as-nil-is-okay nesting, where any failure at any level silently degrades to "allow spawn." The comment acknowledges this: "If state entry doesn't exist (no relay running), degrade gracefully — allow spawn." Graceful degradation is the right default, but the current structure makes it difficult to distinguish intentional bypass (relay not running) from bugs (wrong scope-id, DB read failure). The four reads also add latency to every dispatch spawn when a scope-id is present. This block should be extracted into a helper function `checkPortfolioDispatchLimit(ctx, scopeID) (bool, error)` with explicit error returns and a single `// relay not running or limit not set — allow` fallthrough.

### Anti-pattern: all children share the same goal

In `cmdRunCreate` (run.go lines 185-195), each child run is created with `Goal: goal` — the same goal as the portfolio. Children have no individual goals. This may be intentional for E8 (parallel identical runs across projects), but it means `ic run status <child-id>` will show the same goal text as the portfolio, making it hard to distinguish children in `ic run list`. A `--child-goal=` flag or deriving the child goal from the project directory name would improve observability at low cost.

---

## 5. Schema Analysis (v10)

### New table: project_deps

```sql
CREATE TABLE IF NOT EXISTS project_deps (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    portfolio_run_id    TEXT NOT NULL REFERENCES runs(id),
    upstream_project    TEXT NOT NULL,
    downstream_project  TEXT NOT NULL,
    created_at          INTEGER NOT NULL DEFAULT (unixepoch()),
    UNIQUE(portfolio_run_id, upstream_project, downstream_project)
);
CREATE INDEX IF NOT EXISTS idx_project_deps_portfolio ON project_deps(portfolio_run_id);
```

This is well-formed. The FK to `runs(id)` is correct. The `UNIQUE` constraint prevents duplicate edges. The index on `portfolio_run_id` supports the relay's `List()` call. No concerns.

### New columns on runs table

`parent_run_id TEXT` and `max_dispatches INTEGER DEFAULT 0` were added to the `runs` table. Both are nullable/defaulted, so existing rows are unaffected by migration. The `CREATE INDEX IF NOT EXISTS idx_runs_parent ON runs(parent_run_id) WHERE parent_run_id IS NOT NULL` is a partial index — efficient and appropriate since most rows will have `NULL` parent.

### No is_portfolio discriminator column

As noted above, portfolio identity is inferred from `project_dir = ''`. This is schema-implicit. The `runs` table has no boolean flag for this. The UNIQUE constraint on `(project, goal, status)` (if present on the non-portfolio path) may be affected — portfolio runs with empty project_dir would collide if two portfolio runs share a goal. This should be verified.

---

## 6. Simplicity and YAGNI Assessment

### Relay is appropriately scoped

The relay is a single-purpose long-running process that polls, relays events, and counts active dispatches. It does not attempt to advance runs, make scheduling decisions, or manage child lifecycles. This is the right scope for the kernel layer. No speculative extensibility points were added.

### DepStore is appropriately minimal

`DepStore` provides `Add`, `List`, `Remove`, `GetDownstream`, and `GetUpstream`. All five methods have concrete callers in E8 (`Add`/`Remove`/`List` via CLI; `GetDownstream`/`GetUpstream` are used in relay's dependency propagation logic). No YAGNI concern.

### UpdateMaxDispatches is a single-field setter

`UpdateMaxDispatches` sets `max_dispatches` on a run. It is called from one place (not currently visible in the read paths above, but implied by the `--max-dispatches` flag flow). If this is only called at creation time, the field could be set inline in `CreatePortfolio` rather than requiring a separate update call post-creation. If it is intended for runtime adjustment, the setter is correct.

---

## Findings Summary

### Must-fix

**F1. Schema-version blindness in relay** (`internal/portfolio/relay.go:181`)
The relay issues raw SQL against child databases without checking their schema version. If a child runs a pre-v10 intercore binary, the relay will silently misread or fail. Add `PRAGMA user_version` validation in `DBPool.Get` or `queryChildEvents` before trusting the child schema.

**F2. Portfolio identity is an implicit sentinel** (`internal/phase/gate.go:132`, `internal/phase/store.go:441`)
`project_dir = ""` is an undocumented, unconstrained convention used to identify portfolio runs. Add an `is_portfolio INTEGER NOT NULL DEFAULT 0` column to the `runs` table and set it to `1` in `CreatePortfolio`. Update `isPortfolio` in `gate.go` to read this column from the Run struct. This makes the invariant explicit, queryable, and immune to accidental creation of empty-project-dir runs via other code paths.

### Recommended fixes

**F3. Dispatch limit check complexity** (`cmd/ic/dispatch.go:107-126`)
Extract the four-level nested guard into a helper `func checkPortfolioDispatchLimit(ctx context.Context, phaseStore *phase.Store, stateStore *state.Store, scopeID string) (limited bool, err error)`. Return a structured result instead of collapsing all error cases to "allow." Log a warning when the relay-maintained state entry is absent so operators can detect a missing relay process.

**F4. DBPool has no error-based eviction** (`internal/portfolio/dbpool.go`)
If a child DB query fails (connection error, file moved, inode replaced), the pool retains the stale handle and returns it on the next poll cycle. On error from `queryChildEvents` or `countActiveDispatches`, call `p.evict(projectDir)` (remove the handle from the map) so the next poll reopens a fresh connection.

**F5. Double-encoded cursor values** (`internal/portfolio/relay.go:237-238`)
`strconv.Quote(strconv.FormatInt(cursor, 10))` produces `"\"42\""` — a JSON string containing a quoted integer string. Store as a plain JSON number: `json.RawMessage(strconv.FormatInt(cursor, 10))`. Update `loadCursors` to parse with `json.Unmarshal` into an `int64` directly. This is consistent with how integers are stored elsewhere in the state table.

**F6. AddEvent skips state machine — document it** (`internal/phase/store.go`)
Add a comment to `AddEvent` explicitly stating it writes a raw `phase_events` row without gate evaluation, optimistic-concurrency check, or event callbacks. This prevents future callers from mistaking it for a safe alternative to `Advance`.

### Optional cleanup

**F7. Children share portfolio goal** (`cmd/ic/run.go:186-194`)
Consider accepting `--child-goal=` or deriving child goals from the project directory basename. Low-priority but improves `ic run list` readability when debugging portfolio state.

**F8. Relay writes active-dispatch-count unconditionally** (`internal/portfolio/relay.go:165-166`)
Skip the `stateStore.Set` write when `totalActive` is unchanged from the last observed value. Cache the prior value in the `Relay` struct and compare before writing. Reduces write amplification for idle portfolios.

**F9. Relay uses concrete *phase.Store, not interface** (`internal/portfolio/relay.go:27`)
For testability, define a `PortfolioStoreWriter` interface with `GetChildren` and `AddEvent` and use it instead of the concrete type. This is a low-cost improvement that enables unit testing the relay without a real database.

---

## File Reference Index

| File | Key concern |
|------|------------|
| `/root/projects/Interverse/infra/intercore/internal/portfolio/relay.go` | F4 (pool eviction), F5 (cursor encoding), F6 (AddEvent semantics), F8 (write amplification), F9 (testability) |
| `/root/projects/Interverse/infra/intercore/internal/portfolio/dbpool.go` | F1 (schema-version check), F4 (eviction) |
| `/root/projects/Interverse/infra/intercore/internal/phase/gate.go` | F2 (portfolio sentinel at line 132) |
| `/root/projects/Interverse/infra/intercore/internal/phase/store.go` | F2 (sentinel set at line 441), F6 (AddEvent doc) |
| `/root/projects/Interverse/infra/intercore/cmd/ic/dispatch.go` | F3 (dispatch limit nesting at lines 107-126) |
| `/root/projects/Interverse/infra/intercore/cmd/ic/run.go` | F7 (child goals at lines 185-194) |
| `/root/projects/Interverse/infra/intercore/internal/db/schema.sql` | F2 (missing is_portfolio column) |
