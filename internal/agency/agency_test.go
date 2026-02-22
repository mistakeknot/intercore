package agency

import (
	"os"
	"path/filepath"
	"testing"
)

// testPhases mirrors the 9 phases from phase.DefaultPhaseChain.
var testPhases = []string{
	"brainstorm", "brainstorm-reviewed", "strategized", "planned",
	"executing", "review", "polish", "reflect", "done",
}

var validBuildSpec = `
meta:
  stage: build
  description: Review plan, implement code
  phases: [planned, executing]

agents:
  - phase: planned
    command: /interflux:flux-drive
    args: ["${artifact:plan}"]
    mode: interactive
    priority: 0
    description: Review plan before execution
  - phase: executing
    command: /clavain:work
    args: ["${artifact:plan}"]
    mode: both
    priority: 0

models:
  planned:
    default: sonnet
    categories:
      review: opus
  executing:
    default: sonnet

tools:
  review:
    allow: [Read, Grep, Glob]
    deny: [Edit, Write]

artifacts:
  required:
    - type: plan
      phase: planned
      description: Implementation plan from Design stage
  produces:
    - type: code
      phase: executing

gates:
  entry:
    - check: artifact_exists
      phase: planned
      tier: hard
      description: Plan must exist
  exit:
    - check: agents_complete
      tier: hard
    - check: verdict_exists
      tier: soft

budget:
  allocation: 0.50
  max_agents: 5
  warn_threshold: 0.80

capabilities:
  review:
    kernel: [events.tail, dispatch.status]
    filesystem: [read]
  workflow:
    kernel: [events.tail, dispatch.spawn]
    filesystem: [read, write]
    dispatch: [spawn]
`

func TestParseBytes_Valid(t *testing.T) {
	spec, err := ParseBytes([]byte(validBuildSpec))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	if spec.Meta.Stage != "build" {
		t.Errorf("stage = %q, want %q", spec.Meta.Stage, "build")
	}
	if len(spec.Meta.Phases) != 2 {
		t.Errorf("phases count = %d, want 2", len(spec.Meta.Phases))
	}
	if len(spec.Agents) != 2 {
		t.Errorf("agents count = %d, want 2", len(spec.Agents))
	}
	if spec.Agents[0].Command != "/interflux:flux-drive" {
		t.Errorf("agents[0].command = %q", spec.Agents[0].Command)
	}
	if len(spec.Agents[0].Args) != 1 || spec.Agents[0].Args[0] != "${artifact:plan}" {
		t.Errorf("agents[0].args = %v", spec.Agents[0].Args)
	}
	if spec.Models["planned"].Default != "sonnet" {
		t.Errorf("models.planned.default = %q", spec.Models["planned"].Default)
	}
	if spec.Models["planned"].Categories["review"] != "opus" {
		t.Errorf("models.planned.categories.review = %q", spec.Models["planned"].Categories["review"])
	}
	if spec.Budget.Allocation != 0.50 {
		t.Errorf("budget.allocation = %f, want 0.50", spec.Budget.Allocation)
	}
	if len(spec.Gates.Entry) != 1 {
		t.Errorf("gates.entry count = %d, want 1", len(spec.Gates.Entry))
	}
	if spec.Gates.Entry[0].Tier != "hard" {
		t.Errorf("gates.entry[0].tier = %q, want %q", spec.Gates.Entry[0].Tier, "hard")
	}
	if len(spec.Capabilities["workflow"].Dispatch) != 1 {
		t.Errorf("capabilities.workflow.dispatch = %v", spec.Capabilities["workflow"].Dispatch)
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "build.yaml")
	if err := os.WriteFile(path, []byte(validBuildSpec), 0644); err != nil {
		t.Fatal(err)
	}
	spec, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if spec.Meta.Stage != "build" {
		t.Errorf("stage = %q, want %q", spec.Meta.Stage, "build")
	}
}

func TestParseFile_NotFound(t *testing.T) {
	_, err := ParseFile("/nonexistent/build.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestParseBytes_InvalidYAML(t *testing.T) {
	_, err := ParseBytes([]byte("{{invalid"))
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
}

func TestValidate_Valid(t *testing.T) {
	spec, _ := ParseBytes([]byte(validBuildSpec))
	errs := Validate(spec, testPhases)
	if len(errs) > 0 {
		for _, e := range errs {
			t.Errorf("unexpected error: %s", e)
		}
	}
}

func TestValidate_MissingStage(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Phases: []string{"brainstorm"}},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "meta.stage" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for missing meta.stage")
	}
}

func TestValidate_UnknownStage(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "bogus", Phases: []string{"brainstorm"}},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "meta.stage" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for unknown stage")
	}
}

func TestValidate_UnknownPhase(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"nonexistent"}},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "meta.phases[0]" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for unknown phase")
	}
}

func TestValidate_DuplicateAgent(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Agents: []AgentEntry{
			{Phase: "planned", Command: "/foo", Mode: "interactive"},
			{Phase: "planned", Command: "/foo", Mode: "both"},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "agents[1]" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for duplicate agent")
	}
}

func TestValidate_AgentPhaseNotInSpec(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Agents: []AgentEntry{
			{Phase: "executing", Command: "/foo", Mode: "interactive"},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "agents[0].phase" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for agent phase not in spec")
	}
}

func TestValidate_UnknownGateCheck(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Gates: GateConfig{
			Entry: []GateRule{{Check: "unknown_check", Tier: "hard"}},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "gates.entry[0].check" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for unknown gate check")
	}
}

func TestValidate_InvalidGateTier(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Gates: GateConfig{
			Exit: []GateRule{{Check: "agents_complete", Tier: "medium"}},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "gates.exit[0].tier" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for invalid gate tier")
	}
}

func TestValidate_BudgetOutOfRange(t *testing.T) {
	spec := &Spec{
		Meta:   Meta{Stage: "build", Phases: []string{"planned"}},
		Budget: BudgetConfig{Allocation: 1.5, WarnThreshold: 0.8},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "budget.allocation" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for allocation > 1.0")
	}
}

func TestValidate_InvalidCapability(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Capabilities: map[string]CapabilitySet{
			"review": {Kernel: []string{"INVALID_CAP"}},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "capabilities.review.kernel[0]" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for invalid capability name")
	}
}

func TestValidate_ModelPhaseNotInSpec(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Models: map[string]ModelConfig{
			"executing": {Default: "sonnet"},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "models.executing" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for model phase not in spec")
	}
}

func TestValidate_InvalidAgentMode(t *testing.T) {
	spec := &Spec{
		Meta: Meta{Stage: "build", Phases: []string{"planned"}},
		Agents: []AgentEntry{
			{Phase: "planned", Command: "/foo", Mode: "turbo"},
		},
	}
	errs := Validate(spec, testPhases)
	found := false
	for _, e := range errs {
		if e.Field == "agents[0].mode" {
			found = true
		}
	}
	if !found {
		t.Error("expected validation error for invalid agent mode")
	}
}
