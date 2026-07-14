---
artifact_type: spec
status: SCAFFOLD (contents to be interviewed section-by-section; do not treat stubs as decided)
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
roles:            # ⟨OPEN Q-1.1⟩ fixed enum or open set?
  - planner
  - executor
  - validator
task_classes:     # ⟨OPEN Q-1.2⟩ what IS a task class? enumerated, or descriptor-derived?
  terminal-grind: { ... }
  multi-file-bugfix: { ... }
  client-confidential-synthesis: { ... }
bindings:         # ⟨OPEN Q-1.3⟩ role -> (model@deployment) per harness. Shape?
  claude-code:
    executor: anthropic/claude-sonnet-5@api
  codex:
    executor: openai/gpt-5.6-sol@api
constraints:      # ⟨OPEN Q-1.4⟩ the trust-zone rules. Predicate language?
  - if: { data: client-confidential }
    require: { trust_zone: [local, approved-vendor] }
escalation:       # ⟨OPEN Q-1.5⟩ the two-strikes ladder — and its de-escalation reset
    ...
verification_gates: # ⟨OPEN Q-1.6⟩ which task classes require which gates
    ...
```

**Decisions to interview:**
- `⟨OPEN Q-1.1⟩` Roles: fixed `{planner, executor, validator}` or extensible? (Plan says "extensible" — does v1 need more than three?)
- `⟨OPEN Q-1.2⟩` Task class: is it an enumerated key in the yaml, or is it computed from the task descriptor at `ic route` time? This determines whether adding a task class is a policy edit or a code change.
- `⟨OPEN Q-1.3⟩` Bindings: per-harness is decided. But do bindings bind role→model, or role→(model + effort + gates)? How much rides on the binding vs. the task class?
- `⟨OPEN Q-1.4⟩` Constraint predicate language: how expressive? A flat `if data==X require trust_zone∈Y` list, or something that can express AND/OR/negation? (YAGNI risk: over-building the predicate engine.)
- `⟨OPEN Q-1.5⟩` Escalation encoding **(THE P0 — design grounded below, pick one)**: how is the ladder expressed so it is registry-derived and per-harness, NOT the hardcoded `["sonnet","opus","fable"]` in `dispatch/escalate.go`? And where does the de-escalation reset (`escalation_expired` after N successes) live?

  **Ground truth (verified 2026-07-14):** the ladder is not a naive list; `escalate.go` is a full subsystem (chain state, lesson transport, exhaustion handoff, `MaxEscalations` oscillation guard). Critically, `nextRungModel` (escalate.go:63) hardcodes `["sonnet","opus","fable"]` AND re-implements the fable-window fail-closed degrade (escalate.go:85-90) as a **byte-identical copy** of `routing.fableWindowOpen` (resolve.go:132) — `fableEscalationOpen()` and `fableWindowOpen()` are the same `CLAVAIN_FABLE_AVAILABLE=1` check in two packages that don't share code. `internal/dispatch` does not import `internal/routing` (confirmed). So the P0 is not "generalize a list"; it is "the escalation subsystem re-derives capability ordering and safety-window logic that the routing mechanism also owns, independently."

  **Three options (Q-1.5 chooses):**

  - **(A) escalate.go calls `ic route`/routing for the next rung.** `nextRungModel` stops walking a local list and instead asks the routing mechanism "next-more-capable target for this role/harness above `currentModel`, fable-window respected." Kills the triplicated fable logic; makes escalation vector-aware for free (Sol-vs-Fable ordering comes from the registry). Cost: `internal/dispatch` gains a dependency on routing (or on `ic route` output); the ladder becomes registry-derived per-harness. *This is the option that makes "roles not model names" true at the retry surface.*
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
    cost: { type: per-token, in: 10.00, out: 50.00 }   # ⟨OPEN Q-2.2⟩
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
- `⟨OPEN Q-2.1⟩` Staleness horizon: one global value, or per-axis (benchmarks age slower than reward-hack judgments)? And the downgrade rule: stale `benchmark`→`judgment`→`unknown` is decided in principle; is the horizon per `version_stability` (vendor-live ages faster)?
- `⟨OPEN Q-2.2⟩` Cost types: `per-token | subscription-quota | capacity`. **YAGNI gate (fd-arch):** `EffectiveCost`/`CheapestCapable` have zero non-test callers today. Does v1 actually route on cost, or do we ship cost as `per-token`-only metadata and defer the three-way typing until a real consumer exists?
- `⟨OPEN Q-2.3⟩` Effort map: the abstract 1–5 → vendor-semantics table. Per-deployment (as shown) or per-vendor? Does it carry its own provenance stamp (a conversion table can drift)?
- `⟨OPEN Q-2.4⟩` Capability axes: the plan lists terminal_execution, multi_file_resolution, discernment, long_context, reward_hack_risk. Is that the frozen v1 axis set? Adding an axis later is a schema-version bump.
- `⟨OPEN Q-2.5⟩` Seed contents: which deployments populate v1? (Plan: current-real-only — Anthropic fable-5/opus-4.8/sonnet-5/haiku-4.5, OpenAI sol/terra/luna. Hermes row: schema-fits-but-empty until Phase 7, or seed it now for the pilot?)

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
- `⟨OPEN Q-3.1⟩` Task descriptor input schema: what fields does a caller supply? (role, task_class, data-sensitivity, harness, ...?) This is the *other* half of the contract and the scaffold hasn't stubbed it — needs its own interview pass.
- `⟨OPEN Q-3.2⟩` Exit codes + **caller obligations** (fd-arch F2, finding f-009): `0` decision; non-zero typed `no-eligible-model | constraint-violation | malformed-input`. The spec must state the *obligation* per code (halt, never native-fallback). Is the obligation normative prose here, or also machine-checkable somehow?
- `⟨OPEN Q-3.3⟩` `rationale` vs. `ic route explain`: is `rationale` a one-liner in the decision and `explain` the full trace, or does the decision carry the full trace? (Ties to the PolicyHash reconciliation — don't duplicate the witness.)

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
- `⟨OPEN Q-4.1⟩` The event-type set: is the 7-type list complete? (`gate_executed`/`gate_skipped` added for f-006; `escalation_expired` for f-012.) Any missing outcome interspect needs to learn from?
- `⟨OPEN Q-4.2⟩` Decision-witness reference: every event carries `decision_ref` (the three-part witness). Is that the right join key, or does interspect need the full decision inlined?
- `⟨OPEN Q-4.3⟩` Relationship to the existing `routing_decisions` table and `PolicyHash` field in `decision.go` — the evidence event and the recorded decision must share a witness. Reconcile before defining this (Phase 2 precondition).

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
| Q-1.1 | 1 | Roles: fixed or extensible in v1 |
| Q-1.2 | 1 | Task class: yaml key or descriptor-derived |
| Q-1.3 | 1 | Binding granularity (model vs model+effort+gates) |
| Q-1.4 | 1 | Constraint predicate expressiveness |
| Q-1.5 | 1 | Escalation encoding + de-escalation home **(P0-adjacent)** |
| Q-1.6 | 1 | How a field is marked safety-class |
| Q-2.1 | 2 | Staleness horizon: global vs per-axis vs per-stability |
| Q-2.2 | 2 | Cost typing now vs deferred **(YAGNI gate)** |
| Q-2.3 | 2 | Effort-map granularity + provenance |
| Q-2.4 | 2 | Frozen v1 capability axis set |
| Q-2.5 | 2 | Seed deployment contents (Hermes now or later) |
| Q-3.1 | 3 | Task descriptor input schema |
| Q-3.2 | 3 | Exit codes + caller obligations |
| Q-3.3 | 3 | rationale vs explain (no witness duplication) |
| Q-4.1 | 4 | Event-type set completeness |
| Q-4.2 | 4 | Witness ref vs inlined decision |
| Q-4.3 | 4 | Reconcile with routing_decisions/PolicyHash |
| Q-5.1 | 5 | Transport: local binary vs RPC |
| Q-5.2 | 5 | ic_version drift detection actor |
| Q-5.3 | 5 | Fail-closed on transport failure |
