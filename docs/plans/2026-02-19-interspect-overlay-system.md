# Interspect Overlay System (Type 1) Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use clavain:executing-plans to implement this plan task-by-task.

**Bead:** iv-vrc4
**Phase:** executing (as of 2026-02-19T02:49:24Z)
**Sprint:** iv-h3ey
**Complexity:** 3/5 (moderate)

## Problem

Interspect currently has Type 2 modifications (routing overrides — exclude entire agents) but no Type 1 modifications (prompt overlays — augment agent prompts with learned context). When evidence shows an agent consistently misses certain patterns or over-focuses on irrelevant areas, the only option is to exclude it entirely. Overlays enable surgical tuning: add domain-specific instructions to sharpen an agent's review without removing it.

## Architecture

**Overlay format:** Markdown files with YAML frontmatter at `.clavain/interspect/overlays/<agent>/<overlay-id>.md`.

**Runtime flow:**
1. Interspect `/propose` detects patterns eligible for prompt tuning (vs routing exclusion)
2. User accepts → overlay file written, git committed, canary created
3. At flux-drive dispatch (Phase 2), orchestrator reads active overlays for each selected agent
4. Active overlay content concatenated after domain context, before project context in agent prompt
5. Canary monitors quality metrics. If degradation → `/interspect:revert` disables the overlay

**Budget:** 500 tokens max per agent (sum of all active overlays for that agent).

**Rollback:** Set `active: false` in frontmatter (instant, no git revert needed).

## What Already Exists

- `interspect-init.sh` creates `overlays/` directory (line 20)
- `protected-paths.json` already allows `.clavain/interspect/overlays/**/*.md` in `modification_allow_list` and `always_propose`
- `modifications` table has `mod_type` column that supports `'prompt_tuning'`
- `canary` table + evaluation pipeline fully operational
- `_interspect_flock_git`, `_interspect_validate_target`, `_interspect_sql_escape` all reusable
- Flux-drive `launch.md` prompt template has clear injection point between Domain Context and Project Context sections

## Changes

### Task 1: Add overlay helpers to lib-interspect.sh

**File:** `hub/clavain/hooks/lib-interspect.sh`

Add after the routing override section (~line 785):

#### 1a. Shared YAML frontmatter parsers (F4: single source of truth)

**`_interspect_overlay_is_active`** — Check if an overlay file has `active: true` in frontmatter.
- Args: `$1=overlay_file_path`
- Use awk delimiter state machine (NOT `head | grep` — body content can contain `active: true`):
  ```bash
  awk '/^---$/ { if (++delim == 2) exit } delim == 1 && /^active: true$/ { found=1 } END { exit !found }' "$1"
  ```
- Returns: 0 if active, 1 if not

**`_interspect_overlay_body`** — Extract body content (everything after second `---` delimiter).
- Args: `$1=overlay_file_path`
- Use awk: `awk '/^---$/ { if (++delim == 2) { body=1; next } } body { print }' "$1"`
- Returns body on stdout, empty if no body or malformed frontmatter

#### 1b. Read/count functions

**`_interspect_read_overlays`** — Read all active overlays for an agent.
- Args: `$1=agent_name`
- Validate agent name via `_interspect_validate_agent_name`
- Scan `.clavain/interspect/overlays/<agent>/*.md` (sorted alphabetically for determinism)
- For each file, use `_interspect_overlay_is_active` to filter
- Concatenate body content via `_interspect_overlay_body`
- Return concatenated content on stdout, empty string if no active overlays

**`_interspect_count_overlay_tokens`** — Estimate token count for overlay content.
- Args: `$1=content_string`
- Use `echo "$1" | wc -w` then multiply by 1.3 (canonical implementation — `wc -w` everywhere, F12)
- Return integer on stdout (truncated, not rounded)

#### 1c. Write function (F1: everything inside flock, F3: sanitize content)

