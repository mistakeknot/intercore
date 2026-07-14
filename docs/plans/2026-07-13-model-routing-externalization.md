# Plan: Model Routing Externalization (Cross-Vendor, Cross-Harness)

**Status:** Draft, pending flux-melange review
**Bead:** TBD (create on plan approval)
**Extends:** `docs/prds/2026-02-15-interspect-routing-overrides.md` (cross-plugin contract pattern)
**Design conversation:** 2026-07-13 session (Sol vs. Fable research, metaharness generalization, Hermes Agent pilot)

## Problem

Model routing knowledge is fragmented across four layers that cannot serve a multi-harness, multi-vendor future:

1. **Doctrine is prose in one harness.** Capability routing lives in `clavain/commands/model-routing.md` and a CLAUDE.md block. Only Claude Code can read it. Hermes Agent, Codex-native flows, and any future metaharness get nothing.
2. **Mechanism is vendor-locked.** `intercore/internal/routing` models capability as a totally-ordered Claude tier enum (`TierHaiku=1 … TierFable=4`). GPT-5.6 Sol breaks the ordering: ahead of Fable 5 on terminal execution (Terminal-Bench 2.1: 88.8 vs 83.4), behind on discernment and multi-file resolution (SWE-Bench Pro: unreleased vs 80.3). Capability is a vector, not a scalar.
3. **Facts are stale.** interrank's snapshot predates the GPT-5.6 family and Fable 5.
4. **No feedback loop for model routing.** interspect closes the loop for agent triage (routing-overrides PRD) but nothing learns from executor failures, validator rejections, or cost anomalies at the model-selection layer.

New risks the current design cannot express: reward-hacking risk (METR flagged Sol as elevated; verification gates must be routable), trust zones (client-confidential data must be constrainable to local/approved-vendor deployments), and non-per-token cost models (subscription quota, self-hosted capacity).

## Architecture Decision: Mechanism Location

**Routing mechanism stays in `intercore/internal/routing`, exposed exclusively through the `ic route` CLI contract. No new repo/module.**

Rationale:
- The contract is the module boundary: `ic route` (JSON filter) + `routing.yaml` schema + evidence schema, documented as a spec any implementation could satisfy.
- Pace layering: policy/registry (fast, data files) are already external; the decision mechanism (slow, schema-versioned Go) gains nothing from repo mobility; orchestration (medium) stays put.
- Prior art: routing was previously fragmented across `lib-routing.sh`, `agent-roles.yaml`, and `interserve classify`; consolidation into this package was the fix. Do not re-fragment.
- **Genuine cohesion, not sunk cost.** intercore already owns the adjacent decision: `ic dispatch retry --escalate` (`internal/dispatch/dispatch.go`) already selects a model and writes `routing_decisions` rows; `ic gate` gates it; `ic cost` prices it. Model selection is the other half of the dispatch-and-escalate decision intercore already makes. A new repo would put model choice on one side of a process boundary and the dispatch/escalate that consume it on the other, when today they are one call. (fd-architecture, 2026-07-13.)
- `internal/routing` is a leaf package (two importers, both `cmd/ic/`; zero external; no import cycles), so extracting it later is a mechanical move (own module + two changed import lines). Low cost-to-reverse is what makes "extract later" credible here.

**Pre-committed extraction trigger:** extract `internal/routing` to a standalone Go module when shipping and version-syncing the full `ic` binary to a consumer *host* becomes a burden (the Hermes/zklw process-boundary case: a *when*, not an *if*, per Phase 7), OR a second in-Go consumer needs link-time embedding (compiler-enforced: `internal/` visibility blocks the import, so this trigger trips on its own). Tie the trigger to the mechanism-version witness (F2/Phase 2): the day you cannot cheaply prove which mechanism version produced a *remote* decision is the day the CLI seam stopped paying for itself. NB "non-Sylveste adoption" is not a self-detecting trigger; do not rely on it. (fd-architecture F4.)

## Design Principles

