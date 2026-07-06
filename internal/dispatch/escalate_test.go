package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/state"
)

// testStores builds a dispatch.Store and a state.Store backed by the same
// fully-migrated intercore DB (mirrors testStore(t) in dispatch_test.go, plus
// state_test.go's own DB setup for the state table).
func testStores(t *testing.T) (*Store, *state.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return New(d.SqlDB(), nil), state.New(d.SqlDB())
}

func TestNextRungModelTwoStrikes(t *testing.T) {
	policy := DefaultEscalationPolicy()

	tests := []struct {
		name          string
		currentModel  string
		strikesAtRung int
		fableWindow   bool
		wantModel     string
		wantOK        bool
	}{
		{"sonnet 0 strikes -> sonnet", "sonnet", 0, false, "sonnet", true},
		{"sonnet 1 strike -> sonnet (retry same)", "sonnet", 1, false, "sonnet", true},
		{"sonnet 2 strikes -> opus", "sonnet", 2, false, "opus", true},
		{"opus 2 strikes, fable window open -> fable", "opus", 2, true, "fable", true},
		{"opus 2 strikes, fable window closed -> exhausted", "opus", 2, false, "", false},
		{"fable 2 strikes -> exhausted", "fable", 2, false, "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fableWindow {
				t.Setenv("CLAVAIN_FABLE_AVAILABLE", "1")
			} else {
				t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")
			}
			got, ok := policy.nextRungModel(tt.currentModel, tt.strikesAtRung)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.wantModel {
				t.Errorf("model = %q, want %q", got, tt.wantModel)
			}
		})
	}
}

func TestRetryWithEscalationLadder(t *testing.T) {
	store, stStore := testStores(t)
	ctx := context.Background()
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")

	model := "sonnet"
	orig := &Dispatch{
		AgentType:  "codex",
		ProjectDir: t.TempDir(),
		Model:      &model,
	}
	origID, err := store.Create(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	chainKey := "chain-ladder-1"
	policy := DefaultEscalationPolicy()

	// First failure: strikes go from 0 -> 1, still same rung (sonnet).
	r1, err := RetryWithEscalation(ctx, store, stStore, origID, policy, chainKey, FailError, "boom1")
	if err != nil {
		t.Fatalf("RetryWithEscalation 1: %v", err)
	}
	if r1.Escalated {
		t.Errorf("attempt 1: Escalated = true, want false")
	}
	if r1.Model != "sonnet" {
		t.Errorf("attempt 1: Model = %q, want sonnet", r1.Model)
	}

	// Fail the first retry.
	store.UpdateStatus(ctx, r1.NewID, StatusFailed, UpdateFields{"exit_code": 1})

	// Second failure: strikes go from 1 -> 2, still same rung (sonnet) since
	// nextRungModel checks strikes BEFORE this failure is counted... actually
	// strikes are incremented before nextModel() is called, so this call sees
	// strikesAtRung=2 and escalates.
	r2, err := RetryWithEscalation(ctx, store, stStore, r1.NewID, policy, chainKey, FailError, "boom2")
	if err != nil {
		t.Fatalf("RetryWithEscalation 2: %v", err)
	}
	if !r2.Escalated {
		t.Errorf("attempt 2: Escalated = false, want true")
	}
	if r2.Model != "opus" {
		t.Errorf("attempt 2: Model = %q, want opus", r2.Model)
	}

	newDisp, err := store.Get(ctx, r2.NewID)
	if err != nil {
		t.Fatalf("Get new dispatch: %v", err)
	}
	if newDisp.Model == nil || *newDisp.Model != "opus" {
		t.Errorf("new dispatch Model = %v, want opus", newDisp.Model)
	}

	cs, err := LoadChainState(ctx, stStore, chainKey)
	if err != nil {
		t.Fatalf("LoadChainState: %v", err)
	}
	if cs.Escalations != 1 {
		t.Errorf("Escalations = %d, want 1", cs.Escalations)
	}
	if len(cs.Models) != 2 {
		t.Fatalf("Models = %v, want len 2", cs.Models)
	}
	if cs.Models[0] != "sonnet" || cs.Models[1] != "sonnet" {
		t.Errorf("Models = %v, want [sonnet sonnet] (both failures recorded at sonnet rung)", cs.Models)
	}
}

func TestChainSurvivesReTrigger(t *testing.T) {
	store, stStore := testStores(t)
	ctx := context.Background()
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")

	chainKey := "chain-survive-1"
	policy := DefaultEscalationPolicy()
	model := "sonnet"

	// Dispatch A: fail, escalate call #1 (strikes 0->1, same rung).
	dA := &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &model}
	idA, err := store.Create(ctx, dA)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, idA, StatusFailed, UpdateFields{"exit_code": 1})

	r1, err := RetryWithEscalation(ctx, store, stStore, idA, policy, chainKey, FailError, "fail-A")
	if err != nil {
		t.Fatalf("RetryWithEscalation (A): %v", err)
	}
	if r1.Escalated {
		t.Fatalf("first call should not escalate yet")
	}

	// Fresh root dispatch B: a brand new top-level Create with RetryCount=0 —
	// NOT part of the retry chain via ParentDispatchID — but same chainKey.
	dB := &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &model, RetryCount: 0}
	idB, err := store.Create(ctx, dB)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, idB, StatusFailed, UpdateFields{"exit_code": 1})

	// This is the SECOND failure recorded against chainKey overall (strikes
	// 1->2), even though dispatch B itself has RetryCount=0 — strikes must
	// continue rather than reset.
	r2, err := RetryWithEscalation(ctx, store, stStore, idB, policy, chainKey, FailError, "fail-B")
	if err != nil {
		t.Fatalf("RetryWithEscalation (B): %v", err)
	}
	if !r2.Escalated {
		t.Errorf("third total failure (2nd strike at rung) should escalate; Escalated = false")
	}
	if r2.Model != "opus" {
		t.Errorf("Model = %q, want opus", r2.Model)
	}
}

