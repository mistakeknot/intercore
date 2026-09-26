package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mistakeknot/intercore/internal/routing"
)

func captureRouteStdout(t *testing.T, fn func() int) (int, []byte) {
	t.Helper()
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = writeEnd
	code := fn()
	_ = writeEnd.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(readEnd)
	_ = readEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	return code, out
}

func stringValue(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestRouteDispatchRoleJSON(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.Mkdir(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	config := `
dispatch:
  model_aliases:
    fable: claude-fable-5-1
  roles:
    deep-execution: deep-astra
    validation: fable-review
  tiers:
    fable-review:
      backend: claude
      model: fable
      fallbacks: [deep-sol]
    deep-astra:
      role: deep-execution
      backend: codex
      model: gpt-6-astra
      reasoning_effort: high
      service_tier: standard
      minimum_codex_version: 0.153.1
      fallbacks: [deep-sol]
    deep-sol:
      role: deep-execution
      backend: codex
      model: gpt-5.6-sol
      reasoning_effort: xhigh
      service_tier: standard
`
	if err := os.WriteFile(filepath.Join(configDir, "routing.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	oldDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldDir)
		flagJSON = false
	})
	flagJSON = true

	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStdout := os.Stdout
	os.Stdout = writeEnd
	code := cmdRouteDispatch(context.Background(), []string{"--policy=" + filepath.Join(configDir, "routing.yaml"), "--role=deep-execution"})
	_ = writeEnd.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(readEnd)
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("cmdRouteDispatch code = %d, want 0; output=%s", code, out)
	}
	if bytes.Contains(out, []byte(`"calibration"`)) {
		t.Fatalf("omitted calibration changed role JSON: %s", out)
	}

	var got routing.ResolvedDispatch
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal output %q: %v", out, err)
	}
	if got.RequestedRole != "deep-execution" || got.ProfileRef != "deep-astra" {
		t.Errorf("resolved identity = %q/%q, want deep-execution/deep-astra", got.RequestedRole, got.ProfileRef)
	}
	if got.Profile.Model != "gpt-6-astra" || got.Profile.ReasoningEffort != "high" {
		t.Errorf("profile = %#v, want Astra/high", got.Profile)
	}
	if len(got.FallbackChain) != 1 || got.FallbackChain[0].Profile.Model != "gpt-5.6-sol" {
		t.Errorf("fallback chain = %#v, want one Sol fallback", got.FallbackChain)
	}
	readEnd, writeEnd, err = os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writeEnd
	code = cmdRouteDispatch(context.Background(), []string{"--policy=" + filepath.Join(configDir, "routing.yaml"), "--role=validation", "--producer-identity=anthropic/claude-fable-5-1[1m]"})
	_ = writeEnd.Close()
	os.Stdout = oldStdout
	out, err = io.ReadAll(readEnd)
	_ = readEnd.Close()
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("validation code = %d; output=%s", code, out)
	}
	got = routing.ResolvedDispatch{}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-5.6-sol" || got.ProducerModel != "claude-fable-5-1" || len(got.Excluded) != 1 {
		t.Fatalf("CLI did not enforce canonical separation: %#v", got)
	}
}

