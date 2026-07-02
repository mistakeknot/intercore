# Task 7: Extend clavain:resolve to Emit Disagreement Events

## Summary

Added Step 5b "Emit Disagreement Events" to `/home/mk/projects/Demarch/os/clavain/commands/resolve.md`, immediately after the existing Step 5 "Record Trust Feedback". This completes the resolve-side emission of `disagreement_resolved` kernel events via `ic events emit`.

## What Was Done

### File Modified
- `/home/mk/projects/Demarch/os/clavain/commands/resolve.md` (lines 102-166 added)

### Change Description

A new markdown section `### 5b. Emit Disagreement Events` was inserted after Step 5's closing "Silent failures" note (line 100). No existing content was modified.

The new section:
1. Iterates findings.json for entries with `severity_conflict` (same data source as Step 5)
2. Applies an **impact gate** that filters to only meaningful disagreements:
   - `decision_changed`: finding discarded despite P0/P1 rating from at least one agent
   - `severity_overridden`: finding accepted but at a severity different from what some agents rated
3. Emits `disagreement_resolved` events via `ic events emit` with structured context containing: finding_id, agents map, resolution, dismissal_reason, chosen_severity, and impact type

### Event Schema

```
source: review
type: disagreement_resolved
context: {
  finding_id: string,
  agents: {agent_name: severity_rating, ...},
  resolution: "accepted" | "discarded",
  dismissal_reason: string (only for discarded),
  chosen_severity: string,
  impact: "decision_changed" | "severity_overridden"
}
```

## Design Decisions

### Impact Gate Rationale
Not every severity_conflict warrants an event. The gate ensures only decision-altering disagreements are recorded:
- A P3 vs P4 disagreement where both would be accepted at P3 is noise
- A P0 vs P3 disagreement where the finding is discarded is signal (an agent flagged critical severity but the human disagreed)
- An accepted finding where the chosen severity differs from some agents' ratings is signal (severity was overridden)

### Reuse of Step 5 Variables
Step 5b reuses `$FINDINGS_JSON` which is set in Step 5's code block. It also re-declares `$SESSION_ID` for self-containment (Step 5 sets it in a conditional block that might not execute if the trust plugin isn't found). `$PROJECT_ROOT` is newly computed via `git rev-parse`.

### Silent Fail-Open Pattern
Matches Step 5's approach: `2>/dev/null || true` on the `ic events emit` call ensures resolve never fails due to event emission. The entire block is also guarded by `command -v ic` to skip gracefully when the IC CLI isn't available.

## Verification

After the edit, the file structure is:
- Lines 1-73: Frontmatter, source detection, workflow steps 1-4
- Lines 74-100: Step 5 "Record Trust Feedback" (unchanged)
- Line 101: Blank separator
- Lines 102-166: Step 5b "Emit Disagreement Events" (new)

The markdown renders correctly with Step 5b as a sibling heading to Step 5 under the Workflow section.

## Dependencies

- `ic events emit` CLI command (already tested and working per task context)
- `jq` for JSON processing (standard dependency across Clavain)
- `findings.json` with `severity_conflict` field populated by the synthesis step in flux-drive review

## What's NOT Included (Intentionally)

- No commit was made (per instructions, parent session handles commits)
- No changes to any other files
- No modifications to existing Step 5 content
