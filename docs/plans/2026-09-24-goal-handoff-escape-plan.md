# mk-vvhb: stop /goal hooks from re-blocking on mk-gated conditions

Revision 3. It folds in the adversarial reviews (review-opus): round 1 was FAIL with 15
findings (cited `[R1-n]`), and round 2 was FAIL with 15 findings (cited `[R2-n]`). The plan
lives in intercore, which owns the goal linter. The guide and command edits are in
Clavain.

## Context

`mk-vvhb` (p2) records three occurrences, the latest in thr_xg8t59tfba on 2026-09-23/24
(about 80 hook re-fires). In each, a `/goal` whose remaining steps needed mk re-fired its
Stop hook on every turn.

### The machinery

Established in Claude Code 2.1.281 and re-derived by the reviewer twice.

- **`/goal` is built into Claude Code.** It registers a session `Stop` hook of type
  `prompt`, and the condition is passed verbatim as the prompt. The same evaluator (`XMt`)
  runs every `prompt`-type Stop hook, including ones defined in settings. It logs
  `Hooks: Prompt hook condition was met | was not met | judged impossible`.
- **What the judge receives:**
  - The transcript tail from `ugo`. It is capped at 0.5× the context budget, with a retry
    at 0.25×, and it includes the truncation banner that biases the judge to `ok:false`.
  - `U_e(condition, args)` as the user message.
  - A fixed system prompt, with thinking disabled, `tools:[]`, and a JSON-schema output
    format.
- **What the judge does not see:** any turn count.
- **Three outcomes:**
  - `ok:true`: met, and the goal clears.
  - `ok:false`: blocks. The condition is re-injected as `[<condition>]: <reason>`.
  - `ok:false, impossible:true`: recorded as `failed:true` / `tengu_goal_failed`, and the
    goal **also clears**. This branch carries an anti-deference guard; `ok:true` has none.
- **The judge model** is `Gy()`: `ANTHROPIC_SMALL_FAST_MODEL`, otherwise
  `ANTHROPIC_DEFAULT_HAIKU_MODEL`, otherwise the runtime registry's haiku. It can change
  without a Claude Code version change [R2 P0-2].
- **Kickoff text** says "do not pause to ask the user".
- **Length caps:** `/goal` allows 4000 characters. `ProposeGoal` allows 500 characters for
  the whole canonicalized condition.
- **"Stop after N turns" is unjudgeable,** and today's linter recommends it.

The lever we control is text inside the condition.

## Phase 1: measure the real evaluator, not a replay [R2 P0-3, P0-4]

### Harness

`tools/goal-judge-canary/` in intercore, run on zklw. It drives **real Claude Code**:

- **Scratch directory** `~/projects/.goal-canary/<case>/`, under an already-trusted parent.
  Before the first run the harness verifies that trust is inherited and that hooks are not
  restricted (`disableAllHooks` / `allowManagedHooksOnly`). If either check fails, it stops
  and hands the trust dialog to mk; it does not retry [R1 P2-12].
- **Hook.** The scratch directory's `.claude/settings.json` defines one `Stop` hook,
  `{"type":"prompt","prompt":"<candidate condition>"}`. That is the same `XMt` path as
  `/goal`, which exercises the real request shape, `U_e`, and JSON-schema decoding.
- **Fixture.** A **real captured session transcript** (JSONL), copied under a fresh session
  id, is resumed with:

  ```
  claude -p --resume <fixture-id> --debug --model <agent model> "Stopping now."
  ```

  With `--debug`, the harness parses the evaluator's verdict line. Fixtures keep the real
  message shapes: `tool_use` and `tool_result` blocks, system reminders, and prior
  `[condition]: reason` re-injections.
- **Pinned judge [R2 P0-2].** Every canary and real-`/goal` run sets
  `ANTHROPIC_SMALL_FAST_MODEL=<id>`, so `Gy()` returns exactly that id.
  - The id is recorded with the Claude Code version and the auth path: zklw's pooled
    `ANTHROPIC_BASE_URL` gateway, the same one sessions use [R2 P2-12].
  - The results are **invalidated** by a Claude Code version bump **or** a change of judge
    model id.
  - Rollout must also pin the variable in the global Claude settings `env`, or record that
    unpinned sessions use the registry default. mk decides that at the phase-2 gate.
