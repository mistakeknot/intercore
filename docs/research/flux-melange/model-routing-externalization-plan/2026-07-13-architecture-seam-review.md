---
artifact_type: architecture-review
method: fd-architecture (single-lens, codebase-grounded)
target: "/Users/arouth/projects/intercore/docs/plans/2026-07-13-model-routing-externalization.md"
scope: "The one seam the melange loop left untested: intercore-vs-new-repo, roles-not-models, deployment-keyed registry"
date: 2026-07-13
verdict: "intercore-not-new-repo HOLDS; roles-not-models was under-scoped (P0 fixed); registry key sound"
---

# Architecture Seam Review: Model Routing Externalization

Run after the flux-melange pass to close its one open gap: a review probe died mid-run and the melange lenses read the *plan*, not the *codebase*. This pass grounded every finding in `internal/routing/*.go`, `pkg/phase/`, `internal/dispatch/`, and `cmd/ic/`.

## Verdict

The **intercore-not-new-repo decision holds**, on a better cohesion argument than the plan originally stated: intercore already owns `ic dispatch retry --escalate`, which already selects models and writes `routing_decisions` rows. Routing is the other half of the dispatch decision intercore already makes; a new repo would split model choice from the dispatch/escalate that consumes it across a process boundary. And `internal/routing` is a leaf package (two importers, both `cmd/ic/`, zero external, no cycles), so extraction later is a mechanical move: low cost-to-reverse is what makes "extract later" credible.

The decision that did **not** hold as written: "roles not model names" was cosmetic, because the plan's Phase 2 named only one of three live Claude-scalar orderings.

## Findings (most severe first)

### F1 (P0) — Three Claude-scalar total-order rankings, Phase 2 named one

Live in the tree, all vendor-locked, none deployment-aware:

1. `internal/routing/routing.go` (`ParseModelTier`/`TierFable=4`) + `resolve.go` `applyFloor` (`modelTier >= floorTier`). The one Phase 2 patched.
2. `pkg/phase/phase.go:76` — a second independent `ModelTier(string) int` (haiku=1…opus=3, no fable), a parallel capability truth source the plan never mentioned.
3. `internal/dispatch/escalate.go:50` — `DefaultEscalationPolicy.Ladder = ["sonnet","opus","fable"]`, walked by `nextRungModel` (escalate.go:63-90). **This package does not import `internal/routing`.**

The kill: the two-strikes escalation doctrine (a first-class plan principle, wired with `escalation_expired` events) resolves its next model through #3, a hardcoded Claude-only ladder outside the mechanism being generalized. Escalation is precisely where vector-awareness is mandatory (Sol ahead on terminal, behind on discernment). Generalizing only `resolve.go` ships a system that speaks vectors at `ic route` and `TierFable` at every retry.

**Fix (applied):** Phase 2 now inventories all three FIRST; escalation next-rung is either routed through the decision function or explicitly scoped-out in writing. Acceptance widened.

### F2 (P1) — CLI-only is right in-box, coupling risk on the zklw boundary

CLI-only is correct for Clavain/Codex (they already shell out to `ic`; process-per-decision latency is noise next to an LLM call; a library would be over-engineering, and CLAUDE.md reaffirms CLI-only for v1). The pressure is entirely Phase 7 (Hermes, separate process, separate box). "Static binary is trivially distributable" treats the binary as the unit, but the *decision* depends on binary + registry + policy, three things that version independently and co-locate on a second box only by luck. The plan fingerprints two (`policy_hash`, `registry_as_of`) but not the binary.

**Fix (applied):** added `ic_version` to the decision witness; moved the Phase 7 transport decision (local binary vs RPC) up into the Phase 1 contract, because it determines whether the exit-code/fail-closed contract needs transport-failure semantics.

### F3 (P2) — `@deployment` key is right; "same deployment, two model versions" is unhandled

The key correctly handles "same model, two deployments" (trust/cost are properties of where it runs). The inverse (same key, silent vendor weight reroll: `@api` at 09:00 vs 17:00) is unexpressed. `@api` (vendor-controlled, silently mutable) and `@zklw` (you control redeploys) have categorically different version-stability, and the schema treats them identically. Not a re-key: a one-field add.

**Fix (applied):** per-deployment `version_stability: pinned | vendor-live` in Phase 1 registry; stale-value downgrade weights vendor-live more aggressively.

### F4 (P2) — Extraction trigger is real for in-Go, absent for the likely case

The in-Go trigger is compiler-enforced (`internal/` visibility blocks external import: a genuine trip wire, better than most). But the realistic second consumer is a *process* boundary (Hermes on zklw), which the compiler wall does nothing to signal, and the named "non-Sylveste adoption" trigger has no trip wire and never fires cleanly.

**Fix (applied):** sharpened the trigger to name the process-boundary case (the Hermes/zklw *when*, not *if*) and tied it to the F2 mechanism-version witness.

### Cohesion note — typed-cost is YAGNI on dead code

`EffectiveCost`/`CheapestCapable` (`costs.go`) have zero non-test callers. "Cost is typed three ways" enriches a component with no live consumer.

**Fix (applied):** YAGNI gate in Phase 2: confirm a real decision-time cost consumer before enriching cost.

## Disposition

All findings folded into the plan (Architecture Decision, Design Principles, Phase 1, Phase 2, Risks table), each tagged `fd-architecture F#`. The seam is now adversarially validated; no open review gaps remain before Phase 2 execution planning.
