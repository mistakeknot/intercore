package cost

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestComputeStats_Empty(t *testing.T) {
	s := computeStats(nil, 0, 0)
	if s.Count != 0 || s.P50 != 0 || s.Mean != 0 || s.Total != 0 {
		t.Errorf("expected zero stats for empty input, got %+v", s)
	}
}

func TestComputeStats_Single(t *testing.T) {
	s := computeStats([]int64{1000}, 600, 400)
	if s.Count != 1 {
		t.Errorf("count = %d, want 1", s.Count)
	}
	if s.P50 != 1000 || s.P90 != 1000 || s.P95 != 1000 {
		t.Errorf("percentiles wrong for single value: p50=%d p90=%d p95=%d", s.P50, s.P90, s.P95)
	}
	if s.Mean != 1000 {
		t.Errorf("mean = %d, want 1000", s.Mean)
	}
	if s.Total != 1000 {
		t.Errorf("total = %d, want 1000", s.Total)
	}
	if s.InputTotal != 600 || s.OutputTotal != 400 {
		t.Errorf("input/output = %d/%d, want 600/400", s.InputTotal, s.OutputTotal)
	}
}

func TestComputeStats_Multiple(t *testing.T) {
	// 10 values: sorted = [100, 200, 300, 400, 500, 600, 700, 800, 900, 1000]
	values := []int64{500, 300, 100, 900, 700, 200, 400, 1000, 600, 800}
	s := computeStats(values, 3300, 2200)

	if s.Count != 10 {
		t.Errorf("count = %d, want 10", s.Count)
	}
	// p50 = sorted[10*50/100] = sorted[5] = 600
	if s.P50 != 600 {
		t.Errorf("p50 = %d, want 600", s.P50)
	}
	// p90 = sorted[min(10*90/100, 9)] = sorted[9] = 1000
	if s.P90 != 1000 {
		t.Errorf("p90 = %d, want 1000", s.P90)
	}
	// p95 = sorted[min(10*95/100, 9)] = sorted[9] = 1000
	if s.P95 != 1000 {
		t.Errorf("p95 = %d, want 1000", s.P95)
	}
	// mean = 5500/10 = 550
	if s.Mean != 550 {
		t.Errorf("mean = %d, want 550", s.Mean)
	}
	if s.Total != 5500 {
		t.Errorf("total = %d, want 5500", s.Total)
	}
}

func TestComputeStats_TwoValues(t *testing.T) {
	// Edge case: only 2 values
	values := []int64{100, 200}
	s := computeStats(values, 150, 150)

	if s.Count != 2 {
		t.Errorf("count = %d, want 2", s.Count)
	}
	// p50 = sorted[2*50/100] = sorted[1] = 200
	if s.P50 != 200 {
		t.Errorf("p50 = %d, want 200", s.P50)
	}
	// p90 = sorted[min(2*90/100, 1)] = sorted[1] = 200
	if s.P90 != 200 {
		t.Errorf("p90 = %d, want 200", s.P90)
	}
	if s.Mean != 150 {
		t.Errorf("mean = %d, want 150", s.Mean)
	}
}

func TestFormatText_NoData(t *testing.T) {
	r := &BaselineResult{
		Period:        Period{Days: 30, Start: "2026-01-27", End: "2026-02-26"},
		LandedChanges: 5,
		Source:        "shipped_beads",
		Stats:         TokenStats{}, // no correlated data
	}

	out := FormatText(r)
	if out == "" {
		t.Error("expected non-empty output")
	}
	if !contains(out, "no correlated data") {
		t.Errorf("expected 'no correlated data' message, got:\n%s", out)
	}
	if !contains(out, "5") {
		t.Errorf("expected landed change count, got:\n%s", out)
	}
}

func TestFormatText_WithData(t *testing.T) {
	r := &BaselineResult{
		Period:        Period{Days: 30, Start: "2026-01-27", End: "2026-02-26"},
		LandedChanges: 3,
		Source:        "landed_changes",
		Stats: TokenStats{
			P50: 50000, P90: 90000, P95: 95000, Mean: 60000,
			Total: 180000, InputTotal: 100000, OutputTotal: 80000, Count: 3,
		},
	}

	out := FormatText(r)
	if !contains(out, "50.0k") {
		t.Errorf("expected formatted p50, got:\n%s", out)
	}
	if !contains(out, "Landed changes: 3") {
		t.Errorf("expected landed change count, got:\n%s", out)
	}
}

