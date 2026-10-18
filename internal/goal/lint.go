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

// The markers use the same label grammar as the section parsers, so
// "OUTCOME (the change):" counts as present.
var canonicalOutcome = regexp.MustCompile(`\bOUTCOME` + label)
var canonicalDone = regexp.MustCompile(`\bDONE WHEN` + label)

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

// mergeWord finds each merge word on its own, so one occurrence can never
// swallow another (a noun use next to a real merge, or a span that bridges
// two sections).
var mergeWord = regexp.MustCompile(`(?i)\bmerg\w*`)

// prRef is a PR reference: "PR", "pull request" or "#123".
const prRef = `(?:\bPRs?\b|\bpull\s+requests?\b|#\d+)`

// mergeAfterPR / mergeBeforePR: a PR reference within three words after / before
// the merge word, inside one clause.
var mergeAfterPR = regexp.MustCompile(`(?i)^(?:\s+\S+){0,3}\s+` + prRef)
var mergeBeforePR = regexp.MustCompile(`(?i)` + prRef + `(?:\s+\S+){0,3}\s+$`)

// sectionLabel marks the start of a goal section (header syntax: the colon is
// required, so prose such as "the GATE API" is not a label); a merge is gated when the
// nearest label before it is GATE.
var sectionLabel = regexp.MustCompile(`\b(OUTCOME|GATE(?:\s+(?:\d+|n)\b(?:\s*[\x{2014}\x{2013}-]\s*[^:\n]{1,40})?)?|DONE WHEN|OUT|PREMISE|WHO[\x{2019}']S WAITING)(?:\s*\([^)\n]*\))?:`)

// clauseBreak ends a clause. A period only counts when followed by space or
// the end, so paths like ./dist/plugin.zip do not split.
var clauseBreak = regexp.MustCompile(`[;!?]|\.(?:\s|$)`)

// mergeGated reports whether the nearest section label before pos is GATE.
func mergeGated(text string, pos int) bool {
	locs := sectionLabel.FindAllStringSubmatchIndex(text[:pos], -1)
	if len(locs) == 0 {
		return false
	}
	last := locs[len(locs)-1]
	return strings.HasPrefix(text[last[2]:last[3]], "GATE")
}

// clauseAfter returns the text after pos up to the end of its clause or the
// next section header, whichever comes first.
func clauseAfter(text string, pos int) string {
	rest := text[pos:]
	if loc := sectionLabel.FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	if loc := clauseBreak.FindStringIndex(rest); loc != nil {
		return rest[:loc[0]]
	}
	return rest
}

// clauseBefore returns the text before pos back to the start of its clause or
// the last section header.
func clauseBefore(text string, pos int) string {
	head := text[:pos]
	if loc := sectionLabel.FindAllStringIndex(head, -1); len(loc) > 0 {
		head = head[loc[len(loc)-1][1]:]
	}
	locs := clauseBreak.FindAllStringIndex(head, -1)
	if len(locs) == 0 {
		return head
	}
	return head[locs[len(locs)-1][1]:]
}

// openingClause cuts text at the first sentence end or section label, so the
// landing object has to sit in the opening clause and cannot be borrowed from
// the OUTCOME that follows it.
// Wrapped lines join; only a sentence end or a section header (colon syntax,
// see sectionLabel) closes it.
var openingEnd = regexp.MustCompile(`[!?;]|\.(?:\s|$)|` + sectionLabel.String())

func openingClause(s string) string {
	if loc := openingEnd.FindStringIndex(s); loc != nil {
		return s[:loc[0]]
	}
	return s
}

// mergeNoun marks "merge" used as a technical noun or modifier ("merge
// conflict", "merge commit", "merge strategy"), which is work the agent can do.
var mergeNoun = regexp.MustCompile(`(?i)^\s+(conflicts?|commits?|bases?|strateg\w+|queues?|drivers?|markers?|logic|tools?|results?|algorithms?|requests?)\b`)

