---
artifact_type: melange-synthesis
method: flux-melange
target: "/Users/arouth/projects/intercore/docs/plans/2026-07-13-model-routing-externalization.md"
target_description: "Plan to externalize model routing: cross-vendor registry + ic route CLI in intercore, policy-as-data, multi-harness adapters"
goal: "maximize verified novelty×risk surface until dry"
weights: balanced
rounds_run: 4
halt_reason: DRY
total_fusions: 3
emergent_findings: 2
date: 2026-07-13
---

# Flux-Melange Synthesis: Model Routing Externalization Plan

The loop ran 4 heat-steered rounds and halted DRY (round 3 opened only 1 new qualifying cluster). 23 findings, 18 upheld, 0 refuted. Six lenses ran (metrology/calibration-chain, harbor-pilotage/authority-boundary, coppice/rotation-stewardship, scribal-transmission/collation, contradiction-adjudication as a fusion of pilotage×coppice, and vat-dye/deferred-commitment). The single strongest signal is not a design taste call: it is that **the plan describes a mechanism that already partly exists in `internal/routing`, and several of its "new" schema fields collide with or duplicate half-built code the plan never mentions.** The scribal-collation and metrology lenses kept grounding findings in the actual Go source, and that is where the highest-heat surface is.

Re-scoring note: I re-ranked by heat (novelty × risk.product), not severity. Three findings tie at the top on heat=6 (f-006, f-007, f-009), and f-017 (the fusion adjudication) also lands at 6. f-014 and f-022 sit just behind at heat 12 by my re-score (novelty 3 × risk 4), and I promote both: f-014 because it is a ground-truth collision with existing code, f-022 because it is a self-inflicted precedent-drop the plan cites its own PRD for.

---

## 1. Novelty × Risk Frontier

The Pareto front, surfacing both a max-novelty/mid-risk finding and a mid-novelty/max-risk finding. Severity shown for reference only.

### f-006 — Verification gates are named but their execution is unobservable (heat 6; risk 3×2=6; sev P0)
*Lens: fd-pilotage-authority-boundary*

`ic route` emits a required verification gate (e.g. `behavioral-verify` for an elevated-reward-hack target), but enforcement is delegated to each of three independently-authored adapters, and **nothing in the contract or the evidence schema can detect after the fact whether an adapter actually ran the named gate** before accepting executor output. The Phase 5 evidence event types (`executor_failure`, `validator_rejection`, `cost_anomaly`, `constraint_block`) have no `gate_executed` / `gate_skipped` event. The Risks table states the mitigation as a *division of labor* ("gates are router output; enforcement lands in adapters"), which is an assignment, not a check. Blast radius 3: this is the exact reward-hacking control the Sol research motivated, and a silently-skipped gate looks identical to a passed one. **This is the finding to fix first.**

### f-009 — No safe-harbor obligation on router failure; harnesses can silently fall back (heat 6; risk 3×2=6; sev P1)
*Lens: fd-pilotage-authority-boundary*

Phase 3 types the exit codes (`no-eligible-model` ≠ `constraint-violation` ≠ `malformed-input`) but never states what a calling harness must *do* on each. Because Non-Goals explicitly preserves each harness's native model-selection knob, nothing prevents a harness from catching a non-zero `ic route` exit and silently falling back to its own default model, **bypassing a trust-zone constraint block entirely.** The constraint enforcement is fail-closed inside the mechanism and fail-open at the call site. Blast radius 3: a `client-confidential` task that trips `constraint-violation` could route to a vendor cloud through the fallback path.

### f-007 — Uniform "ignore-with-warning on unknown version" inverts fail-closed on safety fields (heat 6; risk 3×2=6; sev P1)
*Lens: fd-pilotage-authority-boundary*

