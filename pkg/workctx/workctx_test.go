package workctx

import (
	"encoding/json"
	"testing"
)

func TestIsZero(t *testing.T) {
	tests := []struct {
		name string
		wc   WorkContext
		want bool
	}{
		{"empty", WorkContext{}, true},
		{"bead only", WorkContext{BeadID: "Demarch-abc"}, false},
		{"session only", WorkContext{SessionID: "sess-123"}, false},
		{"all set", WorkContext{BeadID: "b", RunID: "r", SessionID: "s"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.wc.IsZero(); got != tt.want {
				t.Errorf("IsZero() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJSONRoundTrip(t *testing.T) {
	original := WorkContext{
		BeadID:    "Demarch-og7m",
		RunID:     "run-456",
		SessionID: "sess-789",
	}
	data, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded WorkContext
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if decoded != original {
		t.Errorf("round-trip mismatch: got %+v, want %+v", decoded, original)
	}
}

func TestJSONUnknownFields(t *testing.T) {
	// Forward-compat: unknown fields are silently ignored.
	input := `{"bead_id":"b","run_id":"r","session_id":"s","tenant_id":"t"}`
	var wc WorkContext
	if err := json.Unmarshal([]byte(input), &wc); err != nil {
		t.Fatalf("Unmarshal with unknown field: %v", err)
	}
	if wc.BeadID != "b" || wc.RunID != "r" || wc.SessionID != "s" {
		t.Errorf("unexpected values: %+v", wc)
	}
}

func TestJSONFieldNames(t *testing.T) {
	wc := WorkContext{BeadID: "b", RunID: "r", SessionID: "s"}
	data, _ := json.Marshal(wc)
	s := string(data)
	for _, key := range []string{`"bead_id"`, `"run_id"`, `"session_id"`} {
		if !contains(s, key) {
			t.Errorf("JSON missing key %s: %s", key, s)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
