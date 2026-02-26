package publish

import "testing"

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input               string
		major, minor, patch int
		pre                 string
		wantErr             bool
	}{
		{"1.2.3", 1, 2, 3, "", false},
		{"0.0.0", 0, 0, 0, "", false},
		{"10.20.30", 10, 20, 30, "", false},
		{"1.0.0-alpha", 1, 0, 0, "alpha", false},
		{"2.1.0-rc.1", 2, 1, 0, "rc.1", false},
		{"", 0, 0, 0, "", true},
		{"1.2", 0, 0, 0, "", true},
		{"1.2.3.4", 0, 0, 0, "", true},
		{"abc", 0, 0, 0, "", true},
		{"v1.2.3", 0, 0, 0, "", true}, // no 'v' prefix
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			maj, min, pat, pre, err := ParseVersion(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if maj != tc.major || min != tc.minor || pat != tc.patch || pre != tc.pre {
				t.Errorf("got %d.%d.%d-%s, want %d.%d.%d-%s",
					maj, min, pat, pre, tc.major, tc.minor, tc.patch, tc.pre)
			}
		})
	}
}

func TestFormatVersion(t *testing.T) {
	tests := []struct {
		major, minor, patch int
		pre                 string
		want                string
	}{
		{1, 2, 3, "", "1.2.3"},
		{0, 0, 0, "", "0.0.0"},
		{1, 0, 0, "alpha", "1.0.0-alpha"},
	}

	for _, tc := range tests {
		got := FormatVersion(tc.major, tc.minor, tc.patch, tc.pre)
		if got != tc.want {
			t.Errorf("FormatVersion(%d, %d, %d, %q) = %q, want %q",
				tc.major, tc.minor, tc.patch, tc.pre, got, tc.want)
		}
	}
}

func TestBumpVersion(t *testing.T) {
	tests := []struct {
		current string
		mode    BumpMode
		want    string
		wantErr bool
	}{
		{"1.2.3", BumpPatch, "1.2.4", false},
		{"0.0.0", BumpPatch, "0.0.1", false},
		{"1.2.9", BumpPatch, "1.2.10", false},
		{"1.2.3", BumpMinor, "1.3.0", false},
		{"1.9.5", BumpMinor, "1.10.0", false},
		{"1.2.3-alpha", BumpPatch, "1.2.4", false}, // pre-release stripped
		{"invalid", BumpPatch, "", true},
	}

	for _, tc := range tests {
		t.Run(tc.current+"_"+tc.want, func(t *testing.T) {
			got, err := BumpVersion(tc.current, tc.mode)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("BumpVersion(%q, %d) = %q, want %q", tc.current, tc.mode, got, tc.want)
			}
		})
	}
}

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.0.1", "1.0.0", 1},
		{"1.0.0", "1.1.0", -1},
		{"1.1.0", "1.0.0", 1},
		{"1.0.0", "2.0.0", -1},
		{"2.0.0", "1.0.0", 1},
		{"1.0.0-alpha", "1.0.0", -1}, // pre-release < release
		{"1.0.0", "1.0.0-alpha", 1},  // release > pre-release
		{"1.0.0-alpha", "1.0.0-beta", -1},
		{"0.1.0", "0.2.0", -1},
	}

	for _, tc := range tests {
		t.Run(tc.a+"_vs_"+tc.b, func(t *testing.T) {
			got := CompareVersions(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("CompareVersions(%q, %q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

func TestParseFormatRoundTrip(t *testing.T) {
	versions := []string{"0.0.0", "1.2.3", "10.20.30", "1.0.0-alpha", "2.1.0-rc.1"}
	for _, v := range versions {
		maj, min, pat, pre, err := ParseVersion(v)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", v, err)
		}
		got := FormatVersion(maj, min, pat, pre)
		if got != v {
			t.Errorf("round-trip: %q → %q", v, got)
		}
	}
}
