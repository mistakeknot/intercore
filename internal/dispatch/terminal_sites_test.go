package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func terminalRecordFor(t *testing.T, store *Store, id string) *TerminalRecord {
	t.Helper()
	rec, err := readTerminal(context.Background(), store.db, id)
	if err != nil {
		t.Fatalf("read terminal record for %s: %v", id, err)
	}
	if rec == nil {
		t.Fatalf("dispatch %s has no terminal record", id)
	}
	return rec
}

func superviseDispatch(t *testing.T, store *Store, id string) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(),
		"INSERT INTO dispatch_supervision (dispatch_id, db_path, prompt_sha256, created_at) VALUES (?, '/tmp/intercore.db', 'abc', 1)", id); err != nil {
		t.Fatal(err)
	}
}

func TestCollectRecordsAtCollectSite(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	output := filepath.Join(dir, "output.md")
	verdict := output + ".verdict"
	if err := os.WriteFile(verdict, []byte("--- VERDICT ---\nSTATUS: pass\nFILES: 0 changed\nFINDINGS: 0\nSUMMARY: clean\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: dir, OutputFile: &output, VerdictFile: &verdict})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{"pid": 999999999}); err != nil {
		t.Fatal(err)
	}

	if err := Collect(ctx, store, id); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rec := terminalRecordFor(t, store, id)
	if rec.Status != StatusCompleted || rec.FromStatus != StatusRunning || rec.Source != TerminalSourceCollect ||
		rec.Evidence != EvidenceNotApplicable || rec.ExitCode != nil {
		t.Fatalf("record = %+v, want completed from running by collect, evidence not applicable, no observed exit code", rec)
	}
	assertOneTerminal(t, store, id)
}

func TestFlereCollectRecordsVerifiedReceipt(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	saveWorkerReceipt(t, output, receipt)

	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	rec := terminalRecordFor(t, store, id)
	if rec.Status != StatusCompleted || rec.Source != TerminalSourceCollect || rec.Evidence != EvidenceVerified ||
		rec.ExitCode == nil || *rec.ExitCode != 0 || rec.UsageStatus != UsageComplete ||
		rec.InputTokens == nil || *rec.InputTokens != 10 || rec.OutputTokens == nil || *rec.OutputTokens != 2 ||
		rec.CacheReadTokens == nil || *rec.CacheReadTokens != 3 || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != 0 ||
		rec.ProducerBackend != "flere" || rec.ProducerModel != "provider/model" {
		t.Fatalf("record = %+v", rec)
	}
	assertOneTerminal(t, store, id)
}

func TestFlereCollectRecordsMissingAndMalformedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		receipt  func(map[string]any)
		evidence string
	}{
		{name: "no receipt", evidence: EvidenceMissing},
		{name: "forged receipt", receipt: func(r map[string]any) { r["dispatch_id"] = "forged" }, evidence: EvidenceMalformed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			id, output, receipt := workerReceiptFixture(t, store)
			if tc.receipt != nil {
				tc.receipt(receipt)
				saveWorkerReceipt(t, output, receipt)
			}
			if err := Collect(context.Background(), store, id); err != nil {
				t.Fatalf("Collect: %v", err)
			}
			rec := terminalRecordFor(t, store, id)
			if rec.Status != StatusFailed || rec.Evidence != tc.evidence || rec.FailureClass != WorkerOutcomeIndeterminate || rec.UsageStatus != UsageUnknown {
				t.Fatalf("record = %+v, want failed with %s evidence", rec, tc.evidence)
			}
		})
	}
}

func TestKillAndWaitTimeoutRecordAtKillSite(t *testing.T) {
	for _, mode := range []string{"kill", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			store := testStore(t)
			ctx := context.Background()
			id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			want := StatusCancelled
			if mode == "timeout" {
				want = StatusTimeout
				if _, err := Wait(ctx, store, id, time.Second, 50*time.Millisecond); err != nil {
					t.Fatalf("Wait: %v", err)
				}
			} else if err := Kill(ctx, store, id); err != nil {
				t.Fatalf("Kill: %v", err)
			}
			rec := terminalRecordFor(t, store, id)
			if rec.Status != want || rec.FromStatus != StatusSpawned || rec.Source != TerminalSourceKill || rec.Evidence != EvidenceNotApplicable {
				t.Fatalf("record = %+v, want %s from spawned by kill", rec, want)
			}
			assertOneTerminal(t, store, id)
		})
	}
}

