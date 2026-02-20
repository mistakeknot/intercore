# Correctness Review: Interspect Overlay System Implementation Plan

**Reviewer:** Julik (flux-drive correctness)
**Date:** 2026-02-19
**Document:** `docs/plans/2026-02-19-interspect-overlay-system.md`
**Prior review reference:** `docs/research/flux-drive/2026-02-15-interspect-routing-overrides-plan/fd-correctness.md`

---

## Invariants That Must Hold

Before reviewing, I am pinning the correctness invariants this system must maintain. Each finding below references one or more of these.

**INV-1 (Atomic Write-Commit-Record):** A new overlay must appear in exactly one state: either the file exists on disk, is committed to git, AND has a modifications row AND a canary row — or none of the above. Partial states (file written but not committed, committed but no DB record, DB record with wrong SHA) are corruption.

**INV-2 (Canary Uniqueness):** Each overlay has at most one `active` canary row at any given time. Reapplying an existing overlay must not create a second canary.

**INV-3 (Token Budget Consistency):** The budget check at write time and the budget check at runtime (flux-drive dispatch) use the same definition of "active overlays for agent". If the check-then-act window is long enough, the budget can be exceeded.

**INV-4 (Disable Completeness):** `_interspect_disable_overlay` must leave the system in a consistent disabled state: `active: false` in file, git committed, modifications row `status='reverted'`, canary row `status='reverted'`. Missing any leg makes the state inconsistent.

**INV-5 (Path Safety):** Overlay files are written only within `.clavain/interspect/overlays/<agent>/`. No write can escape this directory regardless of agent name or overlay ID content.

**INV-6 (YAML Parse Determinism):** The frontmatter parser must produce the same result for any valid overlay file that any write path can produce. Edge cases in `---` delimiter detection must be handled consistently between writer and readers.

---

## Findings Index

- P0 | RACE-01 | Task 1, `_interspect_write_overlay` | TOCTOU on token budget check — check and write are two separate flock acquisitions (INV-1, INV-3)
- P0 | DATA-01 | Task 1, `_interspect_write_overlay` | DB inserts not explicitly scoped inside flock — same bug as RACE-01 from routing overrides plan (INV-1)
- P0 | DATA-02 | Task 1, `_interspect_disable_overlay` | Non-atomic YAML patch — `sed`/string replacement of `active: true` is ambiguous and broken for multi-occurrence frontmatter (INV-4, INV-6)
- P1 | RACE-02 | Task 1, `_interspect_read_overlays` | Read not under flock — concurrent write produces torn read of partially-renamed temp file (INV-3, INV-6)
- P1 | PARSE-01 | Task 1, `_interspect_read_overlays` | YAML `---` delimiter parsing in bash is fragile — body extraction breaks if content contains `---` lines (INV-6)
- P1 | DATA-03 | Task 1, `_interspect_write_overlay` | No dedup guard — reapplying the same overlay_id creates duplicate modifications + canary rows (INV-2)
- P1 | DATA-04 | Task 1, `_interspect_disable_overlay` | DB status updates not conditional on file type — disabling a routing override's canary by group_id collision (INV-4)
- P2 | RACE-03 | Task 3 interspect-propose | Overlay write during propose flow — budget check races with concurrent overlay write for the same agent (INV-3)
- P2 | PARSE-02 | Task 5, interspect-status | `head -10 | grep` for `active: true` is not a frontmatter parser — false positive if content body contains `active: true` (INV-6)
- P2 | DATA-05 | Task 4, interspect-revert | Revert command checks overlays directory existence BEFORE acquiring flock — agent with no overlay directory but active DB records not handled (INV-4)
- P2 | SHELL-01 | Task 1, `_interspect_write_overlay` | Content argument passed as positional `$3` — newlines and special characters in overlay content corrupt argument list (INV-5)

**Verdict:** Needs changes (P0 issues block merge)

---

## Summary

The overlay plan has sound architectural intentions and correctly reuses the validated routing override infrastructure (`_interspect_flock_git`, `_interspect_validate_target`, `_interspect_sql_escape`, `_interspect_compute_canary_baseline`). However, it repeats the most critical bug class from the routing overrides plan: the plan describes the DB inserts and the file write as happening "inside flock" but the actual sequence in the prose is ambiguous, and Task 1's description of `_interspect_write_overlay` says "Atomic write inside `_interspect_flock_git`" for the file, then "Insert `modifications` row... Insert `canary` row" as if these are separate steps that might not be inside the same flock acquisition.

The most dangerous new bug is PARSE-01 and DATA-02: both the reader and the disabler use bash string operations to parse YAML frontmatter. The writer produces files with `---` delimiters, but the reader must find "the second `---`" to split header from body. If overlay content itself contains a line that is exactly `---` (a horizontal rule in Markdown), the parser will split at the wrong boundary. The disabler replaces `active: true` with `active: false` using what will likely be `sed`, which would also match `active: true` in the content body.

