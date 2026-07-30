package autonomy

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type fakeGetter struct {
	payload json.RawMessage
	err     error
}

func (f fakeGetter) Get(ctx context.Context, key, scopeID string) (json.RawMessage, error) {
	if key != StateKey {
		return nil, errors.New("unexpected key: " + key)
	}
	if scopeID != Scope {
		return nil, errors.New("unexpected scope: " + scopeID)
	}
	return f.payload, f.err
}

func TestDerivesAutoAdvance(t *testing.T) {
	// The ladder's operative split: L0/L1 keep the human in the transition,
	// L2+ let the run advance and the human catch up afterward.
	for level, want := range map[int]bool{0: false, 1: false, 2: true, 3: true, 4: true, 5: true} {
		if got := DerivesAutoAdvance(level); got != want {
			t.Errorf("DerivesAutoAdvance(%d) = %t, want %t", level, got, want)
		}
	}
}

func TestParseLevel(t *testing.T) {
	ok := map[string]int{"0": 0, "2": 2, "5": 5, "L2": 2, "l3": 3, " 4 ": 4}
	for in, want := range ok {
		got, err := ParseLevel(in)
		if err != nil {
			t.Errorf("ParseLevel(%q) unexpected error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %d, want %d", in, got, want)
		}
	}

	// Out-of-range must be rejected at write time; if it were stored it would
	// resolve back to the default and look like it had been applied.
	for _, in := range []string{"-1", "6", "99", "", "two", "L9"} {
		if got, err := ParseLevel(in); err == nil {
			t.Errorf("ParseLevel(%q) = %d, want error", in, got)
		}
	}
}

func TestResolveUnsetFallsBackToDefault(t *testing.T) {
	res := Resolve(context.Background(), fakeGetter{err: errors.New("not found")})
	if res.Level != DefaultLevel {
		t.Errorf("level = %d, want %d", res.Level, DefaultLevel)
	}
	if res.Declared {
		t.Error("Declared = true, want false for an unset key")
	}
	// The pre-existing kernel behavior was AutoAdvance hardcoded true. An
	// upgrade that silently started pausing every run would be a regression.
	if !res.AutoAdvance {
		t.Error("AutoAdvance = false; unset must preserve the shipped default")
	}
}

func TestResolveNilGetter(t *testing.T) {
	res := Resolve(context.Background(), nil)
	if res.Level != DefaultLevel || res.Declared {
		t.Errorf("nil getter = %+v, want the undeclared default", res)
	}
}

func TestResolveDeclared(t *testing.T) {
	cases := map[string]struct {
		level       int
		autoAdvance bool
	}{
		"1":   {1, false},
		"2":   {2, true},
		"0":   {0, false},
		"5":   {5, true},
		`"3"`: {3, true}, // tolerate a quoted value
	}
	for payload, want := range cases {
		res := Resolve(context.Background(), fakeGetter{payload: json.RawMessage(payload)})
		if res.Level != want.level {
			t.Errorf("payload %s: level = %d, want %d", payload, res.Level, want.level)
		}
		if res.AutoAdvance != want.autoAdvance {
			t.Errorf("payload %s: autoAdvance = %t, want %t", payload, res.AutoAdvance, want.autoAdvance)
		}
		if !res.Declared {
			t.Errorf("payload %s: Declared = false, want true", payload)
		}
	}
}

func TestResolveInvalidStoredValueFallsBack(t *testing.T) {
	// A typo in a config row must not make the kernel unable to create runs.
	res := Resolve(context.Background(), fakeGetter{payload: json.RawMessage(`"banana"`)})
	if res.Level != DefaultLevel {
		t.Errorf("level = %d, want fallback %d", res.Level, DefaultLevel)
	}
	if res.Declared {
		t.Error("Declared = true, want false when the stored value is unusable")
	}
	if res.Source == ConfigKey {
		t.Errorf("Source = %q, want a fallback marker naming the bad value", res.Source)
	}
}

func TestNameCoversEveryRung(t *testing.T) {
	for l := MinLevel; l <= MaxLevel; l++ {
		if Name(l) == "unknown" {
			t.Errorf("Name(%d) = unknown; every rung needs a canonical meaning", l)
		}
	}
	if Name(MaxLevel+1) != "unknown" {
		t.Errorf("Name(%d) should be unknown", MaxLevel+1)
	}
}
