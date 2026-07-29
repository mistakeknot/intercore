package goal

import (
	"fmt"
	"regexp"
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
	return append(probs, shapeProblems(text)...)
}
