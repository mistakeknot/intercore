package routing

import "testing"

// TestNextRung locks the capability-ordering authority that escalation now
// consumes (fd-architecture F1: ordering lives here, not in dispatch). These
// cases mirror the historical dispatch behavior so the refactor is provably
// behavior-preserving; internal/dispatch's TestNextRungModelTwoStrikes is the
// consumer-side proof.
func TestNextRung(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		fableWindow bool
		wantModel   string
		wantOK      bool
	}{
		{"sonnet -> opus (window irrelevant)", "sonnet", false, "opus", true},
		{"opus -> fable (window open)", "opus", true, "fable", true},
		{"opus -> exhausted (window closed)", "opus", false, "", false},
		{"fable -> exhausted (already top)", "fable", false, "", false},
		{"fable -> exhausted even with window open", "fable", true, "", false},
		{"off-ladder id -> first rung (sonnet)", "codex-gpt-5.6", false, "sonnet", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fableWindow {
				t.Setenv("CLAVAIN_FABLE_AVAILABLE", "1")
			} else {
				t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")
			}
			got, ok := NextRung(tt.current)
			if ok != tt.wantOK {
				t.Fatalf("NextRung(%q) ok = %v, want %v", tt.current, ok, tt.wantOK)
			}
			if got != tt.wantModel {
				t.Errorf("NextRung(%q) = %q, want %q", tt.current, got, tt.wantModel)
			}
		})
	}
}
