package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/routing"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

func TestGovernedDecisionAdmitsDirectAndScheduled(t *testing.T) {
	for _, role := range []string{"planning", "plan-review"} {
		for _, scheduled := range []bool{false, true} {
			name := role + "/direct"
			if scheduled {
				name = role + "/scheduled"
			}
			t.Run(name, func(t *testing.T) {
				setupCommandMetadataDB(t)
				db, err := openDB()
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				dir := t.TempDir()
				policy := filepath.Join(dir, "routing.yaml")
				if err := os.WriteFile(policy, []byte(`reasoning:
  frontier_models: [gpt-6-astra, claude-fable-5-1]
  frontier_reasons: [foundational-invariants]
  dual_review_reasons: [foundational-invariants]
dispatch:
  roles: {planning: sol, plan-review: fable}
  tiers:
    sol: {model: gpt-5.6-sol, backend: codex, reasoning_effort: high, fallbacks: []}
    fable: {model: claude-fable-5-1, backend: claude, reasoning_effort: high, fallbacks: [astra]}
    astra: {model: gpt-6-astra, backend: codex, reasoning_effort: xhigh}
`), 0600); err != nil {
					t.Fatal(err)
				}
				cfg, err := routing.LoadConfig(policy, "")
				if err != nil {
					t.Fatal(err)
				}
				producer := ""
				context := routing.DecisionContext{}
				if role == "plan-review" {
					producer = "gpt-6-astra"
					context.Reasons = []string{"foundational-invariants"}
					context.Rationale = "Review shared admission rules"
				}
				decision, err := routing.NewResolver(cfg).ResolveDecision(role, producer, "", context)
				if err != nil {
					t.Fatal(err)
				}
				prompt, wrapper := filepath.Join(dir, "prompt"), filepath.Join(dir, "wrapper")
				if err := os.WriteFile(prompt, []byte("fixture"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\nprintf 'fixture passed\\n' > \"$out\"\nprintf 'STATUS: pass\\n' > \"$out.verdict\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				opts := dispatch.SpawnOptions{ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper, Decision: &decision, AgentType: decision.Profile.Backend}
				verifyGovernedAdmission(t, db.SqlDB(), opts, scheduled)
			})
		}
	}
}

func verifyGovernedAdmission(t *testing.T, db *sql.DB, opts dispatch.SpawnOptions, scheduled bool) {
	t.Helper()
	ctx := context.Background()
	store := dispatch.New(db, nil)
	var id string
	if scheduled {
		encoded, err := scheduler.MarshalSpawnOpts(opts)
		if err != nil {
			t.Fatal(err)
		}
		job := scheduler.NewSpawnJob("", scheduler.JobTypeDispatch, "governed-fixture")
		job.AgentType, job.ProjectDir, job.SpawnOpts = opts.AgentType, opts.ProjectDir, encoded
		if err := scheduler.DispatchExecutor(store)(ctx, job); err != nil {
			t.Fatal(err)
		}
		id = job.DispatchID
	} else {
		result, err := dispatch.Spawn(ctx, store, opts)
		if err != nil {
			t.Fatal(err)
		}
		if err := result.Cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		id = result.ID
		if err := dispatch.Collect(ctx, store, id); err != nil {
			t.Fatal(err)
		}
	}
	row, err := store.Get(ctx, id)
	if err != nil || row.Status != dispatch.StatusCompleted || row.Model == nil || *row.Model != opts.Decision.Profile.Model {
		t.Fatalf("governed execution: %+v, %v", row, err)
	}
	var hash, raw string
	if err := db.QueryRow("SELECT policy_hash, context_json FROM routing_decisions WHERE dispatch_id = ?", id).Scan(&hash, &raw); err != nil {
		t.Fatal(err)
	}
	var recorded routing.ReasoningDecision
	if err := json.Unmarshal([]byte(raw), &recorded); err != nil || hash != opts.Decision.PolicyHash || recorded.Profile.ReasoningEffort != "high" {
		t.Fatalf("lost decision receipt: %s, %v", raw, err)
	}
	// A valid contract admitting successfully must not make a forged variant valid.
	forged := *opts.Decision
	forged.Profile.ReasoningEffort = "low"
	opts.Decision = &forged
	if _, err := dispatch.Spawn(ctx, store, opts); err == nil || !strings.Contains(err.Error(), "reasoning contract") {
		t.Fatalf("forged effort accepted: %v", err)
	}
}
