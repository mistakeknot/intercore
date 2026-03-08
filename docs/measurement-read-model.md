# Measurement Read Model

The canonical measurement read model for intercore events. Consumers that need correctness guarantees for scoring, routing, or attribution MUST use the typed APIs, not the lossy generic bus.

## Three Event Streams

### 1. Generic Stream — lifecycle awareness

**CLI:** `ic events tail <run_id>` | **Go:** `ListAllEvents()`, `ListEvents()`

Merges 5 tables via UNION ALL: `phase_events`, `dispatch_events`, `discovery_events`, `coordination_events`, `review_events`. Each source's fields are projected into a common shape:

| Field | Meaning |
|---|---|
| `id` | Per-source autoincrement ID |
| `run_id` | Run scope |
| `source` | `"phase"`, `"dispatch"`, `"discovery"`, `"coordination"`, `"review"` |
| `type` | Event type within source |
| `from_state` | Source-specific: from_phase, from_status, owner, finding_id |
| `to_state` | Source-specific: to_phase, to_status, pattern, resolution |
| `reason` | Source-specific: reason text, payload, agents_json |
| `envelope` | Optional provenance envelope (trace_id, span_id, etc.) |
| `timestamp` | Event creation time |

**Lossy for review events.** The generic projection maps review fields as:
- `finding_id` → `from_state`
- `resolution` → `to_state`
- `agents_json` → `reason`
- **Dropped:** `dismissal_reason`, `chosen_severity`, `impact`, `session_id`, `project_dir`

**Use for:** Phase transitions, dispatch lifecycle, discovery status changes, coordination events. Sufficient for "something happened" dashboards. Not sufficient for scoring or attribution.

### 2. Review Events — disagreement resolution (full fidelity)

**CLI:** `ic events list-review [--since=<id>] [--limit=N]` | **Go:** `ListReviewEvents()`

Returns `ReviewEvent` objects with all fields preserved. Contract: [`contracts/events/review-event.json`](../contracts/events/review-event.json).

| Field | Required | Description |
|---|---|---|
| `id` | yes | Autoincrement ID for cursor tracking |
| `finding_id` | yes | Unique identifier for the reviewed finding |
| `agents_json` | yes | JSON map of agent name → vote/severity |
| `resolution` | yes | Final decision: `"accepted"`, `"discarded"`, etc. |
| `chosen_severity` | yes | Priority level chosen by reviewer |
| `impact` | yes | Outcome: `"severity_overridden"`, `"decision_changed"`, etc. |
| `dismissal_reason` | no | Why disagreement was resolved: `"agent_wrong"`, `"not_applicable"`, `"deprioritized"`, `"already_fixed"` |
| `session_id` | no | Claude session ID of the reviewer |
| `project_dir` | no | Project directory context |
| `run_id` | no | Associated run ID |
| `timestamp` | yes | Event creation time |

**Use for:** Agent scoring, routing eligibility, false-positive rate computation, canary baselines, interspect evidence ingestion.

**Cursor pattern:** `ic events list-review --since=<last_seen_id> --limit=100`. Consumers track their own cursor (e.g., via `ic state set`).

### 3. Interspect Events — manual corrections

**Go:** `ListInterspectEvents()` | **CLI:** Not exposed in unified stream

Records explicit human corrections and agent-performance signals. Contract: [`contracts/events/interspect-event.json`](../contracts/events/interspect-event.json).

**Not included in the generic UNION ALL stream.** Consumers must query separately.

**Use for:** Override tracking, manual correction evidence, pattern detection for routing proposals.

## Canonical Measurement Read Model

For complete measurement coverage, consumers need:

```
measurement_read_model = generic_stream + review_events + interspect_events
```

| What you need | Which stream |
|---|---|
| "What phases did this run go through?" | Generic stream |
| "How many dispatches ran and what were their token costs?" | Generic stream |
| "Was this agent's finding accepted or dismissed?" | Review events (full fidelity) |
| "Why was the finding dismissed?" | Review events (`dismissal_reason`) |
| "Has this agent been manually corrected?" | Interspect events |
| "What's this agent's false-positive rate?" | Review events + interspect events |
| "Is this agent routing-eligible?" | Review events + interspect events (via interspect evidence) |

## Design Decision

The system explicitly chose **typed side-channel APIs over a fully-unified stream** for review and interspect events. This preserves field fidelity at the cost of requiring consumers to make multiple queries. The alternative (enriching the generic projection) was rejected because:

1. The generic `Event` struct would need to carry ~15 optional fields from different source types
2. Existing consumers of the generic stream don't need review-specific fields
3. Cursor semantics are per-source anyway — consumers already track independent positions
