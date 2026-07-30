package main

import (
	"encoding/json"
	"testing"

	"github.com/mistakeknot/intercore/pkg/autonomy"
)

func decodeOverride(t *testing.T, merged string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(merged), &doc); err != nil {
		t.Fatalf("merge result is not JSON: %v (%s)", err, merged)
	}
	rec, ok := doc[autonomyOverrideKey].(map[string]any)
	if !ok {
		t.Fatalf("no %s object in %s", autonomyOverrideKey, merged)
	}
	return rec
}

func res(level int, declared bool) autonomy.Resolution {
	return autonomy.Resolution{
		Level:       level,
		Name:        autonomy.Name(level),
		AutoAdvance: autonomy.DerivesAutoAdvance(level),
		Declared:    declared,
		Source:      autonomy.ConfigKey,
	}
}

func TestMergeAutonomyOverride_RecordsBothSides(t *testing.T) {
	// L1 implies auto_advance=false; forcing true is the divergence.
	merged, err := mergeAutonomyOverride("", true, res(1, true))
	if err != nil {
		t.Fatalf("mergeAutonomyOverride: %v", err)
	}
	rec := decodeOverride(t, merged)

	if rec["auto_advance"] != true {
		t.Errorf("auto_advance = %v, want true", rec["auto_advance"])
	}
	if rec["implied_by_level"] != false {
		t.Errorf("implied_by_level = %v, want false", rec["implied_by_level"])
	}
	if rec["delegation_level"] != float64(1) {
		t.Errorf("delegation_level = %v, want 1", rec["delegation_level"])
	}
	if rec["delegation_declared"] != true {
		t.Errorf("delegation_declared = %v, want true", rec["delegation_declared"])
	}
	// Without a timestamp the record cannot be ordered against anything else
	// that happened to the run.
	if ts, ok := rec["recorded_at"].(float64); !ok || ts <= 0 {
		t.Errorf("recorded_at = %v, want a positive unix timestamp", rec["recorded_at"])
	}
}

func TestMergeAutonomyOverride_PreservesCallerKeys(t *testing.T) {
	merged, err := mergeAutonomyOverride(`{"bead_id":"mk-1","nested":{"a":1}}`, false, res(2, true))
	if err != nil {
		t.Fatalf("mergeAutonomyOverride: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(merged), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["bead_id"] != "mk-1" {
		t.Errorf("caller key bead_id lost: %s", merged)
	}
	if nested, ok := doc["nested"].(map[string]any); !ok || nested["a"] != float64(1) {
		t.Errorf("caller key nested lost: %s", merged)
	}
	decodeOverride(t, merged)
}

func TestMergeAutonomyOverride_KeyIsReserved(t *testing.T) {
	// A run must not be able to author its own divergence record.
	merged, err := mergeAutonomyOverride(
		`{"autonomy_override":{"delegation_level":5,"auto_advance":true}}`, false, res(2, true))
	if err != nil {
		t.Fatalf("mergeAutonomyOverride: %v", err)
	}
	rec := decodeOverride(t, merged)
	if rec["delegation_level"] != float64(2) {
		t.Errorf("caller-supplied delegation_level survived: %v", rec["delegation_level"])
	}
	if rec["auto_advance"] != false {
		t.Errorf("caller-supplied auto_advance survived: %v", rec["auto_advance"])
	}
}

func TestMergeAutonomyOverride_UndeclaredLevelIsRecordedAsSuch(t *testing.T) {
	merged, err := mergeAutonomyOverride("", false, autonomy.Resolve(t.Context(), nil))
	if err != nil {
		t.Fatalf("mergeAutonomyOverride: %v", err)
	}
	rec := decodeOverride(t, merged)
	if rec["delegation_declared"] != false {
		t.Error("an override against an undeclared default must say the level was never declared")
	}
	if src, _ := rec["delegation_source"].(string); src == "" {
		t.Error("delegation_source must record where the level came from")
	}
}

func TestMergeAutonomyOverride_RejectsNonObjectMerge(t *testing.T) {
	for _, bad := range []string{"[1,2]", `"str"`, "not json", "7"} {
		if _, err := mergeAutonomyOverride(bad, true, res(1, true)); err == nil {
			t.Errorf("mergeAutonomyOverride(%q) succeeded, want an error", bad)
		}
	}
}
