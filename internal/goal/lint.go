package goal

import (
	"fmt"
	"regexp"
	"strings"
)

// Problem is one lint finding on a /goal completion-condition string.
type Problem struct {
	Severity string `json:"severity"` // "error" | "warning"
	Message  string `json:"message"`
}

// MaxConditionLen is the /goal built-in's condition limit.
const MaxConditionLen = 4000

// demonstrable matches predicates the /goal evaluator can judge from
// surfaced conversation output (commands, exit codes, artifact states).
// Deliberately mechanical — no model judgment (capability-routing doctrine).
var demonstrable = regexp.MustCompile(`(?i)` +
	`exit(s)?\s+(code\s+)?0|` +
	"`[^`]+`|" +
	`tests?\s+(pass|green)|` +
	`git status|` +
	`\b(bd|bead)\b.*\bclose|` +
	`\b(HTTP|http)\s*2\d\d\b|` +
	`file .*exist|` +
	`committed|pushed|published|deployed|merged|` +
	`stop after \d+ turns`)

var turnBound = regexp.MustCompile(`(?i)stop after \d+ turns`)

// ── Shape rules (Sylveste-7t3n) ─────────────────────────────────────────────
//
// The rules above ask "can the evaluator judge this?". These ask "did the
// right person write it, and is it a goal rather than a plan?".
//
// They exist because judgeability alone passed five consecutive goals that
// were wrong in a way nobody caught until the user read them back. A goal
// containing "and I am ruling it", "AnimalCommand::Rotate { animal, site } at
// discriminant 1", "wards.rs:568" and "I am answering its canon question"
// linted to exactly two findings, both about predicates; supplying a
// predicate returned null, exit 0.

// ventriloquism matches an agent writing the USER's ruling in the user's
// voice. The user then pastes it, and the agent's own design call arrives
// pre-approved — which defeats a canon gate by satisfying it with a document
// the agent authored.
//
// Deliberately narrow, and the narrowness is load-bearing because this is an
// ERROR that blocks minting. Three things are NOT caught, all legitimate:
//
//   - first person stating an outcome — "I play a year and can lose a bird";
//   - the deliberative future — "mine to decide", "do I reverse that?", which
//     is exactly the good form (an open call left open);
//   - the past tense reporting a decision the user really did make earlier —
//     "I decided last week to cut water", which is history, not ventriloquism.
//
// What IS caught is the progressive and the performative: a decision being
// made inside the goal text, in the user's voice, by the agent who drafted it.
var ventriloquism = regexp.MustCompile(`(?i)` +
	`\bi(\s+am|'m)\s+(rul|decid|answer|declar)ing\b|` +
	`\bi\s+hereby\b|` +
	`\bmy\s+(ruling|decision|call)\s+(is|stands)\b|` +
	`\bi\s+have\s+(ruled|decided|answered|declared)\b`)

// planDetail matches execution-grade content: a goal that names the mechanism
// cannot be re-planned without being rewritten. Per capability-routing
// doctrine the frontier tier writes goals; exact paths and signatures belong
// in the plan a weaker executor reads.
//
// Heuristic, hence a warning — a goal may legitimately name a file it is
// about ("delete city_run.rs"). What it should not do is specify the shape of
// the code that replaces it.
var planDetail = regexp.MustCompile(`(?i)` +
	`\b[\w/.-]+\.(rs|go|ts|tsx|py|js|sh|md)\s*:\s*\d+|` + // file:line
	`\bdiscriminant\s+\d+|` +
	`\b\w+(::\w+)+\s*\{[^}]*,|` + // Type::Variant { a, b }
	`\bfn\s+\w+\s*\(|` +
	`\bimpl\s+\w+\b`)

// preRuled matches text that both names an open design call and answers it.
// A gate the drafter has already decided is not a gate.
var preRuled = regexp.MustCompile(`(?i)(canon|design|open)\s+(call|question|decision)`)

// ── Form rules (mk-hxgi) ────────────────────────────────────────────────────
//
// The shape rules ask whether the right person wrote it. These ask whether it
// is a goal at all. On 2026-09-01 a Next-goal block recommended "Merge PR #26
// so the intercore goal-close protocol lands" as the successor goal: a merge,
// which is the user's call and not the agent's, in a shape with no OUTCOME:
// and no DONE WHEN:. Only 5 of 24 minted goals carried the canonical form.
//
// All three are WARNINGS, and their precision is what keeps them from being
// learned as noise. The landing rule needs both a landing VERB at the start
// and a landing-shaped OBJECT within the next five words, so "merge the
// ranked lists" is a technical goal and "Ship it." is a landing action. The
// merge rule steps aside when the merge is already under a GATE that names
// who merges, because that is the recommended form.

var canonicalOutcome = regexp.MustCompile(`\bOUTCOME:`)
var canonicalDone = regexp.MustCompile(`\bDONE WHEN:`)

// landingVerb captures the five words after a landing verb at the start of
// the text (after an optional leading "/goal").
var landingVerb = regexp.MustCompile(`(?i)^\s*(?:/goal\s+)?` +
	`(merge|rebuild|reinstall|install|bump|tag|publish|deploy|push|ship|release|roll\s+out|cut)\b` +
	`((?:\s+\S+){1,5})`)

