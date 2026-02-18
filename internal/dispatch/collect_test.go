package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseVerdictFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.verdict")

	content := `--- VERDICT ---
STATUS: pass
FILES: 3 changed
FINDINGS: 2 (P0: 0, P1: 1, P2: 1)
SUMMARY: All critical checks passed, minor style issues found
---`
	os.WriteFile(path, []byte(content), 0644)

	status, summary, err := parseVerdictFile(path)
	if err != nil {
		t.Fatalf("parseVerdictFile: %v", err)
	}
	if status != "pass" {
		t.Errorf("status = %q, want %q", status, "pass")
	}
	if summary != "All critical checks passed, minor style issues found" {
		t.Errorf("summary = %q", summary)
	}
}

func TestParseVerdictFile_Fail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.verdict")

	content := `--- VERDICT ---
STATUS: fail
FILES: 1 changed
FINDINGS: 3 (P0: 1, P1: 2, P2: 0)
SUMMARY: Critical security issue found
---`
	os.WriteFile(path, []byte(content), 0644)

	status, summary, err := parseVerdictFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if status != "fail" {
		t.Errorf("status = %q, want %q", status, "fail")
	}
	if summary != "Critical security issue found" {
		t.Errorf("summary = %q", summary)
	}
}

func TestParseVerdictFile_Missing(t *testing.T) {
	_, _, err := parseVerdictFile("/nonexistent/file.verdict")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestParseSummaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.summary")

	content := `Dispatch: code-review
Duration: 2m 15s
Turns: 5 | Commands: 3 | Messages: 8
Tokens: 12345 in / 67890 out`
	os.WriteFile(path, []byte(content), 0644)

	inTok, outTok, err := parseSummaryFile(path)
	if err != nil {
		t.Fatalf("parseSummaryFile: %v", err)
	}
	if inTok != 12345 {
		t.Errorf("inTokens = %d, want 12345", inTok)
	}
	if outTok != 67890 {
		t.Errorf("outTokens = %d, want 67890", outTok)
	}
}

func TestParseTokenLine(t *testing.T) {
	tests := []struct {
		line     string
		inTok    int
		outTok   int
		wantErr  bool
	}{
		{"Tokens: 1000 in / 2000 out", 1000, 2000, false},
		{"Tokens: 0 in / 0 out", 0, 0, false},
		{"Tokens: 999999 in / 1234567 out", 999999, 1234567, false},
		{"bad format", 0, 0, true},
	}
	for _, tt := range tests {
		inTok, outTok, err := parseTokenLine(tt.line)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parseTokenLine(%q): expected error", tt.line)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseTokenLine(%q): %v", tt.line, err)
			continue
		}
		if inTok != tt.inTok || outTok != tt.outTok {
			t.Errorf("parseTokenLine(%q) = (%d, %d), want (%d, %d)", tt.line, inTok, outTok, tt.inTok, tt.outTok)
		}
	}
}

func TestPoll_DeadProcess(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create a dispatch with a PID that doesn't exist
	d := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/test"}
	id, _ := store.Create(ctx, d)

	// Use PID 1 which exists but is init — we'll use a fake non-existent PID instead
	fakePID := 999999999
	store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
		"pid":        fakePID,
		"started_at": time.Now().Unix(),
	})

	// Poll should detect the dead process and collect
	result, err := Poll(ctx, store, id)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if !result.IsTerminal() {
		t.Errorf("expected terminal status after polling dead process, got %q", result.Status)
	}
}

func TestPoll_AlreadyTerminal(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	d := &Dispatch{AgentType: "codex", ProjectDir: "/tmp/test"}
	id, _ := store.Create(ctx, d)
	store.UpdateStatus(ctx, id, StatusCompleted, UpdateFields{"exit_code": 0})

	result, err := Poll(ctx, store, id)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", result.Status, StatusCompleted)
	}
}

func TestCollect_WithVerdictAndSummary(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()

	outputFile := filepath.Join(dir, "output.md")
	verdictFile := outputFile + ".verdict"
	summaryFile := outputFile + ".summary"

	// Write mock output files
	os.WriteFile(outputFile, []byte("agent output"), 0644)
	os.WriteFile(verdictFile, []byte("--- VERDICT ---\nSTATUS: pass\nFILES: 1 changed\nFINDINGS: 0\nSUMMARY: Clean run\n---\n"), 0644)
	os.WriteFile(summaryFile, []byte("Dispatch: test\nDuration: 1m 0s\nTurns: 3 | Commands: 2 | Messages: 5\nTokens: 500 in / 1000 out\n"), 0644)

	d := &Dispatch{
		AgentType:   "codex",
		ProjectDir:  "/tmp/test",
		OutputFile:  &outputFile,
		VerdictFile: &verdictFile,
	}
	id, _ := store.Create(ctx, d)
	store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{"pid": 999999999})

	// Collect
	if err := Collect(ctx, store, id); err != nil {
		t.Fatalf("Collect: %v", err)
	}

	got, _ := store.Get(ctx, id)
	if got.Status != StatusCompleted {
		t.Errorf("Status = %q, want %q", got.Status, StatusCompleted)
	}
	if got.VerdictStatus == nil || *got.VerdictStatus != "pass" {
		t.Errorf("VerdictStatus = %v, want %q", got.VerdictStatus, "pass")
	}
	if got.VerdictSummary == nil || *got.VerdictSummary != "Clean run" {
		t.Errorf("VerdictSummary = %v", got.VerdictSummary)
	}
	if got.InputTokens != 500 {
		t.Errorf("InputTokens = %d, want 500", got.InputTokens)
	}
	if got.OutputTokens != 1000 {
		t.Errorf("OutputTokens = %d, want 1000", got.OutputTokens)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Errorf("ExitCode = %v, want 0", got.ExitCode)
	}
}

func TestInferExitCode(t *testing.T) {
	tests := []struct {
		verdict string
		want    int
	}{
		{"pass", 0},
		{"warn", 0},
		{"fail", 1},
	}
	for _, tt := range tests {
		fields := UpdateFields{"verdict_status": tt.verdict}
		got := inferExitCode(&Dispatch{}, fields)
		if got != tt.want {
			t.Errorf("inferExitCode(%q) = %d, want %d", tt.verdict, got, tt.want)
		}
	}

	// No verdict
	got := inferExitCode(&Dispatch{}, UpdateFields{})
	if got != -1 {
		t.Errorf("inferExitCode(no verdict) = %d, want -1", got)
	}
}

func TestReadStateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	content := `{"name":"review","workdir":"/tmp/project","started":1700000000,"activity":"thinking","turns":3,"commands":2,"messages":5}`
	os.WriteFile(path, []byte(content), 0644)

	state, err := readStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if state.Name != "review" {
		t.Errorf("Name = %q, want %q", state.Name, "review")
	}
	if state.Activity != "thinking" {
		t.Errorf("Activity = %q, want %q", state.Activity, "thinking")
	}
	if state.Turns != 3 {
		t.Errorf("Turns = %d, want 3", state.Turns)
	}
}
