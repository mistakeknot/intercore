package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// hookFixture is a fake dispatch.sh that records that it ran and the
// environment it was given, next to itself.
type hookFixture struct {
	dir     string
	wrapper string
	opts    SpawnOptions
}

func newHookFixture(t *testing.T) hookFixture {
	t.Helper()
	dir := t.TempDir()
	wrapper := filepath.Join(dir, "dispatch.sh")
	script := "#!/bin/sh\ndir=$(dirname \"$0\")\ntouch \"$dir/ran\"\nenv > \"$dir/env.out\"\nexit 0\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return hookFixture{dir: dir, wrapper: wrapper, opts: SpawnOptions{
		AgentType: "codex", ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper,
		OutputFile: filepath.Join(dir, "out.md"),
	}}
}

func (f hookFixture) ran() bool {
	_, err := os.Stat(filepath.Join(f.dir, "ran"))
	return err == nil
}

func (f hookFixture) env(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "env.out"))
	if err != nil {
		t.Fatalf("worker never wrote its environment: %v", err)
	}
	return string(raw)
}

func spawnAndWait(t *testing.T, store *Store, opts SpawnOptions) (*SpawnResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := Spawn(ctx, store, opts)
	if r != nil {
		_ = r.Cmd.Wait()
	}
	return r, err
}

func dispatchCount(t *testing.T, store *Store) int {
	t.Helper()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM dispatches`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func hookProbeTable(t *testing.T, store *Store) {
	t.Helper()
	if _, err := store.db.Exec(`CREATE TABLE hook_probe(job TEXT PRIMARY KEY, dispatch_id TEXT, epoch INTEGER)`); err != nil {
		t.Fatal(err)
	}
}

// Unit test 4: OnAdmitted.

func TestOnAdmittedRunsInsideAdmissionBeforeStart(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	var hookID string
	var sawRow, workerRanDuringHook bool
	f.opts.OnAdmitted = func(ctx context.Context, q Querier, id string) error {
		hookID = id
		var n int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM dispatches WHERE id=?`, id).Scan(&n); err != nil {
			return err
		}
		sawRow = n == 1
		workerRanDuringHook = f.ran()
		return nil
	}
	r, err := spawnAndWait(t, store, f.opts)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if hookID == "" || hookID != r.ID {
		t.Fatalf("hook saw id %q, Spawn returned %q", hookID, r.ID)
	}
	if !sawRow {
		t.Fatal("the generated dispatch row was not visible to the hook on the admission connection")
	}
	if workerRanDuringHook {
		t.Fatal("the worker had started before the hook ran")
	}
	if !f.ran() {
		t.Fatal("worker did not run after the hook succeeded")
	}
}