- **Mechanism / policy / facts / feedback separation.** Mechanism: intercore. Policy: `routing.yaml` (human-owned, interspect-proposed). Facts: interrank + registry. Feedback: interspect evidence flow.
- **Roles, not model names.** Policy vocabulary is `planner | executor | validator` (extensible). Per-harness bindings resolve roles to `(model, deployment)`. New model = registry row + binding edit; zero doctrine edits.
- **Deployment-keyed registry.** Unit is `vendor/model@deployment` (e.g. `openai/gpt-5.6-sol@api`, `nousresearch/hermes-4@zklw`). Same weights, different deployment = different routing target (cost, trust).
- **Capability vectors with provenance.** Axes: terminal_execution, multi_file_resolution, discernment, long_context, reward_hack_risk, plus cost/limits. Every value tagged `benchmark | judgment | unknown` AND carries `verified_at` (date) plus, for `judgment` values, `judged_by` and a one-line rationale. Router treats `unknown` conservatively; absence of a benchmark score is information. A value whose `verified_at` is older than the registry's staleness horizon is treated as one provenance-rank more conservative than its tag (a stale `benchmark` is read as `judgment`, a stale `judgment` as `unknown`) until re-verified: staleness is a routing input, not just a report line. (Findings f-001, f-003, f-011, f-015, f-021.)
- **Trust zones as first-class constraints.** Task descriptors carry data-sensitivity labels; router enforces `client-confidential → trust_zone ∈ {local, approved-vendor}`. Fail closed. Encodes the argus-adjacent discipline mechanically.
- **Normalized effort knob.** Policy speaks abstract effort 1–5; each registry entry maps to vendor semantics (Anthropic thinking budget, OpenAI reasoning_effort low→max/ultra, n/a for models without the dial).
- **Cost is typed.** `per-token | subscription-quota | capacity`. Decision function switches on type; per-token math alone is a Claude-era simplification.
- **Verification gates are routable outputs AND their execution is observable.** A decision includes required verification (e.g. `behavioral-verify` for elevated reward_hack_risk targets; tests-green is insufficient for Sol-class executors). The adapter that consumes a gated decision MUST emit a `gate_executed` (or `gate_skipped`) evidence event before accepting executor output: naming a gate the mechanism cannot confirm ran is the reward-hacking control the Sol research motivated, silently disabled. A skipped gate must be distinguishable from a passed one after the fact. (Finding f-006.)
- **Decision provenance is self-fingerprinting, including the mechanism.** Each decision carries a witness of *which policy text, which registry snapshot, AND which mechanism build produced it* (`policy_hash`, `registry_as_of`, `ic_version`) plus `decided_at`, so an evidence event can be correlated back to the exact policy, model-version, and binary state that generated the outcome, including a silent same-deployment vendor model swap and a stale remote binary. The binary is the input most likely to drift silently across a process boundary (a zklw `ic` built from a different commit than the registry it reads); staleness-as-routing-input applies to the mechanism itself, not just registry values. **Reconcile with the existing half-wired `PolicyHash` field in `internal/routing/decision.go` before adding any new witness field** (see Phase 2). (Findings f-014, f-023; fd-architecture F2.)
- **Trust-zone constraints fail closed on version mismatch.** The version-degradation policy is split by field class: non-safety fields (bindings, effort tables, cost) degrade `ignore-with-warning` on an unknown schema version; safety fields (trust-zone constraints) fail closed. A consumer on a schema version it does not understand refuses to route rather than routing without constraints. (Findings f-007, f-017.)
- **Two-strikes escalation encoded, with a return path.** Executor fails 2x or validator rejects 2x → escalate to frontier-tier re-scope; never raise effort on a discernment-limited model. Escalation is NOT sticky: it applies to the task instance, not the task class, and an `escalation_expired` evidence event resets the counter after N successful subsequent routings, so a class that hit two strikes once does not ratchet toward frontier tier permanently (the exact scarce-capacity misroute the doctrine exists to prevent). (Finding f-012.)
- **Router failure is a hard stop, not a silent fallback.** When `ic route` returns a non-zero exit (no-eligible-model, constraint-violation, malformed-input), the calling harness MUST halt and surface the reason; it MUST NOT catch the error and fall back to its native model-selection knob, which would bypass trust-zone enforcement. This obligation is part of the adapter conformance bar, not left to each adapter author. (Finding f-009.)

## Non-Goals