The write-then-disable lifecycle is the core correctness path. Both have failure modes that can leave the system in an inconsistent state without any error signal.

---

## Issues Found

### P0 | RACE-01 | Task 1, `_interspect_write_overlay` | TOCTOU on token budget check

**Location:** Task 1, `_interspect_write_overlay` description, step "Check token budget: read existing active overlays + new content must be <= 500 tokens"

**Evidence:**

The plan describes these steps for `_interspect_write_overlay`:

```
- Check token budget: read existing active overlays + new content must be <= 500 tokens
- Write markdown file with YAML frontmatter
- Atomic write inside _interspect_flock_git: temp file → validate → mv → git add → git commit
- Insert modifications row
- Insert canary row
```

The budget check reads "existing active overlays" via `_interspect_read_overlays`. That function scans `.clavain/interspect/overlays/<agent>/*.md` files. If this read happens BEFORE entering `_interspect_flock_git`, there is a window between the check and the write where another session can write a different overlay for the same agent. Both sessions will see the budget as non-exceeded, but together they will exceed it.

**Failure narrative:**

1. Agent `fd-correctness` has 400 tokens of active overlays.
2. Budget = 500. Remaining = 100 tokens.
3. Session A: propose flow generates an 80-token overlay. Budget check passes (400+80=480).
4. Session B: propose flow generates a 90-token overlay for the same agent. Budget check passes (400+90=490) — Session A's write has not happened yet.
5. Session A acquires flock, writes overlay-aaa.md (80 tokens), commits, releases flock.
6. Session B acquires flock, writes overlay-bbb.md (90 tokens), commits, releases flock.
7. Total tokens for fd-correctness: 400+80+90 = 570. Budget exceeded silently.

The budget check must happen inside the same flock acquisition as the write. The plan must be explicit: `_interspect_read_overlays` and the budget gate must run inside `_interspect_flock_git` before the file write.

**Fix:**

The entire body of `_interspect_write_overlay` must execute inside a single `_interspect_flock_git` call. Use the same named-function pattern as `_interspect_apply_override_locked`:

```bash
_interspect_write_overlay() {
    local agent="$1" overlay_id="$2" content_file="$3" evidence_ids="$4" created_by="$5"

    # Pre-flock validation (fast-fail, non-state-mutating)
    _interspect_validate_agent_name "$agent" || return 1
    [[ "$overlay_id" =~ ^[a-z0-9-]+$ ]] || { echo "ERROR: Invalid overlay_id" >&2; return 1; }
    ...

    _interspect_flock_git _interspect_write_overlay_locked \
        "$root" "$agent" "$overlay_id" "$content_file" "$evidence_ids" "$created_by" "$db"
}

_interspect_write_overlay_locked() {
    set -e
    # 1. Budget check (INSIDE flock — TOCTOU-safe)
    local existing_content
    existing_content=$(_interspect_read_overlays "$agent")  # reads from disk, now safe
    local existing_tokens new_tokens total_tokens
    existing_tokens=$(_interspect_count_overlay_tokens "$existing_content")
    new_content=$(cat "$content_file")
    new_tokens=$(_interspect_count_overlay_tokens "$new_content")
    total_tokens=$(( existing_tokens + new_tokens ))
    if (( total_tokens > 500 )); then
        echo "ERROR: Overlay budget exceeded ($total_tokens/500 tokens)" >&2
        return 1
    fi

    # 2. Write file atomically
    # 3. Git add + commit
    # 4. DB inserts (INSIDE flock, with commit SHA from this write)
}
```

Note also that content must arrive as a file path (`$content_file`), not as a positional string argument — see SHELL-01 below.

---

### P0 | DATA-01 | Task 1, `_interspect_write_overlay` | DB inserts not explicitly scoped inside flock

**Location:** Task 1, `_interspect_write_overlay`, steps "Insert modifications row" and "Insert canary row"

**Evidence:**

The plan lists these as bullet points AFTER "Atomic write inside `_interspect_flock_git`", which is ambiguous about whether they are inside the same flock acquisition or follow it. The routing overrides plan had the exact same structural ambiguity, and it was a P0 bug there (RACE-01 in the prior review). The fix implemented in `_interspect_apply_override_locked` (lib-interspect.sh line 723) correctly performs DB inserts inside the flock block. The overlay plan must be equally explicit.

**Failure narrative:**

