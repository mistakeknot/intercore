package coordination

import "testing"

func TestPatternsOverlap(t *testing.T) {
	tests := []struct {
		a, b    string
		overlap bool
	}{
		{"*.go", "*.go", true},
		{"*.go", "*.rs", false},
		{"foo.go", "foo.go", true},
		{"foo.go", "bar.go", false},
		{"*.go", "main.go", true},
		{"internal/*.go", "internal/http.go", true},
		{"internal/*.go", "pkg/*.go", false},
		{"src/[a-z]*.go", "src/main.go", true},
		{"src/[A-Z]*.go", "src/main.go", false},
		{"*", "anything", true},
		{"src/a.go", "src/b.go", false},
	}
	for _, tt := range tests {
		got, err := PatternsOverlap(tt.a, tt.b)
		if err != nil {
			t.Errorf("PatternsOverlap(%q, %q) error: %v", tt.a, tt.b, err)
			continue
		}
		if got != tt.overlap {
			t.Errorf("PatternsOverlap(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.overlap)
		}
	}
}

func TestValidateComplexity(t *testing.T) {
	if err := ValidateComplexity("internal/http/*.go"); err != nil {
		t.Fatalf("normal pattern rejected: %v", err)
	}

	// Overly complex pattern with many wildcards
	complex := "?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?/?"
	if err := ValidateComplexity(complex); err == nil {
		t.Fatal("expected complexity error for pattern with many wildcards")
	}
}

func TestPatternsOverlap_DifferentSegmentCount(t *testing.T) {
	got, err := PatternsOverlap("a/b/c", "a/b")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Error("different segment count should not overlap")
	}
}
