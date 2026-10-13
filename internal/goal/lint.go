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

// ── Judge and bar rules (mk-3mo0w) ──────────────────────────────────────────
//
// A goal whose only predicates are artifacts its producer creates (a commit,
// a push, a closed bead, a file that exists) is graded by the producer: the
// evaluator confirms the work was filed, never that it is right. And an
// OUTCOME with no number, comparison, or universal gives the executor no bar
// to clear, so "done" collapses to "did something". Both are heuristics,
// hence warnings.

// externalJudge matches a predicate decided by something other than the
// producer: a command's exit code or verdict, a test run, an HTTP status, a
// measurement against a reference, a merge someone else performs, or a review
// with a result. A judge noun alone is a topic, not a judge ("test plan filed",
// "benchmark report recorded", "go test instructions"): it needs a verdict or
// execution word within the same clause. Only a measurement may be judged by
// a comparison with a number or a reference ("benchmark under 3 seconds",
// "meter against the sequential run"), and "under docs/bench" is a place, not a
// comparison. "Clean" counts only as a state something reports ("came back
// clean", "exits cleanly"), not as an adjective ("a clean push").
// Words a producer satisfies by writing a file (filed, recorded, shows,
// exists) are not results, and neither are nouns ("test failure report",
// "review approval checklist", "test runs log") or hyphenated modifiers
// ("first-pass report"). A period inside a token such as "meter.py" does not
// end the clause. Only DONE WHEN is searched, so words in OUTCOME or OUT that
// name a judge without requiring one do not count either.
const near = `([^.;,]|\.\S){0,40}?`

// RE2 has no lookaround, so these guards consume the neighbouring character to
// keep a verdict word from counting inside a modifier ("first-pass",
// "pass/fail").
const pre = `(^|[^\w/-])`
const post = `($|[^\w/-])`
const result = pre + `(pass|passes|passed|passing|green|fail|fails|failed|failing|lgtm|approves|approved|ran|executed|succeeds|succeeded|successfully)` + post +
	`|\b(back|is|are|was|were|runs?|ran|exits?|comes?|came|stays?)\s+clean(ly)?\b` +
	`|\b(run|runs|ran)\s+(in|on|against|fresh|clean|green)\b` +
	`|\breturns?\s+(\d|zero|non-?zero|ok|true|false|clean|empty|nothing)\b`
const comparison = `\b(under|within|below|above|at most|at least)\s+\d|\b(against|faster|slower)\b`
const judgeNoun = `\b(tests?|suites?|checks?|review(s|ed|er|ers)?|judge|audit|verdicts?|canar(y|ies)|m(e|\x{e9})lange|flux-(drive|review)|pytest|bats|vitest|tsc)\b`
const measureNoun = `\b(measur\w*|meter|benchmark\w*|profil(ed|ing)|pass_rate)\b`

// outsideReview is a review whose reviewer is not the producer, with an
// outcome verb directly after it ("melange review filed", "Flux Mélange run has
// produced"). Only auxiliaries may sit between, so "independent review
// checklist filed" is still an artifact.
const outsideReview = `\b(cross-lab|independent|external|outside|m(e|\x{e9})lange|flux-(drive|review))\s+(review|run)\s+((has|have|was|were|is|been)\s+)*(filed|produced|completed|returned|found|passed|approved)\b`

var externalJudge = regexp.MustCompile(`(?i)` +
	`exit\w*\s+(code\s+)?(\d|non-?zero)|` +
	pre + `(pass|passes|passed|passing|fails|tested)` + post + `|` +
	`\b(run|ran|executed|re-?run)\s+fresh\b|` +
	`\b(HTTP|http)\s*[2-5]\d\d\b|` +
	`\b(reviewed|checked|judged|verified)\s+by\s+(an?\s+|the\s+)?(cross-lab|independent|external|outside|second|another|different)\b|` +
	`\bmerged\b|\b(approved|signed off)\b|\breproduc(ed|es)\b|\b(demonstrated|observed|live(-session)?)\s+refusals?\b|\brefus(es|ed)\b|` +
	`\b(no|zero|0)\s+(\w+\s+)?failures?\b|` +
	judgeNoun + near + `(` + result + `)|` +
	measureNoun + near + `(` + result + `|` + comparison + `)|` +
	outsideReview)

// negated matches a result the text says did not happen ("tests updated but
// not run in CI", "tests aren't run", "never had to pass", "opened, not
// merged"). It is removed before the judge search, so a negated run is not a
// run.
var negated = regexp.MustCompile(`(?i)\b(not|never|without|no|\w+n['\x{2019}]t)\s+((yet|be|been|being|(have|has|had|need|needs|needed|required)\s+to)\s+)*(run|runs|ran|executed|pass\w*|tested|approved|merged|green|clean|reviewed)\b`)