func TestFormatText_WithPhaseBreakdown(t *testing.T) {
	r := &BaselineResult{
		Period:        Period{Days: 30, Start: "2026-01-27", End: "2026-02-26"},
		LandedChanges: 2,
		Source:        "shipped_beads",
		Stats:         TokenStats{P50: 100000, Count: 2, Total: 200000},
		ByPhase: map[string]TokenStats{
			"executing":  {Total: 150000, Count: 10},
			"brainstorm": {Total: 50000, Count: 4},
		},
	}

	out := FormatText(r)
	if !contains(out, "By Phase") {
		t.Errorf("expected phase breakdown header, got:\n%s", out)
	}
	if !contains(out, "executing") || !contains(out, "brainstorm") {
		t.Errorf("expected phase names, got:\n%s", out)
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{50000, "50.0k"},
		{142300, "142.3k"},
		{1000000, "1.0M"},
		{30054970, "30.1M"},
	}
	for _, tt := range tests {
		got := formatTokens(tt.in)
		if got != tt.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestQueryInterstat_MockScript(t *testing.T) {
	// Create a mock cost-query.sh that returns canned JSON
	tmp := t.TempDir()
	script := filepath.Join(tmp, "cost-query.sh")

	mockJSON := `[{"bead_id":"iv-abc","runs":5,"tokens":50000,"input_tokens":30000,"output_tokens":20000},{"bead_id":"iv-def","runs":3,"tokens":30000,"input_tokens":18000,"output_tokens":12000}]`

	err := os.WriteFile(script, []byte("#!/bin/bash\ncat <<'EOF'\n"+mockJSON+"\nEOF\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := queryInterstat(context.Background(), script, "by-bead")
	if err != nil {
		t.Fatalf("queryInterstat: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].BeadID != "iv-abc" || rows[0].Tokens != 50000 {
		t.Errorf("row 0: %+v", rows[0])
	}
	if rows[1].BeadID != "iv-def" || rows[1].Tokens != 30000 {
		t.Errorf("row 1: %+v", rows[1])
	}
}

func TestQueryInterstat_EmptyOutput(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "cost-query.sh")
	err := os.WriteFile(script, []byte("#!/bin/bash\necho '[]'\n"), 0755)
	if err != nil {
		t.Fatal(err)
	}

	rows, err := queryInterstat(context.Background(), script, "by-bead")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("expected 0 rows for empty output, got %d", len(rows))
	}
}

func TestSafeDivide(t *testing.T) {
	if got := safeDivide(100, 0); got != 0 {
		t.Errorf("safeDivide(100,0) = %d, want 0", got)
	}
	if got := safeDivide(100, 3); got != 33 {
		t.Errorf("safeDivide(100,3) = %d, want 33", got)
	}
}

func TestSortedKeys(t *testing.T) {
	m := map[string]int{"c": 3, "a": 1, "b": 2}
	got := sortedKeys(m)
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		in   string
		want [3]int
	}{
		{"0.2.6", [3]int{0, 2, 6}},
		{"0.10.0", [3]int{0, 10, 0}},
		{"1.0.0", [3]int{1, 0, 0}},
		{"abc", [3]int{}},
		{"1.2", [3]int{}},
		{"", [3]int{}},
	}
	for _, tt := range tests {
		got := parseVersion(tt.in)
		if got != tt.want {
			t.Errorf("parseVersion(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFindInPluginCache_SemverSort(t *testing.T) {
	// Create mock plugin cache with versions that sort differently lexicographically vs semver
	tmp := t.TempDir()
	for _, v := range []string{"0.6.83", "0.10.0", "0.2.6"} {
		dir := filepath.Join(tmp, v, "scripts")
		os.MkdirAll(dir, 0755)
		os.WriteFile(filepath.Join(dir, "cost-query.sh"), []byte("#!/bin/bash\necho '[]'"), 0755)
	}

	got := findInPluginCache(tmp)
	want := filepath.Join(tmp, "0.10.0", "scripts", "cost-query.sh")
	if got != want {
		t.Errorf("findInPluginCache picked %q, want %q (highest semver)", got, want)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
