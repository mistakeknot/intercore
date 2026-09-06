package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func admissionOptions(t *testing.T) SpawnOptions {
	t.Helper()
	dir := t.TempDir()
	prompt, wrapper := filepath.Join(dir, "prompt.md"), filepath.Join(dir, "dispatch.sh")
	for path, content := range map[string]string{prompt: "fixture", wrapper: "#!/bin/sh\nexit 0\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return SpawnOptions{AgentType: "codex", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper, ScopeID: "run"}
}

func admissionRun(t *testing.T, store *Store, enforce bool, budget any, cap int) {
	t.Helper()
	_, err := store.db.Exec(`INSERT INTO runs(id, project_dir, goal, budget_enforce, token_budget, max_agents) VALUES ('run', '/fixture', 'fixture', ?, ?, ?)`, enforce, budget, cap)
	if err != nil {
		t.Fatal(err)
	}
}

func admissionRecord(t *testing.T, store *Store, scope, status string, tokens, depth int) string {
	t.Helper()
	id, err := store.Create(context.Background(), &Dispatch{AgentType: "flere", ProjectDir: "/fixture", ScopeID: &scope, SpawnDepth: depth})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.db.Exec(`UPDATE dispatches SET status = ?, input_tokens = ? WHERE id = ?`, status, tokens, id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func admissionSpawn(t *testing.T, store *Store, opts SpawnOptions) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := Spawn(ctx, store, opts)
	if r != nil {
		if waitErr := r.Cmd.Wait(); waitErr != nil {
			t.Fatal(waitErr)
		}
	}
	return err
}

func requireRejection(t *testing.T, err error, reason string) {
	t.Helper()
	var rejection *SpawnRejection
	if !errors.As(err, &rejection) || rejection.Reason != reason {
		t.Fatalf("error = %v, want rejection %s", err, reason)
	}
}

func TestAdmissionPersistedBudget(t *testing.T) {
	for _, used := range []int{10, 11} {
		for _, flagged := range []bool{false, true} {
			t.Run(fmt.Sprintf("used%d/flagged%v", used, flagged), func(t *testing.T) {
				store := testStore(t)
				admissionRun(t, store, true, 10, 0)
				admissionRecord(t, store, "run", StatusCompleted, used, 0)
				if flagged {
					if _, err := store.db.Exec(`INSERT INTO state(key, scope_id, payload) VALUES ('budget.exceeded', 'run', '{}')`); err != nil {
						t.Fatal(err)
					}
				}
				opts := admissionOptions(t)
				opts.Policy = &SpawnPolicy{}
				requireRejection(t, admissionSpawn(t, store, opts), "budget_exceeded")
				count, _ := store.CountTotalByScope(context.Background(), "run")
				if count != 1 {
					t.Fatalf("rejected spawn inserted a record: %d", count)
				}
			})
		}
	}
}

func TestFlereRequiresStrictRunBinding(t *testing.T) {
	store := testStore(t)
	opts := admissionOptions(t)
	opts.AgentType = "flere"
	var input *InputError
	if err := admissionSpawn(t, store, opts); !errors.As(err, &input) {
		t.Fatalf("generic scope accepted for Flere: %v", err)
	}
	opts.RunID = "run"
	requireRejection(t, admissionSpawn(t, store, opts), "run_not_found")
	admissionRun(t, store, false, nil, 0)
	if err := admissionSpawn(t, store, opts); err != nil {
		t.Fatal(err)
	}
}

func TestAdmissionPersistedAgentCapIncludesTerminal(t *testing.T) {
	store := testStore(t)
	admissionRun(t, store, false, nil, 1)
	admissionRecord(t, store, "run", StatusFailed, 0, 0)
	opts := admissionOptions(t)
	opts.Policy = &SpawnPolicy{MaxAgentsPerRun: 100}
	requireRejection(t, admissionSpawn(t, store, opts), "agent_cap_per_run")
}

func TestAdmissionConcurrentLastSlotSeparateConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admission.db")
	const attempts = 8
	stores := make([]*Store, attempts)
	for i := range stores {
		d, err := db.Open(path, 5*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		if i == 0 {
			if err := d.Migrate(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		stores[i] = New(d.SqlDB(), nil)
	}
	admissionRun(t, stores[0], false, nil, 2)
	admissionRecord(t, stores[0], "run", StatusCompleted, 0, 0)
	opts := admissionOptions(t)
	start := make(chan struct{})
	results := make(chan error, attempts)
	var wg sync.WaitGroup
	for _, store := range stores {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			r, err := Spawn(context.Background(), s, opts)
			if r != nil {
				if waitErr := r.Cmd.Wait(); waitErr != nil {
					results <- waitErr
					return
				}
			}
			results <- err
		}(store)
	}
	close(start)
	wg.Wait()
	close(results)
	accepted := 0
	for err := range results {
		if err == nil {
			accepted++
		} else {
			requireRejection(t, err, "agent_cap_per_run")
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d concurrent last-slot requests, want 1", accepted)
	}
	count, err := stores[0].CountTotalByScope(context.Background(), "run")
	if err != nil || count != 2 {
		t.Fatalf("final count = %d, error = %v", count, err)
	}
}

type admissionBudgetFunc func(context.Context, string) (bool, error)

func (f admissionBudgetFunc) IsBudgetExceeded(ctx context.Context, run string) (bool, error) {
	return f(ctx, run)
}

func TestAdmissionBudgetFailsClosed(t *testing.T) {
	for _, budget := range []any{nil, 0} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			store := testStore(t)
			admissionRun(t, store, true, budget, 0)
			requireRejection(t, admissionSpawn(t, store, admissionOptions(t)), "budget_unavailable")
		})
	}
	t.Run("nil checker cannot authorize generic scope", func(t *testing.T) {
		store := testStore(t)
		opts := admissionOptions(t)
		opts.Policy = &SpawnPolicy{BudgetEnforce: true}
		requireRejection(t, admissionSpawn(t, store, opts), "budget_unavailable")
	})
	t.Run("failed checker", func(t *testing.T) {
		store := testStore(t)
		admissionRun(t, store, true, 10, 0)
		opts := admissionOptions(t)
		opts.Policy = &SpawnPolicy{BudgetEnforce: true}
		failure := errors.New("budget service unavailable")
		opts.BudgetQuerier = admissionBudgetFunc(func(context.Context, string) (bool, error) { return false, failure })
		if err := admissionSpawn(t, store, opts); !errors.Is(err, failure) {
			t.Fatalf("error = %v, want checker failure", err)
		}
	})
	t.Run("checker using same one-connection DB does not deadlock", func(t *testing.T) {
		store := testStore(t)
		admissionRun(t, store, true, 10, 0)
		opts := admissionOptions(t)
		opts.Policy = &SpawnPolicy{BudgetEnforce: true}
		opts.BudgetQuerier = admissionBudgetFunc(func(ctx context.Context, scope string) (bool, error) {
			_, err := store.AggregateTokens(ctx, scope)
			return false, err
		})
		if err := admissionSpawn(t, store, opts); err != nil {
			t.Fatal(err)
		}
	})
}

func TestAdmissionCancellationRollsBackAndReleasesConnection(t *testing.T) {
	store := testStore(t)
	// Hold execution inside INSERT long enough for a real context cancellation.
	// This exercises cleanup after BEGIN and before COMMIT with MaxOpenConns(1).
	_, err := store.db.Exec(`CREATE TRIGGER slow_admission BEFORE INSERT ON dispatches BEGIN
		SELECT sum(x) FROM (WITH RECURSIVE counter(x) AS (VALUES(0) UNION ALL SELECT x+1 FROM counter WHERE x<100000000) SELECT x FROM counter);
	END`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	d := &Dispatch{AgentType: "flere", ProjectDir: "/fixture"}
	_, err = store.admit(ctx, d, SpawnOptions{})
	if err == nil || ctx.Err() == nil {
		t.Fatalf("admission error=%v, context error=%v, want cancellation", err, ctx.Err())
	}
	cleanup, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if _, err := store.db.ExecContext(cleanup, `DROP TRIGGER slow_admission`); err != nil {
		t.Fatalf("connection not reusable: %v", err)
	}
	var count int
	if err := store.db.QueryRowContext(cleanup, `SELECT COUNT(*) FROM dispatches`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("cancelled admission count=%d error=%v", count, err)
	}
	if _, err := store.admit(cleanup, d, SpawnOptions{}); err != nil {
		t.Fatalf("next admission: %v", err)
	}
}

func TestAdmissionInsertFailureRollsBack(t *testing.T) {
	store := testStore(t)
	_, err := store.db.Exec(`CREATE TRIGGER reject_admission BEFORE INSERT ON dispatches BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := admissionSpawn(t, store, admissionOptions(t)); err == nil {
		t.Fatal("insert failure accepted")
	}
	if _, err := store.db.Exec(`DROP TRIGGER reject_admission`); err != nil {
		t.Fatal(err)
	}
	if err := admissionSpawn(t, store, admissionOptions(t)); err != nil {
		t.Fatalf("next admission: %v", err)
	}
}

func TestAdmissionCallerBudgetCannotWeakenPersistedBudget(t *testing.T) {
	store := testStore(t)
	admissionRun(t, store, true, 10, 0)
	admissionRecord(t, store, "run", StatusRunning, 10, 0)
	opts := admissionOptions(t)
	opts.BudgetQuerier = admissionBudgetFunc(func(context.Context, string) (bool, error) { return false, nil })
	requireRejection(t, admissionSpawn(t, store, opts), "budget_exceeded")
}

func TestAdmissionLegacyCheckerPreservesAdvisoryPolicy(t *testing.T) {
	for _, tc := range []struct {
		name              string
		stored, requested bool
	}{
		{"advisory", false, false},
		{"stored enforcement", true, false},
		{"caller enforcement", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			admissionRun(t, store, tc.stored, 100, 0)
			opts := admissionOptions(t)
			opts.Policy = &SpawnPolicy{BudgetEnforce: tc.requested}
			calls := 0
			opts.BudgetQuerier = admissionBudgetFunc(func(context.Context, string) (bool, error) {
				calls++
				return true, nil
			})
			err := admissionSpawn(t, store, opts)
			if tc.stored || tc.requested {
				requireRejection(t, err, "budget_exceeded")
				if calls != 1 {
					t.Fatalf("checker calls=%d, want 1", calls)
				}
			} else if err != nil || calls != 0 {
				t.Fatalf("advisory spawn error=%v checker calls=%d, want accepted without external veto", err, calls)
			}
		})
	}
}
func TestAdmissionParentAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name, scope, reason string
		policy              SpawnPolicy
		depth               int
	}{
		{"per scope", "run", "concurrency_limit_per_run", SpawnPolicy{MaxActivePerRun: 1}, 0},
		{"global", "other", "concurrency_limit_global", SpawnPolicy{MaxActiveGlobal: 1}, 0},
		{"depth", "run", "spawn_depth_exceeded", SpawnPolicy{MaxSpawnDepth: 1}, 1},
		{"scope inheritance", "", "agent_cap_per_run", SpawnPolicy{}, 0},
		{"forged scope", "other", "parent_scope_mismatch", SpawnPolicy{}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			cap := 0
			if tc.name == "scope inheritance" {
				cap = 1
			}
			admissionRun(t, store, false, nil, cap)
			parent := admissionRecord(t, store, "run", StatusRunning, 0, tc.depth)
			opts := admissionOptions(t)
			opts.ScopeID, opts.Policy = tc.scope, &tc.policy
			if tc.name != "global" {
				opts.ParentDispatchID = parent
			}
			requireRejection(t, admissionSpawn(t, store, opts), tc.reason)
		})
	}
	t.Run("missing parent", func(t *testing.T) {
		store := testStore(t)
		opts := admissionOptions(t)
		opts.ParentDispatchID = "missing"
		requireRejection(t, admissionSpawn(t, store, opts), "parent_not_found")
	})
	t.Run("generic scope allowed", func(t *testing.T) {
		if err := admissionSpawn(t, testStore(t), admissionOptions(t)); err != nil {
			t.Fatal(err)
		}
	})
}
