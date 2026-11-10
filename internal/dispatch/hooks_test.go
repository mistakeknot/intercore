package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