- **Execution venue selection.** Hermes routes its own sandboxes (Docker/SSH/Modal); harness-internal concern. Keeping this out of the schema is a deliberate scope wall.
- **Speculative registry population.** Seed only deployments actually in use today. The schema fits Hermes/open-weight entries; rows appear when routed to.
- **Replacing harness-native model switching.** Adapters drive existing knobs (`hermes model`, Claude Code `model` param, Codex `--config`); no new model-invocation plumbing.
- **Autonomous policy self-modification.** All policy writes are human-gated through the interspect proposal flow. Harness-internal learning loops (Hermes skills/memory) never write routing policy directly.

## Phases

Phases are sequential; each gets its own execution-grade task plan (per capability doctrine) before implementation. This document is the architecture plan, not the task list.

### Phase 1 — Contract spec (docs only, no code)

Write `docs/specs/routing-contract.md` defining the three schemas:

1. `routing.yaml` v1: roles, task classes, per-harness bindings, constraints, escalation rules, verification gates. Version integer; env-var path override per the routing-overrides pattern. **Version-mismatch degradation is field-class-split, not uniform:** non-safety fields degrade `ignore-with-warning`; safety fields (trust-zone constraints) fail closed. NB: the routing-overrides "proceed without exclusions" precedent is safe only because it degrades toward *more* review; it is a design *analogy*, not existing code (no version-check exists in `internal/routing` today) and it must not be generalized to safety fields. (Findings f-007, f-017.)
2. Registry v1: deployment-keyed entries, capability vector with provenance tags, per-value `verified_at` + `judged_by`/rationale metadata, a declared `staleness_horizon`, typed cost, trust_zone, effort mapping table (the effort table itself carries a version/provenance stamp), and a per-deployment **`version_stability: pinned | vendor-live`** field. `@api` deployments are vendor-live (weights can silently reroll; identical rows at 09:00 and 17:00 may be different models); `@zklw`/self-hosted are pinned (change only on your redeploy). The two have categorically different self-currency, and stale-value downgrade must weight `vendor-live` more aggressively; the key stays `vendor/model@deployment` (this is a one-field add, not a re-key). (Findings f-023; fd-architecture F3; f-001, f-005, f-015.)
3. Evidence event v1: routing-relevant outcomes (`executor_failure`, `validator_rejection`, `cost_anomaly`, `constraint_block`, `gate_executed`, `gate_skipped`, `escalation_expired`) with a harness-of-origin field AND a decision-witness reference (`policy_hash`, `registry_as_of`, `ic_version`, `decided_at`) so each event ties to the exact decision instance (policy + registry + binary) it reports against. (Findings f-006, f-012, f-014, f-023; fd-architecture F2.)
4. **Remote-transport semantics for `ic route`.** Decide here (not deferred to Phase 7) whether cross-host consumers (Hermes on zklw) call a local shipped binary or `ic route` over an RPC/SSH hop, because that choice determines whether the exit-code/fail-closed contract (Phase 3) needs transport-failure semantics: a route call that times out over SSH must have a defined, fail-closed disposition. Discovering this at Phase 7 is the expensive-late version. (fd-architecture F2.)

Seed files authored alongside spec: current real deployments only (Anthropic fable-5/opus-4.8/sonnet-5/haiku-4.5 via Claude Code; OpenAI gpt-5.6 sol/terra/luna via Codex CLI). Sol/Fable capability rows sourced from the 2026-07 research (Terminal-Bench 2.1, SWE-Bench Pro, METR reward-hacking flag, pricing), provenance-tagged.

**Acceptance:** spec reviewed; seed files validate against schema; a routing decision for three canonical task descriptors (terminal grind, multi-file bugfix, client-confidential synthesis) can be hand-traced from the spec alone.

### Phase 2 — Mechanism generalization (intercore Go)

- **Inventory ALL Claude-scalar total-order model rankings FIRST; there are three, not one (P0).** "Roles not model names" is false while these remain vendor-locked, and Phase 2 as first drafted named only the first:
  1. `internal/routing/routing.go` (`ParseModelTier`/`TierFable=4`) + `resolve.go`'s `applyFloor` (`modelTier >= floorTier`). The one the plan patched.
  2. `pkg/phase/phase.go:76`: a **second** independent `ModelTier(string) int` (haiku=1…opus=3, no fable), a parallel "which model is more capable" truth source in a package the plan never mentioned.
  3. `internal/dispatch/escalate.go:50`: `DefaultEscalationPolicy.Ladder = ["sonnet","opus","fable"]`, walked by `nextRungModel`. **This package does not import `internal/routing`.** The two-strikes escalation doctrine (a first-class design principle, `escalation_expired` events) resolves its next model through this hardcoded Claude-only ladder, entirely outside the mechanism being generalized. Escalation is the exact path that MUST be vector-aware (Sol is ahead on terminal, behind on discernment; "never raise effort on a discernment-limited model"); leaving it un-generalized ships a system that speaks vectors at `ic route` and `TierFable` at every retry. Either route escalation's next-rung through the new decision function so the ladder becomes registry-derived and per-harness, or explicitly scope escalation-ladder generalization to a named later phase and state that until then escalation is Claude-only *by design*. Silent is not an option. (fd-architecture F1.)