func TestOnAdmittedWritesCommitWithTheDispatchRow(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	hookProbeTable(t, store)
	f.opts.OnAdmitted = func(ctx context.Context, q Querier, id string) error {
		_, err := q.ExecContext(ctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', ?, 7)`, id)
		return err
	}
	r, err := spawnAndWait(t, store, f.opts)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	var got string
	if err := store.db.QueryRow(`SELECT dispatch_id FROM hook_probe WHERE job='j1'`).Scan(&got); err != nil || got != r.ID {
		t.Fatalf("hook write = %q, %v; want %q", got, err, r.ID)
	}
}

func TestOnAdmittedErrorRollsBackDispatchAndStartsNothing(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	hookProbeTable(t, store)
	sentinel := errors.New("claim lost")
	f.opts.OnAdmitted = func(ctx context.Context, q Querier, id string) error {
		// The hook's own write must roll back with the dispatch row.
		if _, err := q.ExecContext(ctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', ?, 7)`, id); err != nil {
			return err
		}
		return sentinel
	}
	r, err := spawnAndWait(t, store, f.opts)
	if r != nil {
		t.Fatalf("Spawn returned a result %+v for a refused hook", r)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("Spawn error %v does not wrap the hook's error", err)
	}
	var rej *SpawnRejection
	if errors.As(err, &rej) {
		t.Fatalf("a hook error must not look like an admission rejection: %v", err)
	}
	if n := dispatchCount(t, store); n != 0 {
		t.Fatalf("%d dispatch rows survive a refused hook", n)
	}
	var probes int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM hook_probe`).Scan(&probes); err != nil || probes != 0 {
		t.Fatalf("hook write survived rollback: %d rows, %v", probes, err)
	}
	if f.ran() {
		t.Fatal("a process started for a refused hook")
	}
	// The single connection is usable again: a later spawn is admitted.
	f.opts.OnAdmitted = nil
	if _, err := spawnAndWait(t, store, f.opts); err != nil {
		t.Fatalf("spawn after a refused hook: %v", err)
	}
}

// A guarded link that matches no row (stale token, an already set linked_at,
// or a job that is now cancelling) is the hook's refusal and must abort
// admission. The scratch table stands in for scheduler_jobs; the runner's own
// hook is slice 3.
func TestOnAdmittedGuardedLinkRefusalAborts(t *testing.T) {
	cases := []struct {
		name  string
		setup string
		token string
	}{
		{"stale token", `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', NULL, 7)`, "other"},
		{"link already set", `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', 'old', 7)`, "mine"},
		{"job cancelling", `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', NULL, 8)`, "mine"},
	}
	errClaimLost := errors.New("claim lost")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, f := testStore(t), newHookFixture(t)
			hookProbeTable(t, store)
			if _, err := store.db.Exec(tc.setup); err != nil {
				t.Fatal(err)
			}
			f.opts.OnAdmitted = func(ctx context.Context, q Querier, id string) error {
				res, err := q.ExecContext(ctx, `UPDATE hook_probe SET dispatch_id=? WHERE job='j1' AND dispatch_id IS NULL AND epoch=7 AND ?='mine'`, id, tc.token)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n != 1 {
					return errClaimLost
				}
				return nil
			}
			if _, err := spawnAndWait(t, store, f.opts); !errors.Is(err, errClaimLost) {
				t.Fatalf("err = %v, want the hook's refusal", err)
			}
			if dispatchCount(t, store) != 0 || f.ran() {
				t.Fatal("a refused link left a dispatch row or a process")
			}
		})
	}
}

func TestOnAdmittedPanicDoesNotLeaveTransactionOpen(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	f.opts.OnAdmitted = func(context.Context, Querier, string) error { panic("boom") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("hook panic was swallowed")
			}
		}()
		_, _ = spawnAndWait(t, store, f.opts)
	}()
	if n := dispatchCount(t, store); n != 0 {
		t.Fatalf("%d dispatch rows after a hook panic", n)
	}
	f.opts.OnAdmitted = nil
	if _, err := spawnAndWait(t, store, f.opts); err != nil {
		t.Fatalf("spawn after a hook panic: %v", err)
	}
}

// A hook may issue a query and leave the row unscanned, then fail or panic.
// An open row holds the owned connection, so admission cleanup must not wait
// on it forever: the next spawn on the single-connection store must work.
func TestOnAdmittedUnscannedRowDoesNotWedgeAdmission(t *testing.T) {
	sentinel := errors.New("hook failed")
	// Each hook leaves a row unscanned, one on the hook's own context and one
	// on a context of its own, which the hook's lifetime must still end.
	for _, own := range []bool{false, true} {
		queryCtx := func(ctx context.Context) context.Context {
			if own {
				return context.Background()
			}
			return ctx
		}
		cases := []struct {
			name       string
			hook       func(context.Context, Querier, string) error
			wantErr    error
			wantPanic  bool
			wantCommit bool
		}{
			{"error", func(ctx context.Context, q Querier, _ string) error {
				_ = q.QueryRowContext(queryCtx(ctx), "SELECT 1")
				return sentinel
			}, sentinel, false, false},
			{"panic", func(ctx context.Context, q Querier, _ string) error {
				_ = q.QueryRowContext(queryCtx(ctx), "SELECT 1")
				panic("boom")
			}, nil, true, false},
			{"success", func(ctx context.Context, q Querier, id string) error {
				_ = q.QueryRowContext(queryCtx(ctx), "SELECT 1")
				_, err := q.ExecContext(queryCtx(ctx), `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('j1', ?, 7)`, id)
				return err
			}, nil, false, true},
		}
		for _, c := range cases {
			name := c.name
			if own {
				name += "/own-context"
			}
			t.Run(name, func(t *testing.T) {
				store, f := testStore(t), newHookFixture(t)
				hookProbeTable(t, store)
				f.opts.OnAdmitted = c.hook
				// No short deadline: only admission's own cleanup can free the
				// connection, so a stall is the bug.
				run := func() (r *SpawnResult, err error, panicked bool) {
					defer func() {
						if recover() != nil {
							panicked = true
						}
					}()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					r, err = Spawn(ctx, store, f.opts)
					if r != nil {
						_ = r.Cmd.Wait()
					}
					return r, err, false
				}
				start := time.Now()
				r, err, panicked := run()
				if d := time.Since(start); d > 3*time.Second {
					t.Fatalf("admission stalled %v on an unscanned hook row", d)
				}
				if panicked != c.wantPanic {
					t.Fatalf("panicked = %v, want %v", panicked, c.wantPanic)
				}
				if c.wantErr != nil && !errors.Is(err, c.wantErr) {
					t.Fatalf("err = %v, want %v", err, c.wantErr)
				}
				if c.wantCommit {
					if err != nil || r == nil {
						t.Fatalf("Spawn = %v, %v; want a committed spawn", r, err)
					}
					if !f.ran() {
						t.Fatal("worker did not start after a successful hook")
					}
					var got string
					if err := store.db.QueryRow(`SELECT dispatch_id FROM hook_probe WHERE job='j1'`).Scan(&got); err != nil || got != r.ID {
						t.Fatalf("hook write = %q, %v; want %q", got, err, r.ID)
					}
					if n := dispatchCount(t, store); n != 1 {
						t.Fatalf("%d dispatch rows, want 1", n)
					}
				} else if n := dispatchCount(t, store); n != 0 || f.ran() {
					t.Fatalf("%d dispatch rows, ran=%v after a failed hook", n, f.ran())
				}
				f.opts.OnAdmitted = nil
				start = time.Now()
				if _, err, _ := run(); err != nil {
					t.Fatalf("spawn after the hook: %v", err)
				}
				if d := time.Since(start); d > 3*time.Second {
					t.Fatalf("next spawn stalled %v", d)
				}
			})
		}
	}
}