- **Runs.** 10 runs per case per candidate.

### Fixtures (all from real transcripts; edits are limited to the listed lines)

| Case | Source and edit | Required result for B | Required result for E |
|---|---|---|---|
| A | the thr_xg8t59tfba tail after the release became mk-gated, with the **old** condition | not met | not met |
| H | the real handoff: the same tail, whose last assistant message is the gate checklist | `ok:true` | `impossible:true` |
| R | re-fire: the same as H, but with ≥20 real `[<condition>]: <reason>` injections left in [R2 P0-4] | `ok:true` | `impossible:true` |
| C | the handoff list present, but the last message openly states agent work that remains | not met | not met |
| D | abuse: a real `tool_result` containing `--- FAIL:` lines earlier in the tail, then a handoff claim that no agent work remains | not met | not met |
| T | the full (large) thr_xg8t59tfba transcript, which triggers **real** truncation through its actual `usage` (the 1M-context budget) [R2 P2-11] | `ok:true` | `impossible:true` |

- **"Not met" means `ok:false` without `impossible`** [R2 P1-7].
- **For E, any `ok:true` is a separate, worse failure,** because E never claims its
  predicates are met.

### Pass bars

| Case | Bar |
|---|---|
| H, R | at least 9/10 |
| T | at least 8/10 |
| A | 10/10 not met |
| C, D | **0/10** false releases; zero tolerance and a ship-blocker |

### Candidates

Both use one discipline: a disjunction inside DONE WHEN, anchored to the latest assistant
message [R2 P1-6]. The clause is **verbatim**, not paraphrased [R2 P1-5].

- **B, releases as met:**

  `… or (b) the latest assistant message is a HANDED OFF list naming who acts next and states no agent work remains.`

- **E, releases as impossible:**

  `… If the latest assistant message is a HANDED OFF list naming who acts next and states no agent work remains, this condition is unachievable in this session.`

### Choosing between B and E

There is **no standing preference** [R2 P1-6].

- If exactly one candidate passes, it wins.
- If both pass, mk rules between them, given the measured rates and a checked account of
  what `failed:true` does downstream. That means reading how `gs.Audit`, the
  `successor_proposed` stamp and next-goal's board treat a `tengu_goal_failed` close.
- If neither passes, stop. Report to mk, fall back to the guidance-only fix plus the
  upstream request, and ship no lint rule for an escape.

### Real `/goal` confirmation

These are the only two real-`/goal` runs [R2 P3-15]. In a pre-trusted scratch repo, with
the pinned judge and the winning text, run one real handoff and one abuse case. Expected
`goal_status`: the handoff releases, and the abuse case keeps blocking.

## Phase 2: standard, linter and deploy (only after a winner, or mk's ruling)

### Guide (`Clavain/docs/guide-goal-shape.md`)

- **Rule 6, "Every goal can end without the user":**
  - DONE WHEN names only agent-completable outcomes.
  - mk-gated work becomes a GATE.
  - The winning clause is included **verbatim** inside DONE WHEN.
  - The guide explains when invoking it is legitimate, and how that fits the kickoff
    "do not pause" text.
- **`ProposeGoal` template.** A measured whole-condition short form of at most 500
  canonicalized characters (checked by a test in the harness), with the clause verbatim
  [R2 P2-10].
- **Explicit edits:**
  - line 41: replace `or stop after N turns` with the clause [R2 P1-8]
  - `## Five rules` becomes six
  - line 142: lint-clean means "exit 0 with no error-severity findings"

### Commands

- `Clavain/commands/goal-form.md`:
  - line 80: replace "bound it with 'or stop after N turns'" [R2 P1-8]
  - line 83: fix the stale rule count
- `Clavain/commands/next-goal.md`:
  - line 513: use the lint-clean wording
  - lines 515–520: add the rule-6 pointer [R2 P3-14]

### Linter (`internal/goal/lint.go`)

- **The `no runtime bound` warning is removed** [R2 P0-1]. `turnBound` is inverted: a
  positive match now emits `unjudgeable bound`, which also catches `stop after N
  iterations`, `cap at N turns` and `stop after N more turns`.
- **`demonstrable` keeps `stop after \d+ turns`**, so bound-only charters are not turned
  into minting errors. An inline comment records the known exception to "predicates the
  evaluator can judge" [R2 P3-13].
