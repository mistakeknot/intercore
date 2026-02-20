# Synthesis Report: Wave 2 Event Bus Quality Gates

**Generated:** 2026-02-18
**Context:** 27 files changed across Go and Bash. Risk domains: concurrency (goroutine hook handler), SQL injection (dynamic column names), shell execution (hook handler), schema migration (v4→v5), data consistency (event recording).
**Mode:** quality-gates
**Output Dir:** `/root/projects/Interverse/infra/intercore/.clavain/quality-gates`

---

## Validation

| Agent File | Structure | Verdict |
|------------|-----------|---------|
| `fd-architecture.md` | Valid (Findings Index + Verdict line) | needs-changes |
| `fd-correctness.md` | Valid (Findings Index + Verdict line) | needs-changes |
| `fd-quality.md` | Valid (Findings Index + Verdict line) | needs-changes |
| `fd-safety.md` | Valid (Findings Index + Verdict line) | safe |

**Validation:** 4/4 agents valid, 0 failed

---

## Verdict Summary

| Agent | Status | Summary |
|-------|--------|---------|
| fd-architecture | NEEDS_ATTENTION | Phase callback omits blocked/paused transitions; event recorder fires outside transaction |
| fd-correctness | NEEDS_ATTENTION | Cursor advances past events on broken-pipe; dispatch recorder discards errors; shell grep bug |
| fd-quality | NEEDS_ATTENTION | Column injection in UpdateStatus; dispatch recorder discards errors; cursor silently resets on DB error |
| fd-safety | CLEAN | Safe to deploy; five low/informational items noted, none block deployment |

---

## Findings (Deduplicated and Categorized)

### P1 — Critical (must fix, blocks gate)

**P1-1: Silent cursor advance on broken-pipe encode failure**
- File: `cmd/ic/events.go` lines 200-209
- Detail: `enc.Encode(e)` errors are ignored in the tail loop. If stdout is a broken pipe mid-batch (e.g., `ic events tail | head -5`), the cursor advances past events that were never emitted to the consumer. Those events are permanently invisible on subsequent tails.
- Fix: Check `enc.Encode` error, break the loop, do not save cursor on failure.
- Convergence: 2/4 agents (C5 HIGH by fd-correctness; Q4 LOW by fd-quality — same code path, severity disagreement resolved in favor of HIGH due to permanent data loss consequence)

---

### P2 — Important (should fix)

**P2-1: AddDispatchEvent error silently discarded in dispatchRecorder**
- File: `cmd/ic/run.go` ~line 423
- Detail: The `dispatchRecorder` closure calls `evStore.AddDispatchEvent(...)` and discards the returned error. During the v4→v5 migration window (table not yet created) or on any transient DB error, the persistent event record is lost while the notifier fires anyway. Dispatch status update commits but no event is recorded — silent event loss.
- Fix: Log error to stderr: `fmt.Fprintf(os.Stderr, "[event] AddDispatchEvent %s: %v\n", dispatchID, err)`. Longer-term: move `AddDispatchEvent` inside `UpdateStatus` transaction.
- Convergence: 3/4 agents (C2 MEDIUM fd-correctness; Q2 MEDIUM fd-quality; A1 MEDIUM fd-architecture)

**P2-2: UpdateFields dynamic column injection — no allowlist in UpdateStatus**
- File: `internal/dispatch/dispatch.go` lines 213-223
- Detail: `UpdateFields` is `map[string]interface{}` with column names string-concatenated directly into SQL (`col + " = ?"`). No allowlist or sanitization exists. Values are safely parameterized, so current internal callers pose no immediate injection risk, but the exported type with no guard sets a dangerous precedent — a future caller with attacker-controlled map keys would produce a SQL structure injection.
- Fix: Add compile-time or runtime allowlist of valid column names checked before building the SET clause.
- Convergence: 2/4 agents (Q1 MEDIUM fd-quality; S2 LOW fd-safety — severity disagreement; MEDIUM adopted)

