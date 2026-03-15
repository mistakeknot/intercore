package truncate

import (
	"strings"
	"testing"
)

func TestHeadTail_Short(t *testing.T) {
	s := "hello world"
	got := HeadTail(s, 100)
	if got != s {
		t.Errorf("short string should be unchanged, got %q", got)
	}
}

func TestHeadTail_Exact(t *testing.T) {
	s := strings.Repeat("x", 100)
	got := HeadTail(s, 100)
	if got != s {
		t.Errorf("exact-length string should be unchanged")
	}
}

func TestHeadTail_Truncates(t *testing.T) {
	s := strings.Repeat("A", 400) + strings.Repeat("B", 600)
	got := HeadTail(s, 500)

	// Should have head (40% of 500 = 200 chars)
	if !strings.HasPrefix(got, strings.Repeat("A", 200)) {
		t.Error("head portion should be first 200 A's")
	}

	// Should have truncation notice
	if !strings.Contains(got, "OUTPUT TRUNCATED") {
		t.Error("should contain truncation notice")
	}

	// Should have tail (60% of 500 = 300 chars)
	if !strings.HasSuffix(got, strings.Repeat("B", 300)) {
		t.Error("tail portion should be last 300 B's")
	}

	// Omitted count: 1000 total - 200 head - 300 tail = 500
	if !strings.Contains(got, "500 chars omitted") {
		t.Error("should report 500 chars omitted")
	}
}

func TestHeadTail_ZeroMax(t *testing.T) {
	s := "hello"
	got := HeadTail(s, 0)
	if got != s {
		t.Errorf("zero maxLen should return unchanged, got %q", got)
	}
}

func TestHeadTailLines_Short(t *testing.T) {
	s := "line1\nline2\nline3\n"
	got := HeadTailLines(s, 10)
	if got != s {
		t.Errorf("short input should be unchanged")
	}
}

func TestHeadTailLines_Truncates(t *testing.T) {
	var lines []string
	for i := 0; i < 100; i++ {
		lines = append(lines, "line\n")
	}
	s := strings.Join(lines, "")

	got := HeadTailLines(s, 10)

	// Head: 40% of 10 = 4 lines
	headCount := strings.Count(got[:strings.Index(got, "...")], "line\n")
	if headCount != 4 {
		t.Errorf("expected 4 head lines, got %d", headCount)
	}

	// Should mention omitted count
	if !strings.Contains(got, "90 lines omitted") {
		t.Errorf("should mention 90 lines omitted, got: %s", got)
	}
}

func TestSplitLines(t *testing.T) {
	lines := splitLines("a\nb\nc")
	if len(lines) != 3 {
		t.Errorf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "a\n" || lines[1] != "b\n" || lines[2] != "c" {
		t.Errorf("unexpected split: %v", lines)
	}
}
