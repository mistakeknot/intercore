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

### v1 (current writers)
- [`event.json`](event.json) — Unified Event type (generated)
- [`event-envelope.json`](event-envelope.json) — EventEnvelope v1 provenance data (generated)
- [`review-event.json`](review-event.json) — ReviewEvent type (generated)
- [`interspect-event.json`](interspect-event.json) — InterspectEvent type (generated)

### v2 (og7m.2.1 — types defined, writers migrate in og7m.2.2)
- [`event-envelope-v2.json`](event-envelope-v2.json) — EventEnvelopeV2 with version + typed payload (generated)
- [`event-phase-payload.json`](event-phase-payload.json) — Phase source payload (generated)
- [`event-dispatch-payload.json`](event-dispatch-payload.json) — Dispatch source payload (generated)
- [`event-coordination-payload.json`](event-coordination-payload.json) — Coordination source payload (generated)

Note: `payload` in the v2 schema renders as `true` (JSON Schema 2020-12 boolean schema,
semantically equivalent to `{}`). This is `json.RawMessage` — use `payload_type` to
determine which payload schema applies.

## Versioning

Breaking changes require:
1. Update the Go struct (the source of truth)
2. Re-run `go generate ./contracts/...`
3. Update all consumers listed above
4. Dual-write during migration period

### v1 → v2 Migration

`ParseEnvelopeV2JSON` reads both v1 and v2 formats:
- **v2** (has `"version": 2`): decoded directly into `EventEnvelopeV2`
- **v1** (no `version` field): core tracing fields mapped to v2, full v1 JSON stored as `Payload` with `PayloadType="legacy"`. Use `ParsePayload[EventEnvelope]` to recover all original v1 fields.

**Migration sequence:**
1. og7m.2.1 (this): Define v2 types + helpers. No writers changed.
2. og7m.2.2: Writers emit v2. `scanEvents` updated to use `ParseEnvelopeV2JSON`.
3. og7m.2.5: Clavain subscription uses v2 reader.
4. og7m.2.6: Remove v1 types + `ParseEnvelopeJSON`.

**IMPORTANT:** og7m.2.2 MUST update `scanEvents` to `ParseEnvelopeV2JSON` in the same
commit that changes writers. Otherwise new v2 rows will be silently parsed as v1,
dropping the `version` and `payload_type` fields.