- **Reconcile the existing witness field.** `internal/routing/decision.go` already defines a `PolicyHash` field, plumbed through `Record`/`Get`/`List` and the SQL insert/select, but it is never computed inside the package (the only writer is a caller-supplied CLI flag on `ic route record`; the in-tree escalation caller at `dispatch.go` leaves it empty). Before introducing any new witness field, decide and document: either wire `PolicyHash` to be computed from the loaded `routing.yaml` at decision time (preferred, it is the field the evidence loop needs), or delete it with rationale. Do NOT build a parallel `rationale`/witness field beside a half-built one. (Finding f-014.)
- Registry loader + validation (`internal/routing/registry.go`), including per-value `verified_at` staleness evaluation at decision time (a stale value routes one provenance-rank more conservative; see Design Principles).
- Replace `ModelTier` scalar with capability-vector matching; retain `ParseModelTier` as a compat shim mapping legacy names to registry entries (existing consumers keep working). **Document each legacy-name→registry-row mapping and its basis**, since `applyFloor` (`resolve.go`) still reconstructs a total order via `modelTier >= floorTier` from a space the plan says is not ordered; the shim's ordering assumption must be explicit and tested, not implicit. (Finding f-002.)
- Constraint enforcement (trust zones, harness capability matrix): fail closed with typed errors, including on schema-version mismatch for safety fields.
- Typed cost handling; effort normalization tables. **YAGNI check first:** `EffectiveCost`/`CheapestCapable` (`costs.go`) currently have zero non-test callers. Confirm a real decision-time cost consumer exists (or is landing this phase) before enriching cost from per-token to three types; otherwise typed-cost is speculative flexibility on dead code, so defer it. (fd-architecture cohesion note.)
- Decision function: `(role, task_class, constraints, harness) → (model@deployment, effort, verification_gates[], policy_hash, registry_as_of, ic_version, decided_at, rationale)`.

**Acceptance:** all existing routing tests pass via shim; **all three total-order rankings inventoried and dispositioned** (routing.go, `pkg/phase.ModelTier`, `dispatch/escalate.go` ladder), either generalized or explicitly scoped-out in writing, with escalation's next-rung selection covered; `PolicyHash` reconciliation landed (computed-and-tested, or deleted-with-rationale) before any new witness field exists; new table-driven tests cover vector matching, constraint blocks, unknown-provenance conservatism, stale-value downgrade, safety-field fail-closed on version mismatch, and each cost type actually consumed; the legacy-name→row mapping table is tested; no callers outside the package touch tier integers.

### Phase 3 — CLI surface (`ic route`)

- `ic route` reads task descriptor (flags or JSON stdin), emits decision JSON. Pure filter; composable.
- `ic route explain` prints the decision rationale (which axes, constraints, and rules fired).
- Exit codes: 0 decision, non-zero typed failures (constraint violation ≠ malformed input ≠ no-eligible-model). **The spec states the call-site obligation for each code, not just its meaning:** on any non-zero exit the caller MUST halt and surface the reason; falling back to a native model knob is prohibited (a constraint-violation must never route). This is a normative part of the contract, enforced by the conformance bar (Phase 4), not adapter-author discretion. (Finding f-009.)

**Acceptance:** shell round-trip demos for the three canonical descriptors; constraint-violating descriptor exits non-zero with machine-readable reason; `explain` output names the winning and losing candidates; the spec's per-exit-code caller obligation is written and testable.

### Phase 4 — Harness adapters (Clavain + Codex)

