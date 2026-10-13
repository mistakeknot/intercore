package goal

import (
	"strings"
	"testing"
)

func TestLintCondition(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantErrs int
		wantWarn bool
	}{
		{"good with bound", "all Go tests exit 0 and bead mk-1 closed, or stop after 20 turns", 0, false},
		{"good no bound", "`go test ./...` exits 0 and git status is clean", 0, true},
		{"empty", "", 1, false},
		{"too long", strings.Repeat("x", 4001), 1, false},
		{"subjective only", "the code is good and the feature feels polished", 1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probs := LintCondition(tc.text)
			errs, warns := 0, 0
			for _, p := range probs {
				if p.Severity == "error" {
					errs++
				} else {
					warns++
				}
			}
			if errs != tc.wantErrs {
				t.Errorf("errors = %d (%v), want %d", errs, probs, tc.wantErrs)
			}
			if tc.wantWarn && warns == 0 {
				t.Errorf("expected a warning, got %v", probs)
			}
		})
	}
}

// TestShapeRules pins the shape axis (Sylveste-7t3n). The cases are the real
// texts that motivated the rules: five agent-drafted /goal blocks the user
// pasted verbatim and later called "weird", against the rewrite that fixed
// them. The rewrite MUST lint clean or the rules are punishing the good form.
func TestShapeRules(t *testing.T) {
	// The worst of the five, with a predicate added — this exact text returned
	// null, exit 0 before the shape rules existed.
	badGoal := "RaveNous the spiral becomes the game. [1] ravenous-egq, and I am ruling it: " +
		"retarget re-points the bird's ROTATION via a new AnimalCommand::Rotate { animal, site } " +
		"at discriminant 1. Acceptance: a_click_does_something.rs inverts. [2] ravenous-5vd, and " +
		"I am answering its canon question: a recruit INHERITS the flock's rotation. [3] The " +
		"mechanism exists at wards.rs:568 — rewire it. Tests pass, or stop after 40 turns."

	// The rewrite: outcome first, open calls left open as gates, no mechanism.
	goodGoal := "RaveNous — the year can hurt me. OUTCOME: I play a year and can lose a bird. " +
		"Today I cannot: 52 weeks produces 0 deaths and 0 refusals. GATE 1 — measure before " +
		"building: what can the sim take from me, by what path, at what rate? GATE 2 — canon " +
		"call, mine to make: last goal ruled external danger out of scope because the spiral " +
		"WAS the danger. The spiral is gone. Do I reverse that, make the spiral reachable, or " +
		"accept survival is solved? Recommend one. DONE WHEN: the shipped year raises at least " +
		"one interrupt I can act on and `cargo test` exits 0, or stop after 40 turns."

	t.Run("the bad goal is now caught", func(t *testing.T) {
		probs := LintCondition(badGoal)
		var errs, warns int
		for _, p := range probs {
			if p.Severity == "error" {
				errs++
			} else {
				warns++
			}
		}
		if errs == 0 {
			t.Fatalf("ventriloquism must be an error; got %v", probs)
		}
		if warns == 0 {
			t.Errorf("plan detail must warn; got %v", probs)
		}
		var sawVentriloquism, sawPlan, sawPreRuled bool
		for _, p := range probs {
			switch {
			case strings.HasPrefix(p.Message, "ventriloquism:"):
				sawVentriloquism = true
			case strings.HasPrefix(p.Message, "plan detail:"):
				sawPlan = true
			case strings.HasPrefix(p.Message, "pre-ruled call:"):
				sawPreRuled = true
			}
		}
		if !sawVentriloquism || !sawPlan || !sawPreRuled {
			t.Errorf("wanted all three shape findings, got ventriloquism=%v plan=%v preRuled=%v (%v)",
				sawVentriloquism, sawPlan, sawPreRuled, probs)
		}
	})

	t.Run("the good goal lints clean", func(t *testing.T) {
		if probs := LintCondition(goodGoal); len(probs) != 0 {
			t.Errorf("the good form must lint clean, got %v", probs)
		}
	})
}

