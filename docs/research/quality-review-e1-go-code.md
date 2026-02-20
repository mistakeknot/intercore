# Quality Review: Intercore E1 Kernel Primitives (Go Code)

> Full findings: `/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`

## Key Findings

**Verdict: needs-changes** (2 medium, 3 low, 3 info)

1. **MEDIUM — Silent error swallow in budget.Checker.Check** (`internal/budget/budget.go:58-59`): DB errors and context cancellations are collapsed into `nil, nil`, indistinguishable from "run not found". Only `sql.ErrNoRows` should produce a nil result; all other errors should propagate.

2. **MEDIUM — Checker breaks accept-interfaces/return-structs** (`internal/budget/budget.go:40-41`): `dispatchStore *dispatch.Store` and `stateStore *state.Store` are held as concrete types while the PR itself demonstrates the correct pattern with `PhaseStoreQuerier`. Narrow interfaces for both would enable isolated unit testing and maintain project idioms.

3. **LOW — JSON unmarshal errors silently dropped in three call sites** (`internal/phase/store.go`): A corrupted `phases` column falls back to `DefaultPhaseChain` with no diagnostic, potentially producing incorrect phase-progression. Needs either a returned error or a justified explicit comment.

4. **LOW — Budget side-effect in cmdDispatchTokens uses inconsistent stderr prefix** (`cmd/ic/dispatch.go:225-239`): `[budget]` prefix differs from all other messages in the file, breaking machine-parsing consistency.

5. **INFO — Skip-walk loop in machine.go uses break instead of error propagation** (`internal/phase/machine.go:1763-1769`): Defensive but leaves a skipped phase in `toPhase` with no audit event if the guard were ever bypassed.
