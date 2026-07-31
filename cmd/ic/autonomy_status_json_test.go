package main

import (
	"encoding/json"
	"testing"

	"github.com/mistakeknot/intercore/pkg/autonomy"
)

// TestAutonomyStatusJSONKeepsResolutionAtTopLevel pins the wire contract that
// scripts/gen-autonomy-position.py reads. The generator looks up `level`,
// `name`, `declared` and `derives_auto_advance` at the top of the object; if
// embedding Resolution ever became a named field, those keys would move to
// `resolution.level` and the generator would silently render a canon block
// claiming the level was unavailable.
func TestAutonomyStatusJSONKeepsResolutionAtTopLevel(t *testing.T) {
	out, err := json.Marshal(autonomyStatusJSON{
		Resolution: autonomy.Resolution{
			Level: 3, Name: "human sets policy, agent executes",
			AutoAdvance: true, Declared: true, Source: "state",
		},
		Ops: autonomy.Rulings(),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"level", "name", "declared", "derives_auto_advance", "source"} {
		if _, ok := got[key]; !ok {
			t.Errorf("key %q is not at the top level; the generator reads it there", key)
		}
	}

	ops, ok := got["ops"].([]any)
	if !ok {
		t.Fatalf("ops is %T, want a list", got["ops"])
	}
	if len(ops) != len(autonomy.Rulings()) {
		t.Errorf("ops has %d entries, want %d", len(ops), len(autonomy.Rulings()))
	}
}

// TestAutonomyStatusJSONDistinguishesEmptyFromAbsent guards the generator's
// vacuity check. A kernel with no floors must emit `"ops": []`, never omit the
// key — the generator treats a missing key as "this ic is too old to ask" and
// refuses to rewrite, which would be wrong for a kernel that genuinely has none.
func TestAutonomyStatusJSONDistinguishesEmptyFromAbsent(t *testing.T) {
	out, err := json.Marshal(autonomyStatusJSON{Ops: []autonomy.OpRuling{}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got["ops"]; !ok {
		t.Fatal("empty ops list was omitted from the payload; the generator cannot " +
			"then tell 'no floors' from 'ic too old to report floors'")
	}
}