// TestVentriloquismPrecision guards the narrowness the error severity demands.
// A false positive here BLOCKS a legitimate goal from minting, so each of
// these is a form that must stay allowed.
func TestVentriloquismPrecision(t *testing.T) {
	allowed := []string{
		"I play a year and can lose a bird",
		"canon call, mine to make: do I reverse that?",
		"I will decide once you report the rate",
		"I decided last week to cut water from scope",
		"the decision is mine and I want options",
		"answering this needs the measurement first",
	}
	for _, s := range allowed {
		if ventriloquism.MatchString(s) {
			t.Errorf("false positive on legitimate goal text: %q", s)
		}
	}
	caught := []string{
		"and I am ruling it: the rotation moves",
		"I am answering its canon question",
		"I'm deciding this now",
		"I hereby ratify the charter",
		"my ruling is that recruits inherit",
		"I have decided to reverse the scope",
	}
	for _, s := range caught {
		if !ventriloquism.MatchString(s) {
			t.Errorf("missed ventriloquism: %q", s)
		}
	}
}

// TestJudgeRules pins the judge axis (mk-3mo0w). The self-graded case is the
// shape of a real charter condition: every predicate is a log, doc section, or
// file the producer writes, plus a clean push. Both rules are warnings, so they
// must never add an error.
func TestJudgeRules(t *testing.T) {
	has := func(probs []Problem, prefix string) bool {
		for _, p := range probs {
			if strings.HasPrefix(p.Message, prefix) {
				if p.Severity != "warning" {
					t.Errorf("%q is %s, want warning", prefix, p.Severity)
				}
				return true
			}
		}
		return false
	}

	t.Run("self-graded", func(t *testing.T) {
		cases := []struct {
			name string
			text string
			want bool
		}{
			{"only producer artifacts", "DONE WHEN: the report file exists, committed and pushed, bead mk-1 closed, or stop after 5 turns", true},
			{"tests pass", "DONE WHEN: tests pass and bead mk-1 closed, or stop after 5 turns", false},
			{"command exit code", "DONE WHEN: `ic goal lint-condition --file=x` exits 0 and committed, or stop after 5 turns", false},
			{"independent review", "DONE WHEN: committed, and a cross-lab review passed, or stop after 5 turns", false},
			{"merged by someone else", "DONE WHEN: PR merged, or stop after 5 turns", false},
			{"http status", "DONE WHEN: `curl` returns HTTP 200 and deployed, or stop after 5 turns", false},
			{"no predicate at all", "DONE WHEN: the feature feels polished", false},
			{"judge word only in outcome prose", "OUTCOME: the external API docs are clearer for every reader. DONE WHEN: report file exists, committed and pushed, or stop after 5 turns", true},
			{"judge excluded under OUT", "OUTCOME: every page renders. DONE WHEN: committed and pushed, or stop after 5 turns. OUT: independent review", true},
			{"review with no verdict", "DONE WHEN: report reviewed, committed and pushed, or stop after 5 turns", true},
			{"review with a verdict", "DONE WHEN: committed, and the review came back clean, or stop after 5 turns", false},
			{"judge inside DONE WHEN", "OUTCOME: every page renders. DONE WHEN: committed, and an independent review passed, or stop after 5 turns. OUT: styling", false},
			{"tested commit", "DONE WHEN: bead mk-1 closed against a tested commit, or stop after 5 turns", false},
			{"typecheck passing", "DONE WHEN: `npx tsc --noEmit` passing and pushed, or stop after 5 turns", false},
			{"test output green", "DONE WHEN: go test output surfaced green, or stop after 5 turns", false},
			{"melange review filed", "DONE WHEN: committed, and a melange review filed, or stop after 5 turns", false},
			{"measured against a baseline", "DONE WHEN: the meter reports wall time against the sequential run, or stop after 5 turns", false},
			{"demonstrated refusal", "DONE WHEN: the gate wired with one demonstrated refusal, or stop after 5 turns", false},
			{"test plan is an artifact", "DONE WHEN: test plan file exists, committed and pushed, or stop after 5 turns", true},
			{"benchmark report is an artifact", "DONE WHEN: benchmark report file exists, committed and pushed, or stop after 5 turns", true},
			{"review checklist is an artifact", "DONE WHEN: independent review checklist file exists, committed and pushed, or stop after 5 turns", true},
			{"canary config is an artifact", "DONE WHEN: canary config committed, or stop after 5 turns", true},
			{"tests run", "DONE WHEN: the regression tests run in CI, or stop after 5 turns", false},
			{"check named with a count", "DONE WHEN: a presence-check TSV covering all 561 titles exists, pushed, or stop after 5 turns", true},
			{"script name inside the clause", "DONE WHEN: pattern-f-meter.py reports wall time against the sequential run, or stop after 5 turns", false},
			{"test plan filed", "DONE WHEN: test plan filed and committed, or stop after 5 turns", true},
			{"benchmark report recorded", "DONE WHEN: benchmark report recorded and pushed, or stop after 5 turns", true},
			{"review checklist shows sections", "DONE WHEN: independent review checklist shows all sections and is committed, or stop after 5 turns", true},
			{"test command documented", "DONE WHEN: go test instructions file exists and is committed, or stop after 5 turns", true},
			{"green as an adjective", "DONE WHEN: a green logo file exists and is committed, or stop after 5 turns", true},
			{"outside review run produced", "DONE WHEN: a Flux M\u00e9lange run has produced a heat ledger, committed, or stop after 5 turns", false},
			{"tests updated but not run", "DONE WHEN: the script and its bats tests are updated on main, pushed, or stop after 5 turns", true},
			{"test fixtures under a directory", "DONE WHEN: test fixtures under testdata/ committed, or stop after 5 turns", true},
			{"benchmark under a bound", "DONE WHEN: the import benchmark finishes under 3 seconds, or stop after 5 turns", false},
			{"failure report is an artifact", "DONE WHEN: test failure report file exists and is committed, or stop after 5 turns", true},
			{"approval checklist is an artifact", "DONE WHEN: review approval checklist file exists and is pushed, or stop after 5 turns", true},
			{"hyphenated pass is a modifier", "DONE WHEN: first-pass report file exists and is committed, or stop after 5 turns", true},
			{"test runs log is an artifact", "DONE WHEN: the test runs log file exists and is committed, or stop after 5 turns", true},
			{"tests explicitly not run", "DONE WHEN: bats tests updated but not run in CI, committed, or stop after 5 turns", true},
			{"tests never had to pass", "DONE WHEN: bats tests updated and committed, never had to pass, or stop after 5 turns", true},
			{"tests contracted not run", "DONE WHEN: tests aren't run in CI; report file exists and is committed, or stop after 5 turns", true},
			{"tests curly contracted not run", "DONE WHEN: suite wasn\u2019t run in CI, report committed, or stop after 5 turns", true},
			{"pass/fail report", "DONE WHEN: test pass/fail report file exists and is committed, or stop after 5 turns", true},
			{"pass rate report", "DONE WHEN: test pass rate report file exists and is committed, or stop after 5 turns", true},
			{"pass summary", "DONE WHEN: suite pass summary written and pushed, or stop after 5 turns", true},
			{"pass rate with a bar", "DONE WHEN: test pass rate at least 95% on the full suite, committed, or stop after 5 turns", false},
			{"tests pass then report", "DONE WHEN: go test ./... passes, report committed, or stop after 5 turns", false},
			{"pass ends a line before report", "DONE WHEN: tests pass\n\nReport file exists and is committed, or stop after 5 turns", false},
			{"not merged", "DONE WHEN: PR opened, not merged, or stop after 5 turns", true},
			{"suite runs in CI and passes", "DONE WHEN: the suite runs in CI and passes, or stop after 5 turns", false},
			{"check exits cleanly", "DONE WHEN: `make check` exits cleanly, or stop after 5 turns", false},
			{"check returns zero", "DONE WHEN: the presence check returns 0 for all titles, or stop after 5 turns", false},
			{"suites executed fresh", "DONE WHEN: all four suites (interphase bats, the test runner, interline structural, interflux structural) executed fresh, or stop after 30 turns", false},
			{"benchmark results under a directory", "DONE WHEN: benchmark results under docs/bench committed, or stop after 5 turns", true},
			{"refusal as a noun", "DONE WHEN: refusal policy doc file exists and is committed, or stop after 5 turns", true},
			{"clean as an adjective", "DONE WHEN: new tests and a clean push to main, or stop after 5 turns", true},
			{"no test failures", "DONE WHEN: `go test ./...` shows no test failures, or stop after 5 turns", false},
			{"live refusal journaled", "DONE WHEN: the live-session refusal-then-pass journaled with profiles, or stop after 5 turns", false},
			{"failing test then passing", "DONE WHEN: a failing test is committed and then the test suite passes, or stop after 5 turns", false},
			{"no DONE WHEN section", "the report file exists, committed and pushed, bead mk-1 closed, or stop after 5 turns", false},
			{"reviewed by a cross-lab reviewer", "DONE WHEN: the plan is reviewed by a cross-lab reviewer, or stop after 10 turns", false},
			{"reviewed by the producer", "DONE WHEN: the plan is reviewed, committed and pushed, or stop after 10 turns", true},
			{"not reviewed by an independent reviewer", "DONE WHEN: plan committed, not reviewed by an independent reviewer, or stop after 10 turns", true},
			{"labelled DONE WHEN", "OUTCOME: every page renders. DONE WHEN (judged by the evaluator): report file exists, committed and pushed, or stop after 5 turns", true},
			{"producer-only judge", "Docs \u2014 better docs. OUTCOME: the docs are better. WHO'S WAITING: nobody. DONE WHEN: the report is written and reviewed, or stop after 10 turns.", true},
			{"command judge", "Docs \u2014 fewer broken links. OUTCOME: 12 broken links today, 0 after. DONE WHEN: `make linkcheck` exits 0, or stop after 10 turns.", false},
			{"sections absent", "all Go tests exit 0 and bead mk-1 closed, or stop after 20 turns", false},
			{"reviewed by a cross-lab reviewer, full goal", "Plan \u2014 a reviewed plan. OUTCOME: at least 3 risks named. DONE WHEN: the plan is reviewed by a cross-lab reviewer, or stop after 10 turns.", false},
			{"comparison bar with a command judge", "Perf \u2014 faster start. OUTCOME: startup is faster than 2s. DONE WHEN: `go test ./...` exits 0.", false},
			{"ABOUT in the outcome with a command judge", "X \u2014 y. OUTCOME: we learn ABOUT cost and cut it by 20%. DONE WHEN: `go test` exits 0.", false},
			{"not checked by an independent reviewer", "DONE WHEN: report committed, not checked by an independent reviewer, or stop after 5 turns", true},
			{"not judged by an external reviewer", "DONE WHEN: report committed, not judged by an external reviewer, or stop after 5 turns", true},
			{"never verified by a cross-lab reviewer", "DONE WHEN: report committed, never verified by a cross-lab reviewer, or stop after 5 turns", true},
			{"reviewed by another pass of the producer", "DONE WHEN: report reviewed by another pass of myself and committed, or stop after 5 turns", true},
			{"a final pass is an attempt", "DONE WHEN: a final pass over the docs, committed and pushed, or stop after 5 turns", true},
			{"verified by a second reviewer", "DONE WHEN: report committed and verified by a second reviewer, or stop after 5 turns", false},
			{"a pass from CI", "DONE WHEN: the suite gets a pass from CI, committed, or stop after 5 turns", false},
			{"reviewed by a human", "DONE WHEN: report committed and reviewed by a human, or stop after 5 turns", false},
			{"not reviewed by a human", "DONE WHEN: report committed, not reviewed by a human, or stop after 5 turns", true},
			{"labelled OUT on its own line", "DONE WHEN: report committed.\nOUT (scope): independent review passed", true},
			{"reviewed in a final pass", "DONE WHEN: report reviewed in a final pass and committed, or stop after 5 turns", true},
			{"a pass on CI", "DONE WHEN: the suite gets a pass on CI, committed, or stop after 5 turns", false},
			{"reviewed by an independent engineer", "DONE WHEN: report committed and reviewed by an independent engineer, or stop after 5 turns", false},
			{"reviewed by another engineer", "DONE WHEN: report committed and reviewed by another engineer, or stop after 5 turns", false},
			{"reviewed by an independent pass of the producer", "DONE WHEN: report committed and reviewed by an independent pass of my own, or stop after 5 turns", true},
			{"checked by a different run of the same agent", "DONE WHEN: report committed and checked by a different run of the same agent, or stop after 5 turns", true},
			{"an independent run returned", "DONE WHEN: report committed and an independent run returned a pass, or stop after 5 turns", false},
			{"one more pass is an attempt", "DONE WHEN: one more pass over the docs, committed, or stop after 5 turns", true},
			{"give the docs a pass is an attempt", "DONE WHEN: give the docs a pass and commit the report, or stop after 5 turns", true},
			{"returns a pass summary", "DONE WHEN: the tool returns a pass summary that is committed, or stop after 5 turns", true},
			{"returns a pass/fail report", "DONE WHEN: the tool returns a pass/fail report file that exists and is committed, or stop after 5 turns", true},
			{"reports a pass", "DONE WHEN: the test suite reports a pass, committed, or stop after 5 turns", false},
			{"independent final pass of the producer", "DONE WHEN: report committed and reviewed by an independent final pass of my own, or stop after 5 turns", true},
			{"different invocation of the same agent", "DONE WHEN: report committed and checked by a different invocation of the same agent, or stop after 5 turns", true},
			{"verified by an independent run of the tests", "DONE WHEN: report committed and verified by an independent run of the regression tests, or stop after 5 turns", false},
			{"different run by the same agent", "DONE WHEN: report committed and checked by a different run by the same agent, or stop after 5 turns", true},
			{"different invocation of this agent", "DONE WHEN: report committed and checked by a different invocation of this agent, or stop after 5 turns", true},
			{"my self-review reports a pass", "DONE WHEN: report committed and my self-review reports a pass, or stop after 5 turns", true},
			{"a pass from my own review", "DONE WHEN: the report gets a pass from my own review, or stop after 5 turns", true},
			{"reports a pass and a commit", "DONE WHEN: the test suite reports a pass and the report is committed, or stop after 5 turns", false},
			{"tests pass beside my own review", "DONE WHEN: tests pass and my own review is attached, or stop after 5 turns", false},
			{"our cross-lab review passed", "DONE WHEN: report committed and our cross-lab review passed, or stop after 5 turns", false},
			{"independent run of this suite", "DONE WHEN: report committed and verified by an independent run of this suite, or stop after 5 turns", false},
			{"the patch and its review", "DONE WHEN: the patch and its review are approved, or stop after 5 turns", false},
			{"self-review approved by an independent reviewer", "DONE WHEN: my self-review is approved by an independent reviewer and the report is committed, or stop after 5 turns", false},
			{"tests pass as documented in my own review", "DONE WHEN: tests pass as documented in my own review and the report is committed, or stop after 5 turns", false},
			{"my self-review and sign-off are approved", "DONE WHEN: my self-review and sign-off are approved and the report is committed, or stop after 5 turns", true},
			{"independent run of our regression tests", "DONE WHEN: report committed and verified by an independent run of our regression tests, or stop after 5 turns", false},
			{"a different run of my own", "DONE WHEN: report committed and checked by a different run of my own, or stop after 5 turns", true},
			{"approved in my self-review", "DONE WHEN: the report is approved in my self-review and committed, or stop after 5 turns", true},
			{"my self-review and tests pass", "DONE WHEN: my self-review and tests pass, or stop after 5 turns", false},
			{"my self-review approved by me", "DONE WHEN: my self-review is approved by me, committed, or stop after 5 turns", true},
			{"CI is green", "DONE WHEN: committed and CI is green, or stop after 5 turns", false},
			{"the build is green in CI", "DONE WHEN: the build is green in CI, or stop after 5 turns", false},
			{"my self-review came back clean", "DONE WHEN: my self-review came back clean and the report is committed, or stop after 5 turns", true},
			{"my self-review passes by inspection", "DONE WHEN: my self-review passes by inspection and the report is committed, or stop after 5 turns", true},
			{"my self-review approved by my own agent", "DONE WHEN: my self-review is approved by my own agent and the report is committed, or stop after 5 turns", true},
			{"tests passed in my own review", "DONE WHEN: tests passed in my own review and the report is committed, or stop after 5 turns", false},
			{"my self-review was checked and approved", "DONE WHEN: my self-review was checked and approved, committed, or stop after 5 turns", true},
			{"my self-review passes and an independent reviewer approved it", "DONE WHEN: my self-review passes and an independent reviewer approved it, or stop after 5 turns", false},
			{"my self-review is checked by CI", "DONE WHEN: my self-review is checked by CI, committed, or stop after 5 turns", false},
			{"a cross-lab review of my self-review passed", "DONE WHEN: a cross-lab review of my self-review passed, or stop after 5 turns", false},
			{"the report passed my own review", "DONE WHEN: the report passed my own review, committed, or stop after 5 turns", true},
			{"an independent reviewer passed my self-review", "DONE WHEN: an independent reviewer passed my self-review, committed, or stop after 5 turns", false},
			{"my self-review of the docs passed", "DONE WHEN: my self-review of the docs passed, committed, or stop after 5 turns", true},
			{"my self-review (attached) passes", "DONE WHEN: my self-review (attached) passes, committed, or stop after 5 turns", true},
			{"my self-review is done and approved", "DONE WHEN: my self-review is done and approved, committed, or stop after 5 turns", true},
			{"green after a package path", "DONE WHEN: the final pass of go test ./... is green, or stop after 5 turns", false},
			{"labelled OUT as a bullet", "DONE WHEN: report committed\n- OUT (scope): independent review passed", true},
			{"labelled OUT after a sentence", "OUTCOME: every page renders. DONE WHEN: committed and pushed, or stop after 5 turns. OUT (scope): independent review passed", true},
			{"labelled PREMISE before a command judge", "X \u2014 y. OUTCOME: docs are clearer. PREMISE (C3 and above): churn is 40% in week 1. DONE WHEN: `go test` exits 0.", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := has(LintCondition(tc.text), "self-graded"); got != tc.want {
					t.Errorf("self-graded = %v, want %v: %v", got, tc.want, LintCondition(tc.text))
				}
			})
		}
	})

	t.Run("no bar", func(t *testing.T) {
		cases := []struct {
			name string
			text string
			want bool
		}{
			{"vague outcome", "OUTCOME: the importer is better and cleaner. DONE WHEN: tests pass, or stop after 5 turns", true},
			{"number", "OUTCOME: imports finish in 3 seconds. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"universal", "OUTCOME: every export round-trips. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"comparison", "OUTCOME: imports are faster than the old path. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"identical", "OUTCOME: output is identical to the reference. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"lower-case gate stays in outcome", "OUTCOME: the gate opens for every user. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"bar only after the outcome", "OUTCOME: the importer is better. GATE 1 — who decides: is 3 seconds enough? DONE WHEN: tests pass, or stop after 5 turns", true},
			{"bar only in the premise", "OUTCOME: the importer is better. PREMISE: 3 of 5 slow imports are I/O bound; profile them. DONE WHEN: tests pass, or stop after 5 turns", true},
			{"upper-case word that is not a header", "OUTCOME: the GATE API answers every request. DONE WHEN: tests pass, or stop after 5 turns", false},
			{"curly apostrophe header ends the outcome", "OUTCOME: onboarding is better. WHO\u2019S WAITING: all customers. DONE WHEN: tests pass, or stop after 5 turns", true},
			{"digit inside an identifier", "OUTCOME: improve OAuth2 onboarding. DONE WHEN: tests pass, or stop after 5 turns", true},
			{"no OUTCOME header", "tests pass, or stop after 5 turns", false},
			{"producer-only judge and vague outcome", "Docs \u2014 better docs. OUTCOME: the docs are better. WHO'S WAITING: nobody. DONE WHEN: the report is written and reviewed, or stop after 10 turns.", true},
			{"counts before and after", "Docs \u2014 fewer broken links. OUTCOME: 12 broken links today, 0 after. DONE WHEN: `make linkcheck` exits 0, or stop after 10 turns.", false},
			{"at least a count", "Plan \u2014 a reviewed plan. OUTCOME: at least 3 risks named. DONE WHEN: the plan is reviewed by a cross-lab reviewer, or stop after 10 turns.", false},
			{"faster than a number", "Perf \u2014 faster start. OUTCOME: startup is faster than 2s. DONE WHEN: `go test ./...` exits 0.", false},
			{"sections absent", "all Go tests exit 0 and bead mk-1 closed, or stop after 20 turns", false},
			{"bar only in a labelled premise", "X \u2014 y. OUTCOME: docs are clearer. PREMISE (C3 and above): churn is 40% in week 1. DONE WHEN: `go test` exits 0.", true},
			{"ABOUT is not the OUT header", "X \u2014 y. OUTCOME: we learn ABOUT cost and cut it by 20%. DONE WHEN: `go test` exits 0.", false},
			{"labelled OUTCOME", "OUTCOME (the change): the importer is better. DONE WHEN: tests pass, or stop after 5 turns", true},
			{"labelled OUT on its own line", "OUTCOME: docs are clearer.\nOUT (scope): all translations.\nDONE WHEN: tests pass", true},
			{"labelled OUT as a bullet", "OUTCOME: docs are clearer\n- OUT (scope): all translations\nDONE WHEN: tests pass", true},
			{"OUT with a parenthetical in prose", "OUTCOME: explain SQL OUT (parameter): every procedure has documented output. DONE WHEN: tests pass, or stop after 5 turns", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if got := has(LintCondition(tc.text), "no bar"); got != tc.want {
					t.Errorf("no bar = %v, want %v: %v", got, tc.want, LintCondition(tc.text))
				}
			})
		}
	})
}