// mergeIsNoun reports whether the merge word ending at end is followed by a
// technical noun, in which case it is not a merge of the PR.
func mergeIsNoun(text string, end int) bool {
	return mergeNoun.MatchString(text[end:])
}

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
	if m := landingVerb.FindStringSubmatch(text); m != nil && landingObject.MatchString(openingClause(m[2])) {
		probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
			"landing-first: %q opens with a landing action. Merging, installing, "+
				"deploying and shipping are an mk-step or the tail of the current "+
				"goal, not a successor; if it is a gate, put it under GATE and name "+
				"who does it, and make the goal the work the landing unblocks",
			strings.TrimSpace(m[0]))})
	}
	// Every merge word is judged on its own: a gated first merge must not
	// hide a later ungated one.
	for _, loc := range mergeWord.FindAllStringIndex(text, -1) {
		if mergeIsNoun(text, loc[1]) {
			continue
		}
		if !mergeAfterPR.MatchString(clauseAfter(text, loc[1])) &&
			!mergeBeforePR.MatchString(clauseBefore(text, loc[0])) {
			continue
		}
		if mergeGated(text, loc[0]) {
			continue
		}
		probs = append(probs, Problem{Severity: "warning", Message: fmt.Sprintf(
			"merge is not the agent's: %q. Merging a PR is the user's call; "+
				"state it as a GATE naming who merges, and make the goal the work "+
				"the merge unblocks", strings.TrimSpace(text[loc[0]:loc[1]]))})
		break
	}
	return probs
}

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
// end the clause, nor does a path such as "./...". Only DONE WHEN is searched, so words in OUTCOME or OUT that
// name a judge without requiring one do not count either.
const near = `([^.;,]|\.\.\.|\.\S){0,40}?`

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
const judgeNoun = `\b(tests?|suites?|checks?|ci|builds?|pipelines?|review(s|ed|er|ers)?|judge|audit|verdicts?|canar(y|ies)|m(e|\x{e9})lange|flux-(drive|review)|pytest|bats|vitest|tsc)\b`
const measureNoun = `\b(measur\w*|meter|benchmark\w*|profil(ed|ing)|pass_rate)\b`

// outsideReview is a review whose reviewer is not the producer, with an
// outcome verb directly after it ("melange review filed", "Flux Mélange run has
// produced"). Only auxiliaries may sit between, so "independent review
// checklist filed" is still an artifact.
const outsideReview = `\b(cross-lab|independent|external|outside|m(e|\x{e9})lange|flux-(drive|review))\s+(review|run)\s+((has|have|was|were|is|been)\s+)*(filed|produced|completed|returned|found|passed|approved)\b`

// outsideBy names a judge other than the producer after "by": a cross-lab,
// independent, external, outside or different one, a second or another
// reviewer noun, a human, or CI or a test suite.
const outsideBy = `by\s+(an?\s+|the\s+)?((cross-lab|independent|external|outside|different)\b|` +
	`(second|another)(\s+[\w-]+)?\s+` + reviewerNoun + `\b|` +
	`(humans?|people|persons?|ci|pipelines?|tests?|test\s+suites?|suites?|linters?)\b)`

const reviewerNoun = `(reviewers?|agents?|seats?|models?|part(y|ies)|humans?|teams?|people|persons?|auditors?|judges?|evaluators?|engineers?|maintainers?)`

var externalJudge = regexp.MustCompile(`(?i)` +
	`exit\w*\s+(code\s+)?(\d|non-?zero)|` +
	pre + `(pass|passes|passed|passing|fails|tested)` + post + `|` +
	`\b(run|ran|executed|re-?run)\s+fresh\b|` +
	`\b(HTTP|http)\s*[2-5]\d\d\b|` +
	`\b(reviewed|checked|judged|verified)\s+` + outsideBy + `|` +
	`\bmerged\b|\b(approved|signed off)\b|\breproduc(ed|es)\b|\b(demonstrated|observed|live(-session)?)\s+refusals?\b|\brefus(es|ed)\b|` +
	`\b(no|zero|0)\s+(\w+\s+)?failures?\b|` +
	judgeNoun + near + `(` + result + `)|` +
	measureNoun + near + `(` + result + `|` + comparison + `)|` +
	outsideReview)

// negated matches a result the text says did not happen ("tests updated but
// not run in CI", "tests aren't run", "never had to pass", "opened, not
// merged"). It is removed before the judge search, so a negated run is not a
// run.
var negated = regexp.MustCompile(`(?i)\b(not|never|without|no|\w+n['\x{2019}]t)\s+((yet|be|been|being|(have|has|had|need|needs|needed|required)\s+to)\s+)*(run|runs|ran|executed|pass\w*|tested|approved|merged|green|clean|reviewed|checked|judged|verified)\b`)

// passRate and passReport rewrite "pass" used as a noun modifier before the
// judge search. A pass rate is a measurement, so it needs a comparison ("test
// pass rate at least 95%"); a pass report is an artifact ("test pass rate
// report", "pass summary"). Both stay on one line, so "tests pass" ending a
// line keeps its verdict when the next line starts "Report".
var passRate = regexp.MustCompile(`(?i)\b(pass|fail)[ \t]+(rates?|ratios?|counts?)\b`)

