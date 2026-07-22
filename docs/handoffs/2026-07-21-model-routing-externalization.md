---
artifact_type: handoff
initiative: model-routing-externalization
branch: mk/model-routing-externalization
parent_bead: intercore-8xa
date: 2026-07-21
---

# Handoff: Model Routing Externalization

## One-line state

Contract **locked**, mechanism **built + tested**, the fd-architecture **P0 fixed**, evidence **observable**, Phase 7 **pre-staged**. The only DoD-blocking remainder is the real Hermes-on-zklw run, which is auth-gated (Tailscale) and tracked as `intercore-n6o`.

## What this initiative is

Move model-routing doctrine out of Clavain prose into policy-as-data (`routing.yaml` + a deployment-keyed capability registry) consumed by `ic route` in intercore. Generalize past the Claude-only `ModelTier` enum to cross-vendor, cross-harness routing. Plan: `docs/plans/2026-07-13-model-routing-externalization.md` (hardened by a flux-melange + fd-architecture review; findings folded, tagged).

## Delivered (branch `mk/model-routing-externalization`, local, ~14 code commits)

- **Spec — `docs/specs/routing-contract.md`: MOSTLY LOCKED.** §1 (policy schema), §2 (registry), §3 (`ic route` I/O), §4 (evidence events) all decided (31 decisions). Open: §5 transport (Q-5.1–5.3, deferred — couples to the zklw run), Q-2.3 (effort-map, minor), Q-0 (cosmetic). The bottom-of-file open-question index is the resume map.
- **Registry** (`internal/routing/registry.go`): deployment-keyed (`vendor/model@deployment`), 6 frozen capability axes (terminal_execution, multi_file_resolution, discernment, reward_hack_risk, long_context, tool_use), provenance-tagged values with staleness downgrade (vendor-live ages 2×), typed cost (per-token|subscription-quota|capacity), trust_zone, version_stability. Seed: `internal/routing/testdata/registry-seed.yaml`.
- **Constraint enforcement** (`internal/routing/constraint.go`): flat match-list, always fail-closed. `TestDoD_ClientConfidentialRoutesLocalOnly` is DoD clause 1 as a test.
- **Escalation P0 fixed** (`internal/routing/escalation.go` + `internal/dispatch/escalate.go`): `NextRung` is now the single capability-ordering authority; `dispatch` calls it instead of a hardcoded ladder. The duplicated frontier-window check was deduped into `internal/capability`. Behavior-preserving (existing 6-case test passes).
- **`ic route decide`** (`cmd/ic/route.go`): registry-based constraint-enforcing decision; exit codes 0/1/3/4 (4 = constraint-violation fail-closed); descriptor = class+role (req), data+harness (opt); emits `harness` + `rationale`.
- **`ic route record-evidence`** + `RecordEscalation`/`RecordGate` (`internal/routing/evidence.go`): escalation + gate events observable via `ic route list`, reusing the existing decision store (not a parallel table, per f-014).
- **Phase 7 pre-stage**: `dist/ic-linux-{amd64,arm64}` (gitignored artifacts), `deploy/phase7/hermes-route-adapter.sh` (validated end-to-end against a fake-hermes shim: routes, applies model, emits evidence, HALTS fail-closed on non-zero exit), checklist `docs/plans/phase7-hermes-zklw-deploy-checklist.md`.

## DoD status

| Clause | Status |
|--------|--------|
| client-confidential routes local/approved only, blocks otherwise | ✅ done (mechanism + CLI + adapter, exit-4 fail-closed) |
| escalation + gate observable in evidence | ✅ done (tested, live via `ic route list`) |
| routes end-to-end from a non-Clavain harness | ✅ proven with `hermes` stubbed |
| ...through **real Hermes on zklw** | ⛔ `intercore-n6o` — auth-gated (Tailscale; `ssh zklw` verified unreachable 4× on 2026-07-14/21) |

Original goal was **retargeted** (a.r.'s choice) to the shippable DoD; the physical zklw run split to `intercore-n6o`. `/goal` was cleared.

## Open beads

- **`intercore-8xa`** (parent, in-progress): the umbrella. Remaining under it: §5 transport, Q-2.3, Phase-2 PolicyHash-compute tail, Phase 4 (Clavain/Codex adapters proper), Phase 6 (interrank refresh).
- **`intercore-n6o`** (P2, open): the real Hermes-on-zklw run. Everything pre-staged; blocked only on Tailscale auth. Checklist has exact steps.
- **`intercore-d8r`** (P3, bug): `cli.ParseFlags` doc claims `--key value` support but only `--key=value` works.

## Next-session resume options (both clean)

1. **The zklw run** (`intercore-n6o`) — the moment Tailscale is up: bring up TS, `scp dist/ic-linux-<arch>` + registry + adapter to zklw, point Hermes dispatch at the adapter, run one client-confidential task, capture escalation+gate evidence. This also unblocks §5 transport decisions.
2. **Phase 4 (Clavain/Codex adapters)** — fully unblocked, no auth needed: put `ic route decide` in front of real Claude Code / Codex dispatch, shrinking `clavain/commands/model-routing.md` to an adapter page and building the shared conformance bar.

## Not done / deliberately deferred

- **Push**: branch is local-only (gate-the-push rule). Not yet pushed to the intercore remote — awaiting a.r. go.
- **§5 transport**: local-binary-vs-RPC — decide WITH the zklw run, not ahead of it.
- **PolicyHash compute**: the decision-witness (`policy_hash`/`registry_as_of`/`ic_version`) is designed and referenced by evidence but not yet computed at decision time — the Phase-2 witness tail.
