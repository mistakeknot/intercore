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

// --- Chain-based function tests ---

func TestParsePhaseChain(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    []string
		wantErr bool
	}{
		{"valid 3-phase", `["a","b","c"]`, []string{"a", "b", "c"}, false},
		{"valid default chain", `["brainstorm","brainstorm-reviewed","strategized","planned","executing","review","polish","done"]`, DefaultPhaseChain, false},
		{"empty array", `[]`, nil, true},
		{"single phase", `["a"]`, nil, true},
		{"invalid json", `not json`, nil, true},
		{"duplicates", `["a","b","a"]`, nil, true},
		{"empty string", ``, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParsePhaseChain(tt.json)
			if (err != nil) != tt.wantErr {
				t.Errorf("ParsePhaseChain() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if len(got) != len(tt.want) {
					t.Errorf("ParsePhaseChain() len = %d, want %d", len(got), len(tt.want))
					return
				}
				for i := range got {
					if got[i] != tt.want[i] {
						t.Errorf("ParsePhaseChain()[%d] = %q, want %q", i, got[i], tt.want[i])
					}
				}
			}
		})
	}
}

func TestChainNextPhase(t *testing.T) {
	chain := []string{"draft", "review", "publish", "done"}
	tests := []struct {
		current string
		want    string
		wantErr bool
	}{
		{"draft", "review", false},
		{"review", "publish", false},
		{"publish", "done", false},
		{"done", "", true},
		{"nonexistent", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.current, func(t *testing.T) {
			got, err := ChainNextPhase(chain, tt.current)
			if (err != nil) != tt.wantErr {
				t.Errorf("ChainNextPhase() error = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("ChainNextPhase() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestChainIsValidTransition(t *testing.T) {
	chain := []string{"a", "b", "c", "d"}

	if !ChainIsValidTransition(chain, "a", "b") {
		t.Error("a→b should be valid")
	}
	if !ChainIsValidTransition(chain, "a", "c") {
		t.Error("a→c should be valid (skip)")
	}
	if !ChainIsValidTransition(chain, "a", "d") {
		t.Error("a→d should be valid (multi-skip)")
	}
	if ChainIsValidTransition(chain, "b", "a") {
		t.Error("b→a should be invalid (backward)")
	}
	if ChainIsValidTransition(chain, "a", "nonexistent") {
		t.Error("a→nonexistent should be invalid")
	}
	if ChainIsValidTransition(chain, "a", "a") {
		t.Error("a→a should be invalid (self)")
	}
}

func TestChainIsTerminal(t *testing.T) {
	chain := []string{"a", "b", "ship"}
	if !ChainIsTerminal(chain, "ship") {
		t.Error("ship should be terminal")
	}
	if ChainIsTerminal(chain, "a") {
		t.Error("a should not be terminal")
	}
	if ChainIsTerminal(nil, "ship") {
		t.Error("nil chain: nothing is terminal")
	}
}

func TestChainContains(t *testing.T) {
	chain := []string{"a", "b", "c"}
	if !ChainContains(chain, "b") {
		t.Error("b should be in chain")
	}
	if ChainContains(chain, "z") {
		t.Error("z should not be in chain")
	}
}

func TestResolveChain(t *testing.T) {
	r := &Run{Phases: []string{"x", "y"}}
	if got := ResolveChain(r); len(got) != 2 || got[0] != "x" {
		t.Errorf("ResolveChain with phases = %v, want [x y]", got)
	}
	r2 := &Run{}
	if got := ResolveChain(r2); len(got) != len(DefaultPhaseChain) {
		t.Errorf("ResolveChain without phases len = %d, want %d", len(got), len(DefaultPhaseChain))
	}
}
