package routing

import (
	"reflect"
	"testing"
)

func TestEffortOrderBackendCapabilities(t *testing.T) {
	for _, tc := range []struct {
		backend, identity string
		want              []string
	}{
		{"codex", "gpt-6-astra", []string{"low", "medium", "high", "xhigh"}},
		{"codex", "gpt-6-sol", []string{"low", "medium", "high", "xhigh"}},
		{"claude", "claude-opus-5", []string{"medium", "high"}},
		{"claude", "claude-sonnet-5", []string{"medium", "high"}},
		{"kimi", "kimi-code/k3", []string{"high"}},
		{"main", "gpt-6-astra", nil}, {"claude", "gpt-6-astra", nil},
		{"codex", "claude-opus-5", nil}, {"validation-kimi", "kimi-code/k3", nil},
		{"kimi", "kimi-code/k30", nil}, {"kimi", "kimi-code/k3-preview", nil},
		{"codex", "gpt-5.6-sol", nil}, {"codex", "", nil},
		{"unknown", "gpt-6-astra", nil}, {"claude", "claude-fable-5-1", nil},
	} {
		t.Run(tc.backend+"/"+tc.identity, func(t *testing.T) {
			got, ok := effortOrder(DispatchProfile{Backend: tc.backend, ModelIdentity: tc.identity, Model: "gpt-6-astra"})
			if ok != (tc.want != nil) || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("order = %v, %v; want %v", got, ok, tc.want)
			}
		})
	}
}

func TestApplyEffortFloorBackendOrder(t *testing.T) {
	for _, tc := range []struct {
		backend, identity, from, floor, want string
		fail                                 bool
	}{
		{"codex", "gpt-6-astra", "medium", "high", "high", false},
		{"codex", "gpt-6-astra", "xhigh", "high", "xhigh", false},
		{"claude", "claude-opus-5", "medium", "high", "high", false},
		{"claude", "claude-opus-5", "high", "low", "high", false},
		{"kimi", "kimi-code/k3", "high", "medium", "high", false},
		{"kimi", "kimi-code/k3", "high", "high", "high", false},
		{"kimi", "kimi-code/k3", "high", "xhigh", "high", true},
		{"claude", "claude-opus-5", "medium", "xhigh", "medium", true},
		{"claude", "claude-opus-5", "low", "high", "low", true},
		{"main", "gpt-6-astra", "medium", "high", "medium", true},
	} {
		t.Run(tc.backend+"/"+tc.from+"/"+tc.floor, func(t *testing.T) {
			p := DispatchProfile{Backend: tc.backend, ModelIdentity: tc.identity, ReasoningEffort: tc.from}
			got, reasons, err := applyEffortFloor(p, []string{"reason", "reason"}, map[string]string{"reason": tc.floor})
			if (err != nil) != tc.fail || got.ReasoningEffort != tc.want {
				t.Fatalf("got %+v, %v; want %s, fail=%v", got, err, tc.want, tc.fail)
			}
			wantReasons := []string(nil)
			if tc.from != tc.want {
				wantReasons = []string{"reason"}
			}
			if !reflect.DeepEqual(reasons, wantReasons) {
				t.Fatalf("reasons = %v", reasons)
			}
			if p.ReasoningEffort != tc.from {
				t.Fatal("mutated input")
			}
		})
	}
	t.Run("unsatisfiable-floor-is-atomic", func(t *testing.T) {
		p := DispatchProfile{Backend: "claude", ModelIdentity: "claude-opus-5", ReasoningEffort: "medium"}
		got, reasons, err := applyEffortFloor(p, []string{"first", "second"}, map[string]string{"first": "high", "second": "xhigh"})
		if err == nil || !reflect.DeepEqual(got, p) || len(reasons) != 0 {
			t.Fatalf("partial result = %+v, %v, %v", got, reasons, err)
		}
	})
	t.Run("unknown-adapter-without-matching-floor", func(t *testing.T) {
		p := DispatchProfile{Backend: "main", ModelIdentity: "gpt-6-astra", ReasoningEffort: "high"}
		got, reasons, err := applyEffortFloor(p, []string{"unmatched"}, map[string]string{"first": "high"})
		if err != nil || !reflect.DeepEqual(got, p) || len(reasons) != 0 {
			t.Fatalf("inactive floors changed profile: %+v, %v, %v", got, reasons, err)
		}
	})
}