// A hook that stashes its Querier and calls it the moment the hook returns,
// while the connection is still open and admission is about to commit or roll
// back, must not be able to run SQL: it would persist outside the transaction.
func TestOnAdmittedQuerierIsDeadTheMomentTheHookReturns(t *testing.T) {
	store := testStore(t)
	hookProbeTable(t, store)
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500; i++ {
		var kept Querier
		hook := func(_ context.Context, q Querier, _ string) error { kept = q; return nil }
		if err := runAdmittedHook(ctx, conn, hook, "d"); err != nil {
			t.Fatal(err)
		}
		if _, err := kept.ExecContext(ctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES (?, 'late', 1)`, fmt.Sprint("j", i)); err == nil {
			t.Fatalf("iteration %d: a retained Querier executed SQL after the hook returned", i)
		}
		var n int
		if err := kept.QueryRowContext(ctx, "SELECT 1").Scan(&n); err == nil {
			t.Fatalf("iteration %d: a retained Querier ran a query after the hook returned", i)
		}
	}
	// The store has one connection: release ours before using the store.
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM hook_probe`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("%d late rows persisted (err %v)", rows, err)
	}
}

// A goroutine the hook started may still be writing when the hook returns.
// Admission then rolls back (or commits); a write landing after that would
// persist outside the transaction. Once the hook returns, no hook SQL may run,
// so after the rollback nothing the hook wrote can exist.
func TestOnAdmittedGoroutineCannotWriteAfterTheHookReturns(t *testing.T) {
	store := testStore(t)
	hookProbeTable(t, store)
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i := 0; i < 30; i++ {
		if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		started := make(chan struct{})
		hook := func(_ context.Context, q Querier, _ string) error {
			wg.Add(1)
			go func() {
				defer wg.Done()
				close(started)
				for n := 0; ; n++ {
					job := fmt.Sprintf("j%d-%d", i, n)
					if _, err := q.ExecContext(context.Background(), `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES (?, 'g', 1)`, job); err != nil {
						return
					}
				}
			}()
			<-started
			return nil
		}
		// The goroutine may be mid-write when the hook returns; then the hook is
		// refused (see TestOnAdmittedReturnWithQueryInFlightFailsClosed).
		if err := runAdmittedHook(ctx, conn, hook, "d"); err != nil && !strings.Contains(err.Error(), "still running") {
			t.Fatal(err)
		}
		// Interrupting a write inside an explicit transaction makes SQLite end the
		// transaction itself, so the rollback may find none to roll back.
		if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil && !errNoTransaction(err) {
			t.Fatal(err)
		}
		wg.Wait()
		var n int
		if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM hook_probe`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("iteration %d: %d hook rows survived the rollback", i, n)
		}
	}
}

// A goroutine the hook started may be inside a long query, under a context of
// its own, when the hook returns or panics. Cleanup must interrupt that query,
// not wait for it: admission has to go on to roll back and free the connection.
func TestOnAdmittedInFlightQueryIsInterruptedAtHookEnd(t *testing.T) {
	const forever = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT count(*) FROM c`
	for _, panics := range []bool{false, true} {
		name := "return"
		if panics {
			name = "panic"
		}
		t.Run(name, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			conn, err := store.db.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
				t.Fatal(err)
			}
			var wg sync.WaitGroup
			started := make(chan struct{})
			hook := func(_ context.Context, q Querier, _ string) error {
				wg.Add(1)
				go func() {
					defer wg.Done()
					close(started)
					var n int
					_ = q.QueryRowContext(context.Background(), forever).Scan(&n)
				}()
				<-started
				time.Sleep(100 * time.Millisecond) // let the query start running
				if panics {
					panic("boom")
				}
				return nil
			}
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				defer func() { _ = recover() }()
				_ = runAdmittedHook(ctx, conn, hook, "d")
			}()
			select {
			case <-finished:
			case <-time.After(10 * time.Second):
				t.Fatal("hook cleanup waited on an in-flight query instead of interrupting it")
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the hook's query goroutine never ended")
			}
			if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatalf("rollback after the hook: %v", err)
			}
		})
	}
}