// passRate and passReport rewrite "pass" used as a noun modifier before the
// judge search. A pass rate is a measurement, so it needs a comparison ("test
// pass rate at least 95%"); a pass report is an artifact ("test pass rate
// report", "pass summary"). Both stay on one line, so "tests pass" ending a
// line keeps its verdict when the next line starts "Report".
var passRate = regexp.MustCompile(`(?i)\b(pass|fail)[ \t]+(rates?|ratios?|counts?)\b`)
var passReport = regexp.MustCompile(`(?i)\b(pass|fail)[ \t]+(reports?|summar(y|ies)|criteria|matri(x|ces)|logs?|lists?)\b`)

// header matches the start of any goal-shape section. Case-sensitive: the
// headers are upper-case by convention, so "gate" in prose is not one, and a
// GATE header needs its number ("GATE 1", "GATE n"), so "the GATE API" is not
// one either. Both apostrophes are accepted in WHO'S WAITING, and a label may
// carry a parenthetical before its colon ("PREMISE (C3 and above):").
const label = `(?:\s*\([^)\n]*\))?:`
const header = `\bWHO[\x{2019}']S WAITING` + label + `|\bPREMISE` + label + `|\bGATE\s+(\d+|n)\b|\bDONE WHEN` + label + `|\bOUT` + label

// outcomeSection captures an OUTCOME: block up to the next header.
var outcomeSection = regexp.MustCompile(`(?s)\bOUTCOME` + label + `(.*?)(` + header + `|\z)`)

// doneSection captures a DONE WHEN: block up to the next header. Only it is
// searched for an external judge, so a judge named in OUTCOME prose or
// excluded under OUT does not clear the warning.
var doneSection = regexp.MustCompile(`(?s)\bDONE WHEN` + label + `(.*?)(` + header + `|\z)`)

// bar matches a measurable target: a number, a comparison, or a universal.
// A number has to start a token, so the digit in "OAuth2" is not a bar.
var bar = regexp.MustCompile(`(?i)` +
	`\b\d|%|` +
	`\b(zero|one|two|three|four|five|six|seven|eight|nine|ten|half|twice|double)\b|` +
	`\b(fewer|more|less|faster|slower|higher|lower|under|within|at least|at most|no more than)\b|` +
	`\b(every|all|each|none|never|no longer|identical)\b`)

// judgeProblems returns the judge and bar findings for a goal/condition text.
// Each rule reads one section of the goal form and skips a text that omits it.
// A text with no demonstrable predicate already errors, so the self-graded
// warning only speaks when the predicates exist and none of them is external.
func judgeProblems(text string) []Problem {
	var probs []Problem
	if m := doneSection.FindStringSubmatch(text); m != nil && demonstrable.MatchString(text) &&
		!externalJudge.MatchString(judgeable(m[1])) {
		probs = append(probs, Problem{Severity: "warning", Message: "self-graded: " +
			"DONE WHEN names only artifacts the producer creates (committed, pushed, " +
			"bead closed, file exists) and no command verdict, test run or independent " +
			"review, so nothing outside the producer checks the result. Add a check " +
			"the producer cannot pass by asserting it"})
	}
	if m := outcomeSection.FindStringSubmatch(text); m != nil && !bar.MatchString(m[1]) {
		probs = append(probs, Problem{Severity: "warning", Message: "no bar: " +
			"OUTCOME names no number, comparison, or universal (every, zero, " +
			"identical), so the executor has no target to clear. State the bar " +
			"the outcome must reach"})
	}
	return probs
}

// judgeable removes negated results and rewrites "pass" used as a noun
// modifier, so neither reads as a verdict.
func judgeable(done string) string {
	done = negated.ReplaceAllString(done, " ")
	done = passRate.ReplaceAllString(done, " pass_rate ")
	return passReport.ReplaceAllString(done, " report ")
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
// contract on three axes. JUDGEABILITY: length, non-emptiness, demonstrability,
// and a bounded-runtime recommendation. SHAPE (see shapeProblems): whether the
// text is a goal rather than a plan, and whether the agent drafting it has put
// the user's ruling in the user's mouth. JUDGE (see judgeProblems): whether
// anything other than the producer grades the result, and whether the OUTCOME
// sets a bar. Errors block minting (unless forced);
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
	return append(probs, judgeProblems(text)...)
}
