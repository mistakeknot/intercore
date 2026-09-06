package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/mistakeknot/intercore/internal/state"
)

func TestReviewKernelPolicyCannotBeWeakened(t *testing.T) {
	for _, tc := range []struct{ name, key, value, reason string }{
		{"global", "global_max_dispatches", "1", "concurrency_limit_global"},
		{"depth", "max_spawn_depth", "1", "spawn_depth_exceeded"},
		{"negative", "global_max_dispatches", "-1", ""},
		{"malformed", "max_spawn_depth", `"unbounded"`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			admissionRun(t, store, false, nil, 0)
			parent := admissionRecord(t, store, "run", StatusRunning, 0, 1)
			if _, err := store.db.Exec(`INSERT INTO state(key,scope_id,payload) VALUES (?, 'global', ?)`, "kernel."+tc.key, tc.value); err != nil {
				t.Fatal(err)
			}
			opts := admissionOptions(t)
			opts.ParentDispatchID = parent
			opts.Policy = &SpawnPolicy{MaxActiveGlobal: 99, MaxSpawnDepth: 99}
			err := admissionSpawn(t, store, opts)
			if tc.reason != "" {
				requireRejection(t, err, tc.reason)
			} else if err == nil {
				t.Fatal("invalid stored policy accepted")
			}
		})
	}
}

func TestReviewV2EscalationUsesAdmissionAndEligibility(t *testing.T) {
	for _, tc := range []struct{ name, agent, status, change, reason string }{
		{"cap", "codex", StatusFailed, `UPDATE runs SET max_agents=1 WHERE id='run'`, "agent_cap_per_run"},
		{"budget", "codex", StatusFailed, `UPDATE runs SET token_budget=10,budget_enforce=1 WHERE id='run'`, "budget_exceeded"},
		{"global", "codex", StatusFailed, `INSERT INTO state(key,scope_id,payload) VALUES ('kernel.global_max_dispatches','global','1'); INSERT INTO dispatches(id,project_dir,status) VALUES ('active','.', 'running')`, "concurrency_limit_global"},
		{"flere", "flere", StatusFailed, "", "not_retryable"},
		{"running", "codex", StatusRunning, "", "not_retryable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			admissionRun(t, store, false, nil, 0)
			scope := "run"
			id, err := store.Create(context.Background(), &Dispatch{AgentType: tc.agent, ProjectDir: t.TempDir(), ScopeID: &scope})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateStatus(context.Background(), id, tc.status, UpdateFields{"input_tokens": 10}); err != nil {
				t.Fatal(err)
			}
			if tc.change != "" {
				if _, err := store.db.Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			_, err = RetryWithEscalation(context.Background(), store, state.New(store.db), id, DefaultEscalationPolicy(), "review-v2", FailError, "fixture")
			requireRejection(t, err, tc.reason)
			var count int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM dispatches WHERE parent_dispatch_id=?`, id).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count != 0 {
				t.Fatalf("rejected escalation inserted %d retries", count)
			}
		})
	}
}

func TestReviewRetryReentersAdmission(t *testing.T) {
	for _, tc := range []struct{ name, change, reason string }{
		{"cap", `UPDATE runs SET max_agents=1 WHERE id='run'`, "agent_cap_per_run"},
		{"budget", `UPDATE runs SET token_budget=10,budget_enforce=1 WHERE id='run'`, "budget_exceeded"},
		{"global", `INSERT INTO state(key,scope_id,payload) VALUES ('kernel.global_max_dispatches','global','1'); INSERT INTO dispatches(id,project_dir,status) VALUES ('active','.', 'running')`, "concurrency_limit_global"},
		{"accepted", `INSERT INTO state(key,scope_id,payload) VALUES ('kernel.max_spawn_depth','global','1')`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			admissionRun(t, store, false, nil, 0)
			scope := "run"
			id, err := store.Create(context.Background(), &Dispatch{AgentType: "codex", ProjectDir: ".", ScopeID: &scope, SpawnDepth: 1})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateStatus(context.Background(), id, StatusFailed, UpdateFields{"input_tokens": 10}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(tc.change); err != nil {
				t.Fatal(err)
			}
			result, err := Retry(context.Background(), store, id, DefaultRetryPolicy())
			if tc.reason != "" {
				requireRejection(t, err, tc.reason)
			} else {
				if err != nil {
					t.Fatal(err)
				}
				retry, err := store.Get(context.Background(), result.NewID)
				if err != nil {
					t.Fatal(err)
				}
				if retry.SpawnDepth != 1 {
					t.Fatalf("retry depth increased: %d", retry.SpawnDepth)
				}
			}
		})
	}
}

func TestReviewTerminalFlereTokensAreImmutable(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	saveWorkerReceipt(t, output, receipt)
	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTokens(context.Background(), id, UpdateFields{"input_tokens": 999, "cache_hits": 500}); !errors.Is(err, ErrStaleStatus) {
		t.Fatalf("terminal mutation: %v", err)
	}
	d, _ := store.Get(context.Background(), id)
	if d.InputTokens != 13 || *d.CacheHits != 3 {
		t.Fatalf("verified totals overwritten: %+v", d)
	}
}