// foreverInsert is a write that never finishes on its own.
const foreverInsert = `INSERT INTO hook_probe(job, dispatch_id, epoch) WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c) SELECT 'f' || x, 'd', 1 FROM c`

// waitRunning blocks until n of the Querier's calls are running.
func waitRunning(t *testing.T, q Querier, n int) {
	t.Helper()
	hq := q.(*hookQuerier)
	deadline := time.Now().Add(5 * time.Second)
	for {
		hq.mu.Lock()
		got := hq.running
		hq.mu.Unlock()
		if got >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d queries started", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

// A hook that returns nil while one of its queries is still running has not
// finished. Interrupting that query can end the admission transaction by
// itself, so whether it committed would depend on timing: it must be refused.
func TestOnAdmittedReturnWithQueryInFlightFailsClosed(t *testing.T) {
	store := testStore(t)
	hookProbeTable(t, store)
	ctx := context.Background()
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	hook := func(_ context.Context, q Querier, _ string) error {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = q.ExecContext(context.Background(), foreverInsert)
		}()
		waitRunning(t, q, 1)
		return nil
	}
	err = runAdmittedHook(ctx, conn, hook, "d")
	wg.Wait()
	if err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("a hook that left a query running returned %v; want a refusal", err)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil && !errNoTransaction(err) {
		t.Fatal(err)
	}
}

// When an interrupted hook write has already ended the admission transaction,
// admission's own rollback finds none. That leaves a clean connection, which
// must not be discarded: a per-connection setting made by the hook survives.
func TestOnAdmittedInterruptedWriteKeepsTheConnection(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	hookProbeTable(t, store)
	sentinel := errors.New("claim lost")
	var wg sync.WaitGroup
	f.opts.OnAdmitted = func(ctx context.Context, q Querier, id string) error {
		if _, err := q.ExecContext(ctx, `PRAGMA recursive_triggers = 1`); err != nil {
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = q.ExecContext(context.Background(), foreverInsert)
		}()
		waitRunning(t, q, 1)
		return sentinel
	}
	_, err := spawnAndWait(t, store, f.opts)
	wg.Wait()
	if !errors.Is(err, sentinel) {
		t.Fatalf("Spawn error %v does not wrap the hook's error", err)
	}
	if n := dispatchCount(t, store); n != 0 {
		t.Fatalf("%d dispatch rows survive a refused hook", n)
	}
	var probes int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM hook_probe`).Scan(&probes); err != nil || probes != 0 {
		t.Fatalf("hook writes survived: %d rows, %v", probes, err)
	}
	var like int
	if err := store.db.QueryRow(`PRAGMA recursive_triggers`).Scan(&like); err != nil {
		t.Fatal(err)
	}
	if like != 1 {
		t.Fatal("admission discarded a healthy connection after the interrupt had already rolled back")
	}
}

func TestErrNoTransaction(t *testing.T) {
	if !errNoTransaction(errors.New("SQL logic error: cannot rollback - no transaction is active (1)")) {
		t.Fatal("the driver's message was not recognised")
	}
	if errNoTransaction(nil) || errNoTransaction(errors.New("database is locked")) {
		t.Fatal("an unrelated error was taken for an ended transaction")
	}
}

func TestOnAdmittedNotCalledWhenAdmissionRejects(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	called := false
	f.opts.OnAdmitted = func(context.Context, Querier, string) error { called = true; return nil }
	f.opts.Policy = &SpawnPolicy{MaxActiveGlobal: 1}
	if _, err := store.Create(context.Background(), &Dispatch{AgentType: "codex", ProjectDir: f.dir}); err != nil {
		t.Fatal(err)
	}
	_, err := spawnAndWait(t, store, f.opts)
	var rej *SpawnRejection
	if !errors.As(err, &rej) {
		t.Fatalf("err = %v, want a capacity rejection", err)
	}
	if called {
		t.Fatal("hook ran although admission rejected the spawn")
	}
}

// The hook, ExtraEnv, StrictExecutor and CgroupFD are process-local launch
// controls. A persisted spawn_opts document must not be able to set them.
func TestLaunchControlsAreNeverSerialized(t *testing.T) {
	opts := SpawnOptions{
		AgentType: "codex", ProjectDir: "/p", PromptFile: "/q",
		OnAdmitted:     func(context.Context, Querier, string) error { return nil },
		ExtraEnv:       map[string]string{"IC_SCHEDULER_JOB_ID": "1"},
		StrictExecutor: true, CgroupFD: 9,
	}
	raw, err := json.Marshal(opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"OnAdmitted", "ExtraEnv", "StrictExecutor", "CgroupFD"} {
		if strings.Contains(string(raw), key) {
			t.Fatalf("%s is serialized: %s", key, raw)
		}
	}
	var back SpawnOptions
	hostile := `{"AgentType":"codex","ExtraEnv":{"X":"1"},"StrictExecutor":true,"CgroupFD":9}`
	if err := json.Unmarshal([]byte(hostile), &back); err != nil {
		t.Fatal(err)
	}
	if back.ExtraEnv != nil || back.StrictExecutor || back.CgroupFD != 0 {
		t.Fatalf("persisted options set a launch control: %+v", back)
	}
}

// ExtraEnv.

func TestExtraEnvReachesWorkerAndKeepsAmbientEnvironment(t *testing.T) {
	t.Setenv("IC_TEST_AMBIENT", "kept")
	store, f := testStore(t), newHookFixture(t)
	f.opts.ExtraEnv = map[string]string{"IC_SCHEDULER_JOB_ID": "42", "IC_SCHEDULER_ATTEMPT": "3"}
	r, err := spawnAndWait(t, store, f.opts)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	env := f.env(t)
	for _, want := range []string{"IC_SCHEDULER_JOB_ID=42\n", "IC_SCHEDULER_ATTEMPT=3\n", "IC_TEST_AMBIENT=kept\n", "IC_DISPATCH_ID=" + r.ID + "\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("worker environment lacks %q", strings.TrimSpace(want))
		}
	}
}

func TestExtraEnvCannotOverrideDispatchIdentity(t *testing.T) {
	for _, key := range []string{"IC_RUN_ID", "IC_DISPATCH_ID", "CLAVAIN_DISPATCH_ID", "IC_DISPATCH_ATTEMPT", "IC_PROMPT_HASH"} {
		t.Run(key, func(t *testing.T) {
			store, f := testStore(t), newHookFixture(t)
			f.opts.ExtraEnv = map[string]string{key: "forged"}
			_, err := spawnAndWait(t, store, f.opts)
			var in *InputError
			if !errors.As(err, &in) {
				t.Fatalf("err = %v, want an InputError", err)
			}
			if dispatchCount(t, store) != 0 || f.ran() {
				t.Fatal("a rejected ExtraEnv still admitted or started a dispatch")
			}
		})
	}
}

func TestExtraEnvRejectsMalformedEntries(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"empty key":    {"": "v"},
		"equals":       {"A=B": "v"},
		"leading num":  {"1A": "v"},
		"space":        {"A B": "v"},
		"nul in value": {"A": "v\x00w"},
	} {
		t.Run(name, func(t *testing.T) {
			opts := SpawnOptions{ProjectDir: "/p", PromptFile: "/q", ExtraEnv: env}
			var in *InputError
			if err := opts.Validate(); !errors.As(err, &in) {
				t.Fatalf("Validate = %v, want an InputError", err)
			}
		})
	}
}

// Unit test 16: strict executor.

func TestStrictExecutorResolvesOnlyTheGivenPath(t *testing.T) {
	other := filepath.Join(t.TempDir(), "other-dispatch.sh")
	if err := os.WriteFile(other, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAVAIN_DISPATCH_SH", other)
	// A monorepo layout above the working directory.
	root := t.TempDir()
	scripts := filepath.Join(root, "os", "Clavain", "scripts")
	if err := os.MkdirAll(scripts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "dispatch.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)

	given := "/nonexistent/claimed/dispatch.sh"
	if got := resolveStrictDispatchSH(given); got != given {
		t.Fatalf("strict resolve = %q, want exactly %q even when it does not exist", got, given)
	}
	if got := resolveStrictDispatchSH(""); got != "" {
		t.Fatalf("strict resolve of an empty path = %q, want empty (no search)", got)
	}
	// The permissive resolver still falls back, so the contrast is real.
	if got := resolveDispatchSH(given); got == given || got == "" {
		t.Fatalf("permissive resolve = %q, want a fallback to env or monorepo", got)
	}
}

func TestStrictExecutorRunsExactlyTheClaimedPath(t *testing.T) {
	store, f := testStore(t), newHookFixture(t)
	decoy := newHookFixture(t)
	t.Setenv("CLAVAIN_DISPATCH_SH", decoy.wrapper)
	f.opts.StrictExecutor = true
	if _, err := spawnAndWait(t, store, f.opts); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !f.ran() {
		t.Fatal("the claimed executor did not run")
	}
	if decoy.ran() {
		t.Fatal("CLAVAIN_DISPATCH_SH ran in strict mode")
	}
}

func TestStrictExecutorMissingOrNotExecutableIsSpawnFailedNoFallback(t *testing.T) {
	// A bare codex on PATH must never run, nor the env or monorepo dispatch.sh.
	bin := t.TempDir()
	codexRan := filepath.Join(bin, "codex-ran")
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\ntouch "+codexRan+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	decoy := newHookFixture(t)
	t.Setenv("CLAVAIN_DISPATCH_SH", decoy.wrapper)

	missing := func(f hookFixture) string { return filepath.Join(f.dir, "gone.sh") }
	notExec := func(f hookFixture) string {
		p := filepath.Join(f.dir, "plain.sh")
		if err := os.WriteFile(p, []byte("#!/bin/sh\ntouch \"$(dirname \"$0\")/ran\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	dir := func(f hookFixture) string { return f.dir }
	for name, path := range map[string]func(hookFixture) string{"missing": missing, "not executable": notExec, "directory": dir} {
		t.Run(name, func(t *testing.T) {
			store, f := testStore(t), newHookFixture(t)
			f.opts.StrictExecutor = true
			f.opts.DispatchSH = path(f)
			r, err := spawnAndWait(t, store, f.opts)
			if err == nil || r != nil {
				t.Fatalf("Spawn = %v, %v; want an error and no result", r, err)
			}
			var status string
			var class sql.NullString
			var pid sql.NullInt64
			if err := store.db.QueryRow(`SELECT d.status, t.failure_class, d.pid FROM dispatches d LEFT JOIN dispatch_terminals t ON t.dispatch_id = d.id`).Scan(&status, &class, &pid); err != nil {
				t.Fatalf("expected exactly one dispatch row: %v", err)
			}
			if status != StatusFailed || class.String != "spawn_failed" {
				t.Fatalf("dispatch = status %q class %q; want failed/spawn_failed", status, class.String)
			}
			if pid.Valid {
				t.Fatalf("a process was recorded (pid %d) for an executor that failed the check", pid.Int64)
			}
			if f.ran() || decoy.ran() {
				t.Fatal("an executor ran")
			}
			if _, statErr := os.Stat(codexRan); statErr == nil {
				t.Fatal("bare codex ran as a fallback")
			}
		})
	}
}

func TestStrictExecutorRequiresAnAbsolutePath(t *testing.T) {
	for _, path := range []string{"", "dispatch.sh", "./dispatch.sh", "../dispatch.sh"} {
		opts := SpawnOptions{ProjectDir: "/p", PromptFile: "/q", StrictExecutor: true, DispatchSH: path}
		var in *InputError
		if err := opts.Validate(); !errors.As(err, &in) {
			t.Errorf("DispatchSH %q: Validate = %v, want an InputError", path, err)
		}
	}
	ok := SpawnOptions{ProjectDir: "/p", PromptFile: "/q", StrictExecutor: true, DispatchSH: "/abs/dispatch.sh"}
	if err := ok.Validate(); err != nil {
		t.Errorf("absolute strict path: %v", err)
	}
}

func TestNonStrictBehaviourUnchanged(t *testing.T) {
	// Without StrictExecutor a missing explicit path still falls through to CLAVAIN_DISPATCH_SH.
	store, f := testStore(t), newHookFixture(t)
	decoy := newHookFixture(t)
	t.Setenv("CLAVAIN_DISPATCH_SH", decoy.wrapper)
	f.opts.DispatchSH = filepath.Join(f.dir, "gone.sh")
	if _, err := spawnAndWait(t, store, f.opts); err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if !decoy.ran() {
		t.Fatal("the existing env fallback no longer works")
	}
}

// slowQuery runs for several seconds unless something interrupts it.
const slowQuery = `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c LIMIT 5000000) SELECT sum(x) FROM c`

func hookTx(t *testing.T) (*Store, *sql.Conn) {
	t.Helper()
	store := testStore(t)
	hookProbeTable(t, store)
	conn, err := store.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	return store, conn
}

// The driver sends one interrupt per call, and SQLite drops an interrupt that
// arrives before it has started the statement. A call is cancelled after the
// driver has armed its interrupt and before SQLite has started the statement,
// so its one interrupt lands in that gap. The call is a two-statement script,
// and the driver prepares the second statement after it has armed. The seam
// (hooks_seam_test.go) holds the call there, in the planning of the second
// statement, with channels and no timing, and releases it only once the
// interrupt has reached the connection. Without the pin the statement
// then runs to the end; with the pin the interrupt stays pending and the
// statement ends before it reads a row. Build with -tags nopin for the control.
func TestOnAdmittedLostInterruptCannotHoldTheShutdown(t *testing.T) {
	registerSeamModule(t)
	_, conn := hookTx(t)
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx, "CREATE VIRTUAL TABLE temp.seam USING seamgate"); err != nil {
		t.Fatal(err)
	}
	var raw interface{ IsValid() bool }
	if err := conn.Raw(func(dc any) error {
		var ok bool
		raw, ok = dc.(interface{ IsValid() bool })
		if !ok {
			return fmt.Errorf("driver connection %T cannot report an interrupt", dc)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		s := &seamState{entered: make(chan struct{}), delivered: func() bool { return !raw.IsValid() }}
		setSeam(s)
		var callErr error
		var wg sync.WaitGroup
		hook := func(_ context.Context, q Querier, _ string) error {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, callErr = q.ExecContext(context.Background(), "SELECT 1; SELECT count(*) FROM temp.seam")
			}()
			<-s.entered // the driver is armed and the statement not yet stepped
			return nil
		}
		err := runAdmittedHook(ctx, conn, hook, "d")
		wg.Wait()
		setSeam(nil)
		if v := s.waitErr.Load(); v != nil {
			t.Fatalf("iteration %d: %v", i, v)
		}
		if callErr == nil || !wasInterrupted(callErr) {
			t.Fatalf("iteration %d: the call returned %v, want a cancellation", i, callErr)
		}
		// The driver reports the cancellation whether or not SQLite honoured the
		// interrupt, so the scan itself is the evidence: it must not have run.
		if n := s.scanSteps.Load(); n != 0 {
			t.Fatalf("iteration %d: the cancelled statement still made %d scan calls: its interrupt was lost", i, n)
		}
		if err == nil || !strings.Contains(err.Error(), "still running") {
			t.Fatalf("iteration %d: got %v, want a refusal", i, err)
		}
	}
	// The pin is gone: the connection is clean and ordinary statements run.
	var n int
	if err := conn.QueryRowContext(ctx, "SELECT 1").Scan(&n); err != nil || n != 1 {
		t.Fatalf("a statement after the hook: %v", err)
	}
}

// A query the hook cancels itself may already have been interrupted, and SQLite
// then ends the transaction, so a later write would persist outside it. Every
// later call is refused and the hook is refused even if it returns nil.
func TestOnAdmittedCancelledWriteRefusesLaterCalls(t *testing.T) {
	store, conn := hookTx(t)
	ctx := context.Background()
	var laterErr, laterRowErr error
	hook := func(hctx context.Context, q Querier, _ string) error {
		short, cancel := context.WithTimeout(hctx, 50*time.Millisecond)
		defer cancel()
		if _, err := q.ExecContext(short, foreverInsert); err == nil {
			t.Error("the cancelled write reported success")
		}
		_, laterErr = q.ExecContext(hctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('leak', 'd', 1)`)
		var n int
		laterRowErr = q.QueryRowContext(hctx, "SELECT 1").Scan(&n)
		return nil
	}
	err := runAdmittedHook(ctx, conn, hook, "d")
	if !errors.Is(err, errHookPoisoned) {
		t.Fatalf("a hook that returned nil after a cancelled write got %v", err)
	}
	if !errors.Is(laterErr, errHookPoisoned) {
		t.Fatalf("a write after the cancelled one got %v", laterErr)
	}
	if laterRowErr == nil {
		t.Fatal("a query after the cancelled write ran")
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil && !errNoTransaction(err) {
		t.Fatal(err)
	}
	conn.Close()
	var leaked int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM hook_probe`).Scan(&leaked); err != nil || leaked != 0 {
		t.Fatalf("%d rows persisted outside the admission transaction, %v", leaked, err)
	}
}

// The query-row path poisons the Querier the same way.
func TestOnAdmittedCancelledQueryRowRefusesLaterCalls(t *testing.T) {
	_, conn := hookTx(t)
	var laterErr error
	hook := func(hctx context.Context, q Querier, _ string) error {
		gone, cancel := context.WithCancel(hctx)
		cancel()
		var n int
		if err := q.QueryRowContext(gone, "SELECT 1").Scan(&n); err == nil {
			t.Error("a query under a cancelled context returned a row")
		}
		_, laterErr = q.ExecContext(hctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('leak', 'd', 1)`)
		return nil
	}
	if err := runAdmittedHook(context.Background(), conn, hook, "d"); !errors.Is(err, errHookPoisoned) {
		t.Fatalf("got %v", err)
	}
	if !errors.Is(laterErr, errHookPoisoned) {
		t.Fatalf("a call after the cancelled query got %v", laterErr)
	}
}

