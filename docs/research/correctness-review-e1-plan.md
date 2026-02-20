# Correctness Review — Intercore E1 Kernel Primitives

**Plan reviewed:** `docs/plans/2026-02-19-intercore-e1-kernel-primitives.md`
**Primary output:** `/root/projects/Interverse/.clavain/quality-gates/fd-correctness.md`
**Reviewer:** Julik (Flux-drive Correctness)
**Date:** 2026-02-19

---

This file is the research copy. The canonical findings are in the quality-gates file above.

## Key Findings (Summary)

Three high-severity correctness defects and two medium-severity ones dominate the review.

### C-01 (HIGH): Migration partial-failure leaves ALTER TABLE non-idempotent

If any ALTER TABLE in the v5→v6 sequence fails (disk error, interrupted process), the transaction rolls back cleanly. But on re-run, the first ALTER TABLE that already succeeded in the previous attempt now fails with "duplicate column name" — because SQLite ALTER TABLE is not idempotent. The plan does not use `IF NOT EXISTS` guards and does not mention this recovery path.

Files: `infra/intercore/internal/db/db.go:103-146` (migration structure), `docs/plans/...:119-136` (ALTER TABLE block).

Fix: Use `ALTER TABLE ... ADD COLUMN IF NOT EXISTS ...` (supported in bundled SQLite 3.37+) or check `PRAGMA table_info` before each ALTER.

### C-02 (HIGH): "All phases skipped" fallthrough teleports to terminal without triggering run completion

The proposed skip-walk in Advance sets `toPhase = chain[len(chain)-1]` when all remaining phases are skipped. The `UpdateStatus(completed)` hook that fires when `toPhase == PhaseDone` (machine.go:167) will not fire for custom chains where the terminal phase is not literally "done". The run lands at terminal phase in `status=active` forever.

Files: `infra/intercore/internal/phase/machine.go:167`, `infra/intercore/internal/phase/phase.go:182-184` (`IsTerminalPhase` hardcoded to `PhaseDone`).

Fix: Replace `if toPhase == PhaseDone` with `if ChainIsTerminal(chain, toPhase)`. Update `IsTerminalPhase` to accept the chain, or deprecate it.

### C-03 (HIGH): Budget dedup via state.Store is at-most-once only in the happy path

The state store entry `budget.warning.<run_id>` is deleted by DB restore, operator cleanup, or TTL expiry. After any of these events, `CheckBudget` re-emits the warning/exceeded event. Downstream consumers (Slack alerts, gate overrides) receive duplicates. The plan does not acknowledge this.

Files: Plan Task 8 pseudocode (`budget.go`), `infra/intercore/internal/state/state.go`.

Fix: Store budget events in `dispatch_events` (durable, append-only, included in backups) rather than the mutable state store. Query for existing budget events before emitting.

### C-04 (MEDIUM): Advance skip-walk loses `EventSkip` audit type — all transitions recorded as `EventAdvance`

The existing code in `machine.go:60-66` sets `eventType = EventSkip` when the advance jumps over intermediate phases. The plan's Task 3 refactor replaces this logic but the proposed code does not set `eventType = EventSkip` when the walk skips phases. All transitions, single-hop or multi-hop, will be recorded as `EventAdvance`.

Fix: After the skip-walk, set `eventType = EventSkip` if `toPhase != chain[fromIdx+1]`.

### C-05 (MEDIUM): `IsTerminalPhase` hardcoded blocks custom chain completion

`IsTerminalPhase(p string) bool` returns `p == PhaseDone`. Custom chains whose terminal phase is not "done" will never trigger `UpdateStatus(completed)` and will never be recognized as terminal by the Advance guard.

Fix: Both call sites in `machine.go` must use `ChainIsTerminal(chain, toPhase)` instead.

---

## Additional Findings

- C-06 (MEDIUM): `COALESCE(SUM(cache_hits), 0)` conflates zero-reported and not-reported, making cache ratio display misleading.
- C-07 (MEDIUM): Skip-walk loop has redundant guards and silently teleports to terminal if `fromPhase` is not in the chain.
- C-08 (LOW): `run_artifacts.dispatch_id` has no FK constraint — `ic dispatch prune` can silently orphan artifact references.
- C-09 (LOW): `hashFile` ignores read errors silently — artifact records with non-existent files get NULL hash with no audit.
- C-10 (LOW): Migration early-return exits without explicit rollback; cosmetically unclear.

Full details, evidence, and fixes are in the quality-gates file.
