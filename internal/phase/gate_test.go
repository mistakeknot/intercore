package phase

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/runtrack"
)

// --- Gate evaluation tests ---

func TestGate_ArtifactExists_Pass(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	rtStore.AddArtifact(ctx, &runtrack.Artifact{
		RunID: id, Phase: PhaseBrainstorm, Path: "brainstorm.md", Type: "file",
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Error("Expected gate to pass with artifact present")
	}
	if result.GateResult != GatePass {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GatePass)
	}
}

func TestGate_ArtifactExists_Fail(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// No artifact — hard gate (priority 0) should block
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced {
		t.Error("Expected gate to block without artifact")
	}
	if result.GateResult != GateFail {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GateFail)
	}
	if result.EventType != EventBlock {
		t.Errorf("EventType = %q, want %q", result.EventType, EventBlock)
	}

	// Phase should NOT have changed
	run, _ := store.Get(ctx, id)
	if run.Phase != PhaseBrainstorm {
		t.Errorf("Phase = %q, want %q (should not advance)", run.Phase, PhaseBrainstorm)
	}
}

func TestGate_ArtifactExists_SoftFail(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// No artifact — soft gate (priority 2) should warn but advance
	result, err := Advance(ctx, store, id, GateConfig{Priority: 2}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Error("Soft gate should advance even when failing")
	}
	if result.GateResult != GateFail {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GateFail)
	}
	if result.GateTier != TierSoft {
		t.Errorf("GateTier = %q, want %q", result.GateTier, TierSoft)
	}
}

func TestGate_AgentsComplete_Pass(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	advanceToPhase(t, store, id, PhaseExecuting, rtStore)

	// No active agents — gate should pass
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Error("Expected gate to pass with no active agents")
	}
}

func TestGate_AgentsComplete_Fail(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	advanceToPhase(t, store, id, PhaseExecuting, rtStore)

	// Add an active agent
	rtStore.AddAgent(ctx, &runtrack.Agent{
		RunID: id, AgentType: "claude", Status: "active",
	})

	// Hard gate should block
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced {
		t.Error("Expected gate to block with active agents")
	}
	if result.GateResult != GateFail {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GateFail)
	}
}

func TestGate_VerdictExists_Pass(t *testing.T) {
	store, rtStore, sqlDB, ctx := setupMachineTest(t)

	scopeID := "test-scope-123"
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
		ScopeID: &scopeID,
	})
	advanceToPhase(t, store, id, PhaseReview, rtStore)

	// Add a dispatch with passing verdict
	dStore := dispatch.New(sqlDB, nil)
	dStore.Create(ctx, &dispatch.Dispatch{
		AgentType:  "claude",
		ProjectDir: "/tmp",
		ScopeID:    &scopeID,
	})
	// Set verdict on the dispatch — need to find its ID first
	dispatches, _ := dStore.List(ctx, &scopeID)
	if len(dispatches) == 0 {
		t.Fatal("No dispatches found")
	}
	dStore.UpdateStatus(ctx, dispatches[0].ID, dispatch.StatusCompleted, dispatch.UpdateFields{
		"verdict_status": "approved",
	})

	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, dStore, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Errorf("Expected gate to pass with verdict. Result: %+v", result)
	}
}

func TestGate_VerdictExists_Fail(t *testing.T) {
	store, rtStore, sqlDB, ctx := setupMachineTest(t)

	scopeID := "test-scope-456"
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
		ScopeID: &scopeID,
	})
	advanceToPhase(t, store, id, PhaseReview, rtStore)

	dStore := dispatch.New(sqlDB, nil)

	// No verdict — hard gate should block
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, dStore, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced {
		t.Error("Expected gate to block without verdict")
	}
}

func TestGate_VerdictExists_NoScopeID_Fails(t *testing.T) {
	store, rtStore, sqlDB, ctx := setupMachineTest(t)

	// No scope_id set — verdict gate should fail explicitly
	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	advanceToPhase(t, store, id, PhaseReview, rtStore)

	dStore := dispatch.New(sqlDB, nil)
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, dStore, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced {
		t.Error("Expected gate to fail with nil scope_id")
	}
}

func TestGate_DisableAll(t *testing.T) {
	store, _, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// Gate disabled — should advance regardless of missing artifacts
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0, DisableAll: true}, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Error("Expected advance with DisableAll=true")
	}
	if result.GateResult != GateNone {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GateNone)
	}
}

func TestGate_NoRulesForTransition(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	advanceToPhase(t, store, id, PhasePolish, rtStore)

	// polish → done has no rules — should always pass
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if !result.Advanced {
		t.Error("Expected gate to pass for polish→done (no rules)")
	}
	if result.GateResult != GatePass {
		t.Errorf("GateResult = %q, want %q", result.GateResult, GatePass)
	}
}

