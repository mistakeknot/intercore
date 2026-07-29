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
