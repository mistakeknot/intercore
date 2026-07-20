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

// LintCondition validates a condition string against the /goal built-in's
// contract: length, non-emptiness, demonstrability, and a bounded-runtime
// recommendation. Errors block minting (unless forced); warnings inform.
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
	return probs
}