1. Session A writes overlay-aaa.md, git commits (SHA1), flock released.
2. Session B writes overlay-bbb.md for the same agent, git commits (SHA2), flock released.
3. Session A inserts: `modifications(group_id='fd-correctness/overlay-aaa', commit_sha=SHA1)`, canary row.
4. Session B inserts: `modifications(group_id='fd-correctness/overlay-bbb', commit_sha=SHA2)`, canary row.
5. Process A crashes between step 1 and step 3. No modifications row exists for overlay-aaa.md.
6. `_interspect_disable_overlay` queries `modifications WHERE target_file = 'overlay-aaa.md'` — finds nothing. Treats overlay as untracked. DB update is silently skipped. File is disabled on disk but DB shows no history.

The commit SHA stored in the DB becomes the anchor for `interspect-revert` targeting by SHA. If the SHA is for a different commit than the one that introduced the overlay file, the revert audit trail is wrong.

**Fix:**

Same as RACE-01: all DB inserts must be inside `_interspect_write_overlay_locked` (the named function that runs inside `_interspect_flock_git`). The overlay plan must make this explicit with the same named-function pattern already used by routing overrides.

---

### P0 | DATA-02 | Task 1, `_interspect_disable_overlay` | Non-atomic YAML frontmatter patch

**Location:** Task 1, `_interspect_disable_overlay`, step "Read the file, change `active: true` to `active: false` in frontmatter"

**Evidence:**

The plan specifies:

```
- Read the file, change active: true to active: false in frontmatter
- Atomic write inside flock, git commit
```

The phrase "change `active: true` to `active: false` in frontmatter" implies a text substitution. The natural implementation is `sed 's/^active: true$/active: false/' "$overlay_file"`. This has three failure modes:

**Failure 1 — Match in body content:**

An overlay whose Markdown body contains the exact line `active: true` (a configuration example, a code snippet, a review note) will have that line flipped to `active: false` too. The resulting file will have the frontmatter `active: false` and a corrupted body.

**Failure 2 — No match in frontmatter:**

If the overlay was written with `active:true` (no space) or `Active: true` (capitalized), `sed` will not match and the file is not updated. `_interspect_disable_overlay` returns 0 (success), git commits an unchanged file (or git commit fails with "nothing to commit"), and the overlay remains active.

**Failure 3 — Write race (non-atomic):**

If `_interspect_disable_overlay` reads the file, applies the sed substitution to a variable, and then writes back, another process could write the file between the read and the write. Even with the outer flock, if the write itself is not a temp-then-rename, a crash mid-write leaves a partial file.

**Failure narrative for Failure 1:**

```
Overlay file body:
---
active: true
created: 2026-02-19T00:00:00Z
---
This agent should check for patterns where:
- active: true connections are leaked
```

After `sed 's/^active: true$/active: false/'`:

```
---
active: false      <- correct
created: 2026-02-19T00:00:00Z
---
This agent should check for patterns where:
- active: false connections are leaked  <- corrupted
```

The overlay content has been silently corrupted. `git diff` will show this as intentional. No error is raised.

**Fix:**

Parse and rewrite the YAML frontmatter section only, using delimiter-aware processing. The canonical approach in bash:

```bash
_interspect_disable_overlay_locked() {
    set -e
    local overlay_file="$1"
    local tmpfile="${overlay_file}.tmp.$$"

    # Rewrite only the frontmatter active: field.
    # Strategy: stream-edit only the section between the two --- delimiters.
    # Use awk to track state.
    awk '
        BEGIN { in_front=0; done_front=0; delim_count=0 }
        /^---$/ && !done_front {
            delim_count++
            if (delim_count == 1) { in_front=1; print; next }
            if (delim_count == 2) { in_front=0; done_front=1; print; next }
        }
        in_front && /^active: true$/ { print "active: false"; next }
        { print }
    ' "$overlay_file" > "$tmpfile"

    # Validate: tmpfile must differ from original in exactly the frontmatter
    if ! grep -q "^active: false$" "$tmpfile"; then
        rm -f "$tmpfile"
        echo "ERROR: Could not set active: false in frontmatter of ${overlay_file}" >&2
        return 1
    fi

    mv "$tmpfile" "$overlay_file"
}
```

The awk state machine tracks delimiter count to restrict editing to the frontmatter section. Lines after the second `---` are copied verbatim.

---

### P1 | RACE-02 | Task 1, `_interspect_read_overlays` | Torn read of temp file during concurrent write

**Location:** Task 1, `_interspect_read_overlays`, "Scan `.clavain/interspect/overlays/<agent>/*.md`"

**Evidence:**