// passGiven is a pass something else hands out ("gets a pass on CI", "the
// suite reports a pass", "a pass from CI"), which is a verdict. The pass must
// end the clause or take a preposition or "and", so "returns a pass summary" stays an
// artifact. "Give it a pass" is left out because the producer can give one.
// passNoun is any other "pass" after a determiner ("another pass of myself",
// "reviewed in a final pass"), which names an attempt. passGiven is rewritten
// first.
var passGiven = regexp.MustCompile(`(?i)\b(gets?|got|gotten|receives?|received|earns?|earned|returns?|returned|reports?|reported)[ \t]+(\w+[ \t]+){0,2}(a|an|the)[ \t]+pass(?P<f>[ \t]+(on|from|in|at|under|with|for|and)\b|[ \t]*([,;:)\n]|\.(\s|\z)|\z))|\b(a|an|the)[ \t]+pass[ \t]+from\b`)
var passNoun = regexp.MustCompile(`(?i)\b(a|an|another|one|more|the|this|that|each|every|first|second|third|final|last|next)[ \t]+pass\b`)

// selfRun is an outside qualifier on a word for an attempt ("reviewed by an
// independent final pass", "checked by a different run of the same agent").
// The qualifier describes the attempt, not a judge, so it is rewritten before
// the qualifier can count as one. A run, invocation, execution or session is
// an attempt only when it is the producer's own ("of my own", "by the same
// agent", "of this session"), so "verified by an independent run of our
// regression tests" keeps its judge.
var selfRun = regexp.MustCompile(`(?i)\bby[ \t]+(an?[ \t]+|the[ \t]+)?(cross-lab|independent|external|outside|different)[ \t]+([\w-]+[ \t]+)?` +
	`((pass(es)?|attempts?|iterations?|rounds?|tries|try|drafts?)\b|` +
	`(runs?|invocations?|executions?|sessions?)[ \t]+(of|by|from)[ \t]+(myself|itself|me|us|the[ \t]+producer|` +
	`((my|our|its|their)[ \t]+own|(my|our|its|their|this|the[ \t]+same)[ \t]+` + agentNoun + `)\b))`)

const agentNoun = `(agent|model|session|seat|producer|thread|author)s?`

// selfNoun is a judge that is the producer ("my self-review", "my own
// review", "our review", "the producer's review"). "Its review" needs "own",
// since "the patch and its review" is the patch's reviewer.
const selfNoun = `(\b((my|our|its|their|the)[ \t]+)?self-(review|check|assess|grad|approv)\w*|` +
	`\b((my|our)[ \t]+(own[ \t]+)?|(its|their)[ \t]+own[ \t]+|(the[ \t]+)?producer[\x{2019}']s[ \t]+(own[ \t]+)?)` +
	selfJudgeNoun + `\b)`

const selfJudgeNoun = `(reviews?|judg(e)?ments?|assessments?|read-?throughs?|pass(es)?|verdicts?|approvals?|sign-?offs?)`

// selfSubject is a producer judge, with an optional parenthetical or short
// "of" phrase, followed by its predicate ("my self-review came back clean",
// "my self-review of the docs passed", "my self-review and sign-off are
// approved"). selfSource is
// a verdict a producer judge gives as its source ("a pass from my own
// review", "approved in my self-review", "passed my own review").
// commandJudge is a judge the producer cannot be: a test, check, build or exit
// code. A producer judge after "of" or another preposition is not the
// subject ("a cross-lab review of my self-review passed"), and a source
// verdict after an outside agent is that agent's ("an independent reviewer
// passed my self-review").
var selfSubject = regexp.MustCompile(`(?i)` + selfNoun + `(([ \t]*,|[ \t]+(and|or))[ \t]+(own[ \t]+)?` + selfJudgeNoun + `)*` +
	`([ \t]*\([^)\n]*\)|[ \t]+(of|on|for|about|over)([ \t]+[\w./-]+){1,3}?)?[ \t]+` +
	`((is|are|was|were|has|have|had|will|would|does|do|did)[ \t]+((been|be|not)[ \t]+)*[\w-]+|` +
	`(came|comes|got|gets|gave|gives|found|finds|ran|runs|shows?|reports?|returns?|pass(es)?|approves?|accepts?|signs?|green|clean|lgtm|succeeds|\w+ed)\b)`)
var selfSource = regexp.MustCompile(`(?i)\b(pass|passed)[ \t]+((from|by|in|on)[ \t]+)?` + selfNoun + `|\b(approved|accepted|signed[ \t]+off)[ \t]+(from|by|in|on)[ \t]+` + selfNoun)
var commandJudge = regexp.MustCompile(`(?i)\b(tests?|suites?|checks?|ci|builds?|pipelines?|canar(y|ies)|pytest|bats|vitest|tsc)\b|exit\w*\s+(code\s+)?(\d|non-?zero)|\b(HTTP|http)\s*[2-5]\d\d\b`)
var outsideJudge = regexp.MustCompile(`(?i)\b` + outsideBy)
var outsideAgent = regexp.MustCompile(`(?i)\b(cross-lab|independent|external|outside|different|second|another)(\s+[\w-]+)?\s+(` + reviewerNoun + `|reviews?)\b|\bhumans?\b`)
var objectOf = regexp.MustCompile(`(?i)\b(of|on|about|for|to|in|from|with|into|than)[ \t]+$`)