// landingObject is what makes the verb a landing action rather than a
// technical one: a PR, a branch, a release, a version, an environment, a
// plugin, or a bare "it"/"this".
var landingObject = regexp.MustCompile(`(?i)(?:\b(?:PRs?|pull\s+requests?|branch(?:es)?|releases?|versions?|` +
	`v\d[\w.]*|tags?|main|master|prod|production|staging|plugins?|packages?|wave|it|this|the\s+fix|the\s+change)\b|#\d+)`)

// mergeNearPR: a merge verb within three words of a PR reference, either order.
var mergeNearPR = regexp.MustCompile(`(?i)\bmerg\w*(?:\s+\S+){0,3}\s+(?:\bPRs?\b|\bpull\s+requests?\b|#\d+)` +
	`|(?:\bPRs?\b|\bpull\s+requests?\b|#\d+)(?:\s+\S+){0,3}\s+merg\w*`)

// gateBefore: a GATE marker in the same sentence before the merge phrase.
var gateBefore = regexp.MustCompile(`(?i)\bGATE\b[^.]*$`)

// formProblems returns the form findings: canonical-form markers, a
// landing-only opening, and a merge the agent cannot perform.
func formProblems(text string) []Problem {
	var probs []Problem
	missing := []string{}
	if !canonicalOutcome.MatchString(text) {
		missing = append(missing, "OUTCOME:")
	}
	if !canonicalDone.MatchString(text) {
		missing = append(missing, "DONE WHEN:")
	}
	if len(missing) > 0 {
		probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
			"canonical form: no %s line. A goal states the outcome first and names "+
				"the observable condition it is done at (OUTCOME / GATE / DONE WHEN, "+
				"see docs/guide-goal-shape.md); without them the successor cannot "+
				"tell finished from abandoned", strings.Join(missing, " or "))})
	}
	if m := landingVerb.FindStringSubmatch(text); m != nil && landingObject.MatchString(m[2]) {
		probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
			"landing-first: %q opens with a landing action. Merging, installing, "+
				"deploying and shipping are an mk-step or the tail of the current "+
				"goal, not a successor; if it is a gate, put it under GATE and name "+
				"who does it, and make the goal the work the landing unblocks",
			strings.TrimSpace(m[0]))})
	}
	if loc := mergeNearPR.FindStringIndex(text); loc != nil {
		before := text[:loc[0]]
		if start := len(before) - 80; start > 0 {
			before = before[start:]
		}
		if !gateBefore.MatchString(before) {
			probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
				"merge is not the agent's: %q. Merging a PR is the user's call; "+
					"state it as a GATE naming who merges, and make the goal the work "+
					"the merge unblocks", strings.TrimSpace(text[loc[0]:loc[1]]))})
		}
	}
	return probs
}

// shapeProblems returns the shape findings for a goal/condition text.
// Separated from LintCondition so a caller that only wants judgeability (a
// bare condition extracted from a longer charter, say) can keep using the
// original rules alone.
func shapeProblems(text string) []Problem {
	var probs []Problem
	if m := ventriloquism.FindString(text); m != "" {
		probs = append(probs, Problem{Severity: "error", Message: fmt.Sprintf(
			"ventriloquism: %q writes the user's ruling in the user's voice. A goal "+
				"the agent drafted cannot also contain the user's sign-off — that "+
				"satisfies a canon gate with the agent's own document. State the open "+
				"call as a QUESTION and recommend an answer outside the goal text", m)})
	}
	if m := planDetail.FindString(text); m != "" {
		probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
			"plan detail: %q is execution-grade (exact signature, discriminant, or "+
				"file:line). A goal states the outcome; the plan states the mechanism, "+
				"and a goal carrying both cannot be re-planned without a rewrite", m)})
	}
	if preRuled.MatchString(text) && ventriloquism.MatchString(text) {
		probs = append(probs, Problem{Severity: "warning", Message: "pre-ruled call: " +
			"the text names an open canon/design call and answers it in the same " +
			"breath. Leave it open as a gate the user decides"})
	}
	return probs
}

// LintCondition validates a condition string against the /goal built-in's
// contract on two axes. JUDGEABILITY: length, non-emptiness, demonstrability,
// and a bounded-runtime recommendation. SHAPE (see shapeProblems): whether the
// text is a goal rather than a plan, and whether the agent drafting it has put
// the user's ruling in the user's mouth. Errors block minting (unless forced);
// warnings inform.
func LintCondition(text string) []Problem {
	var probs []Problem
	if len(text) == 0 {
		return []Problem{{Severity: "error", Message: "condition is empty"}}
	}
	if len(text) > MaxConditionLen {
		return []Problem{{Severity: "error", Message: fmt.Sprintf(
			"condition is %d chars; the /goal built-in caps at %d", len(text), MaxConditionLen)}}
	}
	if !demonstrable.MatchString(text) {
		probs = append(probs, Problem{Severity: "error", Message: "no demonstrable predicate " +
			"(the evaluator only judges surfaced output — reference a command, exit code, " +
			"artifact state, or bead close; not subjective quality)"})
	}
	if !turnBound.MatchString(text) {
		probs = append(probs, Problem{Severity: "warning", Message: "no runtime bound — " +
			"consider appending 'or stop after N turns'"})
	}
	probs = append(probs, shapeProblems(text)...)
	return append(probs, formProblems(text)...)
}