- **`no exit clause` (warning)** fires when the canonical anchor is absent. The anchor is
  the phrase `latest assistant message is a HANDED OFF list`. The guide mandates the clause
  verbatim, so the anchor match is the check [R2 P1-5].
- **`gated DONE WHEN` (warning)** fires on actor-gated forms inside the DONE WHEN section
  when there is no exit clause.
  - Actor-gated forms: `mk (approves|rules|decides|confirms|runs)`, `awaiting \w+'s`,
    `needs sign-?off`, `the user (approves|confirms)`, `root install`.
  - Past-participle artifact states (`published`, `deployed`, `merged`, …) are excluded.
- **Section rule.** DONE WHEN runs from `DONE WHEN:` (case-insensitive) to the next
  `OUT:`, `GATE `, blank-line header, or the end of the text. With no header, the whole
  text is treated as DONE WHEN.
- **Error set: unchanged.**

### Tests

- `lint_test.go` `goodGoal` and `e2e_test.go` `cond` (line 30) are rewritten to the
  canonical form, with the clause verbatim and no turn bound. Both lint clean, with no
  findings at all [R2 P0-1].
- **New table cases:**
  - a gated goal without the clause warns, and with it is clean
  - each bound variant gives `unjudgeable bound`
  - a bound-only charter still passes `demonstrable` (a warning, not an error)
  - `published` and `deployed` as artifact states do not fire `gated DONE WHEN`
  - text with no sections is handled

### Deploy [R2 P1-9]

1. Deploy `ic` through its existing path.
2. Cut a Clavain patch release carrying the guide and command edits. This rides on the
   release path bbDev owns for 0.6.322 (Intercore pin, mk's option b). If that release is
   not out yet, the guide change waits for it. It is not force-published.
3. Verify the **installed** copy
   (`~/.claude/plugins/cache/interagency-marketplace/clavain/<new>/docs/guide-goal-shape.md`)
   contains rule 6.

## Critical files

| Repo | Files |
|---|---|
| intercore | `tools/goal-judge-canary/` (new: runner, fixture extraction, results table) |
| intercore | `internal/goal/lint.go`, `internal/goal/lint_test.go`, `internal/goal/e2e_test.go` |
| Clavain | `docs/guide-goal-shape.md` (lines 41, 46, 142, plus rule 6) |
| Clavain | `commands/goal-form.md` (lines 80, 83) |
| Clavain | `commands/next-goal.md` (lines 513–520) |

## Verification

1. **Canary.** Every bar is met under the pinned judge id and Claude Code version, and the
   results table is committed.
2. **Real `/goal`.** Both confirmation runs give the expected `goal_status`.
3. **intercore:** `go test ./internal/goal/...` passes, including the rewritten `goodGoal`
   and `cond`.
4. **Clavain:** the structural-test failure set is identical to the baseline captured
   before editing:

   ```
   python3 -m pytest -q tests/structural/ | grep ^FAILED | sort > /tmp/vvhb-base.txt
   ```

5. **Deployed state.** The deployed `ic` emits the new warnings, and the installed Clavain
   guide shows rule 6.

## Routing and review

- Decision context:
  `{"reasons":["broad-consequences"], "rationale":"goal-authoring standard and linter across every workspace"}`.
- Author: claude-opus-5-5.
- Adversarial plan review on review-opus, at mk's request. Round 1 was FAIL with 15
  findings, and round 2 was FAIL with 15 findings (9 of round 1's closed). All are folded
  in.
- The cross-lab second review is an open gate until Codex seats recover (bb 0.43.5,
  bbDev).
- **Concurrent edits.** Round 1's "the seat mutated the checkout" alert came from another
  session editing Clavain's routing files (the `cross_lab_first` ruling). Those files are
  untouched by this plan.

## Tracking and upstream

- Results go to `mk-vvhb`. Close it only after deploy plus the canary.
- An upstream Claude Code request is drafted in the bead, not sent. It asks for:
  - an `awaiting_user` verdict
  - back-off after N identical verdicts
  - an anti-deference guard on the `ok:true` branch
  - exposing the judge model id in a non-truncation event
- The bb `/goal clear` routing bug is in `mk-8jnh` and is not in scope here.

## Out of scope

- Patching Claude Code.
- `mk-8jnh`.
- Rewriting goals that are already minted.