func TestRouteModelCalibrationJSONAndOmittedCompatibility(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "routing.yaml")
	calibration := filepath.Join(dir, "calibration.json")
	if err := os.WriteFile(policy, []byte("subagents:\n  defaults:\n    model: sonnet\ncalibration:\n  mode: enforce\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(calibration, []byte(`{"schema_version":2,"agents":{"worker":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { flagJSON = false })
	flagJSON = true

	code, out := captureRouteStdout(t, func() int {
		return cmdRouteModel(context.Background(), []string{"--policy=" + policy, "--agent=worker"})
	})
	if code != 0 || bytes.Contains(out, []byte(`"calibration"`)) {
		t.Fatalf("omitted artifact changed JSON: code=%d out=%s", code, out)
	}
	var legacy map[string]string
	if err := json.Unmarshal(out, &legacy); err != nil || legacy["model"] != "sonnet" {
		t.Fatalf("legacy JSON = %s, %v", out, err)
	}

	code, out = captureRouteStdout(t, func() int {
		return cmdRouteModel(context.Background(), []string{"--policy=" + policy, "--agent=worker", "--calibration=" + calibration})
	})
	if code != 0 {
		t.Fatalf("calibrated route code=%d out=%s", code, out)
	}
	var got struct {
		Model       string                      `json:"model"`
		Calibration routing.CalibrationMetadata `json:"calibration"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "haiku" || got.Calibration.Disposition != "applied" || got.Calibration.SHA256 == nil || got.Calibration.SchemaVersion == nil {
		t.Fatalf("calibrated JSON = %#v", got)
	}
}

func TestRouteBatchCalibrationPreservesTextAndNestsJSONMetadata(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "routing.yaml")
	calibration := filepath.Join(dir, "calibration.json")
	if err := os.WriteFile(policy, []byte("subagents:\n  defaults:\n    model: sonnet\ncalibration:\n  mode: enforce\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(calibration, []byte(`{"schema_version":3,"skills":{"kept":true},"agents":{"one":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true},"two":{"recommended_model":"opus","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { flagJSON = false })

	flagJSON = false
	code, out := captureRouteStdout(t, func() int {
		return cmdRouteBatch(context.Background(), []string{"--policy=" + policy, "--calibration=" + calibration, "one", "two"})
	})
	if code != 0 || string(out) != "one\thaiku\ntwo\topus\n" {
		t.Fatalf("batch text changed: code=%d out=%q", code, out)
	}

	flagJSON = true
	code, out = captureRouteStdout(t, func() int {
		return cmdRouteBatch(context.Background(), []string{"--policy=" + policy, "one", "two"})
	})
	var legacy map[string]string
	if err := json.Unmarshal(out, &legacy); err != nil || code != 0 || legacy["one"] != "sonnet" || bytes.Contains(out, []byte(`"models"`)) {
		t.Fatalf("omitted calibration changed batch JSON: code=%d out=%s err=%v", code, out, err)
	}

	code, out = captureRouteStdout(t, func() int {
		return cmdRouteBatch(context.Background(), []string{"--policy=" + policy, "--calibration=" + calibration, "one", "two"})
	})
	var got struct {
		Models      map[string]string                      `json:"models"`
		Calibration map[string]routing.CalibrationMetadata `json:"calibration"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if code != 0 || got.Models["one"] != "haiku" || got.Calibration["two"].Disposition != "applied" {
		t.Fatalf("batch JSON: code=%d got=%#v", code, got)
	}
}

func TestRouteRecordRejectsInvalidCrossLabReorderJSON(t *testing.T) {
	for _, raw := range []string{
		`{not valid json`,
		`null`,
		`{}`,
		`{"from":[],"to":[]}`,
		`{"from":["a","b"],"to":["a"]}`,       // not a permutation: different length
		`{"from":["a","b"],"to":["a","c"]}`,   // not a permutation: different elements
		`{"from":["a"],"to":["a"],"frm":[1]}`, // unknown field
	} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			code, _ := captureRouteStdout(t, func() int {
				return cmdRouteRecord(context.Background(), []string{
					"--agent=fd-safety", "--model=opus", "--rule=override",
					"--cross-lab-reorder=" + raw,
				})
			})
			if code != 3 {
				t.Fatalf("expected exit 3 for --cross-lab-reorder=%s, got %d", raw, code)
			}
		})
	}
}

func TestRouteRecordPersistsAndReadsBackCrossLabReorder(t *testing.T) {
	setupCommandMetadataDB(t)
	code, _ := captureRouteStdout(t, func() int {
		return cmdRouteRecord(context.Background(), []string{
			"--agent=fd-safety", "--model=claude-opus-5", "--rule=cross_lab_first",
			`--cross-lab-reorder={"from":["opus","sonnet","sol","kimi"],"to":["sol","opus","sonnet","kimi"]}`,
		})
	})
	if code != 0 {
		t.Fatalf("cmdRouteRecord rc = %d", code)
	}

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	decisions, err := routing.NewDecisionStore(d.SqlDB()).List(context.Background(), routing.ListDecisionOpts{Agent: "fd-safety"})
	if err != nil {
		t.Fatal(err)
	}
	if len(decisions) != 1 || decisions[0].ContextJSON == nil {
		t.Fatalf("decisions = %#v", decisions)
	}
	var context struct {
		CrossLabReorder routing.CandidateReorder `json:"cross_lab_reorder"`
	}
	if err := json.Unmarshal([]byte(*decisions[0].ContextJSON), &context); err != nil {
		t.Fatalf("unmarshal context_json: %v", err)
	}
	if want := []string{"sol", "opus", "sonnet", "kimi"}; len(context.CrossLabReorder.To) != len(want) || context.CrossLabReorder.To[0] != want[0] {
		t.Fatalf("cross_lab_reorder.to = %#v, want %#v", context.CrossLabReorder.To, want)
	}
	if len(context.CrossLabReorder.From) != 4 || context.CrossLabReorder.From[0] != "opus" {
		t.Fatalf("cross_lab_reorder.from = %#v", context.CrossLabReorder.From)
	}
}

func TestRouteRoleCalibrationIsDiagnosticOnly(t *testing.T) {
	dir := t.TempDir()
	policy := filepath.Join(dir, "routing.yaml")
	contextPath := filepath.Join(dir, "context.json")
	calibration := filepath.Join(dir, "calibration.json")
	config := `
reasoning:
  frontier_models: [gpt-6-astra]
  frontier_reasons: [foundational-invariants]
  dual_review_reasons: [foundational-invariants]
dispatch:
  roles: {frontier-planning: astra}
  tiers:
    astra: {role: frontier-planning, backend: codex, model: gpt-6-astra, reasoning_effort: xhigh, service_tier: standard}
calibration: {mode: enforce}
`
	for path, body := range map[string]string{
		policy:      config,
		contextPath: `{"reasons":["foundational-invariants"],"rationale":"shared invariant"}`,
		calibration: `{"schema_version":2,"agents":{"planning":{"recommended_model":"haiku","confidence":0.9,"evidence_sessions":3,"propagation_eligible":true}}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { flagJSON = false })
	flagJSON = true
	code, out := captureRouteStdout(t, func() int {
		return cmdRouteDispatch(context.Background(), []string{"--policy=" + policy, "--role=planning", "--context-file=" + contextPath, "--calibration=" + calibration})
	})
	if code != 0 {
		t.Fatalf("role code=%d out=%s", code, out)
	}
	var got routing.ReasoningDecision
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Profile.Model != "gpt-6-astra" || got.Calibration == nil || got.Calibration.Disposition != "static" || got.Calibration.SelectedArm != nil || stringValue(got.Calibration.FallbackReason) != "unsupported_artifact_kind_for_role" {
		t.Fatalf("role calibration escaped diagnostics: %#v", got)
	}
}