func TestLessonPromptCarriesFailures(t *testing.T) {
	store, stStore := testStores(t)
	ctx := context.Background()
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")

	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("# Original prompt\nDo the thing.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	verdictPath := filepath.Join(dir, "verdict.txt")
	if err := os.WriteFile(verdictPath, []byte("FAIL: criteria not met"), 0o644); err != nil {
		t.Fatal(err)
	}

	model := "sonnet"
	orig := &Dispatch{
		AgentType:   "codex",
		ProjectDir:  dir,
		Model:       &model,
		PromptFile:  &promptPath,
		VerdictFile: &verdictPath,
	}
	origID, err := store.Create(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	chainKey := "chain-lesson-1"
	policy := DefaultEscalationPolicy()

	result, err := RetryWithEscalation(ctx, store, stStore, origID, policy, chainKey, FailCriteria, "criteria X not satisfied")
	if err != nil {
		t.Fatalf("RetryWithEscalation: %v", err)
	}

	newDisp, err := store.Get(ctx, result.NewID)
	if err != nil {
		t.Fatalf("Get new dispatch: %v", err)
	}
	if newDisp.PromptFile == nil {
		t.Fatal("new dispatch PromptFile is nil")
	}
	if *newDisp.PromptFile == promptPath {
		t.Errorf("new dispatch PromptFile = orig path %q, want a different lesson-carrying file", promptPath)
	}

	content, err := os.ReadFile(*newDisp.PromptFile)
	if err != nil {
		t.Fatalf("read lesson prompt: %v", err)
	}
	if !strings.Contains(string(content), "Prior attempt lesson") {
		t.Errorf("lesson prompt missing 'Prior attempt lesson' section:\n%s", content)
	}
	if !strings.Contains(string(content), "criteria X not satisfied") {
		t.Errorf("lesson prompt missing failure detail:\n%s", content)
	}
}

func TestExhaustionWritesLessonChain(t *testing.T) {
	store, stStore := testStores(t)
	ctx := context.Background()
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "0")

	dir := t.TempDir()
	model := "fable"
	orig := &Dispatch{AgentType: "codex", ProjectDir: dir, Model: &model}
	origID, err := store.Create(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	chainKey := "chain-exhaust-1"
	policy := DefaultEscalationPolicy()

	// Already at the top rung ("fable") with 2 strikes -> immediately exhausted.
	cs := &ChainState{ChainKey: chainKey, CurrentModel: "fable", StrikesAtRung: 1}
	if err := SaveChainState(ctx, stStore, cs); err != nil {
		t.Fatal(err)
	}

	result, err := RetryWithEscalation(ctx, store, stStore, origID, policy, chainKey, FailTimeout, "timed out again")
	if err == nil {
		t.Fatal("expected error on exhaustion")
	}
	if !strings.Contains(err.Error(), "exhausted") {
		t.Errorf("error = %v, want mention of 'exhausted'", err)
	}
	if result == nil || !result.Exhausted {
		t.Fatalf("result.Exhausted = %v, want true", result)
	}
	if result.LessonFile == "" {
		t.Fatal("LessonFile is empty")
	}
	content, rerr := os.ReadFile(result.LessonFile)
	if rerr != nil {
		t.Fatalf("read lesson file: %v", rerr)
	}
	if !strings.Contains(string(content), "Escalation chain exhausted") {
		t.Errorf("lesson file missing header:\n%s", content)
	}
	if !strings.Contains(string(content), "timed out again") {
		t.Errorf("lesson file missing failure detail:\n%s", content)
	}
}

func TestEscalationCap(t *testing.T) {
	store, stStore := testStores(t)
	ctx := context.Background()
	t.Setenv("CLAVAIN_FABLE_AVAILABLE", "1")

	dir := t.TempDir()
	model := "opus"
	orig := &Dispatch{AgentType: "codex", ProjectDir: dir, Model: &model}
	origID, err := store.Create(ctx, orig)
	if err != nil {
		t.Fatal(err)
	}
	store.UpdateStatus(ctx, origID, StatusFailed, UpdateFields{"exit_code": 1})

	chainKey := "chain-cap-1"
	policy := DefaultEscalationPolicy()
	policy.MaxEscalations = 1

	// Pre-seed chain state as if one escalation already happened (opus, 2
	// strikes) so this call would be the SECOND escalation attempt.
	cs := &ChainState{ChainKey: chainKey, CurrentModel: "opus", StrikesAtRung: 1, Escalations: 1}
	if err := SaveChainState(ctx, stStore, cs); err != nil {
		t.Fatal(err)
	}

	result, err := RetryWithEscalation(ctx, store, stStore, origID, policy, chainKey, FailError, "still failing")
	if err == nil {
		t.Fatal("expected MaxEscalations error")
	}
	if !strings.Contains(err.Error(), "MaxEscalations") {
		t.Errorf("error = %v, want mention of MaxEscalations", err)
	}
	if result == nil || !result.Exhausted {
		t.Fatalf("result.Exhausted = %v, want true", result)
	}

	loaded, lerr := LoadChainState(ctx, stStore, chainKey)
	if lerr != nil {
		t.Fatalf("LoadChainState: %v", lerr)
	}
	if !loaded.Exhausted {
		t.Errorf("chain state Exhausted = false, want true")
	}
}
