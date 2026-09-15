package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/mistakeknot/intercore/internal/dispatch"
)

func TestReviewV2ConfigSetterConstrainsSpawn(t *testing.T) {
	setupCommandMetadataDB(t)
	flagJSON = true
	ctx := context.Background()
	if code := cmdConfigSet(ctx, []string{"global_max_dispatches", "1"}); code != 0 {
		t.Fatalf("config setter exit=%d", code)
	}
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var payload string
	if err := db.SqlDB().QueryRow(`SELECT payload FROM state WHERE key='kernel.global_max_dispatches' AND scope_id='global'`).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if payload != "1" {
		t.Fatalf("actual writer payload=%q", payload)
	}
	dir := t.TempDir()
	prompt, wrapper := filepath.Join(dir, "prompt"), filepath.Join(dir, "wrapper")
	if err := os.WriteFile(prompt, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nsleep 60\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--project=" + dir, "--prompt-file=" + prompt, "--dispatch-sh=" + wrapper}
	out := captureDispatchOutput(t, func() int {
		code := cmdDispatchSpawn(ctx, args)
		if code != 0 {
			t.Errorf("first spawn=%d", code)
		}
		return code
	})
	var spawned struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	if err := json.Unmarshal(out, &spawned); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			if p, err := os.FindProcess(spawned.PID); err == nil {
				_, _ = p.Wait()
			}
			close(done)
		}()
		_ = dispatch.Kill(ctx, dispatch.New(db.SqlDB(), nil), spawned.ID)
		<-done
	})
	out = captureDispatchOutput(t, func() int {
		code := cmdDispatchSpawn(ctx, args)
		if code != 1 {
			t.Errorf("second spawn=%d", code)
		}
		return 0
	})
	var rejected dispatch.SpawnRejection
	if err := json.Unmarshal(out, &rejected); err != nil {
		t.Fatal(err)
	}
	if rejected.Reason != "concurrency_limit_global" {
		t.Fatalf("rejection=%+v", rejected)
	}
}

// isolateRoutingPolicyDiscovery removes every source routing.ResolvePolicyPath
// consults other than an explicit --policy, so a test cannot pass on a machine
// that happens to have a Clavain installation. setupCommandMetadataDB already
// moves into a temp directory, which rules out the working-directory walk.
func isolateRoutingPolicyDiscovery(t *testing.T) {
	t.Helper()
	for _, key := range []string{"CLAVAIN_ROUTING_POLICY", "CLAVAIN_ROOT", "CLAUDE_PLUGIN_ROOT"} {
		// t.Setenv records the original value for cleanup; Unsetenv then makes the
		// variable absent, not empty, for any source that reads it with LookupEnv.
		t.Setenv(key, "")
		_ = os.Unsetenv(key)
	}
	t.Setenv("HOME", t.TempDir())
}

// Escalation resolves a routing policy before it creates any retry, so with no
// policy available the command must stop without mutating dispatch state.
func TestReviewV2EscalationWithoutRoutingPolicyFailsClosed(t *testing.T) {
	isolateRoutingPolicyDiscovery(t)
	setupCommandMetadataDB(t)
	db, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.SqlDB().Exec(`INSERT INTO dispatches(id,project_dir,agent_type,status) VALUES ('attempt','.','codex','failed')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if code := cmdDispatch(context.Background(), []string{"retry", "attempt", "--escalate"}); code != 2 {
		t.Fatalf("exit=%d, want 2 when no routing policy is available", code)
	}

	db, err = openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var dispatches int
	if err := db.SqlDB().QueryRow(`SELECT count(*) FROM dispatches`).Scan(&dispatches); err != nil {
		t.Fatal(err)
	}
	if dispatches != 1 {
		t.Fatalf("dispatches=%d, want 1: no retry may be created without a routing policy", dispatches)
	}
}

func TestReviewV2MutationRejectionsAreStructured(t *testing.T) {
	isolateRoutingPolicyDiscovery(t)
	for _, tc := range []struct {
		name, setup, reason string
		args                []string
	}{
		{"retry budget", `INSERT INTO runs(id,project_dir,goal,token_budget,budget_enforce) VALUES ('run','.','fixture',1,1); INSERT INTO dispatches(id,project_dir,agent_type,status,scope_id,input_tokens) VALUES ('attempt','.','codex','failed','run',1)`, "budget_exceeded", []string{"retry", "attempt"}},
		{"escalated retry budget", `INSERT INTO runs(id,project_dir,goal,token_budget,budget_enforce) VALUES ('run','.','fixture',1,1); INSERT INTO dispatches(id,project_dir,agent_type,status,scope_id,input_tokens) VALUES ('attempt','.','codex','failed','run',1)`, "budget_exceeded", []string{"retry", "attempt", "--escalate"}},
		{"terminal tokens", `INSERT INTO dispatches(id,project_dir,agent_type,status) VALUES ('attempt','.','flere','completed')`, "stale_status", []string{"tokens", "attempt", "--in=99"}},
		{"generic scope requires run", `INSERT INTO dispatches(id,project_dir,agent_type,status,scope_id) VALUES ('attempt','.','codex','failed','legacy-scope')`, "run_not_found", []string{"retry", "attempt"}},
		{"escalated Flere", `INSERT INTO dispatches(id,project_dir,agent_type,status) VALUES ('attempt','.','flere','failed')`, "not_retryable", []string{"retry", "attempt", "--escalate"}},
		{"escalated indeterminate", `INSERT INTO dispatches(id,project_dir,agent_type,status,quarantine_reason) VALUES ('attempt','.','codex','failed','worker_outcome_indeterminate')`, "not_retryable", []string{"retry", "attempt", "--escalate"}},
		{"kill missing attempt", `INSERT INTO dispatches(id,project_dir,agent_type,status) VALUES ('other','.','codex','running')`, "not_found", []string{"kill", "attempt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupCommandMetadataDB(t)
			flagJSON = true
			db, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.SqlDB().Exec(tc.setup); err != nil {
				t.Fatal(err)
			}
			db.Close()
			args := tc.args
			for _, arg := range tc.args {
				if arg == "--escalate" {
					policy := filepath.Join(t.TempDir(), "routing.yaml")
					if err := os.WriteFile(policy, []byte("subagents:\n  defaults:\n    model: sonnet\n"), 0o600); err != nil {
						t.Fatal(err)
					}
					args = append(append([]string{}, tc.args...), "--policy="+policy)
				}
			}
			out := captureDispatchOutput(t, func() int {
				code := cmdDispatch(context.Background(), args)
				if code != 1 {
					t.Errorf("exit=%d, want 1", code)
				}
				return 0
			})
			var rejection dispatch.SpawnRejection
			if err := json.Unmarshal(out, &rejection); err != nil {
				t.Fatalf("invalid rejection %q: %v", out, err)
			}
			if rejection.Reason != tc.reason {
				t.Fatalf("reason=%q want %q", rejection.Reason, tc.reason)
			}
		})
	}
}
