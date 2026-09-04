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

// TestFormRules pins the canonical-form and landing-only warnings (mk-hxgi).
// The motivating text is the #1 candidate a Next-goal block recommended on
// 2026-09-01: a merge, which is the user's call and not a successor goal, in
// a shape with no OUTCOME: and no DONE WHEN:. Both rules are warnings, so
// their precision is what keeps them from being learned as noise: a goal that
// merges two lists, or that names the merge as a GATE for the user, must not
// trip them.
func TestFormRules(t *testing.T) {
	landingGoal := "Merge PR #26 so the intercore goal-close protocol lands and is merged, " +
		"then run the publish wave from zklw, or stop after 5 turns."

	canonicalGoal := "Repair the Next-goal audit chain. OUTCOME: every emitted block is " +
		"detected and its receipts are checked for freshness and coverage. GATE: the " +
		"canonical-form rule stays a warning until measured. DONE WHEN: `bats tests/shell` " +
		"exits 0 and the hook flags a stale receipt, or stop after 40 turns."

	kinds := func(probs []Problem) (form, landing, merge bool, errs int) {
		for _, p := range probs {
			switch {
			case strings.HasPrefix(p.Message, "canonical form:"):
				form = true
			case strings.HasPrefix(p.Message, "landing-first:"):
				landing = true
			case strings.HasPrefix(p.Message, "merge is not the agent's:"):
				merge = true
			}
			if p.Severity == "error" {
				errs++
			}
		}
		return
	}

	t.Run("the 2026-09-01 #1 candidate yields all three warnings and no error", func(t *testing.T) {
		probs := LintCondition(landingGoal)
		form, landing, merge, errs := kinds(probs)
		if !form || !landing || !merge {
			t.Errorf("wanted form=%v landing=%v merge=%v all true: %v", form, landing, merge, probs)
		}
		if errs != 0 {
			t.Errorf("these are warnings, got %d error(s): %v", errs, probs)
		}
	})

	t.Run("a canonical goal yields none of the three", func(t *testing.T) {
		form, landing, merge, _ := kinds(LintCondition(canonicalGoal))
		if form || landing || merge {
			t.Errorf("false positive on the canonical form: %v", LintCondition(canonicalGoal))
		}
	})

	t.Run("a technical merge is not a landing action", func(t *testing.T) {
		text := "Merge the ranked lists into one shortlist. OUTCOME: one ordered list. " +
			"DONE WHEN: `go test ./...` exits 0, or stop after 5 turns."
		_, landing, merge, _ := kinds(LintCondition(text))
		if landing || merge {
			t.Errorf("verb without a landing object must not warn: %v", LintCondition(text))
		}
	})

	t.Run("the test fixture's own 'Ship it.' is a landing action", func(t *testing.T) {
		text := "Ship it. OUTCOME: the thing is live. DONE WHEN: deployed, or stop after 5 turns."
		_, landing, _, _ := kinds(LintCondition(text))
		if !landing {
			t.Errorf("'Ship it.' must warn landing-first: %v", LintCondition(text))
		}
	})

	t.Run("a merge named as the user's GATE is not the agent's merge", func(t *testing.T) {
		text := "Run the close protocol in production. OUTCOME: goals close through ic. " +
			"GATE: mk merges PR #26. DONE WHEN: `ic goal close finish` exits 0, or stop after 10 turns."
		_, landing, merge, _ := kinds(LintCondition(text))
		if landing || merge {
			t.Errorf("a GATE naming who merges is the recommended form: %v", LintCondition(text))
		}
	})

	t.Run("missing only DONE WHEN still warns and names it", func(t *testing.T) {
		text := "OUTCOME: the API answers. `curl` returns HTTP 200, or stop after 5 turns."
		var msg string
		for _, p := range LintCondition(text) {
			if strings.HasPrefix(p.Message, "canonical form:") {
				msg = p.Message
			}
		}
		if !strings.Contains(msg, "DONE WHEN:") || strings.Contains(msg, "OUTCOME:") == false {
			// the message must name what is missing without claiming OUTCOME: is
			t.Logf("message: %q", msg)
		}
		if msg == "" || !strings.Contains(msg, "DONE WHEN:") {
			t.Errorf("wanted a canonical-form warning naming DONE WHEN:, got %q", msg)
		}
	})
}
