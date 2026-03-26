# Intercore Event Contracts

Machine-readable contracts for the Intercore event bus. Schemas in this
directory are **generated** by `go generate ./contracts/...` — do not
hand-edit the `.json` files. To add constraints, use `jsonschema` struct
tags on the Go types in `internal/event/`.

## Event Sources

| Source | Table | In unified stream? | Key consumers |
|---|---|---|---|
| `phase` | `phase_events` | Yes (ListEvents, ListAllEvents) | Clavain gate_calibration, interspect |
| `dispatch` | `dispatch_events` | Yes (ListEvents, ListAllEvents) | Clavain sprint, Skaffen |
| `discovery` | `discovery_events` | ListAllEvents only (no run_id) | interphase |
| `coordination` | `coordination_events` | Yes (ListEvents, ListAllEvents) | interlock, Clavain |
| `review` | `review_events` | Yes (ListEvents, ListAllEvents) | interspect evidence pipeline |
| `interspect` | `interspect_events` | **No** — use `ListInterspectEvents` | interspect, Skaffen evidence emitter |
| `intent` | `intent_events` | **No** — planned for future unification | Ockham (future) |

Note: `interspect` and `intent` are valid Source values (accepted by `Event.Validate()`)
but are NOT included in the `ListEvents`/`ListAllEvents` UNION ALL queries. They have
dedicated query methods. The `.2` sub-epic (Demarch-og7m.2) will unify all sources.

## Schemas

- [`event.json`](event.json) — Unified Event type (generated)
- [`event-envelope.json`](event-envelope.json) — EventEnvelope provenance data (generated)
- [`review-event.json`](review-event.json) — ReviewEvent type (generated)
- [`interspect-event.json`](interspect-event.json) — InterspectEvent type (generated)

## Versioning

Breaking changes require:
1. Update the Go struct (the source of truth)
2. Re-run `go generate ./contracts/...`
3. Update all consumers listed above
4. Dual-write during migration period

The planned EventEnvelope v2 (Demarch-og7m.2.1) will update the struct and regenerate.
