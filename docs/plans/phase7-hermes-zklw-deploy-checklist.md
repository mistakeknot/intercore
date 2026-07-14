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

## Once zklw is reachable (I can drive these)

- [ ] Build the `ic` binary for zklw's arch (`GOOS=linux GOARCH=<zklw arch> go build -o ic ./cmd/ic`) and copy it over.
- [ ] Copy the seed registry (`internal/routing/testdata/registry-seed.yaml`) to a zklw config path; confirm the `nousresearch/hermes-4@zklw` row's endpoint matches the actual local Hermes.
- [ ] Confirm Hermes Agent is installed on zklw (`command -v hermes`); if absent, install per hermes-agent.org (single curl).
- [ ] Write the Hermes adapter hook: on task dispatch, call `ic route decide --class=<c> --role=<r> --data=<sensitivity> --registry=<path> --json`, parse the model, apply via `hermes model`. On any non-zero exit: HALT (the fail-closed obligation — do NOT fall back to hermes's default model).
- [ ] Wire evidence emission: after the task, call the `ic` binary to record gate + escalation evidence (RecordGate/RecordEscalation are library funcs; expose via `ic route` record subcommands if not already, OR emit from the adapter).

## The DoD-proving run

- [ ] Route ONE real task through Hermes → `ic route decide` → model selection → execution.
- [ ] Route a **client-confidential** task; confirm it lands on `hermes-4@zklw` (local) and that a vendor-cloud attempt is blocked (exit 4) — clause 1, now from a non-Clavain harness.
- [ ] Trigger (or simulate) an escalation and a gate check; confirm both appear in `ic route list` — clause 2 observable, end-to-end.
- [ ] Capture the evidence output as the DoD artifact.

## Definition of done (this checklist)

One real task routed end-to-end through `ic route` from Hermes (a harness
Clavain does not control), on zklw, with the escalation and gate evidence
visible in `ic route list`. That closes the last ⬜ in intercore-8xa.