func TestGate_Evidence_Serialized(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})
	rtStore.AddArtifact(ctx, &runtrack.Artifact{
		RunID: id, Phase: PhaseBrainstorm, Path: "test.md", Type: "file",
	})

	_, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}

	// Check event's reason contains evidence JSON
	events, _ := store.Events(ctx, id)
	if len(events) == 0 {
		t.Fatal("No events recorded")
	}
	if events[0].Reason == nil {
		t.Fatal("Event reason is nil, expected evidence JSON")
	}

	// Parse the JSON evidence
	var evidence GateEvidence
	if err := json.Unmarshal([]byte(*events[0].Reason), &evidence); err != nil {
		t.Fatalf("Failed to parse evidence JSON: %v (raw: %s)", err, *events[0].Reason)
	}
	if len(evidence.Conditions) != 1 {
		t.Errorf("Conditions count = %d, want 1", len(evidence.Conditions))
	}
	if evidence.Conditions[0].Check != CheckArtifactExists {
		t.Errorf("Check = %q, want %q", evidence.Conditions[0].Check, CheckArtifactExists)
	}
	if evidence.Conditions[0].Result != GatePass {
		t.Errorf("Result = %q, want %q", evidence.Conditions[0].Result, GatePass)
	}
}

func TestEvaluateGate_DryRun(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// Dry-run without artifact — should fail but not advance
	result, err := EvaluateGate(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil)
	if err != nil {
		t.Fatalf("EvaluateGate: %v", err)
	}
	if result.Result != GateFail {
		t.Errorf("Result = %q, want %q", result.Result, GateFail)
	}

	// Phase should NOT have changed
	run, _ := store.Get(ctx, id)
	if run.Phase != PhaseBrainstorm {
		t.Errorf("Phase = %q, want %q (dry-run should not advance)", run.Phase, PhaseBrainstorm)
	}
}

func TestGate_DBError(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// Use EvaluateGate (dry-run) with cancelled context — this calls Get first,
	// then evaluateGate. Either way, we expect a propagated error.
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()

	_, err := EvaluateGate(cancelCtx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil)
	if err == nil {
		t.Fatal("Expected error from gate check with cancelled context")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Errorf("Error = %q, expected to contain 'context canceled'", err.Error())
	}
}

func TestGate_BlockEvidenceInReason(t *testing.T) {
	store, rtStore, _, ctx := setupMachineTest(t)

	id, _ := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true,
	})

	// No artifact — hard gate should block and record evidence
	result, err := Advance(ctx, store, id, GateConfig{Priority: 0}, rtStore, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced {
		t.Fatal("Expected block")
	}

	// Check that block event has evidence JSON in reason
	events, _ := store.Events(ctx, id)
	if len(events) == 0 {
		t.Fatal("No events recorded")
	}
	if events[0].Reason == nil {
		t.Fatal("Block event reason is nil")
	}

	var evidence GateEvidence
	if err := json.Unmarshal([]byte(*events[0].Reason), &evidence); err != nil {
		t.Fatalf("Failed to parse block evidence: %v", err)
	}
	if len(evidence.Conditions) != 1 {
		t.Errorf("Conditions = %d, want 1", len(evidence.Conditions))
	}
	if evidence.Conditions[0].Result != GateFail {
		t.Errorf("Condition result = %q, want %q", evidence.Conditions[0].Result, GateFail)
	}
}

func TestGateRulesInfo(t *testing.T) {
	rules := GateRulesInfo()
	if len(rules) == 0 {
		t.Fatal("GateRulesInfo returned empty")
	}

	// Should have entries for brainstorm→brainstorm-reviewed through review→polish
	found := map[string]bool{}
	for _, r := range rules {
		found[r.From+"→"+r.To] = true
	}

	expected := []string{
		"brainstorm→brainstorm-reviewed",
		"brainstorm-reviewed→strategized",
		"strategized→planned",
		"planned→executing",
		"executing→review",
		"review→polish",
	}
	for _, e := range expected {
		if !found[e] {
			t.Errorf("Missing rule for %s", e)
		}
	}
}

func TestGateEvidence_String(t *testing.T) {
	e := &GateEvidence{
		Conditions: []GateCondition{
			{Check: CheckArtifactExists, Phase: PhaseBrainstorm, Result: GatePass},
		},
	}
	s := e.String()
	if !strings.Contains(s, `"artifact_exists"`) {
		t.Errorf("String() = %q, expected to contain artifact_exists", s)
	}
}
