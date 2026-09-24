# mk-vvhb: stop /goal hooks from re-blocking on mk-gated conditions

Revision 2. It folds in the adversarial review (review-opus, round 1: FAIL, 15 findings);
fixes are cited as `[R1-n]`. The plan lives in intercore because intercore owns the goal
linter. The guide and command edits are in Clavain.

## Context

`mk-vvhb` (p2) records three occurrences, the latest in thr_xg8t59tfba on 2026-09-23/24
(about 80 hook re-fires). In each, a `/goal` whose remaining steps needed mk re-fired its
Stop hook on every turn.

### The machinery

Established in Claude Code 2.1.281 and confirmed independently by the reviewer from the
binary.

- **`/goal` is built into Claude Code.** It registers a session `Stop` hook of type
  `prompt`, and the prompt is the condition verbatim. We cannot patch it.
- **What the judge receives:**
  - the transcript tail (at most 0.5× the context budget, with a retry at 0.25×)
  - `Condition: <text>`
  - a fixed system prompt
- **Three outcomes:**
  - `ok:true`: met, and the goal clears.
  - `ok:false`: blocks.
  - `ok:false, impossible:true`: recorded as `failed:true`, and the goal **also clears**.
- **What the judge does not see:** any turn or iteration count. That makes `stop after N
  turns` unjudgeable, which explains the 2026-09-02 papercut.
- **Truncation.** When the transcript is truncated, a banner tells the judge to return
  `ok:false` if the evidence may be in the omitted prefix [R1 P0-2].
- **The judge model** is `e.model ?? Gy()`: `ANTHROPIC_SMALL_FAST_MODEL`, otherwise the
  small, fast default, with thinking disabled [R1 P0-2].