// A hook that never cancels anything is unaffected, and the pin is released
// before admission commits: the commit succeeds and keeps the hook's write.
func TestOnAdmittedPinIsReleasedBeforeCommit(t *testing.T) {
	store, conn := hookTx(t)
	ctx := context.Background()
	hook := func(hctx context.Context, q Querier, _ string) error {
		_, err := q.ExecContext(hctx, `INSERT INTO hook_probe(job, dispatch_id, epoch) VALUES ('ok', 'd', 1)`)
		return err
	}
	if err := runAdmittedHook(ctx, conn, hook, "d"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		t.Fatalf("commit after the hook: %v", err)
	}
	conn.Close()
	var n int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM hook_probe`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows after commit, %v", n, err)
	}
}

// An SQL error whose text happens to say "interrupted" is an ordinary error:
// it must neither poison the Querier nor be taken for an interrupt.
func TestOnAdmittedErrorTextIsNotAnInterrupt(t *testing.T) {
	_, conn := hookTx(t)
	ctx := context.Background()
	var firstErr, laterErr error
	hook := func(hctx context.Context, q Querier, _ string) error {
		_, firstErr = q.ExecContext(hctx, `SELECT * FROM "interrupted"`) // no such table
		var n int
		laterErr = q.QueryRowContext(hctx, "SELECT 1").Scan(&n)
		return nil
	}
	if err := runAdmittedHook(ctx, conn, hook, "d"); err != nil {
		t.Fatalf("a hook whose query failed with ordinary text was refused: %v", err)
	}
	if firstErr == nil || !strings.Contains(firstErr.Error(), "interrupted") {
		t.Fatalf("the probe query did not fail with the expected text: %v", firstErr)
	}
	if wasInterrupted(firstErr) {
		t.Fatalf("an ordinary error was taken for an interrupt: %v", firstErr)
	}
	if laterErr != nil {
		t.Fatalf("a later call was refused after an ordinary error: %v", laterErr)
	}
	if !wasInterrupted(context.Canceled) || !wasInterrupted(fmt.Errorf("x: %w", context.DeadlineExceeded)) {
		t.Fatal("a context error is not taken for an interrupt")
	}
}