`_interspect_read_overlays` scans the overlays directory with a glob. `_interspect_write_overlay` uses a temp-then-rename write pattern (plan says "temp file → validate → mv"). The glob can expand BEFORE the rename, then by the time the file is read, the rename has happened and the glob misses the new file (or vice versa: glob expands AFTER rename, reading a new file that wasn't counted in the budget check of the caller).

More critically: if `_interspect_read_overlays` is called at flux-drive launch time (Task 2, Step 2.1d) while a write is in progress, the glob may match the `.tmp.$$` file if the implementation does not restrict the glob to `*.md` files only, or may match a partially-renamed file on some filesystems.

The plan says "Sort by filename (alphabetical) for deterministic ordering" — but the glob output under concurrent writes is non-deterministic: the temp file pattern is `${overlay_file}.tmp.$$` which sorts differently than the final name.

**Failure narrative:**

1. Flux-drive dispatch is reading overlays for `fd-correctness` to build agent prompt.
2. Concurrently, user runs `_interspect_write_overlay` for `fd-correctness`.
3. Write produces `/tmp` file at `overlay-aaa.md.tmp.12345`, then renames to `overlay-aaa.md`.
4. If the glob runs between temp creation and rename, it misses `overlay-aaa.md` entirely.
5. Agent prompt is built without the new overlay.
6. This is a benign miss (the overlay will be picked up next session), but if the budget check for a SECOND write was based on this read, the budget accounting is wrong.

**Fix:**

`_interspect_read_overlays` when called at dispatch time is a read-only operation and a benign TOCTOU is acceptable (the overlay will be active next session). The plan should document this explicitly with a comment: "Optimistic read — does not acquire flock. Concurrent writes may be missed, picked up on next dispatch." This is the same reasoning applied to `_interspect_read_routing_overrides` in the routing overrides implementation.

However, when `_interspect_read_overlays` is called FROM INSIDE `_interspect_write_overlay_locked` (for budget checking), it IS inside the flock, so that path is safe. The function must document whether it requires flock context or not.

The glob must be restricted to `*.md` only (not `*.md.tmp.*`) — this should be explicit in the implementation note.

---

### P1 | PARSE-01 | Task 1, `_interspect_read_overlays` | YAML `---` delimiter parsing breaks on content with `---`

**Location:** Task 1, `_interspect_read_overlays`, "parse YAML frontmatter (between `---` delimiters)" and "Concatenate the body content (everything after second `---`)"

**Evidence:**

The plan describes the algorithm as:

```
For each file, parse YAML frontmatter (between --- delimiters)
Filter: only files where active: true in frontmatter
Concatenate the body content (everything after second ---)
```

In bash, the naive implementation of "everything after the second `---`" is:

```bash
# Find line number of second ---
second_delim=$(grep -n "^---$" "$file" | awk -F: 'NR==2{print $1}')
tail -n +$((second_delim + 1)) "$file"
```

This is correct for well-formed overlays where the body contains no line that is exactly `---`. But Markdown uses `---` as a horizontal rule. An overlay body with content like:

```markdown
---
active: true
created: 2026-02-19T00:00:00Z
---
When reviewing Go code, pay attention to:
- Mutex acquisition ordering

---

Also watch for channel direction violations.
```

has three `---` lines. The parser will find the second one (the frontmatter close) correctly IF it counts from the beginning. But if the grep returns all occurrences and the awk takes line `NR==2`, it will correctly identify the second one. However, the frontmatter `active:` check then searches the content between line 1 and line `second_delim`, which is correct.

The deeper problem is the active-check. If the implementation does:

```bash
if grep -q "^active: true$" "$file"; then
```

without restricting to frontmatter lines, it will include `active: true` that appears anywhere in the body, producing false positives.

**Failure narrative:**

Overlay body contains a code snippet:

```markdown
---
active: false
created: 2026-02-19T00:00:00Z
---
Watch for state machines that transition to active: true
without validation.

Example of a bug:
```yaml
active: true  # should be false
```
```

A naive `grep -q "^active: true"` would NOT match here (the yaml code block line has leading spaces). But `grep -q "active: true"` (without `^`) would match. The plan must specify the exact grep pattern and that it must be restricted to frontmatter.

The spec says "parse YAML frontmatter" without defining the exact algorithm. This vagueness will lead to inconsistent implementations between `_interspect_read_overlays`, `_interspect_status` (which uses `head -10 | grep`), and `_interspect_disable_overlay` (which patches via replacement). All three callers must agree on what "active in frontmatter" means.

**Fix:**

Define a single shared helper `_interspect_overlay_is_active "$file"` that all callers use:

```bash
_interspect_overlay_is_active() {
    local file="$1"
    # Parse ONLY the frontmatter (between first and second ---)
    awk '
        BEGIN { in_front=0; delim_count=0 }
        /^---$/ {
            delim_count++
            if (delim_count == 1) { in_front=1; next }
            if (delim_count == 2) { exit }
        }
        in_front && /^active: true$/ { found=1 }
        END { exit !found }
    ' "$file"
}
```

And a shared `_interspect_overlay_body "$file"` that extracts only the body:

```bash
_interspect_overlay_body() {
    local file="$1"
    awk '
        BEGIN { delim_count=0; printing=0 }
        /^---$/ {
            delim_count++
            if (delim_count == 2) { printing=1; next }
            next
        }
        printing { print }
    ' "$file"
}
```

These helpers centralize the parsing logic and prevent the three callers from diverging.

---

### P1 | DATA-03 | Task 1, `_interspect_write_overlay` | No dedup guard for existing overlay_id

**Location:** Task 1, `_interspect_write_overlay`, general description

**Evidence:**

The plan does not specify what happens when `_interspect_write_overlay` is called with an `overlay_id` that already exists for the given agent. The overlay file path is `.clavain/interspect/overlays/<agent>/<overlay-id>.md`. If the file already exists and the function overwrites it, the existing modifications row and canary row for that overlay will become orphaned (they reference the old commit SHA) and a new modifications + canary row will be inserted, violating INV-2.

The routing overrides implementation handles this via `unique_by(.agent)` in the JSON merge, which updates metadata in place and skips DB inserts for existing overrides. The overlay system has no equivalent.

**Failure narrative:**

1. Overlay `overlay-abc123` is written for `fd-correctness`. modifications row with SHA1, canary row inserted.
2. A bug or race causes `_interspect_write_overlay` to be called again with the same overlay_id.
3. File is overwritten (temp-then-rename), new commit SHA2. New modifications row inserted. New canary row inserted.
4. Result: two modifications rows and two canary rows for the same overlay file, both "active".
5. `_interspect_disable_overlay` updates `canary SET status='reverted' WHERE group_id = 'fd-correctness/overlay-abc123'` — this hits both rows.
6. But the modifications rows remain both "applied". Status display shows duplicate applied entries.

**Fix:**

Inside `_interspect_write_overlay_locked`, after acquiring flock, check if the target file exists:

```bash
if [[ -f "$overlay_fullpath" ]]; then
    echo "INFO: Overlay ${overlay_id} already exists for ${agent}. Use _interspect_disable_overlay to remove it first, or choose a different overlay_id." >&2
    return 1
fi
```

Unlike routing overrides (where updating metadata on an existing override is explicitly supported), overlay IDs are immutable identifiers. A new overlay should always have a new ID. If the intent is to update overlay content, that should be a disable + create cycle to maintain a clean audit trail.

---

### P1 | DATA-04 | Task 1, `_interspect_disable_overlay` | DB update by `group_id` can collide with routing override records

**Location:** Task 1, `_interspect_disable_overlay`, "Update modifications status to 'reverted'" and "Update canary status to 'reverted'"

**Evidence:**

The plan says:

```
Update modifications status to 'reverted'
Update canary status to 'reverted'
```

In the routing overrides implementation, `modifications.group_id` is set to the agent name (e.g., `fd-correctness`). In the canary table, `group_id` is also the agent name. The plan does not specify what `group_id` value is used for overlay modifications and canary rows.

If the overlay `_interspect_write_overlay` inserts a modifications row with `group_id = 'fd-correctness'` (same as a routing override for the same agent), then `_interspect_disable_overlay` running:

```sql
UPDATE modifications SET status = 'reverted' WHERE group_id = 'fd-correctness' AND status = 'applied';
UPDATE canary SET status = 'reverted' WHERE group_id = 'fd-correctness' AND status = 'active';
```

would also revert any active routing override for the same agent. This is a cross-contamination bug between Type 1 (overlay) and Type 2 (routing override) records.

**Failure narrative:**

1. `fd-correctness` has an active routing override (modifications row: `group_id='fd-correctness', mod_type='routing'`, canary row active).
2. An overlay `overlay-abc` is written for `fd-correctness` (modifications row: `group_id='fd-correctness', mod_type='prompt_tuning'`, canary row active).
3. User runs `_interspect_disable_overlay fd-correctness overlay-abc`.
4. DB update: `UPDATE canary SET status='reverted' WHERE group_id='fd-correctness' AND status='active'` hits BOTH canary rows.
5. The routing override's canary is silently closed. The routing override itself remains in `routing-overrides.json` but is now unmonitored.
6. Next run of `interspect-status` shows the routing override as "no canary" — confusing and incorrect.

**Fix:**

Use a compound `group_id` for overlay records that includes the overlay_id:

```
modifications.group_id = 'fd-correctness/overlay-abc123'
canary.group_id        = 'fd-correctness/overlay-abc123'
```

The DB updates in `_interspect_disable_overlay` then target the specific overlay:

```sql
UPDATE modifications SET status = 'reverted'
  WHERE group_id = 'fd-correctness/overlay-abc123' AND status = 'applied';
UPDATE canary SET status = 'reverted'
  WHERE group_id = 'fd-correctness/overlay-abc123' AND status = 'active';
```

This also requires the revert command (Task 4) to query by compound group_id when disabling specific overlays (vs "all" overlays for an agent).

---

### P2 | RACE-03 | Task 3, interspect-propose | Budget check races with concurrent overlay write

**Location:** Task 3, interspect-propose.md, "auto-generate overlay content from evidence patterns"

**Evidence:**

The propose flow (Task 3) checks the token budget before presenting the proposal to the user (so it can show "80 tokens, budget allows 100 more"). Between the propose display and the user acceptance, another session can write a new overlay, consuming the remaining budget. When the user accepts and `_interspect_write_overlay` is called, the budget check inside the flock will fail, but the user experience is confusing: the proposal said "budget available" and then the apply step fails with "budget exceeded".

This is inherent to any propose-then-confirm UX with shared state. The routing overrides implementation has the same pattern (an existing override may appear between propose display and user acceptance). The existing implementation handles it correctly: the flock-internal dedup check is the source of truth, and the pre-proposal check is just for display.

**Failure narrative:**

1. `fd-safety` has 400 active tokens.
2. Session A shows proposal: "New overlay: 80 tokens (total: 480/500)".
3. Session B concurrently writes a 70-token overlay for `fd-safety`. Total: 470 tokens.
4. User accepts Session A's proposal.
5. `_interspect_write_overlay_locked` checks budget: 470+80=550 > 500. Fails.
6. User sees: "Error: Overlay budget exceeded for fd-safety".
7. There is no clear explanation of why the budget check contradicts what was shown.

This is a P2 because the budget enforcement is correct (it catches the problem), but the UX is misleading. The failure message should say "Budget was consumed by a concurrent overlay write — current total is X/500 tokens." This requires re-reading the current token total in the error path.

**Fix:**

In `_interspect_write_overlay_locked`, when the budget check fails, compute and report the current total before returning the error:

```bash
if (( total_tokens > 500 )); then
    echo "ERROR: Overlay budget exceeded for ${agent}: ${total_tokens}/500 tokens. Another overlay may have been added concurrently. Run /interspect:status to review active overlays." >&2
    return 1
fi
```

---

### P2 | PARSE-02 | Task 5, interspect-status | `head -10 | grep` is not a frontmatter parser

**Location:** Task 5, interspect-status.md, the proposed overlay status section code block

**Evidence:**

The plan shows:

```bash
if head -10 "$overlay" | grep -q "^active: true"; then
    active_count=$((active_count + 1))
fi
```

This is different from the parser used (or to be used) in `_interspect_read_overlays`. Two problems:

First, `head -10` assumes the frontmatter fits in 10 lines. The plan's frontmatter format has 4 fields (`active`, `created`, `created_by`, `evidence_ids`). The `evidence_ids` field is a JSON array that could be long. If the frontmatter is longer than 10 lines (e.g., evidence_ids is a long array), `head -10` may not include the `active:` line.

Second, this is a third implementation of the frontmatter parser (joining `_interspect_read_overlays` and `_interspect_disable_overlay`). Three implementations will diverge over time. If the overlay format evolves (add a field before `active:`), `head -10` breaks while the awk parsers continue to work.

**Fix:**

Replace the inline `head -10 | grep` with a call to `_interspect_overlay_is_active "$overlay"` (the shared helper proposed in PARSE-01 fix). This consolidates all frontmatter parsing logic.

---

### P2 | DATA-05 | Task 4, interspect-revert | Overlay directory check races with disable

**Location:** Task 4, interspect-revert.md, "Check if the target matches an agent with active overlays in `.clavain/interspect/overlays/<agent>/`"

**Evidence:**

The revert command uses a filesystem check (`[[ -d "...overlays/$agent/" ]]`) to decide whether to offer overlay disable. This check runs BEFORE acquiring flock. Between the check and the flock acquisition:

1. Another session could delete the overlay directory (after disabling all overlays for the agent and pruning empty directories — though the plan does not include pruning).
2. More commonly: the overlay directory exists but contains only inactive (already disabled) overlays. The revert command would list "active overlays" to disable, find none, and show a confusing "no active overlays" message even though the directory exists.

The plan handles this in the revert flow with "List all active overlays for the agent" — which is a second check. This second check is the correct source of truth. The directory existence check is just UX optimization (to distinguish "no overlays at all" vs "no active overlays"). This is acceptable, but the plan should make clear that the directory check is non-authoritative.

**Fix:**

Document the two-phase check explicitly: "Directory existence is a fast-path filter. The authoritative check is whether any overlay files have `active: true` in frontmatter. If the directory exists but no active overlays are found, report 'No active overlays for {agent}' rather than treating it as a routing override target."

---

### P2 | SHELL-01 | Task 1, `_interspect_write_overlay` | Overlay content as positional argument `$3` is fragile

**Location:** Task 1, `_interspect_write_overlay`, args: `$1=agent_name $2=overlay_id $3=content $4=evidence_ids_json $5=created_by`

**Evidence:**

Passing overlay content as positional argument `$3` means the caller does:

```bash
_interspect_write_overlay "$agent" "$overlay_id" "$content" "$evidence_ids" "$created_by"
```

where `$content` is a multi-line string with Markdown content. Bash positional argument passing is safe for simple strings, but the call to the locked inner function through `_interspect_flock_git` adds another layer:

```bash
_interspect_flock_git _interspect_write_overlay_locked \
    "$root" "$agent" "$overlay_id" "$content" "$evidence_ids" "$created_by" "$db"
```

Inside `_interspect_flock_git`, this becomes `"$@"` which expands to calling `_interspect_write_overlay_locked` with each argument as a separate word. Multi-line content with no special characters will work. Content with `$`, backticks, or null bytes may cause problems depending on how the content originates.

More importantly: the content is passed through `_interspect_flock_git` which itself spawns a subshell `( ... ) 9>"$lockfile"`. Within that subshell, `"$@"` expands to call the function with all positional args. This is actually safe in bash (quoted expansion preserves newlines). But it is fragile: if anyone changes `_interspect_flock_git` to use `bash -c` instead of a direct call, content would break.

**Fix:**

Write the content to a temp file before calling `_interspect_write_overlay`, and pass the file path as `$3`:

```bash
content_file=$(mktemp)
printf '%s' "$content" > "$content_file"
_interspect_write_overlay "$agent" "$overlay_id" "$content_file" "$evidence_ids" "$created_by"
rm -f "$content_file"
```

The inner function reads the file, writing it into the overlay. This is consistent with how commit messages are handled in the routing overrides implementation (written to a tempfile via `mktemp`, passed as a path, deleted after flock exits). It also avoids the `bash -c` quoting hell flagged as SHELL-01 in the prior routing overrides review.

---

## Missing Steps (Gaps Not in Plan)

### Gap 1: No migration for existing `modifications` and `canary` tables to distinguish overlay vs routing records

The plan says `modifications.mod_type = 'prompt_tuning'` distinguishes overlay records from `mod_type = 'routing'` records. This is correct for new records. But the `group_id` collision bug (DATA-04) requires either a compound `group_id` or a query filter by `mod_type`. Neither the schema nor the migration in `_interspect_ensure_db` adds any index on `(group_id, mod_type)` jointly.

Current indexes: `idx_modifications_group ON modifications(group_id)`, `idx_modifications_status`. If `_interspect_disable_overlay` queries `WHERE group_id = '...' AND mod_type = 'prompt_tuning'`, a composite index on `(group_id, mod_type)` would help performance. Not P0, but the plan should add it to Task 6.

### Gap 2: No cleanup of orphaned canary rows when an overlay file is deleted manually

The plan mentions "Set `active: false` in frontmatter (instant, no git revert needed)" as the rollback mechanism. But it does not handle the case where an overlay file is manually deleted from the filesystem (user deletes the file, or a `git clean` runs). In this case, the canary row remains active but the file being monitored does not exist. `_interspect_read_overlays` would not return it (no file), so it wouldn't contribute to the token budget. But the canary would continue to accumulate samples and eventually evaluate — and its `file` column would reference a non-existent path.

The plan should add: "If `_interspect_read_overlays` fails to find a file referenced by an active canary row (detected by cross-referencing canary.file against the filesystem), close the canary with status `expired_unused` and verdict_reason `file removed manually`."

This could be a post-launch health-check addition, not necessarily in the initial implementation.

### Gap 3: Token counting unit mismatch between write-time and runtime

Task 1 specifies `word_count * 1.3` for token estimation. Task 2 (flux-drive Step 2.1d) also specifies `word_count * 1.3`. These must produce the same result for the same content. If the implementations differ (one uses `wc -w`, the other uses a Python word splitter), the budget can be exceeded at runtime even though the write-time check passed.

The plan should specify: "Both the write-time budget check in `_interspect_count_overlay_tokens` and the runtime budget check in Step 2.1d must use `wc -w` (POSIX word count) multiplied by 1.3, truncated to integer."

### Gap 4: Integration test does not cover concurrent writes

Task 7 proposes a sequential integration test. The test lifecycle is:

```
write → read → count → budget check → disable → read again → verify DB
```

None of the test steps verify the race conditions identified in RACE-01 and RACE-02. The budget TOCTOU can only be detected under concurrent load. The test plan should add:

```
8. Concurrent write test: launch two _interspect_write_overlay calls for the same agent
   in parallel (background subshells with sleep 0.1 between). Assert that only one
   succeeds and total tokens do not exceed 500.
```

### Gap 5: `_interspect_read_overlays` return value for empty result is undefined

The plan says "Return concatenated content on stdout, empty string if no active overlays." But the caller in `_interspect_write_overlay` does:

```bash
existing_tokens=$(_interspect_count_overlay_tokens "$existing_content")
```

If `$existing_content` is empty, `_interspect_count_overlay_tokens` receives an empty string. The plan specifies `word_count * 1.3` — `wc -w ""` returns 0, so `0 * 1.3 = 0`. This is correct. But the plan should document this edge case explicitly: "Empty content returns 0 tokens."

---

## Canary Baseline Computation — Overlay vs Routing Override

The plan says to "reuse `_interspect_compute_canary_baseline`". This function computes global metrics (override rate, FP rate, finding density) across all sessions, optionally filtered by project. It is not agent-specific.

For routing overrides, the baseline is meaningful: "before this agent was excluded, what was the global correction rate?" After exclusion, the global rate should drop if the exclusion is beneficial.

For overlays, the baseline meaning is different: "before this overlay was applied, how often did this specific agent produce corrections?" A global baseline will not show signal specific to the overlay — it conflates all agents' behavior. If `fd-correctness` has an overlay applied, you want to see whether `fd-correctness`'s correction rate changes, not whether the global rate changes.

The plan does not address this distinction. Reusing `_interspect_compute_canary_baseline` without modification will produce canaries that monitor global signal, not overlay-specific signal. An overlay that dramatically improves `fd-correctness` behavior would be invisible if other agents are producing the same global noise floor.

This is a correctness gap but not a blocking one for the initial implementation. The plan should acknowledge it: "The canary baseline for overlays uses the same global metric as routing overrides. A future enhancement should add agent-scoped canary baseline computation." If the intent is to observe overlay-specific impact, a filter on `evidence.source = agent_name` in `_interspect_compute_canary_baseline` should be added as a `$3` parameter.

---

## Recommendations Summary

### Must fix before merge (P0)

1. **RACE-01:** Move budget check inside the same `_interspect_flock_git` call as the file write. Use a named locked function pattern (same as `_interspect_apply_override_locked`).
2. **DATA-01:** Make explicit that DB inserts are inside the named locked function. No DB writes after flock release.
3. **DATA-02:** Replace the `active: true` → `active: false` text substitution with an awk state-machine that restricts replacement to the frontmatter section between the first and second `---` delimiters.

### Should fix before merge (P1)

4. **RACE-02:** Document the flock-vs-unlocked duality of `_interspect_read_overlays` and restrict glob to `*.md` only.
5. **PARSE-01:** Define shared `_interspect_overlay_is_active` and `_interspect_overlay_body` helpers. All callers use these — no inline `grep` or `head` for frontmatter parsing.
6. **DATA-03:** Add existence check in `_interspect_write_overlay_locked` — reject if overlay_id already exists.
7. **DATA-04:** Use compound `group_id = '<agent>/<overlay_id>'` for modifications and canary rows to prevent cross-contamination with routing override records for the same agent.

### Nice to have (P2, can defer)

8. **RACE-03:** Improve error message when budget check fails due to concurrent write.
9. **PARSE-02:** Replace `head -10 | grep` in status command with shared `_interspect_overlay_is_active` helper.
10. **DATA-05:** Document that overlay directory check in revert is non-authoritative.
11. **SHELL-01:** Pass overlay content via tempfile, not as positional argument.

### Gaps to document or schedule

12. Add `(group_id, mod_type)` composite index to `_interspect_ensure_db` migration.
13. Add orphaned canary cleanup when overlay files are deleted manually.
14. Specify `wc -w * 1.3` as the canonical token counting algorithm in both write-time and runtime code paths.
15. Add concurrent write test to Task 7.
16. Acknowledge agent-scoped canary baseline gap and plan for future fix.

---

## Closing Note

The overlay plan inherits the correct structural patterns from the routing overrides implementation (named locked functions, temp-then-rename writes, SQL escaping, validate-before-write). The two P0 issues are the same class of bug that the routing overrides plan had: flock scope ambiguity (RACE-01/DATA-01) and unsafe text mutation of structured data (DATA-02, the YAML patch). The YAML parsing problems (PARSE-01, PARSE-02, DATA-02) are new to this plan because routing overrides use JSON (parsed by `jq`, which is reliable). Overlays use YAML frontmatter parsed in bash, which requires careful delimiter-aware state machine logic that the plan currently does not specify precisely enough.

Fix the three P0 issues and the four P1 issues, and the plan is production-ready. The P2 items are reliability and UX improvements that can follow in the next session.
