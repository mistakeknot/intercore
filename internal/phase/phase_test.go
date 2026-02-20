package phase

import "testing"

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
		{"valid default chain", `["brainstorm","brainstorm-reviewed","strategized","planned","executing","review","polish","reflect","done"]`, DefaultPhaseChain, false},
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

func TestChainPhaseIndex(t *testing.T) {
	chain := []string{"a", "b", "c", "d"}
	tests := []struct {
		phase string
		want  int
	}{
		{"a", 0}, {"b", 1}, {"d", 3}, {"x", -1},
	}
	for _, tt := range tests {
		if got := ChainPhaseIndex(chain, tt.phase); got != tt.want {
			t.Errorf("ChainPhaseIndex(%q) = %d, want %d", tt.phase, got, tt.want)
		}
	}
}

func TestChainPhasesBetween(t *testing.T) {
	chain := []string{"a", "b", "c", "d", "e"}
	tests := []struct {
		from, to string
		want     []string
	}{
		{"a", "d", []string{"b", "c", "d"}},
		{"b", "d", []string{"c", "d"}},
		{"a", "b", []string{"b"}},
		{"d", "a", nil}, // backward = nil
		{"a", "a", nil}, // same = nil
		{"x", "d", nil}, // not found = nil
	}
	for _, tt := range tests {
		got := ChainPhasesBetween(chain, tt.from, tt.to)
		if len(got) != len(tt.want) {
			t.Errorf("ChainPhasesBetween(%q, %q) = %v, want %v", tt.from, tt.to, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("ChainPhasesBetween(%q, %q)[%d] = %q, want %q", tt.from, tt.to, i, got[i], tt.want[i])
			}
		}
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
