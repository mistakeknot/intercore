package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

func TestDispatchAdmissionCLI(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		flags       []string
		code        int
		reason      string
	}{
		{"budget", `INSERT INTO runs(id, project_dir, goal, token_budget, budget_enforce) VALUES ('run', '.', 'fixture', 1, 1); INSERT INTO dispatches(id, project_dir, scope_id, input_tokens) VALUES ('prior', '.', 'run', 1); INSERT INTO state(key, scope_id, payload) VALUES ('budget.exceeded', 'run', '{}');`, []string{"--scope-id=run", "--budget-enforce=false"}, 1, "budget_exceeded"},
		{"cap", `INSERT INTO runs(id, project_dir, goal, max_agents) VALUES ('run', '.', 'fixture', 1); INSERT INTO dispatches(id, project_dir, scope_id, status) VALUES ('prior', '.', 'run', 'completed');`, []string{"--scope-id=run", "--max-agents-per-run=99"}, 1, "agent_cap_per_run"},
		{"portfolio strict binding", `INSERT INTO runs(id, project_dir, goal, max_dispatches) VALUES ('portfolio', '.', 'fixture', 1); INSERT INTO runs(id, project_dir, goal, parent_run_id) VALUES ('child', '.', 'fixture', 'portfolio'); INSERT INTO state(key, scope_id, payload) VALUES ('active-dispatch-count', 'portfolio', '"1"');`, []string{"--run-id=child"}, 1, "portfolio_dispatch_limit"},
		{"missing strict run", "", []string{"--run-id=missing"}, 1, "run_not_found"},
		{"empty strict run", "", []string{"--run-id="}, 3, ""},
		{"bare strict run", "", []string{"--run-id"}, 3, ""},
		{"unscoped per-run cap", "", []string{"--max-active-per-run=1"}, 3, ""},
		{"mismatched binding", "", []string{"--run-id=run", "--scope-id=other"}, 3, ""},
		{"missing parent", "", []string{"--parent-dispatch-id=missing"}, 1, "parent_not_found"},
		{"invalid limit", "", []string{"--max-active-global=-1"}, 3, ""},
		{"missing limit value", "", []string{"--max-spawn-depth"}, 3, ""},
		{"invalid bool", "", []string{"--budget-enforce=perhaps"}, 3, ""},
		{"global cap", `INSERT INTO dispatches(id, project_dir, scope_id) VALUES ('prior', '.', 'elsewhere');`, []string{"--max-active-global=1"}, 1, "concurrency_limit_global"},
		{"DB query failure", `DROP TABLE runs;`, []string{"--scope-id=run"}, 2, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupCommandMetadataDB(t)
			flagJSON = true
			d, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			if tc.setup != "" {
				if _, err := d.SqlDB().Exec(tc.setup); err != nil {
					d.Close()
					t.Fatal(err)
				}
			}
			d.Close()
			prompt, wrapper := filepath.Join(t.TempDir(), "prompt"), filepath.Join(t.TempDir(), "dispatch.sh")
			if err := os.WriteFile(prompt, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"--prompt-file=" + prompt, "--dispatch-sh=" + wrapper}, tc.flags...)
			out := captureDispatchOutput(t, func() int {
				code := cmdDispatchSpawn(context.Background(), args)
				if code != tc.code {
					t.Errorf("exit=%d, want %d", code, tc.code)
				}
				return 0
			})
			if tc.reason != "" {
				var got struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(out, &got); err != nil {
					t.Fatalf("invalid JSON %q: %v", out, err)
				}
				if got.Reason != tc.reason {
					t.Fatalf("reason=%q, want %q", got.Reason, tc.reason)
				}
			}
		})
	}
}

