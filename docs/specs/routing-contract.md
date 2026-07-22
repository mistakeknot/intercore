---
artifact_type: spec
status: MOSTLY LOCKED (§1/§2/§3/§4 DECIDED; only §5 transport + Q-2.3/Q-0 open — §5 deferred, couples to the Phase-7 zklw run intercore-n6o). Last interview 2026-07-21.
phase: 1
plan: docs/plans/2026-07-13-model-routing-externalization.md
bead: intercore-8xa
date: 2026-07-14
---

# Routing Contract Spec (v1)

> **This is a scaffold.** Section shapes and open questions are laid down; the *contents* are decided by interview. Every `⟨OPEN Q-N⟩` marker is a decision that must be resolved before this leaves SCAFFOLD status. Stubs show a *candidate* shape from the plan/reviews, not a ruling.

The contract that decouples routing mechanism from every consumer. Three schemas + one transport decision. Any implementation satisfying these is a valid `ic route`; any harness reading them is a valid consumer.

---

## 0. Contract surface (what this document freezes)

- **`ic route`** — the CLI filter: task descriptor in, decision out. (§3, and the CLI phase.)
- **`routing.yaml`** — human-owned policy. (§1)
- **registry** — models × deployments × capability. (§2)
- **evidence event** — outcomes flowing back to interspect. (§4)
- **transport semantics** — how a cross-host consumer (Hermes/zklw) reaches `ic route`. (§5)

`⟨OPEN Q-0⟩` Is this the complete surface, or does the decision-witness (`policy_hash`/`registry_as_of`/`ic_version`) warrant its own §? (Leaning: fold into the decision output in §3.)

---

## 1. `routing.yaml` — policy schema

Human-owned, interspect-proposed (never auto-written). Roles-not-model-names.

```yaml
# CANDIDATE SHAPE — not decided
version: 1
roles:            # DECIDED Q-1.1: fixed set of FOUR in v1. Extend via schema-version bump.
  - planner       # frontier authors the plan (discernment)
  - executor      # cheaper tier runs an execution-grade plan (cost)
  - validator     # checks output against acceptance criteria (judgment); absorbs "reviewer"
  - researcher    # long-context / retrieval / breadth (Explore, deep-research) — distinct binding from executor
  # NOT roles: orchestrator (it is the harness/caller ABOVE ic route, or routes == planner);
  # first candidate for the extensibility escape hatch if a distinctly-routed orchestrator sub-agent appears.
task_classes:     # DECIDED Q-1.2: enumerated keys, human-defined. Adding a class = policy edit,
  terminal-grind: { ... }              # no code change. Caller names the class: `ic route --class=terminal-grind`.
  multi-file-bugfix: { ... }           # interspect can propose new classes. ic route errors on an unknown class name.
  client-confidential-synthesis: { ... }
bindings:         # ⟨OPEN Q-1.3⟩ role -> (model@deployment) per harness. Shape?
  claude-code:
    executor: anthropic/claude-sonnet-5@api
  codex:
    executor: openai/gpt-5.6-sol@api
constraints:      # DECIDED Q-1.4: flat match list (if {field: value} require {trust_zone: [...]}).
  - if: { data: client-confidential }     # No boolean-expression evaluator in v1 (YAGNI on a safety path).
    require: { trust_zone: [local, approved-vendor] }
  # DECIDED Q-1.6: this `constraints:` block is ALWAYS safety-class / fail-closed by construction.
  # Everything OUTSIDE it (bindings, effort_map, cost) is non-safety: warn-and-degrade on version mismatch.
  # Safety = "is it in constraints?" — no per-field flag to forget.
escalation:       # DECIDED Q-1.5 (option A): NO local ladder here. escalate.go calls the routing
    # mechanism for the next rung (registry-derived, per-harness). This block declares only the
    # DECIDED Q-1.7: de-escalation reset after 3 successful routings (mechanism
    # enforces on the chain state it already owns; policy declares the N).
    reset_after_successes: 3
verification_gates: # DECIDED Q-1.8: gates are declared PER TASK-CLASS (not per-model), but
    # the mechanism ALSO auto-attaches behavioral-verify whenever the chosen deployment's
    # reward_hack_risk exceeds a threshold — so a Sol-class executor gets the gate even if the
    # class didn't ask for it. Class-declared gates ∪ risk-triggered gates.
    #   <task-class>: [gate, ...]   e.g. client-confidential-synthesis: [behavioral-verify]
    reward_hack_gate_threshold: 0.4   # deployments above this auto-get behavioral-verify
```

