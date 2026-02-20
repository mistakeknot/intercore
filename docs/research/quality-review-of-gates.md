# Quality Review — Gate System

**Scope:** cmd/ic/gate.go (NEW), internal/phase/gate.go (NEW), internal/phase/gate_test.go (NEW), internal/phase/machine.go, internal/runtrack/store.go, internal/dispatch/dispatch.go, lib-intercore.sh, test-integration.sh

**Verdict:** needs-changes

## Summary

The gate system is structurally sound. Interface-based decoupling (RuntrackQuerier, VerdictQuerier), the gateRules table, evidence serialisation to JSON in phase_events.reason, and the optimistic-concurrency ordering in cmdGateOverride (UpdatePhase before AddEvent) all follow project conventions and are correctly implemented. The 16-test suite covers all three check types plus dry-run, soft-gate, no-rules, DisableAll, and DB error paths. Three issues require fixing before production use.

## Key Findings (from fd-quality.md)

1. **Q2 MEDIUM — Discarded errors in cmdGateOverride.** `store.AddEvent(...)` and `store.UpdateStatus(...)` return errors that are silently dropped. The AddEvent discard is intentional for crash-safety ordering (documented), but the UpdateStatus drop in the `toPhase == PhaseDone` branch is a silent failure: the run is advanced to done but status stays "active" if the DB write fails. This is the highest-priority fix.

2. **Q1 MEDIUM — strPtr redefined in cmd package.** `cmd/ic/gate.go` defines `strPtr` locally. If any other file in the `cmd/ic` package also needs it, the duplicate will cause a compile error. It should be moved to a shared `cmd/ic/helpers.go` to match how the rest of the package shares small utilities.

3. **Q4 LOW — No nil guard on rt before use in evaluateGate.** When `Priority <= 3` and `rt == nil` is passed by a future caller for a transition that has artifact or agent rules, the code will panic. The current caller convention prevents this, but there is no enforcement at the function boundary. A guard returning a structured error would make the function safe without breaking any existing test.

## Full Findings

Full findings index, evidence, and fix suggestions written to:
`/root/projects/Interverse/infra/intercore/.clavain/quality-gates/fd-quality.md`
