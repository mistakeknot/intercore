# Architecture Review: Interspect Overlay System (Type 1)

**Plan:** `docs/plans/2026-02-19-interspect-overlay-system.md`
**Reviewer role:** fd-architecture
**Date:** 2026-02-19

---

## Findings Index

- P1 | A-1 | "Task 2 / Task 3" | Overlay logic split across two incompatible execution environments
- P1 | A-2 | "Task 2" | flux-drive dispatch template absorbs ownership of overlay filtering logic
- P2 | A-3 | "Task 1 / _interspect_write_overlay" | Token budget check uses a race-prone read-before-write pattern
- P2 | A-4 | "Task 3" | Propose command generates overlay content without sanitization coverage
- P2 | A-5 | "Task 4 / interspect-revert" | Revert command type-detection is ambiguous for a dual-type target
- P3 | A-6 | "Task 5" | Status overlay display uses `head -10 | grep` instead of the shared manifest/parser path
- P3 | A-7 | "Task 7" | Integration test does not cover the flux-drive injection path

Verdict: needs-changes

---

## Summary

The plan correctly extends an existing, well-structured library (`lib-interspect.sh`) with a new modification type. The security primitives (flock, `_interspect_validate_target`, SQL escaping, atomic write-then-rename) are applied consistently and the canary lifecycle is properly wired. The primary structural problem is that the overlay read/filter/budget logic is specified twice: once in `_interspect_read_overlays` (a bash library function), and again inline in `launch.md` (a markdown prompt template executed by an LLM). These two implementations will diverge. The secondary problem is that `launch.md` is now responsible for understanding the YAML frontmatter format that `lib-interspect.sh` owns, which creates a hidden coupling between a bash library and an LLM instruction document. Both issues are fixable without changing the plan's scope.

---

## Issues Found

### A-1. P1: Overlay logic split across two incompatible execution environments

**Location:** Task 1 (`_interspect_read_overlays` in `lib-interspect.sh`) and Task 2 (Step 2.1d in `launch.md`)

The plan specifies the same behavior — scan overlay directory, parse YAML frontmatter, filter `active: true`, concatenate body, enforce 500-token budget — in two places. One is a bash function intended for use in automated hook contexts. The other is an LLM-instruction prose block that asks an orchestrating agent to repeat the same logic manually during dispatch.

This is a structural duplication with no isolation boundary between them. The YAML parsing in `_interspect_read_overlays` uses awk/sed or positional delimiter scanning. The version in `launch.md` Step 2.1d says to "parse YAML frontmatter" with no specified tool — the LLM orchestrator will use whatever approach it finds natural in context, which will differ between models and sessions. When the overlay file format changes (even a field rename), both sites must be updated and kept consistent. History in this codebase shows that markdown prompt templates and bash libraries drift at different rates.

More critically, `_interspect_write_overlay` is never called from `launch.md` — the bash path and the LLM path are not coordinated at runtime. The bash library function exists as dead weight from the dispatch perspective unless something else calls it.

**Fix (smallest viable change):** Make the dispatch template's injection point entirely dependent on a pre-computed bash output. In `launch.md` Step 2.1d, replace the inline YAML-parsing instructions with a single bash call:

```bash
OVERLAY_CONTENT=$(source "$INTERSPECT_LIB" && _interspect_read_overlays "$agent_name")
```

The template then only needs to decide whether `$OVERLAY_CONTENT` is non-empty and where to inject it. The filtering, parsing, and budget logic stays exclusively in `lib-interspect.sh`. The LLM orchestrator does not need to understand the overlay file format at all.

---

### A-2. P1: flux-drive dispatch template absorbs ownership of overlay filtering logic

**Location:** Task 2, `plugins/interflux/skills/flux-drive/phases/launch.md`

The plan injects Step 2.1d between the existing Step 2.1a (domain criteria) and Step 2.1c (temp file writing). The step tells the orchestrating agent to read overlay directories, parse frontmatter, apply `active: true` filtering, count tokens with `word_count * 1.3`, and truncate to fit the 500-token budget.

This is not the right owner for any of those responsibilities. `launch.md` is a dispatch orchestration template — its job is to assemble prompts from already-computed pieces and route agents. The manifest pattern, confidence thresholds, and token budget enforcement all live in `lib-interspect.sh` and its companion config files (`confidence.json`). Putting budget enforcement prose in `launch.md` means:

1. The 500-token limit appears in the plan, in `_interspect_write_overlay`, and now also in `launch.md`. Three locations for one policy value.
2. If the budget is changed in `confidence.json`, `launch.md` will silently continue enforcing the old hardcoded number.
3. The warning message `"WARNING: Overlay budget exceeded for {agent}. Using first N of M overlays."` will be emitted to the agent prompt, not to a log. The user will never see it.

**Fix:** As stated in A-1, move all overlay resolution logic into `_interspect_read_overlays`. The only change to `launch.md` is the injection point for the already-resolved `{OVERLAY_CONTEXT}` variable — matching exactly the pattern used for `{DOMAIN_CONTEXT}` (which is also pre-computed before the template is applied). No budget arithmetic, no frontmatter parsing, no directory scanning belongs in the template.

---

### A-3. P2: Token budget check uses a race-prone read-before-write pattern

**Location:** Task 1, `_interspect_write_overlay`

The specified budget check reads existing active overlays, adds the new content size, and rejects if over 500 tokens — but this check happens before the flock is acquired. The spec says "Atomic write inside `_interspect_flock_git`" but describes the budget check as a pre-flock step in the validation list:

> "Check token budget: read existing active overlays + new content must be <= 500 tokens"

