# PRD: Level 4 Auto-Ship — The Ship Trust Chain

**Beads:** iv-vs5h (this design), iv-e752 (prerequisite, re-scoped here), unblocks iv-qr0f (E10)
**Status:** design — decomposes into execution slices S1–S6 below
**Date:** 2026-07-02

## Problem

The vision doc defines Level 4 as: *"The system merges and deploys when confidence
thresholds are met. The human approves shipping policy (which thresholds, which
repos), not individual changes."* The substrate for this now exists — signed
receipts (migration 035, canon `signed-receipts-v1`), the `verdict_clean` ship
gate (sylveste-0ly7), the authz policy engine with `committed_by_this_session`,
authz tokens v2 (single-use, scoped, atomic consume), merge intents
(transactional outbox), `landed_changes` with revert columns, and E6 rollback.

But these are **isolated assertions**. A verdict says "review found nothing" —
nothing binds it to the exact SHA being shipped. A receipt proves an agent
emitted an action — nothing requires receipts to exist for the commits in a ship
candidate. `landed_changes` records lineage — after the fact. The policy engine
checks freshness — only on the ops that already wire `CheckInput`. Any process
with DB write access can fabricate the individual assertions (canon:
tamper-evident-post-write, not tamper-proof-at-write), and no check today asks
"was the reviewer a different identity than the author?"

Auto-ship without those bindings is a rubber stamp. **The missing design is the
chain**: what set of links, each fail-closed and each bound to the candidate SHA
and to an attributable caller, is sufficient to merge and deploy without a human
reading the diff?

## Outcomes and failure modes (start here, per doctrine)

**Outcome wanted:** a repo that has earned Level 4 ships agent-produced changes
end-to-end (merge → deploy → verify) with zero human touches on the happy path,
and demotes itself the moment evidence stops supporting that trust.

Failure modes this design must close, worst first:

| # | Failure mode | Closed by |
|---|---|---|
| F1 | A change nobody reviewed ships (review skipped, verdict missing/stale, or bound to an older SHA) | Links L3 (verdict bound to head SHA, fresh) — fail-closed like `verdict_clean` |
| F2 | An agent self-certifies (author and reviewer are the same identity) | L4 independence check — requires caller identity (iv-e752) |
| F3 | An orphan commit rides along (work not produced by any tracked dispatch) | L1 provenance — generalization of `committed_by_this_session` |
| F4 | TOCTOU: gate passes, branch moves, stale evaluation ships new commits | L7 single-use authz token scoped to `(op, project, head_sha)`, atomic consume |
| F5 | Ship succeeds, production breaks, nothing notices | L8 post-ship verification window + auto-revert (E6 reuse) + tripwire demotion |
| F6 | Evidence rows forged or mutated post-hoc | L2 receipt verification (fail-closed on exit codes 2/3/4) + signed authz audit rows |
| F7 | Blast radius unbounded (a "clean" 40-file refactor of the deploy scripts ships itself) | L6 policy-owned caps: diffstat limits, protected paths |
| F8 | Trust ratchets up silently ("no level is self-promoting") | Grants are explicit human acts; promotion only ever *proposed* by Interspect from shadow-mode evidence |
| F9 | Trust never ratchets down | Tripwires: any auto-shipped change reverted → automatic demotion + event |

## The Ship Trust Chain

The unit of evaluation is a **ship candidate**: `(project_dir, branch, base_sha,
head_sha, run_id)`. A new gate check, `ship_chain_clean`, evaluates eight links.
**All links are conjunctive and fail-closed** — any link failing downgrades the
op to `confirm` (never silently to `block`, never fail-open), with the failing
link named in `GateEvidence` per kernel observability doctrine.

| Link | Claim | Mechanism (exists → extend) |
|---|---|---|
| L1 Provenance | Every commit in `base..head` maps to a tracked dispatch | `merge_intents` + `landed_changes` lineage → pre-land mapping; generalizes `committed_by_this_session` to *committed-by-tracked-dispatch* |
| L2 Evidence | Those dispatches emitted receipts; `ic receipt verify` passes on all | `action_receipts` + verify CLI → bulk verify wired as gate querier; exit codes 2/3/4 all fail the link (canon: never accept unverifiable evidence) |
| L3 Verdict | Review ran on **this** SHA, recently, and is clean | `verdict_clean` + authz `vetted_sha_matches_head` + `vetted_within_minutes` → compose into one link: clean ∧ SHA-bound ∧ fresh |
| L4 Independence | Verdict-producing identity ≠ change-producing identity | **new**; requires caller identity on verdict writes (iv-e752). Policy MAY additionally require distinct model families (author=executor-tier, reviewer=validator-tier — same separation the routing doctrine already practices) |
| L5 Policy | The op resolves to `mode: auto` under the project's policy rules | `pkg/authz` `Check()` — thresholds live ONLY in policy (kernel = mechanism) |
| L6 Blast radius | Diffstat within caps; no protected path touched | **new requirement keys**: `max_files_changed`, `max_insertions`, `protected_paths` — cheap, `landed_changes` already tracks diffstat fields pre-land via merge intents |
| L7 Atomicity | The executing ship consumes a single-use token minted by THIS evaluation | authz tokens v2: scope `(op, project_dir, head_sha)`, short TTL (~10 min), atomic consume; re-evaluation required after any failure — no retry on a stale token |
| L8 Outcome | Post-ship verification passes within the policy window, else auto-revert | verification dispatch → verdict; on fail: E6 `rollback --layer=code`, set `landed_changes.reverted_at/by`, emit event, trip demotion |

