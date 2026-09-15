---
artifact_type: card
card_version: 1
project: intercore
status: confirmed
confirmed_by: mk
confirmed_at: 2026-09-14
line: "OS layers need crash-safe agent state"
fields:
  persona:
    state: confirmed
    value: "The OS layer (Clavain) and companion plugins that shell out to ic for every state operation"
    evidence:
      - { path: "README.md:9", scope: project }
      - { path: "PHILOSOPHY.md:6", scope: project }
  pain:
    state: confirmed
    value: "Agents moving through a multi-phase lifecycle need something durable to record where they are, what they did, and whether they may proceed"
    evidence:
      - { path: "README.md:7", scope: project }
  cuj:
    state: declined
    reason: "No CUJ file exists in the repo; docs/cujs/ is absent"
    needs: "A written journey for an OS-layer author driving a run through ic"
  success:
    state: declined
    reason: "The repo states a direction (durable, crash-safe primitives; every state change produces a typed durable event) but no threshold or measure of whether the kernel is succeeding"
    needs: "mk's measure, e.g. a rate of lost or unattributable state transitions, with a baseline"
    evidence:
      - { path: "PHILOSOPHY.md:9", scope: project }
  guardrail:
    state: confirmed
    value: "Every operation stays crash-safe and every state change stays auditable"
    evidence:
      - { path: "PHILOSOPHY.md:12", scope: project }
decisions: []
---

intercore is the kernel the rest of the agency writes its state into: runs, phases, gates, dispatches, events and token budgets, recorded durably so the layers above can stop and resume without losing their place.

Drafted by an agent from in-repo citations while working bead mk-qozg and confirmed by mk on 2026-09-14. `cuj` and `success` are declined because nothing in the repo supports them yet.