**Decisions to interview:**
- ✅ **Q-1.1 DECIDED: fixed set of FOUR** — planner, executor, validator, researcher. researcher earns v1 inclusion via a distinct binding (long-context/retrieval ≠ executor's precision/code). "reviewer" collapses into validator; "orchestrator" is excluded (it is the harness/caller above `ic route`, or routes identically to planner) and is the documented first candidate for the extensibility escape hatch. Extending the set is a schema-version bump.
- ✅ **Q-1.2 DECIDED: enumerated keys in the yaml.** Task classes are human-defined named keys; the caller passes `--class=<name>`; `ic route` errors on an unknown class. Adding a class is a policy edit (interspect can propose one), never a code change. Consequence for §3: the task descriptor carries a `class` field the caller supplies, not descriptor fields `ic route` infers from (simplifies Q-3.1).
- ✅ **Q-1.3 DECIDED: binding = role → model@deployment only.** The binding says WHO runs a role on a given harness; effort and verification gates come from the `task_class` (HOW), not the binding. Clean separation, no effort/gate duplication across harnesses. Matches what `ic route decide` resolves today.
- ✅ **Q-1.4 DECIDED: flat match list.** `if {field: value} require {trust_zone: [...]}`, evaluated all-match (every constraint whose `if` matches must have its `require` satisfied). No boolean-expression evaluator in v1 (YAGNI on a safety path); add expressiveness only when a real compound rule needs it.
- ✅ **Q-1.6 DECIDED: reserved `constraints:` block is safety-class.** The top-level `constraints:` block is always fail-closed on version mismatch by construction; everything outside it warns-and-degrades. No per-field `safety:` flag (a forgotten flag would default wrong). Safety-class membership = "is this field inside `constraints:`?"
- `⟨OPEN Q-1.5⟩` Escalation encoding **(THE P0 — design grounded below, pick one)**: how is the ladder expressed so it is registry-derived and per-harness, NOT the hardcoded `["sonnet","opus","fable"]` in `dispatch/escalate.go`? And where does the de-escalation reset (`escalation_expired` after N successes) live?

  **Ground truth (verified 2026-07-14):** the ladder is not a naive list; `escalate.go` is a full subsystem (chain state, lesson transport, exhaustion handoff, `MaxEscalations` oscillation guard). Critically, `nextRungModel` (escalate.go:63) hardcodes `["sonnet","opus","fable"]` AND re-implements the fable-window fail-closed degrade (escalate.go:85-90) as a **byte-identical copy** of `routing.fableWindowOpen` (resolve.go:132) — `fableEscalationOpen()` and `fableWindowOpen()` are the same `CLAVAIN_FABLE_AVAILABLE=1` check in two packages that don't share code. `internal/dispatch` does not import `internal/routing` (confirmed). So the P0 is not "generalize a list"; it is "the escalation subsystem re-derives capability ordering and safety-window logic that the routing mechanism also owns, independently."

  **✅ Q-1.5 DECIDED: option (A).** `escalate.go`'s `nextRungModel` calls the routing mechanism for the next rung; the local `["sonnet","opus","fable"]` ladder and the duplicated fable-window check are deleted. Retry becomes vector-aware, one source of capability order. `internal/dispatch` takes a dependency on routing (acceptable: it is the same-half-of-the-decision cohesion the architecture review established). The three options are kept below for the record.

  - **(A) escalate.go calls `ic route`/routing for the next rung. [CHOSEN]** `nextRungModel` stops walking a local list and instead asks the routing mechanism "next-more-capable target for this role/harness above `currentModel`, fable-window respected." Kills the triplicated fable logic; makes escalation vector-aware for free (Sol-vs-Fable ordering comes from the registry). Cost: `internal/dispatch` gains a dependency on routing (or on `ic route` output); the ladder becomes registry-derived per-harness. *This is the option that makes "roles not model names" true at the retry surface.*
  - **(B) Ladder becomes a policy-supplied field, still walked locally.** `routing.yaml` emits a per-harness ordered ladder; `EscalationPolicy.Ladder` is populated from it instead of `DefaultEscalationPolicy()`. Cheaper (no cross-package call at retry time), but the *ordering* is still a flat list a human maintains, not derived from capability vectors — so it can silently disagree with `ic route`'s vector ranking. Half-fixes the P0.
  - **(C) Explicitly scope escalation-generalization OUT of this initiative.** Document that until a named later phase, escalation stays Claude-only `["sonnet","opus","fable"]` by design, and `ic route` covers only fresh routing, not retry. Honest and small, but ships the exact "vectors at `ic route`, `TierFable` at every retry" split the review flagged — acceptable only if retry-time vendor diversity is genuinely not needed yet.

  **De-escalation reset home:** `escalation_expired` is an evidence event (§4). The *counter* (`StrikesAtRung`, `Escalations` in `ChainState`) already lives in mechanism state (escalate.go:97-106); the reset *rule* (after N successful routings, decay the escalation) is policy. Likely split: policy declares N, mechanism enforces on the chain state it already owns.

  **Also fold in regardless of A/B/C:** the duplicated fable-window check is a latent bug (two copies drift independently). Even option C should de-duplicate `fableWindowOpen`/`fableEscalationOpen` into one shared function.
- `⟨OPEN Q-1.6⟩` **Version-mismatch degradation is field-class-split** (safety fails closed, non-safety warns). How does the schema *mark* a field as safety-class? An explicit `safety: true` per field, a reserved top-level `constraints:` block that is always fail-closed, or a documented allowlist in the mechanism?

---

## 2. Registry — models × deployments × capability

Deployment-keyed. The unit is `vendor/model@deployment`.

```yaml
# CANDIDATE SHAPE — not decided
version: 1
staleness_horizon: 90d   # ⟨OPEN Q-2.1⟩ one global horizon, or per-axis?
deployments:
  anthropic/claude-fable-5@api:
    version_stability: vendor-live   # pinned | vendor-live  (fd-arch F3)
    trust_zone: vendor-cloud
    cost: { type: per-token, in: 10.00, out: 50.00 }   # DECIDED Q-2.2: all 3 types built in v1
    # cost.type ∈ { per-token | subscription-quota | capacity }. Hermes@zklw = capacity; Max-plan = subscription-quota.
    effort_map: { 3: { thinking_budget: 16000 } }       # ⟨OPEN Q-2.3⟩
    capabilities:
      discernment:    { value: 0.95, provenance: judgment, verified_at: 2026-07-13, judged_by: ar, rationale: "..." }
      terminal_exec:  { value: 0.83, provenance: benchmark, verified_at: 2026-07-13 }
      multi_file:     { value: 0.92, provenance: benchmark, verified_at: 2026-07-13 }
      reward_hack_risk: { value: low, provenance: judgment, verified_at: 2026-07-13, judged_by: ar }
  openai/gpt-5.6-sol@api:
    version_stability: vendor-live
    ...
  nousresearch/hermes-4@zklw:
    version_stability: pinned
    trust_zone: local
    cost: { type: capacity }
    ...
```

**Decisions to interview:**
- ✅ **Q-2.1 DECIDED: one global `staleness_horizon`, halved for `vendor-live` deployments.** Ratifies the shipped implementation (`registry.go` `StalenessAdjusted`, tested by `TestRegistry_StalenessDowngrade`): a single horizon (seed = 90d), automatically halved for `vendor-live` (so 45d effective) because vendor-controlled weights drift faster; `pinned` uses the full horizon. Stale values downgrade one provenance rank (`benchmark`→`judgment`→`unknown`). Not per-axis in v1.
- ✅ **Q-2.2 DECIDED: type all three in v1** (`per-token | subscription-quota | capacity`). YAGNI gate cleared *because the consumer is on the roadmap*: Hermes (capacity) is a committed Phase-7 pilot in the goal DoD and Max-plan quota is real, so typing now avoids a registry schema-version bump the moment Phase 7 lands. Follow-on: the decision function must actually switch on `cost.type` (no per-token-only shortcut), and `EffectiveCost`/`CheapestCapable` in `costs.go` get their first real caller here (retires the dead-code flag).
- `⟨OPEN Q-2.3⟩` Effort map: the abstract 1–5 → vendor-semantics table. Per-deployment (as shown) or per-vendor? Does it carry its own provenance stamp (a conversion table can drift)?
- ✅ **Q-2.4 DECIDED: SIX frozen v1 axes** — `terminal_execution`, `multi_file_resolution`, `discernment`, `reward_hack_risk`, `long_context`, `tool_use`. The four seeded axes plus the plan's `long_context` plus `tool_use` (function-calling/agentic capability). `long_context` and `tool_use` may carry `provenance: judgment` or `unknown` until benchmark data exists; the provenance + staleness machinery handles absent/soft values (router treats `unknown` conservatively). Maximal set chosen so no schema-version bump is needed when those axes start driving decisions. Adding a 7th axis later IS a schema bump.
- ✅ **Q-2.5 DECIDED (by shipped seed): current-real deployments, Hermes seeded now.** `internal/routing/testdata/registry-seed.yaml` populates fable-5, gpt-5.6-sol, and `nousresearch/hermes-4@zklw` with real capability rows (2026-07 research, provenance-tagged). Hermes is seeded now (not empty-until-Phase-7) so the pilot has a real target. Opus/sonnet/haiku and sol's siblings (terra/luna) get added as they enter real use (seed-only-what-you-route-to).

---

## 3. `ic route` — decision output

Task descriptor in (flags or JSON stdin), decision JSON out.

```json
// CANDIDATE decision shape — not decided
{
  "model": "openai/gpt-5.6-sol@api",
  "effort": 3,
  "verification_gates": ["behavioral-verify"],
  "policy_hash": "sha256:...",
  "registry_as_of": "2026-07-14T...",
  "ic_version": "0.x.y+commit",
  "decided_at": "2026-07-14T...",
  "rationale": "executor role, terminal-grind class, ..."
}
```

**Decisions to interview:**
- ✅ **Q-3.1 DECIDED: descriptor = `class` + `role` (required), `data` + `harness` (optional).**
  ```
  class:   <task-class key>        # required — drives effort + gates (Q-1.2)
  role:    planner|executor|validator|researcher   # required — drives binding (Q-1.1)
  data:    <sensitivity, e.g. client-confidential>  # optional — drives trust-zone constraints (Q-1.4)
  harness: claude-code|codex|hermes                 # optional — selects the per-harness binding (Q-1.3)
  ```
  `class`, `role`, `data` are already shipped in `ic route decide`. **`harness` is the one field not yet wired** — it is required to pick the correct per-harness binding, so add `--harness` to `ic route decide` (small follow-on). When `harness` is omitted, fall back to a default binding block.
- `⟨OPEN Q-3.2⟩` Exit codes + **caller obligations** (fd-arch F2, finding f-009): `0` decision; non-zero typed `no-eligible-model | constraint-violation | malformed-input`. The spec must state the *obligation* per code (halt, never native-fallback). Is the obligation normative prose here, or also machine-checkable somehow?
- ✅ **Q-3.3 DECIDED: `rationale` is a one-liner in the decision; `ic route explain` gives the full trace.** The decision JSON carries a short `rationale` string (which role/class/constraint fired, the winning deployment); the separate `ic route explain` subcommand re-runs the decision verbose (candidates considered, why each was excluded, floors/gates applied) for debugging. The full trace is NOT embedded in every decision (keeps decisions small; no witness duplication). Follow-on: add `ic route explain` (mirrors the shipped `decide` path with verbose output).

---

## 4. Evidence event — outcomes to interspect

```json
// CANDIDATE shape — not decided
{
  "event": "gate_skipped",   // executor_failure | validator_rejection | cost_anomaly |
                             // constraint_block | gate_executed | gate_skipped | escalation_expired
  "harness": "hermes",
  "decision_ref": { "policy_hash": "...", "registry_as_of": "...", "ic_version": "...", "decided_at": "..." },
  "payload": { ... }
}
```

**Decisions to interview:**
- ✅ **Q-4.1 DECIDED: event-type set is complete for v1.** The seven types (`executor_failure`, `validator_rejection`, `cost_anomaly`, `constraint_block`, `gate_executed`, `gate_skipped`, `escalation_expired`) plus the `escalation` / `escalation_exhausted` pair the code already emits. No `decision_made` baseline in v1 (failure/gate/escalation events are the signal; add a baseline event via schema-version bump only if interspect needs a rate denominator it can't get from `ic route list` counts). Adding a type later is a schema-version bump.
- ✅ **Q-4.2 DECIDED: witness REFERENCE (join key), not inlined decision.** Every event carries a `decision_ref` = (`policy_hash`, `registry_as_of`, `ic_version`, `decided_at`); the full decision lives once in `routing_decisions`. Normalized, no per-event duplication. This matches the shipped emission direction (`RecordEscalation`/`RecordGate` write to the existing decision store, not a parallel table). **Follow-on:** the shipped events currently ride `ContextJSON` + `RuleMatched`; wiring the full `decision_ref` tuple onto them is the remaining Phase-5 work once `ic_version`/`registry_as_of` are populated on decisions (Phase 2 witness work).
- ⏳ **Q-4.3 (still Phase-2-gated): reconcile with `PolicyHash`.** Partially resolved — evidence reuses the existing decision store (the f-014 direction, not a parallel table), so the store-level reconciliation is done. The remaining half is computing `PolicyHash` (and `registry_as_of`/`ic_version`) at decision time so `decision_ref` is fully populated; that is the Phase-2 `PolicyHash`-wiring task, unchanged.

---

## 5. Transport semantics (pulled forward from Phase 7 — fd-arch F2)

The cross-host case: Hermes on zklw reaching `ic route`.

**Decisions to interview:**
- `⟨OPEN Q-5.1⟩` **Local shipped binary vs. RPC/SSH hop.** This choice determines whether §3's exit-code contract needs transport-failure semantics (a route call that times out over SSH must have a defined, fail-closed disposition). Decide here, not at Phase 7.
- `⟨OPEN Q-5.2⟩` If local binary: how is `ic_version` drift on zklw detected/surfaced? (The witness makes it *detectable*; what *acts* on the detection?)
- `⟨OPEN Q-5.3⟩` Fail-closed on transport failure: a Hermes `ic route` call that cannot reach the mechanism must halt, not fall back to `hermes model` default (extends f-009 across the wire).

---

## Acceptance (from the plan, restated)

- [ ] Spec reviewed; all `⟨OPEN Q⟩` resolved (leaves SCAFFOLD status).
- [ ] Seed `routing.yaml` + registry validate against the schemas.
- [ ] Three canonical descriptors hand-traceable from the spec alone: **terminal grind** (→ Sol-class), **multi-file bugfix** (→ Fable-class), **client-confidential synthesis** (→ local/approved-vendor only, blocks otherwise).
- [ ] Field-class-split version degradation is expressible (a safety field and a non-safety field, shown degrading differently).

## Open-question index

| ID | Section | The decision |
|----|---------|--------------|
| Q-0 | 0 | Is the witness its own contract surface? |
| ~~Q-1.1~~ | 1 | ✅ Roles: **fixed four** (planner/executor/validator/researcher) |
| ~~Q-1.2~~ | 1 | ✅ Task class: **enumerated yaml keys**, caller passes `--class` |
| ~~Q-1.3~~ | 1 | ✅ Binding: **model only** (effort/gates from task_class) |
| ~~Q-1.4~~ | 1 | ✅ Constraint predicate: **flat match list**, all-match |
| ~~Q-1.5~~ | 1 | ✅ Escalation: **option A** (escalate.go calls routing; local ladder + dup fable check deleted) |
| ~~Q-1.6~~ | 1 | ✅ Safety-class: **reserved `constraints:` block** (no per-field flag) |
| ~~Q-1.7~~ | 1 | ✅ De-escalation reset: **3 successes** |
| ~~Q-1.8~~ | 1 | ✅ Gates: **per-class ∪ reward_hack-risk auto-trigger** (>0.4 → behavioral-verify) |
| ~~Q-2.1~~ | 2 | ✅ Staleness: **global horizon, halved for vendor-live** (ratifies shipped code) |
| ~~Q-2.2~~ | 2 | ✅ Cost: **type all three now** (consumer on roadmap; costs.go gets first real caller) |
| Q-2.3 | 2 | Effort-map granularity + provenance |
| ~~Q-2.4~~ | 2 | ✅ Axes: **6 frozen** (terminal, multi_file, discernment, reward_hack, long_context, tool_use) |
| ~~Q-2.5~~ | 2 | ✅ Seed: **current-real, Hermes seeded now** (shipped registry-seed.yaml) |
| ~~Q-3.1~~ | 3 | ✅ Descriptor: **class+role (req), data+harness (opt)**; --harness to wire |
| ~~Q-3.2~~ | 3 | ✅ Exit codes: **0/1/3/4 shipped** with caller-halt obligation (ic route decide) |
| ~~Q-3.3~~ | 3 | ✅ rationale=one-liner in decision; **ic route explain**=full trace |
| ~~Q-4.1~~ | 4 | ✅ Event types: **7 + escalation pair, complete** (no decision_made baseline in v1) |
| ~~Q-4.2~~ | 4 | ✅ Witness: **reference/join key** (decision stored once, events point at it) |
| ⏳ Q-4.3 | 4 | Store-reconcile done (reuses decision store); PolicyHash-compute still Phase-2-gated |
| Q-5.1 | 5 | Transport: local binary vs RPC |
| Q-5.2 | 5 | ic_version drift detection actor |
| Q-5.3 | 5 | Fail-closed on transport failure |
