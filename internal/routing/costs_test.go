package routing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveCost(t *testing.T) {
	mc := ModelCost{InputPerMTok: 3.0, OutputPerMTok: 15.0}
	got := mc.EffectiveCost()
	// 3.0 + 15.0*15.0 = 3.0 + 225.0 = 228.0
	want := 228.0
	if got != want {
		t.Errorf("EffectiveCost() = %v, want %v", got, want)
	}
}

func TestEffectiveCostZero(t *testing.T) {
	mc := ModelCost{}
	got := mc.EffectiveCost()
	if got != 0.0 {
		t.Errorf("EffectiveCost() = %v, want 0", got)
	}
}

func TestLoadCostTable(t *testing.T) {
	dir := t.TempDir()
	yaml := `
models:
  haiku:
    input_per_mtok: 0.25
    output_per_mtok: 1.25
  sonnet:
    input_per_mtok: 3.0
    output_per_mtok: 15.0
  opus:
    input_per_mtok: 15.0
    output_per_mtok: 75.0
`
	path := filepath.Join(dir, "costs.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0644); err != nil {
		t.Fatal(err)
	}

	ct, err := LoadCostTable(path)
	if err != nil {
		t.Fatalf("LoadCostTable: %v", err)
	}

	if len(ct.Models) != 3 {
		t.Errorf("got %d models, want 3", len(ct.Models))
	}
	haiku := ct.Models["haiku"]
	if haiku.InputPerMTok != 0.25 {
		t.Errorf("haiku input = %v, want 0.25", haiku.InputPerMTok)
	}
	if haiku.OutputPerMTok != 1.25 {
		t.Errorf("haiku output = %v, want 1.25", haiku.OutputPerMTok)
	}
}

func TestLoadCostTableMissing(t *testing.T) {
	_, err := LoadCostTable("/nonexistent/costs.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadCostTableEmptyModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "costs.yaml")
	if err := os.WriteFile(path, []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	ct, err := LoadCostTable(path)
	if err != nil {
		t.Fatalf("LoadCostTable: %v", err)
	}
	if ct.Models == nil {
		t.Error("Models map should be initialized even when empty")
	}
}

func TestCheapestCapable(t *testing.T) {
	ct := &CostTable{
		Models: map[string]ModelCost{
			"haiku":  {InputPerMTok: 0.25, OutputPerMTok: 1.25},
			"sonnet": {InputPerMTok: 3.0, OutputPerMTok: 15.0},
			"opus":   {InputPerMTok: 15.0, OutputPerMTok: 75.0},
		},
	}

	// All models capable → haiku is cheapest
	got := ct.CheapestCapable([]string{"opus", "sonnet", "haiku"})
	if got != "haiku" {
		t.Errorf("all capable: got %q, want haiku", got)
	}

	// Only expensive models
	got = ct.CheapestCapable([]string{"opus", "sonnet"})
	if got != "sonnet" {
		t.Errorf("opus+sonnet: got %q, want sonnet", got)
	}

	// Single model
	got = ct.CheapestCapable([]string{"opus"})
	if got != "opus" {
		t.Errorf("single: got %q, want opus", got)
	}

	// Empty list
	got = ct.CheapestCapable([]string{})
	if got != "" {
		t.Errorf("empty: got %q, want empty", got)
	}

	// Unknown models (not in cost table) are skipped
	got = ct.CheapestCapable([]string{"gpt4", "unknown"})
	if got != "" {
		t.Errorf("unknown only: got %q, want empty", got)
	}

	// Mix of known and unknown
	got = ct.CheapestCapable([]string{"gpt4", "sonnet", "unknown"})
	if got != "sonnet" {
		t.Errorf("mixed: got %q, want sonnet", got)
	}
}

func TestModelTierParsing(t *testing.T) {
	tests := []struct {
		input string
		want  ModelTier
	}{
		{"haiku", TierHaiku},
		{"sonnet", TierSonnet},
		{"opus", TierOpus},
		{"unknown", TierUnknown},
		{"", TierUnknown},
	}
	for _, tt := range tests {
		got := ParseModelTier(tt.input)
		if got != tt.want {
			t.Errorf("ParseModelTier(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestModelTierString(t *testing.T) {
	tests := []struct {
		tier ModelTier
		want string
	}{
		{TierHaiku, "haiku"},
		{TierSonnet, "sonnet"},
		{TierOpus, "opus"},
		{TierUnknown, "unknown"},
	}
	for _, tt := range tests {
		got := tt.tier.String()
		if got != tt.want {
			t.Errorf("ModelTier(%d).String() = %q, want %q", tt.tier, got, tt.want)
		}
	}
}
