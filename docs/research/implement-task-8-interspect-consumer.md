# Task 8 Implementation: Interspect Disagreement Event Consumer

## Summary

Added the interspect disagreement event consumer that converts kernel review events into evidence records. This bridges the disagreement pipeline (kernel-side review_events table) with interspect's evidence system, enabling agent accuracy tracking from disagreement resolution data.

## Changes Made

**File:** `/home/mk/projects/Demarch/interverse/interspect/hooks/lib-interspect.sh`

### 1. `_interspect_process_disagreement_event` (line 2064)

Processes a single `disagreement_resolved` event from the kernel review_events table. For each agent whose severity was overridden (i.e., differs from `chosen_severity`), it creates an evidence record via `_interspect_insert_evidence`.

**Dismissal reason mapping:**
| dismissal_reason | override_reason (evidence) |
|---|---|
| `agent_wrong` | `agent_wrong` |
| `deprioritized` | `deprioritized` |
| `already_fixed` | `stale_finding` |
| `not_applicable` | `agent_wrong` |
| (empty, severity_overridden) | `severity_miscalibrated` |

**Evidence record fields:**
- `session_id`: from event JSON
- `source`: agent name (from `agents_json` keys)
- `event`: `disagreement_override`
- `override_reason`: mapped from dismissal_reason
- `context_json`: finding_id, agent_severity, chosen_severity, resolution, impact, dismissal_reason
- `hook_id`: `interspect-disagreement`

### 2. `_interspect_consume_review_events` (line 2125)

Cursor-based consumer that queries review events via `ic events list-review --since=N --limit=100`. Uses `ic state` for cursor persistence with key `interspect-disagreement-review-cursor`. This is separate from the main event cursor system because review events are NOT in the UNION ALL stream.

**Flow:**
1. Read cursor from `ic state get interspect-disagreement-review-cursor`
2. Query `ic events list-review --since=$cursor --limit=100`
3. For each event, call `_interspect_process_disagreement_event`
4. Track max event ID, persist cursor via `ic state set`

### 3. Wiring in `_interspect_consume_kernel_events` (line 2057-2058)

Added a single call at the end of the existing kernel event consumer:
```bash
# Poll review events via separate query (not in UNION ALL)
_interspect_consume_review_events || true
```

This means review events are consumed every time kernel events are consumed (at session start), keeping both pipelines in sync.

## Verification

```
$ bash -n /home/mk/projects/Demarch/interverse/interspect/hooks/lib-interspect.sh
SYNTAX OK
```

## Function Placement

| Function | Line |
|---|---|
| `_interspect_consume_kernel_events` | 2013 |
| `_interspect_process_disagreement_event` | 2064 |
| `_interspect_consume_review_events` | 2125 |
| `_interspect_get_canary_summary` (next existing) | 2160 |

## Architecture Notes

- Review events are separate from the UNION ALL event stream because they have different schema/semantics (finding_id, agents_json, resolution vs. run_id, from_state, to_state)
- The cursor is stored via `ic state` rather than the consumer cursor system used by `ic events tail --consumer=`
- Only agents whose severity **differs** from chosen_severity get evidence records — agents who agreed with the final decision are not penalized
- All jq parsing uses `// empty` fallback to handle missing fields gracefully
- Error handling is permissive (`|| return 0`, `|| true`) — a single bad event should not block the pipeline
