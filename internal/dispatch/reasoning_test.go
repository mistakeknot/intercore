package dispatch

import (
	"context"
	"github.com/mistakeknot/intercore/internal/routing"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationalFailuresDoNotConsumeStrikes(t *testing.T) {
	for _, mode := range []FailureMode{FailTimeout, FailError, FailUnknown, ParseFailureMode("authentication"), ParseFailureMode("rate-limit"), ParseFailureMode("permission"), ParseFailureMode("infrastructure")} {
		t.Run(string(mode), func(t *testing.T) {
			store, st := testStores(t)
			ctx := context.Background()
			m := "base"
			id, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &m})
			store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{"exit_code": 1})
			p := DefaultEscalationPolicy()
			p.Ladder = []string{"base", "frontier"}
			result, err := RetryWithEscalation(ctx, store, st, id, p, "ops", mode, "operational")
			if err != nil {
				t.Fatal(err)
			}
			cs, _ := LoadChainState(ctx, st, "ops")
			if cs.StrikesAtRung != 0 || result.Escalated {
				t.Fatalf("operational strike: %+v", cs)
			}
		})
	}
}

func TestGovernedEscalationPreservesBackendAndEffort(t *testing.T) {
	store, st := testStores(t)
	ctx := context.Background()
	m := "gpt-5.6-sol"
	id, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &m})
	store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{"exit_code": 1})
	cfg := &routing.Config{PolicyHash: "fixture", Reasoning: routing.ReasoningPolicy{Strikes: 2, FrontierModels: []string{"claude-fable-5-1"}, FrontierReasons: []string{"capability-failure"}}, Dispatch: routing.DispatchConfig{Roles: map[string]string{"escalation": "frontier"}, Tiers: map[string]routing.DispatchProfile{"frontier": {Backend: "claude", Model: "claude-fable-5-1", ReasoningEffort: "high", ServiceTier: "standard"}}}}
	p := EscalationPolicyFromConfig(cfg)
	result, err := RetryWithEscalation(ctx, store, st, id, p, "premise", ParseFailureMode("premise-failure"), "plan premise disproven")
	if err != nil {
		t.Fatal(err)
	}
	next, _ := store.Get(ctx, result.NewID)
	if !result.Escalated || next.AgentType != "claude" || result.Decision == nil || result.Decision.Profile.ReasoningEffort != "high" {
		t.Fatalf("lost profile: %+v %+v", next, result)
	}
	// A fresh top-level retry shares the durable chain but starts at the old model.
	fresh, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &m})
	store.UpdateStatus(ctx, fresh, StatusFailed, UpdateFields{"exit_code": 1})
	retried, err := RetryWithEscalation(ctx, store, st, fresh, p, "premise", FailInfrastructure, "transport")
	if err != nil {
		t.Fatal(err)
	}
	row, _ := store.Get(ctx, retried.NewID)
	if row.AgentType != "claude" || *row.Model != "claude-fable-5-1" || retried.Decision.Profile.ReasoningEffort != "high" {
		t.Fatalf("fresh retry lost decision: %+v %+v", row, retried)
	}
}

func TestBlockedEscalationKeepsSingleStrikeReceipt(t *testing.T) {
	store, st := testStores(t)
	ctx := context.Background()
	m := "gpt-5.6-sol"
	id, _ := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), Model: &m})
	store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{"exit_code": 1})
	p := EscalationPolicyFromConfig(&routing.Config{Reasoning: routing.ReasoningPolicy{Strikes: 2, FrontierReasons: []string{"capability-failure"}}})
	for i := 0; i < 2; i++ {
		if _, err := RetryWithEscalation(ctx, store, st, id, p, "blocked", FailPremise, "premise disproven"); err == nil {
			t.Fatal("missing frontier accepted")
		}
	}
	cs, err := LoadChainState(ctx, st, "blocked")
	if err != nil || len(cs.Failures) != 1 || cs.StrikesAtRung != 2 {
		t.Fatalf("lost or duplicated evidence: %+v %v", cs, err)
	}
}

func TestGovernedSpawnRequiresRoleWrapper(t *testing.T) {
	t.Setenv("CLAVAIN_DISPATCH_SH", "")
	t.Chdir(t.TempDir())
	d := &routing.ReasoningDecision{}
	d.RequestedRole = "planning"
	d.PolicySource = "/selected/config/routing.yaml"
	d.Profile.Model = "gpt-6-astra"
	d.Profile.Backend = "codex"
	d.Profile.ReasoningEffort = "xhigh"
	opts := SpawnOptions{AgentType: "codex", ProjectDir: t.TempDir(), PromptFile: "prompt", Decision: d}
	if _, err := buildCmd(opts, "out"); err == nil {
		t.Fatal("governed route silently used bare codex")
	}
	opts.DispatchSH = filepath.Join(t.TempDir(), "dispatch.sh")
	os.WriteFile(opts.DispatchSH, []byte("#!/bin/sh\n"), 0700)
	cmd, err := buildCmd(opts, "out")
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--reasoning-effort xhigh") || !strings.Contains(args, "--resolved-route-json") {
		t.Fatalf("lost contract: %s", args)
	}
}

func TestGovernedSpawnRejectsUnverifiableContractBeforeAdmission(t *testing.T) {
	store, _ := testStores(t)
	d := &routing.ReasoningDecision{PolicySource: "/missing/routing.yaml", PolicyHash: "fake"}
	d.RequestedRole = "planning"
	d.Profile = routing.DispatchProfile{Backend: "codex", Model: "gpt-5.6-sol", ReasoningEffort: "high"}
	_, err := Spawn(context.Background(), store, SpawnOptions{ProjectDir: t.TempDir(), PromptFile: "missing", Decision: d})
	if err == nil || !strings.Contains(err.Error(), "reasoning contract") {
		t.Fatalf("did not reject contract before other work: %v", err)
	}
}