func TestRunAutoSpawnBindsAdmissionToOriginatingRun(t *testing.T) {
	for _, cap := range []int{0, 1} {
		t.Run(string(rune('0'+cap)), func(t *testing.T) {
			setupCommandMetadataDB(t)
			root, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			promptDir := filepath.Join(root, ".ic", "prompts")
			if err := os.MkdirAll(promptDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(promptDir, "fixture.md"), []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			wrapper := filepath.Join(root, "dispatch.sh")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("CLAVAIN_DISPATCH_SH", wrapper)
			d, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.SqlDB().Exec(`INSERT INTO runs(id, project_dir, goal, phase, max_agents) VALUES ('run', ?, 'fixture', 'planned', ?)`, root, cap); err != nil {
				t.Fatal(err)
			}
			if _, err := d.SqlDB().Exec(`INSERT INTO run_agents(id, run_id, agent_type, name) VALUES ('agent', 'run', 'flere', 'fixture'); INSERT INTO dispatches(id, project_dir, scope_id, status) VALUES ('prior', '.', 'run', 'completed')`); err != nil {
				t.Fatal(err)
			}
			if code := cmdRunAdvance(context.Background(), []string{"run", "--disable-gates"}); code != 0 {
				t.Fatalf("advance exit=%d", code)
			}
			rows, err := d.SqlDB().Query(`SELECT COALESCE(scope_id, ''), pid FROM dispatches WHERE id != 'prior'`)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			count := 0
			for rows.Next() {
				var scope string
				var pid int
				if err := rows.Scan(&scope, &pid); err != nil {
					t.Fatal(err)
				}
				if p, err := os.FindProcess(pid); err == nil {
					_, _ = p.Wait()
				}
				count++
				if scope != "run" {
					t.Errorf("auto-spawn scope=%q, want run", scope)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			want := 1 - cap
			if count != want {
				t.Fatalf("auto-spawn count=%d, want %d", count, want)
			}
		})
	}
}

// The CLI persists jobs; there is no production executor in this repository.
// This fixture exercises the required executor boundary using persisted options.
func TestScheduledDispatchPreservesAdmissionAtExecution(t *testing.T) {
	for _, tc := range []struct{ name, change, reason string }{
		{"accepted", "", ""},
		{"budget changed after enqueue", `UPDATE runs SET token_budget=1 WHERE id='run'; UPDATE dispatches SET input_tokens=1 WHERE id='parent'`, "budget_exceeded"},
		{"cap changed after enqueue", `UPDATE runs SET max_agents=1 WHERE id='run'`, "agent_cap_per_run"},
		{"parent depth changed after enqueue", `UPDATE dispatches SET spawn_depth=1 WHERE id='parent'`, "spawn_depth_exceeded"},
		{"run removed after enqueue", `DELETE FROM runs WHERE id='run'`, "run_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setupCommandMetadataDB(t)
			flagJSON = true
			d, err := openDB()
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err := d.SqlDB().Exec(`INSERT INTO runs(id, project_dir, goal, token_budget, budget_enforce, max_agents) VALUES ('run', '.', 'fixture', 100, 1, 2); INSERT INTO dispatches(id, project_dir, scope_id, status) VALUES ('parent', '.', 'run', 'completed')`); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			prompt, wrapper, marker := filepath.Join(dir, "prompt"), filepath.Join(dir, "dispatch.sh"), filepath.Join(dir, "launched")
			if err := os.WriteFile(prompt, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n: > \"$IC_ADMISSION_MARKER\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("IC_ADMISSION_MARKER", marker)
			out := captureDispatchOutput(t, func() int {
				return cmdDispatchSpawn(context.Background(), []string{"--scheduled", "--type=flere", "--prompt-file=" + prompt, "--dispatch-sh=" + wrapper, "--project=" + dir, "--run-id=run", "--parent-dispatch-id=parent", "--max-spawn-depth=1", "--max-agents-per-run=5"})
			})
			var queued struct {
				ID        string `json:"job_id"`
				Scheduled bool   `json:"scheduled"`
			}
			if err := json.Unmarshal(out, &queued); err != nil || !queued.Scheduled || queued.ID == "" {
				t.Fatalf("queue response=%s error=%v", out, err)
			}
			job, err := scheduler.NewStore(d.SqlDB()).Get(context.Background(), queued.ID)
			if err != nil {
				t.Fatal(err)
			}
			var opts dispatch.SpawnOptions
			if err := scheduler.UnmarshalSpawnOpts(job.SpawnOpts, &opts); err != nil {
				t.Fatal(err)
			}
			if opts.RunID != "run" || opts.ScopeID != "run" || opts.ParentDispatchID != "parent" || opts.Policy == nil || opts.Policy.MaxSpawnDepth != 1 || opts.Policy.MaxAgentsPerRun != 5 {
				t.Fatalf("lost queued admission options: %+v", opts)
			}
			if tc.change != "" {
				if _, err := d.SqlDB().Exec(tc.change); err != nil {
					t.Fatal(err)
				}
			}
			store := dispatch.New(d.SqlDB(), nil)
			err = scheduler.DispatchExecutor(store)(context.Background(), job)
			if tc.reason != "" {
				var rejection *dispatch.SpawnRejection
				if !errors.As(err, &rejection) || rejection.Reason != tc.reason {
					t.Fatalf("error=%v, want %s", err, tc.reason)
				}
				if _, err := os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("rejected job launched child: %v", err)
				}
			} else {
				// The fake Flere launcher has no native receipt, so the real
				// executor must preserve its launch as a failed, indeterminate
				// attempt and prohibit scheduling a replay.
				if err == nil || job.CanRetry() {
					t.Fatal("missing terminal proof was accepted or retryable")
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatal(err)
				}
				child, err := store.Get(context.Background(), job.DispatchID)
				if err != nil {
					t.Fatal(err)
				}
				if child.ScopeID == nil || *child.ScopeID != "run" || child.ParentDispatchID != "parent" || child.SpawnDepth != 1 {
					t.Fatalf("lost child binding: %+v", child)
				}
				if child.Status != dispatch.StatusFailed || child.QuarantineReason == nil || *child.QuarantineReason != dispatch.WorkerOutcomeIndeterminate {
					t.Fatalf("missing proof collection: %+v", child)
				}
			}
		})
	}
}

func TestSchedulerDispatchExecutorDoesNotReplayIndeterminateWorker(t *testing.T) {
	setupCommandMetadataDB(t)
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.SqlDB().Exec(`INSERT INTO runs(id, project_dir, goal) VALUES ('run', '.', 'fixture')`); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	prompt, wrapper := filepath.Join(dir, "prompt"), filepath.Join(dir, "dispatch.sh")
	if err := os.WriteFile(prompt, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nexit 1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	opts, err := scheduler.MarshalSpawnOpts(dispatch.SpawnOptions{AgentType: "flere", RunID: "run", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper})
	if err != nil {
		t.Fatal(err)
	}
	job := scheduler.NewSpawnJob("", scheduler.JobTypeDispatch, "fixture")
	job.AgentType, job.SpawnOpts = "flere", opts
	completed := make(chan *scheduler.SpawnJob, 1)
	job.Callback = func(job *scheduler.SpawnJob) { completed <- job.Clone() }
	cfg := scheduler.DefaultConfig()
	cfg.DefaultRetries = 5
	s := scheduler.New(cfg)
	s.SetExecutor(scheduler.DispatchExecutor(dispatch.New(d.SqlDB(), nil)))
	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	if err := s.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-completed:
		if result.Status != scheduler.StatusFailed || result.RetryCount != 0 || result.DispatchID == "" {
			t.Fatalf("unexpected job %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler never completed bounded job")
	}
	var count int
	if err := d.SqlDB().QueryRow("SELECT COUNT(*) FROM dispatches WHERE scope_id='run'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("dispatches=%d err=%v", count, err)
	}
}

func TestReviewScheduledDefaultBackendExecutes(t *testing.T) {
	setupCommandMetadataDB(t)
	flagJSON = true
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	dir := t.TempDir()
	prompt, wrapper := filepath.Join(dir, "prompt"), filepath.Join(dir, "wrapper")
	if err := os.WriteFile(prompt, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nwhile [ \"$#\" -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; shift; fi; shift; done\nprintf 'STATUS: pass\\n' > \"$out.verdict\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out := captureDispatchOutput(t, func() int {
		return cmdDispatchSpawn(context.Background(), []string{"--scheduled", "--prompt-file=" + prompt, "--project=" + dir, "--dispatch-sh=" + wrapper})
	})
	var response struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		t.Fatal(err)
	}
	job, err := scheduler.NewStore(d.SqlDB()).Get(context.Background(), response.JobID)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.DispatchExecutor(dispatch.New(d.SqlDB(), nil))(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	spawned, err := dispatch.New(d.SqlDB(), nil).Get(context.Background(), job.DispatchID)
	if err != nil {
		t.Fatal(err)
	}
	if spawned.AgentType != "codex" || spawned.Status != dispatch.StatusCompleted {
		t.Fatalf("default backend not preserved: %+v", spawned)
	}
}
