package phase

import "testing"

func TestNextPhase_FullChain(t *testing.T) {
	expected := []struct{ from, to string }{
		{PhaseBrainstorm, PhaseBrainstormReviewed},
		{PhaseBrainstormReviewed, PhaseStrategized},
		{PhaseStrategized, PhasePlanned},
		{PhasePlanned, PhaseExecuting},
		{PhaseExecuting, PhaseReview},
		{PhaseReview, PhasePolish},
		{PhasePolish, PhaseDone},
	}

	for _, tt := range expected {
		got, err := NextPhase(tt.from)
		if err != nil {
			t.Errorf("NextPhase(%q) error: %v", tt.from, err)
			continue
		}
		if got != tt.to {
			t.Errorf("NextPhase(%q) = %q, want %q", tt.from, got, tt.to)
		}
	}
}

func TestNextPhase_Done_ReturnsError(t *testing.T) {
	_, err := NextPhase(PhaseDone)
	if err != ErrNoTransition {
		t.Errorf("NextPhase(done) error = %v, want ErrNoTransition", err)
	}
}

func TestNextPhase_Unknown_ReturnsError(t *testing.T) {
	_, err := NextPhase("nonexistent")
	if err != ErrNoTransition {
		t.Errorf("NextPhase(nonexistent) error = %v, want ErrNoTransition", err)
	}
}

func TestShouldSkip_Complexity1(t *testing.T) {
	tests := []struct {
		phase string
		skip  bool
	}{
		{PhaseBrainstorm, false},
		{PhaseBrainstormReviewed, true},
		{PhaseStrategized, true},
		{PhasePlanned, false},
		{PhaseExecuting, false},
		{PhaseReview, true},
		{PhasePolish, true},
		{PhaseDone, false},
	}

	for _, tt := range tests {
		got := ShouldSkip(tt.phase, 1)
		if got != tt.skip {
			t.Errorf("ShouldSkip(%q, 1) = %v, want %v", tt.phase, got, tt.skip)
		}
	}
}

func TestShouldSkip_Complexity2(t *testing.T) {
	tests := []struct {
		phase string
		skip  bool
	}{
		{PhaseBrainstorm, false},
		{PhaseBrainstormReviewed, false},
		{PhaseStrategized, true},
		{PhasePlanned, false},
		{PhaseExecuting, false},
		{PhaseReview, true},
		{PhasePolish, true},
		{PhaseDone, false},
	}

	for _, tt := range tests {
		got := ShouldSkip(tt.phase, 2)
		if got != tt.skip {
			t.Errorf("ShouldSkip(%q, 2) = %v, want %v", tt.phase, got, tt.skip)
		}
	}
}

func TestShouldSkip_Complexity3_NoSkips(t *testing.T) {
	for _, p := range allPhases {
		if ShouldSkip(p, 3) {
			t.Errorf("ShouldSkip(%q, 3) = true, want false", p)
		}
	}
}

func TestShouldSkip_Complexity5_NoSkips(t *testing.T) {
	for _, p := range allPhases {
		if ShouldSkip(p, 5) {
			t.Errorf("ShouldSkip(%q, 5) = true, want false", p)
		}
	}
}

func TestNextRequiredPhase_Complexity1(t *testing.T) {
	// brainstorm → planned (skips brainstorm-reviewed + strategized)
	got := NextRequiredPhase(PhaseBrainstorm, 1, false)
	if got != PhasePlanned {
		t.Errorf("NextRequiredPhase(brainstorm, 1) = %q, want %q", got, PhasePlanned)
	}

	// planned → executing
	got = NextRequiredPhase(PhasePlanned, 1, false)
	if got != PhaseExecuting {
		t.Errorf("NextRequiredPhase(planned, 1) = %q, want %q", got, PhaseExecuting)
	}

	// executing → done (skips review + polish)
	got = NextRequiredPhase(PhaseExecuting, 1, false)
	if got != PhaseDone {
		t.Errorf("NextRequiredPhase(executing, 1) = %q, want %q", got, PhaseDone)
	}
}