- **Define a shared adapter conformance bar FIRST** (before writing either adapter): a checklist + shared test vectors every adapter must pass to have its fail-closed claims trusted. Minimum: halts on each non-zero exit code (no native fallback, f-009); emits `gate_executed`/`gate_skipped` before accepting gated output (f-006); refuses to route on safety-field version mismatch (f-007); resets escalation on `escalation_expired` (f-012). Phase 4's two adapters share an author, so passing them proves nothing about portability; the conformance bar is what Phase 7 (Hermes) actually re-runs. (Finding f-008.)
- `clavain/commands/model-routing.md` shrinks to an adapter page: invoke `ic route`, apply decision; doctrine prose deleted in favor of the spec.
- `clavain/agents/workflow/codex-delegate.md` consumes decisions: model/effort from `ic route`, verification gates enforced AND their execution recorded (emit `gate_executed` after behavioral-verify for elevated reward-hack targets; never accept executor output on a `gate_skipped`), two-strikes escalation returns to planner.
- CLAUDE.md capability-doctrine block collapses to a pointer at the spec.

**Acceptance:** the conformance bar (checklist + test vectors) exists and both adapters pass it; one real delegation flows end-to-end through `ic route` in each harness path, emitting a `gate_executed` event; a forced non-zero `ic route` exit makes the adapter halt (not fall back) in a test; grep confirms no residual hardcoded model-name routing logic in clavain command/agent docs.

### Phase 5 — Feedback loop (interspect)

- Extend interspect evidence schema with the routing event types from Phase 1.
- Proposal flow targets `routing.yaml` edits, human-gated, reusing the routing-overrides F2 pattern (threshold detection → AskUserQuestion with evidence summary → approved write).
- **Adopt F3/F4, not just F2.** The routing-overrides PRD does not stop at propose/approve: after an approved write it inserts a canary record (20-use window or 14-day expiry) and monitors for post-commit quality degradation, with an `ALERT: quality degraded` verdict state. A routing-policy edit is exactly the deferred-commitment case that pattern exists for; adopting only F2 lands the write with no automatic detection that it made routing worse. Insert the canary + verdict step after a successful `routing.yaml` commit. (Finding f-022.)

**Acceptance:** synthetic evidence stream produces a correctly-formed proposal; approved proposal lands a valid `routing.yaml` edit AND opens a canary record; a synthetic post-write degradation trips the canary to `ALERT`; rejected proposal leaves policy untouched.

### Phase 6 — Facts layer (interrank)

- Refresh interrank snapshot to include GPT-5.6 family and Fable 5.
- Add a registry-refresh script cross-checking registry cost/benchmark values against interrank, flagging drift (does not auto-write; provenance stays explicit).
- **Specify the drift report's disposition, matching Phase 5's rigor.** The report is a two-witness collation (hand-maintained registry vs. interrank snapshot); define: (a) *destination + owner*: where a flag goes and who acts on it (not a silent log line); (b) *tie-break rule*: which witness is presumed correct pending human review, and a flag category distinguishing stale-registry vs. stale-interrank vs. differently-scoped-deployment; (c) *interaction with staleness*: a drift flag updates the field's `verified_at`/provenance disposition (per Design Principles), it does not just print. interrank is benchmark-only, so it is structurally silent on `judgment` axes: the drift report must not imply it validated those. (Findings f-004, f-016.)

**Acceptance:** `interrank compare_models` resolves current-generation slugs; drift report runs clean against seed registry; a seeded registry/interrank disagreement produces a flag with a named owner, destination, category, and applied tie-break.

### Phase 7 — Second-metaharness pilot (Hermes Agent on zklw)

The validating case for the whole externalization: a harness that cannot read Clavain markdown.

- Install Hermes Agent on zklw; bind through OpenRouter or local endpoint.
- Adapter: gateway hook calls `ic route` (local binary), applies decision via `hermes model`.
- Evidence emission: Hermes task outcomes → interspect evidence store, same schema, `harness: hermes` origin.
- Trust-zone verification: confirm a `client-confidential` descriptor routes only to a local-model binding and is blocked otherwise.

**Acceptance:** one real task routed, executed, and evidenced end-to-end from Hermes without any Clavain involvement; constraint block demonstrated live.

## Risks