Everything the chain does is receipted: the gate evaluation, the ship action,
the verification, and any revert each emit signed receipts. The chain that
authorizes evidence-producing work is itself evidence-producing.

### Why a boolean chain, not a numeric confidence score

Autarch's `ConfidenceScore` (weighted multi-dimensional) was considered and
**rejected for the ship decision itself**:

1. A blended score can average away a hard failure — an unclean verdict offset
   by great cost-efficiency is still an unclean verdict. Shipping is
   lexicographic, not additive.
2. Boolean links are explainable: `GateEvidence` names the failed link; a score
   of 0.71 vs threshold 0.75 explains nothing and invites threshold-fiddling.
3. Numbers enter the system where they belong: **earn-in and calibration**
   (below). Interspect aggregates shadow-mode outcomes numerically to *propose*
   grants; the grant itself, and every ship under it, is a chain of booleans.

### Trust grants — the human's policy surface (`autonomy_grants`)

New table + CLI (`ic autonomy grant|revoke|status`):

```sql
CREATE TABLE autonomy_grants (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    project_dir   TEXT NOT NULL,
    op            TEXT NOT NULL,          -- e.g. "ship-main", "deploy:prod"
    level         INTEGER NOT NULL,       -- 3 = shadow, 4 = auto-ship
    granted_by    TEXT NOT NULL,          -- human identity, never an agent
    granted_at    INTEGER NOT NULL,
    evidence      TEXT,                   -- JSON: shadow-window stats at grant time
    revoked_at    INTEGER,
    revoked_by    TEXT,                   -- human ID or "tripwire:<event_id>"
    revoke_reason TEXT,
    UNIQUE(project_dir, op, granted_at)
);
```