**P2-3: Shell grep tab-literal bug in lib-intercore.sh cursor wrapper**
- File: `lib-intercore.sh` ~line 2145
- Detail: `grep "^${consumer}\t"` uses literal backslash-t (two characters `\`, `t`), not a real tab character. The cursor list output uses real tabs as field separators. The pattern never matches, causing the cursor wrapper to always return empty string.
- Fix: Use `$'...'` quoting: `grep "^${consumer}"$'\t'` or `grep -F` with a real tab.
- Convergence: 2/4 agents (M3 MEDIUM fd-correctness; S3 LOW fd-safety — severity disagreement; MEDIUM adopted)

**P2-4: Phase callback omits blocked/paused transitions**
- File: `internal/phase/machine.go` lines 71-93, 114-139, 168-170
- Detail: `callback` fires only after `UpdatePhase` succeeds (line 168-170). The `EventBlock` path (lines 114-139) and `EventPause` path (lines 71-93) return early without invoking the callback. Consumers using `ic events tail` see only successful advances — blocked and paused states are invisible. This creates an asymmetry between the event bus view and the database audit trail.
- Fix: Fire callback from block and pause return paths with `e.Advanced = false`, or document explicitly that these are excluded and filter them at the `ListEvents` query level.
- Convergence: 1/4 agents (A2 MEDIUM fd-architecture only)

---

### P3 — Low severity

**P3-1: dispatch_events.dispatch_id missing FK constraint**
- File: `internal/db/schema.sql` lines 113-125
- Detail: `dispatch_id TEXT NOT NULL` has no `REFERENCES dispatches(id)`. Every other child table (`phase_events`, `run_agents`, `run_artifacts`) uses explicit FK references. After `ic dispatch prune`, dispatch_events rows become orphaned. Decision may be intentional (audit log retained after prune) but the schema has no comment documenting this.
- Fix: Either add `REFERENCES dispatches(id) ON DELETE CASCADE` or add a schema comment making orphan-retention intent explicit.
- Convergence: 4/4 agents (A3 LOW fd-architecture; C4 LOW fd-correctness; S5 INFO fd-safety; noted in Q8 fd-quality)

**P3-2: loadCursor silently swallows DB errors, causing event replay**
- File: `cmd/ic/events.go` lines 303-305
- Detail: Any error from `store.Get` causes `loadCursor` to silently return `(0, 0)`, including transient DB errors that are not "not found". A temporary DB failure silently resets cursor to the beginning, causing full event replay for the consumer.
- Fix: Return `(int64, int64, error)` or log the error before defaulting.
- Convergence: 1/4 agents (Q6 LOW fd-quality)

**P3-3: Hook goroutine untracked, no drain on process exit**
- File: `internal/event/handler_hook.go` lines 53-68
- Detail: Goroutine spawned with no `sync.WaitGroup` and no channel. For CLI use case, OS cleans up. For any future in-process embedding, leaks goroutine and subprocess handle. The existing comment claiming the goroutine avoids blocking the single DB connection is misleading — the goroutine does no DB I/O.
- Fix: Accept optional `*sync.WaitGroup` parameter; document the goroutine's actual purpose.
- Convergence: 1/4 agents (C1 LOW fd-correctness)

**P3-4: Hook tests use time.Sleep for goroutine synchronization**
- File: `internal/event/handler_hook_test.go` lines 851, 906, 927
- Detail: Three tests use `time.Sleep(200 * time.Millisecond)`. Non-deterministic under CI load. Adds 600ms to the test run. Known Go test smell.
- Fix: Expose optional `*sync.WaitGroup` in handler or add `runHookSync` test helper bypassing goroutine.
- Convergence: 2/4 agents (M1 LOW fd-correctness; Q5 LOW fd-quality)

**P3-5: SpawnHandler dead code — wired nowhere in production path**
- File: `internal/event/handler_spawn.go`
- Detail: `AgentQuerier`, `AgentSpawner`, `NewSpawnHandler` defined with five unit tests but nothing in `cmd/ic/` subscribes a `SpawnHandler`. No `AgentSpawner` implementation exists. Violates YAGNI discipline documented in project.
- Fix: Remove or add explicit scaffolding comment with follow-up work item reference.
- Convergence: 1/4 agents (A5 LOW fd-architecture)

**P3-6: ListPendingAgentIDs scan error lacks context wrapping**
- File: `internal/runtrack/store.go` line 159
- Detail: `rows.Scan` error returned bare without `%w` wrapping. Every other scan in the package returns `fmt.Errorf("...: %w", err)`. Breaks error chain consistency.
- Fix: `return nil, fmt.Errorf("list pending agents: scan: %w", err)`
- Convergence: 1/4 agents (Q3 LOW fd-quality)

---

### Informational / Improvements

**IMP-1: Schema inconsistency — dispatch_events.created_at uses SQL unixepoch() default**
- Agent: fd-quality (Q8 INFO)
- File: `internal/db/schema.sql` line 594
- Detail: Project CLAUDE.md states TTL computation should be in Go not SQL to avoid float promotion. `dispatch_events` uses SQL DEFAULT `(unixepoch())` while `phase_events` uses Go-supplied value. The float-promotion rationale applies to WHERE comparisons, not DEFAULT values, so this may be intentional — but the design split across tables warrants a comment.

**IMP-2: Add composite index on dispatch_events (run_id, created_at)**
- Agent: fd-architecture (I3)
- Detail: Current separate indexes `idx_dispatch_events_run` and `idx_dispatch_events_created` are suboptimal for the common query filtering by `run_id` and ordering by `created_at`.

**IMP-3: Add dispatch_events prune subcommand**
- Agents: fd-architecture (I2), fd-safety (I4)
- Detail: `dispatches` has `ic dispatch prune --older-than`. `dispatch_events` has no prune path and no FK cascade. Will grow unboundedly.

**IMP-4: Document cursor key format in help text**
- Agent: fd-architecture (A4)
- Detail: `cmdEventsCursorReset` accepts opaque `consumer:runID` key without explaining the format. If `cursor reset` is meant to reset a consumer across all runs, it cannot do so with the current single `Delete` call — a `List` + filter would be needed.

**IMP-5: Document ListEvents OR-empty pattern**
- Agent: fd-quality (Q7 INFO)
- File: `internal/event/store.go` line 50
- Detail: `WHERE (run_id = ? OR ? = '') AND id > ?` is correct but non-obvious. Add comment to prevent future misreading.

**IMP-6: Cap hook stderr buffer**
- Agent: fd-safety (I1)
- File: `internal/event/handler_hook.go`
- Detail: `bytes.Buffer` with no size cap. A noisy hook writes until OOM. Replace with `io.LimitedReader` (4 KB).

**IMP-7: Log Notifier handler errors to stderr**
- Agent: fd-correctness (I3)
- Detail: `Notifier.Notify` returns the first handler error but callers (`dispatchRecorder`, `phaseCallback`) discard it. Add logging for non-nil errors in `--verbose` mode.

---

## Conflicts

**Severity disagreement — enc.Encode issue:**
- fd-correctness: HIGH (C5) — cursor permanently loses events, calling it a broken-pipe data loss
- fd-quality: LOW (Q4) — framed as a write-error handling gap
- Resolution: HIGH adopted. Consequence is permanent event loss visible to consumers; deserves P1.

**Severity disagreement — dispatch_events FK:**
- fd-architecture: LOW; fd-correctness: LOW; fd-safety: INFO
- Resolution: LOW is appropriate — orphan accumulation after prune is a real maintenance concern.

**Overall verdict disagreement:**
- fd-safety rates the implementation "safe" (deploy-safe, informational findings)
- fd-architecture, fd-correctness, fd-quality rate "needs-changes"
- Resolution: needs-changes is correct at the gate level. P1-1 (cursor data loss) and P2-1 through P2-3 (silent event loss, column injection, shell grep bug) require fixes before consumers depend on the event bus.

---

## Overall Verdict

**needs-changes**

Rationale: One P1 finding (cursor permanently advances past events on broken-pipe — permanent data loss for the consumer). Three P2 findings require fixes: silent event loss in dispatchRecorder, column injection pattern in UpdateStatus, and shell grep bug that makes the cursor wrapper return empty. The fd-safety agent's "safe" verdict applies specifically to the security threat model (no injection, no traversal, no privilege escalation) and is not contradicted here; the gate verdict is driven by correctness and functional concerns.

**Gate: FAIL** — `needs-changes` verdict blocks gate until P1 and P2 findings are addressed.

---

## Fix Priority Order

1. **P1-1** — Check `enc.Encode` error in `events.go` tail loop (10 min, 2 lines)
2. **P2-3** — Fix `lib-intercore.sh` grep tab-literal bug (5 min, 1 line)
3. **P2-1** — Log `AddDispatchEvent` error in `run.go` dispatchRecorder (5 min, 2 lines)
4. **P2-2** — Add UpdateFields column allowlist in `dispatch.go` (20 min)
5. **P2-4** — Fire phase callback on block/pause paths in `machine.go` (30 min, design decision needed)
6. **P3-1** — Add FK comment or constraint in `schema.sql` (5 min)
7. **P3-2** — Surface `loadCursor` DB errors in `events.go` (15 min)

---

## Files

- Architecture report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-architecture.md`
- Correctness report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-correctness.md`
- Quality report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`
- Safety report: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-safety.md`
- This synthesis: `/root/projects/Interverse/infra/intercore/docs/research/synthesize-review-findings.md`