**`_interspect_write_overlay`** — Write a new overlay file atomically inside flock.
- Args: `$1=agent_name $2=overlay_id $3=content $4=evidence_ids_json $5=created_by`
- **Pre-flock validation** (fast-fail):
  - Validate agent name via `_interspect_validate_agent_name`
  - Validate overlay_id matches `^[a-z0-9-]+$` (no path traversal, F9)
  - Validate evidence_ids is a JSON array
  - **Sanitize content** via `_interspect_sanitize` (F3: prevent prompt injection in overlay body). Use 2000-char limit variant (matches 500-token budget). Also run `_interspect_redact_secrets` on content.
  - Assemble target path from validated components: `${root}/.clavain/interspect/overlays/${agent}/${overlay_id}.md`
  - Assert resolved path starts with `$(git rev-parse --show-toplevel)/.clavain/interspect/overlays/` (F9: containment check)
  - Validate target path via `_interspect_validate_target`
- **All remaining operations inside `_interspect_flock_git`** using named function pattern (like `_interspect_apply_override_locked`):
  - **Dedup check** (F6): If file already exists, reject with error (no silent overwrite)
  - **Token budget check** (F1: inside flock, TOCTOU-safe): read existing active overlays via `_interspect_read_overlays` + new content, compute total tokens, reject if > 500
  - Write markdown file with YAML frontmatter:
    ```
    ---
    active: true
    created: <ISO 8601>
    created_by: <created_by>
    evidence_ids: <JSON array>
    ---
    <content>
    ```
  - Atomic write: temp file → mv → git add → git commit (using `-F <tempfile>` for commit message)
  - Insert `modifications` row: `mod_type='prompt_tuning'`, `group_id='<agent>/<overlay_id>'` (F5: compound group_id to avoid collision with routing overrides)
  - Insert `canary` row: `group_id='<agent>/<overlay_id>'`, baseline via `_interspect_compute_canary_baseline`
  - On git commit failure: rollback (git reset HEAD + git restore), same pattern as `_interspect_apply_override_locked` lines 710-718 (F11)
- Return 0 on success, 1 on failure

#### 1d. Disable function (F2: awk state machine for frontmatter-only edit)

**`_interspect_disable_overlay`** — Toggle an overlay to inactive.
- Args: `$1=agent_name $2=overlay_id`
- Validate agent name + overlay_id
- **Inside `_interspect_flock_git`**:
  - Read the file
  - Use **awk state machine** to change `active: true` to `active: false` ONLY within frontmatter (between first and second `---`). NOT sed — sed would also match body content (F2)
  - Atomic write: temp file → mv → git add → git commit
  - Update modifications: `SET status='reverted' WHERE group_id='<agent>/<overlay_id>'`
  - Update canary: `SET status='reverted' WHERE group_id='<agent>/<overlay_id>'` (F5: compound group_id prevents accidental closure of routing override canaries)
  - On git commit failure: rollback file + leave DB unchanged (F11)

### Task 2: Add overlay injection to flux-drive launch template

**File:** `plugins/interflux/skills/flux-drive/phases/launch.md`

Add a new section **"Step 2.1d: Load active overlays"** after Step 2.1a (domain criteria) and before Step 2.1c (temp file writing).

Content of the new step:

```markdown
### Step 2.1d: Load active overlays (interspect Type 1)

For each selected agent, load pre-computed overlay content using the bash library (NOT inline parsing — F4):

1. Source `lib-interspect.sh` and call `_interspect_read_overlays "{agent-name}"`
2. If the function returns non-empty content, store it as `{OVERLAY_CONTEXT}` for that agent
3. The function handles directory existence checks, YAML parsing, active filtering, and body extraction
4. **Defense-in-depth re-sanitization** (F3): Run `_interspect_sanitize` on the returned content before injection, even though it was sanitized at write time. This catches overlays created by hand-editing or older code.

**Budget check:** `_interspect_count_overlay_tokens` uses `wc -w * 1.3` (canonical, same as write-time check — F12). If over 500, log warning and truncate to first N overlays that fit.

**Fallback:** If `_interspect_read_overlays` returns empty, skip silently. The Overlay Context section is omitted from that agent's prompt.
```