- **Granting is the act the vision assigns to the human** ("approves shipping
  policy, not individual changes"). No grant row → the entire Level-4 surface is
  inert and the system behaves exactly as today. That is the rollback story for
  the whole feature.
- **Demotion is mechanical.** Tripwires (any auto-shipped change reverted; any
  L2 verification failure on a landed change; N chain-failures in a window)
  write `revoked_by = 'tripwire:<event_id>'` and emit `autonomy.demoted`. The
  vision's "any level can be revoked if the evidence stops supporting it,"
  made a mechanism instead of a promise.
- **Promotion is never mechanical.** Interspect may emit
  `autonomy.promotion_proposed` with the shadow evidence attached; only a human
  writes a grant.

### Earn-in: shadow mode before live mode

A repo cannot receive a level-4 grant without a shadow window. At level 3
(shadow), every would-be ship runs the full chain and records the decision
(receipted, evented) — but a human still confirms. The divergence metric
(would-ship vs. human decision) over the policy-defined window is the evidence
attached to a promotion proposal. This is the same discipline as piloting 2–3
items before fanning out a backlog: measure agreement before delegating.

### Failure and rollback semantics (normative)

- **Chain-eval failure** → op degrades to `confirm` with the failed link named.
  Never `block` (that's reserved for policy saying "never"), never fail-open.
- **Ship-action failure mid-flight** (merge conflict, push rejected, crash):
  merge-intent outbox already gives crash-safe states; the consumed token is
  spent. Recovery = full re-evaluation and a fresh token — **never retry on
  stale evidence**, because the failure may itself have changed the world.
- **Post-ship verification failure** → auto-revert. Revert is a ship-shaped
  action but is pre-authorized by the same grant at a *lower* evidence bar
  (returning to last-known-good is the safe direction); it is still receipted
  and still consumes a token. Then: `landed_changes.reverted_*` set,
  `autonomy.demoted` tripwire fires, human notified with the whole chain's
  evidence.
- **Unverifiable evidence** (receipt exit code 4 — unknown key; schema
  mismatch — code 3) is a chain failure, not a warning. Canon rule: never
  silently accept an unverifiable receipt.
- **Kernel/db unavailability** → no evaluation → `confirm`. The chain cannot
  vouch for what it cannot read.

### iv-e752 re-scoped: the thin prerequisite slice

"Caller identity on all mutations" has been blocked at P3 as an ocean-boiler.
Level 4 does not need the ocean. It needs a uniform caller envelope
`(session_id, dispatch_id, agent_id)` on writes to the **chain tables only**:
dispatch verdicts, receipts (already carry `agent_id`), `merge_intents`,
`landed_changes` (already carries `session_id`), `phase_events`, and
`authorizations`. That is S1 below — small, additive (nullable columns +
populated-by-new-writes), and it is what makes L4 independence checkable.
Full-surface auditing remains iv-e752's long tail, no longer a blocker.

### Threat-model honesty (what Level 4 v1 does NOT claim)

Inherited from `signed-receipts-v1` and `authz-signing-trust-model`: keys are
co-resident with the emitting process, so the chain is
**tamper-evident-post-write, not tamper-proof-at-write**. Level 4 v1 protects
against drift, staleness, skipped process, orphan commits, self-certification,
unbounded blast radius, and post-hoc tampering. It does **not** protect against
a fully compromised host or an adversary holding the signing keys — that
upgrade path (out-of-band signer daemon, authz v1.6 / receipts v2) changes key
custody only; **no link in the chain changes**. Residual risk: L4 independence
is identity-based, not capability-based — a misconfigured OS layer could
dispatch the same model as author and reviewer under different agent ids;
mitigation is the policy-level distinct-model-family requirement.

Single-user, single-host threat model per canon. Sandboxing (E10 / iv-qr0f)
later adds "the change was produced inside a recorded sandbox policy" as a
**ninth link** — the chain is extensible exactly where gate.go already extends:
a new check constant + querier interface per evidence type.

## Options considered (per brainstorming doctrine)

1. **Conservative — shadow only.** Build S1–S2, never live-ship. Full
   calibration data, zero new blast radius. Rejected as the end state (it never
   delivers Level 4) but **adopted as the mandatory first phase** (earn-in).
2. **Balanced — boolean chain + grants + earn-in + tripwires.** *Chosen.* All
   eight links; live auto-ship only behind a human grant that shadow evidence
   earned; mechanical demotion.
3. **Aggressive — numeric confidence with Interspect-tuned thresholds.**
   Rejected for v1: averages away hard failures, unexplainable decisions, and
   there is no outcome data yet to tune thresholds honestly. Revisit only as a
   *proposal* input after real auto-ship history exists.

## Execution slices (each small, testable, reversible)

| Slice | What | Acceptance criteria | Rollback |
|---|---|---|---|
| S1 | Caller envelope on chain tables (iv-e752 thin slice) | New nullable columns populated on all new writes via one shared helper; existing rows untouched; `go test ./...` green | Columns are additive/nullable; writers revert to previous signatures |
| S2 | `ship_chain_clean` gate check + shadow mode | Check composes L1–L3, L5–L6 (L4 once S1 lands); evaluation recorded + receipted + evented with per-link `GateEvidence`; zero behavior change to any existing op | Check unused unless referenced by a gate rule — remove the rule |
| S3 | `autonomy_grants` + `ic autonomy` CLI + tripwire demotion | Grant/revoke/status round-trip; demotion fires on synthetic revert event; no grant ⇒ byte-identical behavior to today | Drop table; feature inert without grants by construction |
| S4 | Token-bound ship executor | Gate pass mints `(op, project, head_sha)`-scoped single-use token; executor consumes atomically; SHA-moved and double-consume both refuse and re-evaluate; ship actions receipted | Executor behind grant check (S3); revoke grants |
| S5 | Post-ship verification + auto-revert | Verification dispatch wired; failure produces revert plan (E6), sets `landed_changes.reverted_*`, emits `autonomy.demoted`; happy path emits `ship.verified` | Disable verification rule in policy → S4 behavior with manual verify |
| S6 | Interspect shadow read-model + promotion proposals | Divergence metric over policy window; `autonomy.promotion_proposed` event with evidence attached; **no write path to grants** | Read-only consumer; unregister cursor |

Dependency order: S1 → S2 → S3 → {S4, S6}; S4 → S5. S1–S3 are safe to land in
any release (inert without grants). S4 is the first slice that can touch a real
branch, and only under a grant that S2's shadow evidence justified.

## Open questions

- **Verification window semantics:** wall-clock window vs. explicit verify
  dispatch per deploy target? Leaning explicit dispatch (evented, receipted,
  scope-bound) with policy-owned timeout.
- **Portfolio ships:** does a portfolio run's ship gate require all child
  chains clean (mirror `children_at_phase`)? Deferred until a real multi-repo
  ship candidate exists.
- **Revert storms:** auto-revert of change A that change B already built on.
  v1 answer: tripwire demotes the repo to confirm-mode after the *first*
  revert, so storms require a human ignoring a demotion. Revisit with data.