func TestNextRequiredPhase_Complexity2(t *testing.T) {
	// brainstorm → brainstorm-reviewed
	got := NextRequiredPhase(PhaseBrainstorm, 2, false)
	if got != PhaseBrainstormReviewed {
		t.Errorf("NextRequiredPhase(brainstorm, 2) = %q, want %q", got, PhaseBrainstormReviewed)
	}

	// brainstorm-reviewed → planned (skips strategized)
	got = NextRequiredPhase(PhaseBrainstormReviewed, 2, false)
	if got != PhasePlanned {
		t.Errorf("NextRequiredPhase(brainstorm-reviewed, 2) = %q, want %q", got, PhasePlanned)
	}

	// executing → done (skips review + polish)
	got = NextRequiredPhase(PhaseExecuting, 2, false)
	if got != PhaseDone {
		t.Errorf("NextRequiredPhase(executing, 2) = %q, want %q", got, PhaseDone)
	}
}

func TestNextRequiredPhase_Complexity3_FullChain(t *testing.T) {
	expected := []struct{ from, to string }{
		{PhaseBrainstorm, PhaseBrainstormReviewed},
		{PhaseBrainstormReviewed, PhaseStrategized},
		{PhaseStrategized, PhasePlanned},
		{PhasePlanned, PhaseExecuting},
		{PhaseExecuting, PhaseReview},
		{PhaseReview, PhasePolish},
		{PhasePolish, PhaseDone},
	}

	for _, tt := range expected {
		got := NextRequiredPhase(tt.from, 3, false)
		if got != tt.to {
			t.Errorf("NextRequiredPhase(%q, 3) = %q, want %q", tt.from, got, tt.to)
		}
	}
}

func TestNextRequiredPhase_ForceFull_OverridesComplexity(t *testing.T) {
	// Even at complexity 1, force_full should step through every phase
	got := NextRequiredPhase(PhaseBrainstorm, 1, true)
	if got != PhaseBrainstormReviewed {
		t.Errorf("NextRequiredPhase(brainstorm, 1, force) = %q, want %q", got, PhaseBrainstormReviewed)
	}
}

func TestIsValidTransition(t *testing.T) {
	valid := []struct{ from, to string }{
		{PhaseBrainstorm, PhaseBrainstormReviewed},
		{PhaseBrainstorm, PhasePlanned},            // skip
		{PhaseBrainstormReviewed, PhaseStrategized},
		{PhaseBrainstormReviewed, PhasePlanned},     // skip
		{PhaseExecuting, PhaseDone},                 // skip
		{PhaseReview, PhaseDone},                    // skip
	}

	for _, tt := range valid {
		if !IsValidTransition(tt.from, tt.to) {
			t.Errorf("IsValidTransition(%q, %q) = false, want true", tt.from, tt.to)
		}
	}

	invalid := []struct{ from, to string }{
		{PhaseDone, PhaseBrainstorm},   // backward
		{PhaseExecuting, PhasePlanned}, // backward
		{PhaseBrainstorm, PhaseDone},   // too far
		{"nonexistent", PhaseDone},     // unknown
	}

	for _, tt := range invalid {
		if IsValidTransition(tt.from, tt.to) {
			t.Errorf("IsValidTransition(%q, %q) = true, want false", tt.from, tt.to)
		}
	}
}

func TestIsTerminalPhase(t *testing.T) {
	if !IsTerminalPhase(PhaseDone) {
		t.Error("IsTerminalPhase(done) = false, want true")
	}
	if IsTerminalPhase(PhaseBrainstorm) {
		t.Error("IsTerminalPhase(brainstorm) = true, want false")
	}
}

func TestIsTerminalStatus(t *testing.T) {
	for _, s := range []string{StatusCompleted, StatusCancelled, StatusFailed} {
		if !IsTerminalStatus(s) {
			t.Errorf("IsTerminalStatus(%q) = false, want true", s)
		}
	}
	if IsTerminalStatus(StatusActive) {
		t.Error("IsTerminalStatus(active) = true, want false")
	}
}