Then add the overlay injection point to the agent prompt template (after Domain Context, before Project Context):

```markdown
## Overlay Context

[If overlays were loaded in Step 2.1d for this agent:]

The following review adjustments have been learned from previous sessions. Apply them in addition to your standard review approach.

{overlay_content}

[If no overlays for this agent:]
(Omit this section entirely.)
```

### Task 3: Update interspect-propose command to support overlay proposals

**File:** `hub/clavain/commands/interspect-propose.md`

Currently, `/interspect:propose` only proposes routing overrides (Type 2 — agent exclusion). Add overlay proposal support:

After the routing override proposal section, add a new section for Type 1 proposals:

**Overlay eligibility criteria** (different from routing):
- Pattern classified as "ready" (same thresholds)
- `agent_wrong_pct` is 40-79% (between "sometimes wrong" and "almost always wrong")
  - Below 40%: too noisy, not enough signal
  - 80%+: should be a routing override instead (agent is almost always wrong)
- Pattern has specific, actionable context (from evidence `context` field) that could sharpen the agent

When eligible patterns exist, present in a separate section:

```
### Prompt Tuning Proposals (Type 1)

These agents produce SOME useful findings but have recurring blind spots. Overlays can sharpen their focus.

| Agent | Events | Wrong% | Proposed Adjustment |
|-------|--------|--------|---------------------|
```

For each proposal, auto-generate overlay content from evidence patterns:
1. Query recent `context` field from evidence for this agent
2. Summarize the pattern (what the agent gets wrong, in what contexts)
3. Draft a 2-3 sentence instruction for the overlay
4. **Sanitize the draft BEFORE presenting to user** (F8): Run `_interspect_sanitize` on the LLM-generated text so the user approves exactly what will be written
5. Present sanitized draft to user for approval/editing

On accept:
1. Generate overlay ID: `overlay-<8-char-random>`
2. Call `_interspect_write_overlay "$agent" "$overlay_id" "$content" "$evidence_ids" "interspect"`
3. Report success with canary info

### Task 4: Update interspect-revert to handle overlays

**File:** `hub/clavain/commands/interspect-revert.md`

Extend the revert command to detect whether the target is a routing override or an overlay:

After parsing the target argument:

1. Check if the target matches an existing routing override
2. Check if the target matches an agent with active overlays in `.clavain/interspect/overlays/<agent>/`
3. **If BOTH exist** (F7: disambiguation): Ask user "Agent has both a routing override and active overlays. Which do you want to revert?" Options: "Routing override", "Overlays", "Both"
4. If only routing override: current revert behavior
5. If only overlays (or user chose overlays):
   - List all active overlays for the agent (using `_interspect_overlay_is_active` — F4)
   - If multiple, ask user which to disable (or "all")
   - Call `_interspect_disable_overlay` for selected overlays
   - Ask about blacklisting (same flow as routing override revert)

### Task 5: Update interspect-status to show overlays

**File:** `hub/clavain/commands/interspect-status.md`

Add an "Overlays" section after the "Routing Overrides" section:

```markdown
## Active Overlays

Check for overlay files:

```bash
OVERLAY_DIR="${ROOT}/.clavain/interspect/overlays"
if [[ -d "$OVERLAY_DIR" ]]; then
    # Count active overlays per agent using shared parser (F4, F10)
    for agent_dir in "$OVERLAY_DIR"/*/; do
        agent=$(basename "$agent_dir")
        active_count=0
        total_count=0
        for overlay in "$agent_dir"*.md; do
            [[ -f "$overlay" ]] || continue
            total_count=$((total_count + 1))
            if _interspect_overlay_is_active "$overlay"; then
                active_count=$((active_count + 1))
            fi
        done
        # Display if any overlays exist
    done
fi
```

Present:

```
### Overlays: N active across M agents

| Agent | Active | Total | Est. Tokens | Canary | Next Action |
|-------|--------|-------|-------------|--------|-------------|
```
```

### Task 6: Ensure overlays directory exists in production

**File:** `hub/clavain/hooks/lib-interspect.sh`

In `_interspect_ensure_db`, add `mkdir -p` for the overlays directory (line ~74, after DB creation):

```bash
mkdir -p "$(dirname "$_INTERSPECT_DB")/overlays" 2>/dev/null || true
```

This is the fast-path change — ensures the overlays directory exists even if `interspect-init.sh` was never run.

### Task 7: Integration test

**File:** `hub/clavain/test-interspect-overlay.sh` (new file)

Test the full overlay lifecycle:
1. Create a test DB and overlays directory in a temp location
2. Test shared parsers: `_interspect_overlay_is_active` and `_interspect_overlay_body` with edge cases (body containing `---` horizontal rules, body containing `active: true`, missing frontmatter)
3. Write an overlay with `_interspect_write_overlay`
4. Read it back with `_interspect_read_overlays` — verify content matches sanitized input
5. Verify token counting uses `wc -w` consistently
6. Verify budget enforcement (write overlay that exceeds budget → should fail)
7. Verify dedup enforcement (write overlay with same ID → should fail, F6)
8. Disable overlay with `_interspect_disable_overlay`
9. Read again — verify empty result
10. Verify DB records: modifications row has `group_id='<agent>/<overlay_id>'` (F5), canary row has matching compound group_id
11. Verify sanitization: write overlay with `<system>` in body → verify rejection
12. Verify path containment: write overlay with `../` in overlay_id → verify rejection
13. Clean up

## Key Conventions

- All file operations under `_interspect_flock_git` (30s timeout)
- All SQL values through `_interspect_sql_escape`
- Agent names validated via `_interspect_validate_agent_name`
- Path traversal protection via `_interspect_validate_target`
- Overlay IDs validated: `^[a-z0-9-]+$`
- Git commits use `-F <tempfile>` (no shell injection)
- Canary auto-created on apply, auto-closed on disable

## Review Findings Incorporated

All P0-P2 findings from flux-drive review (fd-architecture, fd-safety, fd-correctness) have been incorporated:

| ID | Fix | Location |
|----|-----|----------|
| F1 | Budget check + DB inserts inside flock | Task 1c |
| F2 | awk state machine for frontmatter-only edit | Task 1d |
| F3 | Sanitize overlay content at write + re-sanitize at injection | Task 1c, Task 2 |
| F4 | Shared YAML parsers (`_interspect_overlay_is_active`, `_interspect_overlay_body`) | Task 1a |
| F5 | Compound `group_id='<agent>/<overlay_id>'` for DB records | Task 1c, 1d |
| F6 | Dedup guard — reject if overlay_id file already exists | Task 1c |
| F7 | Revert disambiguation when agent has both override + overlays | Task 4 |
| F8 | Sanitize LLM-drafted text before presenting to user | Task 3 |
| F9 | Path assembly from validated components + containment assertion | Task 1c |
| F10 | Status uses shared parser instead of `head | grep` | Task 5 |
| F11 | Git-failure rollback in write + disable functions | Task 1c, 1d |
| F12 | `wc -w` as canonical token counting everywhere | Task 1b, Task 2 |

Deferred to future:
- F13: Agent-scoped canary baseline (useful but not blocking — current global baseline is conservative)
- F14: Parallel-write integration test (low risk given flock serialization)

## Not in Scope

- Auto-generating overlay content from evidence (Task 3 does manual proposal with auto-draft)
- A/B testing multiple overlays simultaneously (future: iv-ynbh trust scoring)
- Overlay consolidation (merging multiple overlays into one when budget fills)
- Cross-project overlay sharing