The "consumers ignore-with-warning on unknown schema versions" rule is lifted from the routing-overrides PRD, where degrading open is *safe* because losing an exclusion list fails toward **more** review. Applied uniformly to `routing.yaml`, which also carries fail-closed trust-zone constraints, the same rule degrades toward **less** security: a consumer on a newer schema version it doesn't understand proceeds without the constraints. Grounding twist the lens verified: `grep -n version internal/routing/*.go` returns **zero** matches — the "routing-overrides contract precedent" the plan cites at line 58 does not exist as version-check code in the mechanism being extended. The precedent is real in the PRD, but it was never implemented here, and it is being generalized past its safe envelope.

### f-014 — `PolicyHash` field already exists in `decision.go`, half-wired, and the plan never mentions it (heat 12; risk 2×2=4; sev P1)
*Lens: fd-scribal-transmission-collation*

`internal/routing/decision.go` already defines a `PolicyHash` field on `Decision`, plumbed through `Record`/`Get`/`List` and the SQL insert/select — the exact "which policy text produced this decision" fingerprint the plan needs for its evidence loop. But **no code in the package ever computes and assigns it**; the only writer is a caller-supplied CLI flag (`cmd/ic/route.go:363`), and the in-tree escalation caller (`dispatch.go:549`) leaves it empty. Phase 2 introduces a *new* `rationale` field on the decision tuple and never references the existing `PolicyHash`. Highest novelty×risk in the ledger by my re-score: the plan is about to build a second witness field next to a half-built one that does the same job. (The raw finding's "zero call sites anywhere" was overstated — the CLI-flag path exists — but the structural gap, mechanism never self-fingerprints, holds.)

### f-022 — Plan cites its own PRD for the deferred-commitment problem, then adopts only the half that doesn't solve it (heat 12; risk 2×2=4; sev P1)
*Lens: fd-vat-dye-deferred-commitment*

Phase 5 reuses the routing-overrides PRD's **F2** propose/approve pattern for `routing.yaml` edits, but the PRD's actual answer to "did this write make things worse" is **F3/F4**: a post-commit canary (20-use window or 14-day expiry) that monitors for quality degradation after a routing write and can raise `ALERT: quality degraded`. Phase 5 adopts F2 and silently drops F3/F4. The plan cites its own project's precedent for exactly this deferred-commitment problem and takes only the half that opens the commitment, not the half that verifies it landed well. An approved-but-bad routing policy edit has no automatic detection.

---

## 2. Top Fusions

**3 fusions attempted, 2 emergent** (the workflow summary's "0 fusions" is a slot-accounting artifact: design agents didn't decrement the slot counter, so the fusion probes ran but weren't tallied as such). The fused lens was `fd-contradiction-adjudication`, built from pilotage × coppice.

- **f-017 (emergent) — adjudicates f-007 vs f-013.** Both parents hit the same plan line (the ignore-with-warning rule) from different angles; neither connected the causes. The fusion did the connective work: read the actual borrowed PRD, grepped the actual mechanism, and ruled **f-007 survives over f-013** — the fail-closed inversion is the real defect, and f-013's "just add a sunset/ratchet" fix, applied alone, leaves the trust-zone hazard live during the ratchet's grace window. This is the model's-eye view neither base lens had: the safety-field subset must fail closed regardless of version, and the sunset/ratchet applies only to the non-safety path.
- **f-018 (emergent) — a synthesis-hygiene meta-finding.** The ledger's own auto-tag calling f-013 "complementary-not-contradictory" with f-007 is true only *post-fix*; a naive synthesis dedup by `cluster_id` could pick f-013's milder P3 framing as "coverage achieved" and downrank the P1 f-007. This is a warning about *this very report* — I have honored it by leading with f-007, not f-013.
- **f-019 (demoted to convergence) — the round-3 re-run of the same adjudication produced no new cause; the emergence gate correctly fired emergent=false.** Negative result: the fusion lens went dry on this pair, a DRY signal.

No other lens pair cleared the shared-heat gate for fusion. The pilotage×coppice pair was the only hot one.

---

## 3. Taste Calls

**Empty.** Every upheld finding scored taste 0. This is a structural-completeness review, not an elegance review: the lenses that fired (metrology, pilotage, collation, coppice, vat-dye) are all provenance/authority/lifecycle lenses, and they found gaps, not smells. The absence is itself a mild signal — no lens argued the plan's *shape* is wrong, only that specific fields and flows are underspecified. The intercore-not-new-repo decision drew zero fire.

---

## 4. Convergence Spine

High-confidence, low-novelty — the commodity findings you can trust because multiple independent lenses landed on them:

- **Provenance staleness (c-provenance-staleness-lifecycle): f-001, f-003, f-011, f-021.** Four lenses (metrology, coppice, vat-dye) independently: the `benchmark|judgment|unknown` tag has no timestamp, no re-verification cadence, and no downgrade rule for a stale-but-still-labeled-benchmark value. A benchmark tag from Phase-1 seed time looks as authoritative as one certified yesterday. Highest-confidence gap in the review.
- **Drift-report underspecification (c-drift-report-disposition): f-004, f-016, f-020.** Phase 6's drift report "flags drift" with no destination, owner, urgency, or tie-break rule for when the two witnesses (hand-maintained registry vs. interrank) disagree — contrasted repeatedly against Phase 5's fully-specified flow.

Trust these; they are not exciting, but they are real and cheap to fix (add fields + a disposition paragraph).

---

## 5. Live Disagreements

One, and it is already adjudicated: **f-007 (P1) vs f-013 (P3)** on the ignore-with-warning line. The fusion resolved it in favor of f-007 (see §2), but it remains "live" in the sense that the *plan* still contains the contradiction f-007 identifies (line 37 declares trust-zone constraints "fail closed"; line 121 blankets the same file with ignore-with-warning). The disagreement is between two findings; the contradiction is inside the plan. Fixing the plan (split safety vs. non-safety fields in the version-degradation rule) closes both.

No unresolved elegant-vs-reckless taste disagreements (taste was uniformly 0).

---

## If You Read One Thing

**f-014.** Not the highest severity (f-006 is P0), but the highest heat and the most actionable single fact: `internal/routing/decision.go` already has a `PolicyHash` witness field, half-wired and never computed. The plan's entire Phase 5 evidence loop depends on being able to say "this outcome came from this policy," and the field to do it already exists in the code the plan is extending — the plan just doesn't know it's there and is about to build a parallel `rationale` field beside it. Before writing Phase 2, read `decision.go` and decide: wire `PolicyHash` up, or delete it and document why. Do not build the second witness without reconciling the first.

---

## Caveats

- **One adjacent-tier seed probe died** on a transient API connection error in round 0 (`seed-probe:adjacent`). Round 0 still yielded 9 (novel_cluster_rate 0.90), so seed coverage was strong, but one domain-expert adjacent lens is missing — the review may under-cover conventional software-architecture objections (coupling, testing strategy, API surface) that an adjacent lens would have raised. The distant/esoteric lenses that did fire are well-represented; the *adjacent* register is the thin one.
- **The original synthesis agent also died** on the same transient error; this synthesis was written from the intact ledger (append-only, fully replayable) on the rerun.
- **Fusion count discrepancy:** the workflow return object reported `fusions: {attempted: 0, emergent: 0}`, but the ledger contains 3 fusion-sourced findings (f-017/f-018/f-019) with 2 emergent. The mismatch is a slot-accounting quirk (design-stage agents not counted as fusion slots), not a missing-work problem — the fusions ran and are in the ledger.
- **No lens challenged the core architecture decision** (intercore vs. new repo, roles-not-models, deployment-keyed registry). Either the decision is sound or the fired lenses were the wrong ones to test it; the missing adjacent seed probe is the most likely lens that would have pushed on it. Consider one targeted architecture-adjacent pass before treating the seam as validated.
- **Regions never deeply probed:** the Hermes/Phase-7 pilot mechanics, the cost-type switching (per-token vs. subscription-quota vs. capacity), and the effort-normalization table each drew only one finding apiece (f-005, f-023, f-010). Light coverage, not zero.