func TestSpawnFailureRecordsAtSpawnSite(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CLAVAIN_DISPATCH_SH", "")
	t.Setenv("PATH", dir)
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("test prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := testStore(t)
	ctx := context.Background()

	if _, err := Spawn(ctx, store, SpawnOptions{AgentType: "claude", ProjectDir: dir, PromptFile: prompt}); err == nil {
		t.Fatal("Spawn ran a backend that needs dispatch.sh without it")
	}
	all, err := store.List(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("dispatches = %d, want the one admitted attempt", len(all))
	}
	rec := terminalRecordFor(t, store, all[0].ID)
	if rec.Status != StatusFailed || rec.FromStatus != StatusSpawned || rec.Source != TerminalSourceSpawn ||
		rec.Evidence != EvidenceNotApplicable || rec.FailureClass != "spawn_failed" {
		t.Fatalf("record = %+v, want failed from spawned by spawn", rec)
	}
	assertOneTerminal(t, store, all[0].ID)
}

// A supervised dispatch's outcome belongs to its supervisor (contract I7). Legacy
// collection must not infer it from a verdict file, and kill must not signal
// the worker behind the supervisor's back.
func TestSupervisedDispatchNeverVerdictInferred(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	output := filepath.Join(dir, "output.md")
	verdict := output + ".verdict"
	if err := os.WriteFile(verdict, []byte("--- VERDICT ---\nSTATUS: pass\nFILES: 0 changed\nFINDINGS: 0\nSUMMARY: looks done\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: dir, OutputFile: &output, VerdictFile: &verdict})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{"pid": 999999999}); err != nil {
		t.Fatal(err)
	}
	superviseDispatch(t, store, id)

	if err := Collect(ctx, store, id); !errors.Is(err, ErrSupervised) {
		t.Fatalf("Collect: err = %v, want ErrSupervised", err)
	}
	if _, err := Poll(ctx, store, id); !errors.Is(err, ErrSupervised) {
		t.Fatalf("Poll: err = %v, want ErrSupervised", err)
	}
	if err := Kill(ctx, store, id); !errors.Is(err, ErrSupervised) {
		t.Fatalf("Kill: err = %v, want ErrSupervised", err)
	}
	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusRunning {
		t.Fatalf("status = %s, want running", d.Status)
	}
	if rec, err := readTerminal(ctx, store.db, id); err != nil || rec != nil {
		t.Fatalf("terminal record = %+v (%v), want none", rec, err)
	}
}

// Run rollback cancels every unsupervised dispatch of the run in one
// transaction with its record, or none of them; supervised dispatches get an
// intent for their supervisor instead.
func TestCancelByRunAtomic(t *testing.T) {
	ctx := context.Background()
	run := "run-rollback"
	create := func(t *testing.T, store *Store) string {
		t.Helper()
		id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir(), ScopeID: &run})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("all rows or none", func(t *testing.T) {
		store := testStore(t)
		first, second := create(t, store), create(t, store)
		// A record already present for a non-terminal dispatch makes the second
		// row's write fail after the first row's has been made.
		if _, err := store.db.ExecContext(ctx, `INSERT INTO dispatch_terminals
			(dispatch_id, attempt, status, from_status, terminal_source, evidence_status, event_id, created_at)
			VALUES (?, 0, 'failed', 'spawned', 'kill', 'not_applicable', 0, 1)`, second); err != nil {
			t.Fatal(err)
		}
		events := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_events")

		if n, err := store.CancelByRun(ctx, run); err == nil {
			t.Fatalf("CancelByRun = %d, nil; want the second row's failure", n)
		}
		for _, id := range []string{first, second} {
			d, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if d.Status != StatusSpawned {
				t.Fatalf("dispatch %s status = %s after a failed rollback, want spawned", id, d.Status)
			}
		}
		if rec, err := readTerminal(ctx, store.db, first); err != nil || rec != nil {
			t.Fatalf("first dispatch record = %+v (%v), want none", rec, err)
		}
		if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_events"); n != events {
			t.Fatalf("dispatch events = %d, want %d", n, events)
		}
	})

	t.Run("supervised dispatch gets an intent", func(t *testing.T) {
		store := testStore(t)
		plain, supervised := create(t, store), create(t, store)
		superviseDispatch(t, store, supervised)

		n, err := store.CancelByRun(ctx, run)
		if err != nil {
			t.Fatalf("CancelByRun: %v", err)
		}
		if n != 1 {
			t.Fatalf("cancelled = %d, want 1", n)
		}
		rec := terminalRecordFor(t, store, plain)
		if rec.Status != StatusCancelled || rec.Source != TerminalSourceCancelByRun || rec.Evidence != EvidenceNotApplicable || rec.RunID != run {
			t.Fatalf("record = %+v, want cancelled by cancel_by_run in %s", rec, run)
		}
		d, err := store.Get(ctx, supervised)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != StatusSpawned {
			t.Fatalf("supervised dispatch status = %s, want spawned", d.Status)
		}
		if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_intents WHERE dispatch_id = ? AND kind = 'run_rollback'", supervised); n != 1 {
			t.Fatalf("run_rollback intents = %d, want 1", n)
		}
	})
}