| Risk | Mitigation |
|---|---|
| Contract drift across intercore/clavain/interflux/interspect | Single spec doc (Phase 1) is canonical; version integers in every schema; **field-class-split degradation**: non-safety fields ignore-with-warning, safety (trust-zone) fields fail closed on unknown version (f-007/f-017) |
| Schema creep (venue selection, harness internals leaking in) | Non-goals wall; spec review gate; **schema-level MUST-NOT** forbidding an adapter from reading `trust_zone`/constraints as a venue hint (f-010) |
| Speculative registry rot | Seed-only-what-you-use rule; Phase 6 drift report with defined disposition (f-004/f-016) |
| Compat breakage for legacy tier consumers | Phase 2 shim + full existing test suite as regression gate; documented+tested legacy-name→row mapping, not implicit ordering (f-002) |
| Reward-hacking slips past verification gates | Gate execution is *observable*: adapter emits `gate_executed`/`gate_skipped` before accepting output; a skipped gate is detectable after the fact, not just assigned to adapters (f-006) |
| Silent router-failure fallback bypasses constraints | Per-exit-code caller obligation in the spec + conformance bar: non-zero exit halts, native fallback prohibited (f-009) |
| Approved-but-bad policy edit ships undetected | Phase 5 adopts F3/F4 canary + degradation verdict, not just F2 propose/approve (f-022) |
| Escalation ratchets scarce tier permanently | Escalation scoped to task instance + `escalation_expired` reset event (f-012) |
| Adapter fail-closed claims trusted without proof | Shared conformance bar (checklist + test vectors) re-run by every adapter incl. Hermes, not same-author construction (f-008) |
| "Roles not model names" cosmetic: escalation stays Claude-scalar | Phase 2 inventories all 3 total-order rankings (routing.go, pkg/phase, dispatch/escalate ladder); escalation next-rung generalized or explicitly scoped-out (fd-arch F1) |
| Stale remote `ic` binary produces unattributable decisions | `ic_version` in decision witness; transport semantics decided in Phase 1 not Phase 7 (fd-arch F2) |
| `@api` weights silently reroll under a stable key | Per-deployment `version_stability: pinned\|vendor-live`; vendor-live weighted more staleness-prone (fd-arch F3) |
| Typed-cost built on dead code | YAGNI gate in Phase 2: confirm a live decision-time cost consumer before enriching cost (fd-arch cohesion note) |
| Hermes evidence schema mismatch | Evidence schema defined in Phase 1 before any emitter exists; Hermes emits to the same store, no side channel |

## Sequencing Notes

- Phases 1–3 are intercore-only and independently shippable; value exists even if adapters lag.
- Phase 4 delivers the immediate payoff (Sol/Fable delegation discipline) and can begin once Phase 3 is stable.
- Phases 5–7 are ordered by dependency but each is independently valuable; Phase 7 can float.
- Per capability doctrine: this plan and its reviews stay on frontier tier; per-phase execution plans get written for a weaker executor with machine-checkable acceptance criteria.

## Review Trail

- **2026-07-13 flux-melange pass** (4 rounds, DRY halt, 18 upheld findings): `docs/research/flux-melange/model-routing-externalization-plan/2026-07-13-synthesis.md`. Findings folded into Design Principles, Phases 1–6, and the Risks table (each edit tagged with its `f-NNN` id). The highest-heat finding (f-014) was a ground-truth collision: `PolicyHash` already exists half-wired in `decision.go`, so reconciliation is now a Phase 2 precondition.
- **2026-07-13 fd-architecture seam pass** (grounded in the actual codebase, not just the plan): closed the gap the melange left open. Verdict: **intercore-not-new-repo holds** (routing is the other half of the `ic dispatch retry --escalate` decision intercore already owns; `internal/routing` is a low-cost-to-reverse leaf package). Found one **P0 the plan-only melange could not see** (F1): three independent Claude-scalar total-order rankings live in the tree (`routing.go`, `pkg/phase/phase.go:76`, `dispatch/escalate.go` ladder), and the two-strikes escalation doctrine routes through the third, which does not import `internal/routing` at all. "Roles not model names" was cosmetic until Phase 2's scope was widened to all three. Plus F2 (mechanism-version witness for the zklw boundary), F3 (`version_stability` for vendor-live deployments), F4 (sharpen the extraction trigger to the process boundary), and a YAGNI flag on typed-cost (`costs.go` has zero non-test callers). All folded into the Architecture Decision, Design Principles, Phase 1, Phase 2, and the Risks table.
- **Seam now adversarially validated.** No open review gaps remain before Phase 2 execution planning.
