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
