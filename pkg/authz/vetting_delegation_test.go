package authz

import (
	"encoding/json"
	"testing"

	"github.com/mistakeknot/intercore/pkg/autonomy"
)

func TestDelegationVettingShape(t *testing.T) {
	res := autonomy.Resolution{
		Level: 1, Name: "human approves at phase gates",
		Declared: true, Source: "state",
	}
	got := DelegationVetting(res, "git-push-main", true)

	sub, ok := got[DelegationVettingKey].(map[string]interface{})
	if !ok {
		t.Fatalf("vetting[%q] is %T, want a map", DelegationVettingKey, got[DelegationVettingKey])
	}
	if sub["level"] != 1 || sub["declared"] != true || sub["capped"] != true {
		t.Errorf("unexpected subtree: %#v", sub)
	}
	// min_for_auto comes from the kernel table, not the caller — a refusal must
	// record the floor that actually applied, not one supplied alongside it.
	if sub["min_for_auto"] != PushAutoFloorForTest() {
		t.Errorf("min_for_auto = %v, want %d", sub["min_for_auto"], PushAutoFloorForTest())
	}
}

// TestDelegationVettingRecordsUncappedDecisionsToo guards the denominator. A
// store holding only interventions can say how often the ceiling fired but not
// how often it could have and did not, and "never withheld anything" is only
// meaningful against a count of decisions that carried the evidence at all.
func TestDelegationVettingRecordsUncappedDecisionsToo(t *testing.T) {
	got := DelegationVetting(autonomy.Resolution{Level: 5}, "bead-close", false)
	sub := got[DelegationVettingKey].(map[string]interface{})
	if sub["capped"] != false {
		t.Error("an uncapped decision must still be recorded, marked false")
	}
	if sub["min_for_auto"] != 0 {
		t.Errorf("min_for_auto = %v, want 0 for an unfloored op", sub["min_for_auto"])
	}
}

// TestMarshalVettingMatchesStoredBytes is the invariant that keeps signatures
// verifiable: `vetting` is inside the signed payload, so the bytes signed and
// the bytes stored must be identical. Two independent json.Marshal calls happen
// to agree today because Go sorts map keys — this pins it as a contract rather
// than a coincidence.
func TestMarshalVettingMatchesStoredBytes(t *testing.T) {
	v := DelegationVetting(autonomy.Resolution{Level: 2, Name: "n", Source: "s"}, "git-push-main", true)
	a, err := MarshalVetting(v)
	if err != nil {
		t.Fatalf("MarshalVetting: %v", err)
	}
	b, err := MarshalVetting(v)
	if err != nil {
		t.Fatalf("MarshalVetting: %v", err)
	}
	if a != b {
		t.Fatalf("two marshals of one map differ:\n  %s\n  %s", a, b)
	}
	var round map[string]interface{}
	if err := json.Unmarshal([]byte(a), &round); err != nil {
		t.Fatalf("stored bytes are not valid JSON (the column CHECKs json_valid): %v", err)
	}
}

func TestMarshalVettingNilIsCanonicalNull(t *testing.T) {
	// The signing spec encodes SQL NULL as an empty string at that field's
	// position; returning "{}" or "null" here would change every unvetted row's
	// payload and invalidate existing signatures.
	got, err := MarshalVetting(nil)
	if err != nil {
		t.Fatalf("MarshalVetting(nil): %v", err)
	}
	if got != "" {
		t.Errorf("MarshalVetting(nil) = %q, want \"\"", got)
	}
}

func PushAutoFloorForTest() int { return autonomy.MinLevelForAuto("git-push-main") }