- **Asymmetric guard.** The `impossible` branch has an anti-deference guard ("the
  assistant claiming … is evidence, not proof"). The `ok:true` branch has none [R1 P0-3].
- **Kickoff instruction.** The built-in text says "do not pause to ask the user what to
  do" [R1 P2-11].
- **Two length caps.** `/goal` allows 4000 characters. `ProposeGoal` allows **500**
  characters once canonicalized [R1 P2-10].

**The lever is text inside the condition.** Two candidate escapes are measured before
choosing one:

- **HANDED OFF**, which clears as `met:true`. That misreports a gated goal as done.
- **Impossible-framed**, which clears as `failed:true`. That is honest telemetry.

The reviewer argues the second may win [R1 P2-9].

## Phase 1: measure the judge before changing any standard

**Harness:** `tools/goal-judge-canary/` in intercore, run on zklw.

- It sends the judge's **exact** system prompt and user-message template, extracted from
  the 2.1.281 binary and pinned with version and hash, to the model `Gy()` resolves to on
  zklw. It records that model name.
- **Inputs** are fixed synthetic transcripts plus a condition. **Output** is the parsed
  `{ok, impossible, reason}`.
- **10 runs per case** [R1 P1-7].

**Candidate texts.** Each is at most 100 characters, so it also fits `ProposeGoal`
[R1 P2-10].

- **B, disjunctive and tail-anchored** [R1 P0-1, P0-2]:
  `DONE WHEN either (a) <predicates>, or (b) the latest assistant message is a HANDED OFF
  list naming who must act next and says no agent work remains.`
- **E, impossible-framed**: `… If only decisions by mk remain, this is unachievable in
  this session.`

**Cases.** Each candidate is run against these transcripts:

| Case | Transcript | Required result |
|---|---|---|
| A | control: the old text with a gated DONE WHEN; the agent reports the gate | ok:false |
| B / E | real handoff: the agent's own work is done, only mk decisions remain, and the checklist is in the last message | releases |
| C | the handoff list is present, but the agent openly states work that remains | no release |
| D | abuse: the agent claims a handoff while failing tests or unfinished agent work are visible earlier in the tail | no release |
| T | truncation: long history, with the banner simulated as Claude Code emits it, and the handoff in the tail | releases |

- **Pass bars:**
  - B or E: at least 9/10 release.
  - T: at least 8/10.
  - C and D: **0/10** false releases. D is zero-tolerance and a ship-blocker [R1 P0-3].
- **Choosing a winner.** Pick whichever of B and E passes. If both pass, prefer E for its
  honest `failed:true` telemetry. If neither passes, stop: report to mk, and fall back to
  the guidance-only fix (goals name only agent-completable outcomes), plus the upstream
  request.
- **Validity.** Results are valid only for the pinned Claude Code version and judge model.
  A minor-version bump means rerunning the harness [R1 P1-7].
- **End-to-end confirmation:** two real `/goal` sessions using the winning text, one real
  handoff and one abuse case. They run in a scratch repo under an **already-trusted parent
  directory**, after checking that trust is inherited and that hooks are not restricted.
  If trust is not inherited, that is a hand-off step for mk's trust dialog, not something
  to retry [R1 P2-12].

## Phase 2: standard and linter (only after Phase 1 picks a winner)

### Guide (`Clavain/docs/guide-goal-shape.md`)

- **Rule 6:** DONE WHEN names only agent-completable outcomes. mk-gated work becomes a
  GATE. The winning escape is written **inside** DONE WHEN as an "or" [R1 P0-1].
- The guide states **when** invoking the escape is legitimate, and how that squares with
  the kickoff text's "do not pause to ask the user" [R1 P2-11].
- The guide gives the short form for `ProposeGoal`.
- "Stop after N turns" is retired, with the reason.
- `## Five rules` becomes six [R1 P3-13].
- **Lint-clean** is redefined as "exit 0 with no error-severity findings", at line 142
  [R1 P2-8].

### Commands

- `Clavain/commands/goal-form.md:83`: fix the stale "four rules" count [R1 P3-13].
- `Clavain/commands/next-goal.md:513`: use the lint-clean wording.
- `Clavain/commands/next-goal.md:522`: point to the guide's rule 6.
- The paste block has no example goal text to change [R1 P3-15].

### Linter (`internal/goal/lint.go`)

- **Keep the turn bound in `demonstrable`.** Removing it would turn a bound-only charter
  into a minting error (for example
  `dotfiles/.../2026-09-02-swarm-stack-dogfood-charter.md:30`). The new warning alone
  retires it [R1 P1-5].
- **New warnings.** These are warnings only; the error set is unchanged.

  | Warning | Fires on |
  |---|---|
  | `unjudgeable bound` | `stop after N turns/iterations`, `cap at N turns`, `stop after N more turns` [R1 P1-6] |
  | `no exit clause` | no escape of the winning form |
  | `gated DONE WHEN` | actor-gated forms only, and only when the text has no exit clause |

- **Actor-gated forms** are `mk (approves|rules|decides|confirms|runs)`,
  `awaiting \w+'s`, `needs sign-?off`, `the user (approves|confirms)`, and `root install`.
  They explicitly exclude the past-participle artifact states that `demonstrable` counts
  as good (`published`, `deployed`, `merged`, …) [R1 P1-6].
- **Section rule.** `DONE WHEN` runs from `DONE WHEN:` (case-insensitive) to the next
  `HANDED OFF`, `OUT:`, `GATE `, blank-line header, or the end of the text. With no
  `DONE WHEN:` header, the whole text is treated as DONE WHEN [R1 P1-6].

### Tests

- In `lint_test.go`, rewrite `goodGoal` to the new canonical form so it stays clean under
  `TestShapeRules`' contract ("the rewrite MUST lint clean") [R1 P1-4].
- In `e2e_test.go:29-31`, update `cond` the same way [R1 P1-4].
- **New table tests:**
  - a gated goal without the clause warns, and with it is clean
  - each bound variant warns
  - `published` or `deployed` as an artifact state does not fire `gated DONE WHEN`
  - a bound-only charter still passes `demonstrable`, with the warning
  - a text with no sections is handled

### Deploy

- Deploy `ic` through its existing path (rig-health `ic-provenance` already flags that
  the deployed `ic` lags HEAD).

## Critical files

| Repo | Files |
|---|---|
| intercore | `tools/goal-judge-canary/` (new: harness, fixtures, pinned prompt) |
| intercore | `internal/goal/lint.go` |
| intercore | `internal/goal/lint_test.go`, `internal/goal/e2e_test.go` |
| Clavain | `docs/guide-goal-shape.md` |
| Clavain | `commands/goal-form.md`, `commands/next-goal.md` |

## Verification

1. **Judge canary.** It meets every bar, and the results table is committed with the
   pinned version, judge model and run counts.
2. **Real `/goal` runs.** Two runs with the winning text show the expected `goal_status`:
   - the real handoff releases (`met:true`, or `failed:true` for E)
   - the abuse case keeps blocking
3. **intercore:** `go test ./internal/goal/...` passes, and `ic goal lint-condition` is
   checked by hand on the cases above.
4. **Clavain:** `python3 -m pytest -q tests/structural/` produces a failure set identical
   to the parent commit's. That set is captured before editing with
   `python3 -m pytest -q tests/structural/ | grep ^FAILED | sort > /tmp/vvhb-base.txt`
   [R1 P3-14].
5. The deployed `ic` emits the new warnings.

## Routing and review

- Decision context:
  `{"reasons":["broad-consequences"], "rationale":"goal-authoring standard and linter across every workspace"}`.
- Author: claude-opus-5-5.
- Adversarial plan review on review-opus, at mk's request. Round 1 was FAIL with 15
  findings, all folded in here.
- The cross-lab second review is an open gate until Codex seats recover (bb 0.43.5,
  bbDev).
- **Note.** The round-1 dispatch flagged "the seat mutated the checkout". The mutation was
  a concurrent, unrelated edit by another session to Clavain `config/routing.yaml`,
  `docs/canon/reasoning-routing.md` and `tests/routing/reasoning-contract-test.sh` (the
  `cross_lab_first` ruling, file times 07:21–07:22). The reviewer was read-only. Those
  files are not touched by this plan.

## Tracking and upstream

- Results go to `mk-vvhb`. Close it only after deploy plus the canary.
- An upstream Claude Code request is drafted in the bead, not sent. It asks for:
  - an `awaiting_user` verdict
  - back-off after N identical verdicts
  - the anti-deference guard on the `ok:true` branch
- The bb `/goal clear` routing bug is in `mk-8jnh` and is not in scope here.

## Out of scope

- Patching Claude Code.
- `mk-8jnh`.
- Rewriting goals that are already minted.
