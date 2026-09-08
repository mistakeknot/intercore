package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mistakeknot/intercore/internal/routing"
)

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
