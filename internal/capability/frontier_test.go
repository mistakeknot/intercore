package capability

import (
	"testing"
)

func TestFrontierWindowOpen(t *testing.T) {
	cases := []struct {
		name string
		val  string
		set  bool
		want bool
	}{
		{"unset is closed (fail-closed)", "", false, false},
		{"empty is closed", "", true, false},
		{"explicit 1 opens", "1", true, true},
		{"0 is closed", "0", true, false},
		{"true is closed (only 1 opens)", "true", true, false},
		{"whitespace is closed", " 1 ", true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(frontierEnvVar, tc.val)
			} else {
				// t.Setenv can't unset; ensure it's cleared for this subtest.
				t.Setenv(frontierEnvVar, "")
			}
			if got := FrontierWindowOpen(); got != tc.want {
				t.Errorf("FrontierWindowOpen() with %q = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}