// selfBreak ends a clause. "And" or "or" before another verdict word
// continues it ("was checked and approved").
var selfBreak = regexp.MustCompile(`(?i)[,;\n]|\.(\s|$)|\b(and|or)\b`)
var selfContinue = regexp.MustCompile(`(?i)^[ \t]+(then[ \t]+)?(approv|pass|accept|sign|green|clean|lgtm|succeed|merged|verified|checked)`)

var passReport = regexp.MustCompile(`(?i)\b(pass|fail)[ \t]+(reports?|summar(y|ies)|criteria|matri(x|ces)|logs?|lists?)\b`)

// header matches the start of any goal-shape section. Case-sensitive: the
// headers are upper-case by convention, so "gate" in prose is not one, and a
// GATE header needs its number ("GATE 1", "GATE n"), so "the GATE API" is not
// one either. Both apostrophes are accepted in WHO'S WAITING, and a label may
// carry a parenthetical before its colon ("PREMISE (C3 and above):"). OUT
// with a parenthetical is a header only at the start of the text, a line, a
// bullet or a sentence, so "SQL OUT (parameter):" in prose does not end a section.
const label = `(?:\s*\([^)\n]*\))?:`
const header = `\bWHO[\x{2019}']S WAITING` + label + `|\bPREMISE` + label + `|\bGATE\s+(\d+|n)\b|\bDONE WHEN` + label + `|\bOUT:|(?:\A|[\n.!?;])\s*(?:[-*+]\s+|\d+[.)]\s+)?OUT\s*\([^)\n]*\):`

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

// dropSelfJudged blanks a producer judge's predicate to the end of its
// clause, unless the predicate names an outside judge after "by" ("is
// approved by an independent reviewer"). When the rest of the clause gives
// its own verdict from an outside agent or a command judge ("is attached
// once an independent reviewer approved it", "once the tests pass"), only
// the producer judge and its verb are blanked. A command named as the topic
// ("my self-review of the build passed") is not a judge. A verdict whose
// source is a producer judge is blanked unless a command judge or an outside
// agent with a reviewer noun comes before it in the clause ("tests passed in
// my own review", "an independent reviewer passed my self-review"), so "the
// second draft passed my self-review" is still the producer's.
func dropSelfJudged(done string) string {
	b := []byte(done)
	for _, m := range selfSubject.FindAllStringIndex(done, -1) {
		if objectOf.MatchString(done[clauseStart(done, m[0]):m[0]]) {
			continue
		}
		end := clauseEnd(done, m[1])
		if outsideJudge.MatchString(done[m[0]:end]) {
			continue
		}
		if tail := done[m[1]:end]; !selfContinue.MatchString(strings.TrimPrefix(strings.TrimLeft(tail, " \t"), "and")) &&
			(outsideAgent.MatchString(tail) || commandJudge.MatchString(tail)) && externalJudge.MatchString(tail) {
			blank(b, m[0], m[1])
			continue
		}
		blank(b, m[0], end)
	}
	for _, m := range selfSource.FindAllStringIndex(done, -1) {
		if before := done[clauseStart(done, m[0]):m[0]]; !commandJudge.MatchString(before) && !outsideAgent.MatchString(before) {
			blank(b, m[0], m[1])
		}
	}
	return string(b)
}

func clauseEnd(s string, from int) int {
	for {
		loc := selfBreak.FindStringIndex(s[from:])
		if loc == nil {
			return len(s)
		}
		if w := strings.ToLower(s[from+loc[0] : from+loc[1]]); w != "and" && w != "or" || !selfContinue.MatchString(s[from+loc[1]:]) {
			return from + loc[0]
		}
		from += loc[1]
	}
}

func clauseStart(s string, to int) int {
	start := 0
	for _, loc := range selfBreak.FindAllStringIndex(s[:to], -1) {
		start = loc[1]
	}
	return start
}

func blank(b []byte, from, to int) {
	for i := from; i < to; i++ {
		if b[i] != '\n' {
			b[i] = ' '
		}
	}
}

// judgeable rewrites an outside qualifier on an attempt, drops verdicts a
// producer judge gives, removes negated results, and rewrites "pass" used as
// a noun or a noun modifier, so none of them reads as a verdict.
func judgeable(done string) string {
	done = selfRun.ReplaceAllString(done, " by attempt ")
	done = dropSelfJudged(done)
	done = negated.ReplaceAllString(done, " ")
	done = passRate.ReplaceAllString(done, " pass_rate ")
	done = passReport.ReplaceAllString(done, " report ")
	done = passGiven.ReplaceAllString(done, " passed${f}")
	return passNoun.ReplaceAllString(done, " attempt ")
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
	probs = append(probs, judgeProblems(text)...)
	return append(probs, formProblems(text)...)
}