The flock only serializes the write. If two concurrent `_interspect_write_overlay` calls both pass the budget check before either holds the lock, both writes succeed and the combined budget is violated.

The existing `_interspect_apply_routing_override` handles this correctly: the dedup check happens inside `_interspect_apply_override_locked` (the function passed to `_interspect_flock_git`), not before it. The overlay budget check must follow the same pattern: perform the budget check inside the locked function, after the flock is acquired.

**Fix:** Move the budget check into a new locked inner function (`_interspect_write_overlay_locked`), following the exact pattern of `_interspect_apply_override_locked`. Pre-flock validation should be limited to input format checks (agent name, overlay ID regex, JSON array validation) that do not depend on filesystem state.

---

### A-4. P2: Propose command generates overlay content without sanitization coverage

**Location:** Task 3, `hub/clavain/commands/interspect-propose.md`

The plan specifies that the propose command queries the `context` field from evidence rows, summarizes the pattern, and drafts a 2-3 sentence instruction for the overlay. That draft is then passed to `_interspect_write_overlay` as `$3=content`.

Evidence `context` fields pass through `_interspect_sanitize` at insertion time. However, the LLM orchestrator constructing the overlay draft in Task 3 is generating new content by synthesizing evidence summaries — it is not passing the raw evidence string through. The generated draft is arbitrary text produced by an LLM reading evidence context. That draft bypasses `_interspect_sanitize` entirely before being written to a file that will later be injected into agent prompts.

This creates a path where a malformed or adversarially influenced evidence entry could, through LLM synthesis, produce overlay content that bypasses the sanitization checks. The `_interspect_sanitize` function specifically checks for `<system>`, `<instructions>`, `ignore previous`, and `system:` patterns. Overlay content should be subjected to those same checks at write time.

**Fix:** Call `_interspect_sanitize` on `$3=content` at the top of `_interspect_write_overlay`, before the token count or path checks. This is consistent with how `_interspect_insert_evidence` sanitizes all user-controlled fields. One line added to `_interspect_write_overlay`.

---

### A-5. P2: Revert command type-detection is ambiguous for a dual-type target

**Location:** Task 4, `hub/clavain/commands/interspect-revert.md`

The plan extends `/interspect:revert` to handle both routing overrides and overlays. The detection logic is sequential: first check for a routing override match, then check for overlays under `.clavain/interspect/overlays/<agent>/`. The argument-hint remains `<agent-name or commit-sha>` and the command description says "Revert a routing override."

This creates two problems:

1. An agent name can match both a routing override AND have active overlays simultaneously. The plan's sequential check stops at the first match (routing override), leaving overlays untouched. If the user's intent was to revert an overlay, the command silently succeeds on the wrong target.
2. After Task 3 is implemented, users will have two distinct things to revert per agent with no disambiguation path in the command signature. The revert command will need to present a disambiguating question ("Revert routing override or overlay?") for the dual-match case — but the plan does not specify this.

**Fix (smallest viable change):** Add a disambiguation step in the revert command when both a routing override and active overlays exist for the same agent. Present a question: "Both a routing override and overlays exist for `{agent}`. Which do you want to revert?" with options for each. Update the argument-hint to `<agent-name, overlay-id, or commit-sha>` so a specific overlay ID can be targeted directly without ambiguity.

---

## Improvements

### A-6. Status command uses `head -10 | grep` instead of the shared YAML parser path

**Location:** Task 5, `hub/clavain/commands/interspect-status.md`

The inline bash block uses `head -10 "$overlay" | grep -q "^active: true"` to detect active overlays. This is a fragile line-order assumption — if the frontmatter is reordered or the `active` field moves past line 10, the check silently fails. The `_interspect_read_overlays` function defined in Task 1 already handles frontmatter parsing correctly. The status command should call `_interspect_read_overlays "$agent"` and check for non-empty output rather than re-implementing frontmatter parsing inline. This also ensures the token estimate in the status table is computed by the same function as the budget enforcement.

### A-7. Integration test does not cover the flux-drive injection path

**Location:** Task 7, `hub/clavain/test-interspect-overlay.sh`

The proposed test exercises the full overlay lifecycle in `lib-interspect.sh`: write, read, budget enforcement, disable, DB records. This is the right scope for a library unit test. However, nothing in the test plan verifies that the overlay content actually reaches an agent prompt. If the injection point in `launch.md` is mis-specified (wrong variable name, wrong section ordering), the integration will silently fail — overlays will exist but never be injected. A minimal integration test that renders a synthetic agent prompt and asserts `{OVERLAY_CONTEXT}` is present would catch this class of failure. This is low priority given that A-1 and A-2 address the structural gap, but worth noting as a gap in the test plan.

---

## Must-Fix Summary

| ID | Priority | File | Action |
|----|----------|------|--------|
| A-1 | P1 | `launch.md` + `lib-interspect.sh` | Remove overlay logic from `launch.md`; make dispatch call `_interspect_read_overlays` as a bash step, inject result as pre-computed variable matching the `{DOMAIN_CONTEXT}` pattern |
| A-2 | P1 | `launch.md` | Remove inline budget arithmetic, directory scanning, and frontmatter parsing from the dispatch template |
| A-3 | P2 | `lib-interspect.sh` | Move budget check inside the flock-protected inner function, following `_interspect_apply_override_locked` pattern |
| A-4 | P2 | `lib-interspect.sh` | Call `_interspect_sanitize` on overlay content at entry to `_interspect_write_overlay` |
| A-5 | P2 | `interspect-revert.md` | Add disambiguation step for agents with both routing overrides and active overlays |

A-1 and A-2 are the same fix applied to the same file — resolve them together. A-3, A-4, and A-5 are independent and can be sequenced in any order.
