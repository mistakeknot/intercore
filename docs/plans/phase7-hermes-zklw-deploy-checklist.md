---
artifact_type: deploy-checklist
phase: 7
bead: intercore-8xa
blocks: "DoD clause 2 — one real task end-to-end from Hermes on zklw"
date: 2026-07-14
---

# Phase 7 Deploy Checklist: Hermes-on-zklw End-to-End

This is the ONLY remaining DoD gap. Everything upstream (registry, constraint
enforcement, `ic route decide`, escalation refactor, evidence emission) is
landed and tested on the `mk/model-routing-externalization` branch. This
checklist takes it to a real task routed through Hermes on zklw.

## Blocker (needs a.r. — interactive auth gate)

- [ ] **Bring up Tailscale / confirm `ssh zklw` works from Clavain.**
      Verified 2026-07-14: `ssh zklw` timed out (`100.78.63.67:22`,
      Operation timed out) — Tailscale is down or unauthed on this machine.
      This is the gate; nothing below can run until it clears.
      Run `! tailscale status` and `! ssh zklw echo ok` to confirm.

## Pre-staged 2026-07-14 (ready to ship, no build needed)

- [x] **`ic` cross-compiled** for both Linux arches: `dist/ic-linux-amd64`, `dist/ic-linux-arm64` (static, no CGO — run on any zklw libc). Pick the arch matching `ssh zklw uname -m`.
- [x] **Hermes adapter written + validated**: `deploy/phase7/hermes-route-adapter.sh`. Calls `ic route decide`, applies the model via `hermes model`, and HALTS fail-closed on any non-zero exit (never falls back). Tested locally against a fake-hermes shim: non-confidential routes to fable (rc 0), client-confidential routes to zklw-local (rc 0), client-confidential with no local deployment is BLOCKED (rc 4, hermes never called). The only thing the shim replaced was the `hermes` binary itself.
- [x] **Seed registry** ready at `internal/routing/testdata/registry-seed.yaml` (fable-5, sol, hermes-4@zklw).

## Once zklw is reachable (I can drive these — now a copy, not a build)

- [ ] `scp dist/ic-linux-<arch>` → zklw as `ic`; `scp` the seed registry + `deploy/phase7/hermes-route-adapter.sh`.
- [ ] Confirm/adjust the `nousresearch/hermes-4@zklw` registry row's endpoint to the actual local Hermes.
- [ ] Confirm Hermes Agent is installed on zklw (`command -v hermes`); if absent, install per hermes-agent.org (single curl).
- [ ] Point the Hermes dispatch hook at `hermes-route-adapter.sh` (it already implements the fail-closed contract).
- [ ] Wire evidence emission: expose `RecordGate`/`RecordEscalation` via an `ic route record-evidence` subcommand (small, ~1 file) OR call them from the adapter, so gate + escalation events land in `ic route list`.

## The DoD-proving run

- [ ] Route ONE real task through Hermes → `ic route decide` → model selection → execution.
- [ ] Route a **client-confidential** task; confirm it lands on `hermes-4@zklw` (local) and that a vendor-cloud attempt is blocked (exit 4) — clause 1, now from a non-Clavain harness.
- [ ] Trigger (or simulate) an escalation and a gate check; confirm both appear in `ic route list` — clause 2 observable, end-to-end.
- [ ] Capture the evidence output as the DoD artifact.

## Definition of done (this checklist)

One real task routed end-to-end through `ic route` from Hermes (a harness
Clavain does not control), on zklw, with the escalation and gate evidence
visible in `ic route list`. That closes the last ⬜ in intercore-8xa.
