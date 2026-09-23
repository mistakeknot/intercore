package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// recoveryOrders enumerates every order; subtest names are the replay seed.
func recoveryOrders(items ...string) [][]string {
	if len(items) == 0 {
		return [][]string{{}}
	}
	var result [][]string
	for i, item := range items {
		rest := append(append([]string{}, items[:i]...), items[i+1:]...)
		for _, tail := range recoveryOrders(rest...) {
			result = append(result, append([]string{item}, tail...))
		}
	}
	return result
}

// Only the fault location is substituted: every SQL statement still executes
// against the real migrated database through the production terminal writer.
type crashTerminalQuerier struct {
	*sql.Conn
	boundary string
}

func (q crashTerminalQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	result, err := q.Conn.ExecContext(ctx, query, args...)
	if err == nil && strings.Contains(query, q.boundary) {
		os.Exit(86)
	}
	return result, err
}

func recoveryTerminal(kind string) Terminal {
	status := map[string]string{"result": StatusCompleted, "cancel": StatusCancelled, "lost": StatusFailed}[kind]
	source := map[string]string{"result": TerminalSourceCollect, "cancel": TerminalSourceKill, "lost": TerminalSourceReconcile}[kind]
	input := map[string]int64{"result": 17, "cancel": 23, "lost": 31}[kind]
	return Terminal{Status: status, Source: source, Evidence: EvidenceVerified,
		UsageStatus: UsageComplete, InputTokens: int64Ptr(input), OutputTokens: int64Ptr(5),
		CacheReadTokens: int64Ptr(3), Fields: UpdateFields{"input_tokens": input + 3, "output_tokens": 5}}
}

func TestTerminalRecoveryCrashHelper(t *testing.T) {
	path := os.Getenv("IC_RECOVERY_CRASH_DB")
	if path == "" {
		return
	}
	d, err := db.Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	id, boundary := os.Getenv("IC_RECOVERY_DISPATCH"), os.Getenv("IC_RECOVERY_BOUNDARY")
	err = withImmediateTx(context.Background(), d.SqlDB(), "fault replay", func(conn *sql.Conn) error {
		_, _, err := terminalizeTx(context.Background(), crashTerminalQuerier{conn, boundary}, id,
			recoveryTerminal(os.Getenv("IC_RECOVERY_OUTCOME")), traceContext{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if boundary == "after-commit" {
		os.Exit(86)
	}
	t.Fatal("crash boundary was not reached")
}

func TestTerminalGeneratedCrashRecovery(t *testing.T) {
	for _, order := range recoveryOrders("result", "cancel", "lost") {
		for _, boundary := range []string{"INSERT INTO dispatch_events", "INSERT INTO dispatch_terminals", "UPDATE dispatches SET", "after-commit"} {
			t.Run(strings.Join(order, "-")+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				path := filepath.Join(t.TempDir(), "recovery.db")
				d, err := db.Open(path, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				store := New(d.SqlDB(), nil)
				id, other := runningDispatch(t, store), runningDispatch(t, store)
				otherBefore, err := store.Get(ctx, other)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}
				childCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestTerminalRecoveryCrashHelper$")
				cmd.Env = append(os.Environ(), "IC_RECOVERY_CRASH_DB="+path, "IC_RECOVERY_DISPATCH="+id,
					"IC_RECOVERY_BOUNDARY="+boundary, "IC_RECOVERY_OUTCOME="+order[0])
				out, err := cmd.CombinedOutput()
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 86 {
					t.Fatalf("fault child: %v\n%s", err, out)
				}
				d, err = db.Open(path, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { d.Close() })
				store = New(d.SqlDB(), nil)
				committed := boundary == "after-commit"
				wantCount := 0
				if committed {
					wantCount = 1
				}
				for _, table := range []string{"dispatch_terminals", "dispatch_events"} {
					query := fmt.Sprintf("SELECT count(*) FROM %s WHERE dispatch_id = ?", table)
					if table == "dispatch_events" {
						query += " AND event_type = 'terminal'"
					}
					if n := terminalRowCount(t, store, query, id); n != wantCount {
						t.Fatalf("%s after crash = %d, want %d", table, n, wantCount)
					}
				}
				winner := order[1]
				if committed {
					winner = order[0]
				}
				var authoritative *TerminalRecord
				// Late results, cancellation, and duplicate delivery after restart.
				for i, kind := range []string{order[1], order[2], order[0], order[1]} {
					rec, err := store.Terminalize(ctx, id, recoveryTerminal(kind))
					if i == 0 && !committed {
						if err != nil {
							t.Fatal(err)
						}
					} else if !errors.Is(err, ErrAlreadyTerminal) {
						t.Fatalf("replay %s: %v", kind, err)
					}
					if rec == nil || rec.Status != recoveryTerminal(winner).Status || rec.Source != recoveryTerminal(winner).Source {
						t.Fatalf("wrong authority: %+v", rec)
					}
					if authoritative != nil && !reflect.DeepEqual(rec, authoritative) {
						t.Fatal("late writer changed terminal evidence")
					}
					authoritative = rec
					assertOneTerminal(t, store, id)
				}
				wantInput := map[string]int64{"result": 17, "cancel": 23, "lost": 31}[winner]
				if authoritative.InputTokens == nil || *authoritative.InputTokens != wantInput || authoritative.OutputTokens == nil || *authoritative.OutputTokens != 5 || authoritative.CacheReadTokens == nil || *authoritative.CacheReadTokens != 3 {
					t.Fatalf("usage lost: %+v", authoritative)
				}
				got, err := store.Get(ctx, id)
				if err != nil || got.Status != authoritative.Status || int64(got.InputTokens) != wantInput+3 || got.OutputTokens != 5 {
					t.Fatalf("dispatch disagrees: %+v, %v", got, err)
				}
				otherAfter, err := store.Get(ctx, other)
				if err != nil || !reflect.DeepEqual(otherBefore, otherAfter) {
					t.Fatalf("another attempt changed: %+v, %v", otherAfter, err)
				}
				// Its active slot still blocks admission, even after duplicate cleanup.
				_, err = store.admit(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()}, SpawnOptions{Policy: &SpawnPolicy{MaxActiveGlobal: 1}})
				requireRejection(t, err, "concurrency_limit_global")
			})
		}
	}
}

func runningDispatch(t *testing.T, store *Store) string {
	t.Helper()
	ctx := context.Background()
	id, err := store.Create(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{"pid": 999999999}); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	return id
}

func terminalRowCount(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := store.db.QueryRowContext(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func assertOneTerminal(t *testing.T, store *Store, id string) {
	t.Helper()
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_terminals WHERE dispatch_id = ?", id); n != 1 {
		t.Fatalf("terminal records for %s = %d, want 1", id, n)
	}
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_events WHERE dispatch_id = ? AND event_type = 'terminal'", id); n != 1 {
		t.Fatalf("terminal events for %s = %d, want 1", id, n)
	}
}

// The writer's order (event, record naming it, then the guarded status update)
// commits under the schema-40 triggers: one record and one terminal event, with
// the outbox trigger adding nothing. A later writer gets that record back.
func TestTerminalizeOrderSurvivesImmutabilityTrigger(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := runningDispatch(t, store)

	exit := 0
	rec, err := store.Terminalize(ctx, id, Terminal{
		Status:   StatusCompleted,
		Source:   TerminalSourceCollect,
		Evidence: EvidenceNotApplicable,
		Reason:   "collected",
		ExitCode: &exit,
		Fields:   UpdateFields{"exit_code": 0, "completed_at": time.Now().Unix()},
	})
	if err != nil {
		t.Fatalf("Terminalize: %v", err)
	}
	if rec.Status != StatusCompleted || rec.FromStatus != StatusRunning || rec.Source != TerminalSourceCollect ||
		rec.Evidence != EvidenceNotApplicable || rec.ExitCode == nil || *rec.ExitCode != 0 {
		t.Fatalf("record = %+v", rec)
	}
	var eventType, toStatus string
	if err := store.db.QueryRowContext(ctx, "SELECT event_type, to_status FROM dispatch_events WHERE id = ? AND dispatch_id = ?",
		rec.EventID, id).Scan(&eventType, &toStatus); err != nil {
		t.Fatalf("record's event %d: %v", rec.EventID, err)
	}
	if eventType != "terminal" || toStatus != StatusCompleted {
		t.Fatalf("record's event = %s to %s, want terminal to completed", eventType, toStatus)
	}
	assertOneTerminal(t, store, id)
	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusCompleted || d.ExitCode == nil || *d.ExitCode != 0 {
		t.Fatalf("dispatch status %s exit %v, want completed exit 0", d.Status, d.ExitCode)
	}

	again, err := store.Terminalize(ctx, id, Terminal{Status: StatusFailed, Source: TerminalSourceKill, Evidence: EvidenceMissing})
	if !errors.Is(err, ErrAlreadyTerminal) || !errors.Is(err, ErrStaleStatus) {
		t.Fatalf("second Terminalize: err = %v, want ErrAlreadyTerminal wrapping ErrStaleStatus", err)
	}
	if again == nil || again.Status != StatusCompleted || again.EventID != rec.EventID {
		t.Fatalf("second Terminalize returned %+v, want the first record", again)
	}
	assertOneTerminal(t, store, id)
}

// The supervision guard refuses a bare status update on a supervised dispatch
// and admits the writer, whose record precedes its update.
func TestTerminalizeSupervisedDispatchPassesGuard(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := runningDispatch(t, store)
	if _, err := store.db.ExecContext(ctx,
		"INSERT INTO dispatch_supervision (dispatch_id, db_path, prompt_sha256, created_at) VALUES (?, '/tmp/intercore.db', 'abc', 1)", id); err != nil {
		t.Fatal(err)
	}

	err := store.UpdateStatus(ctx, id, StatusCompleted, UpdateFields{"exit_code": 0})
	if err == nil || !strings.Contains(err.Error(), "supervised dispatch") {
		t.Fatalf("UpdateStatus on a supervised dispatch: err = %v, want the supervision guard", err)
	}
	rec, err := store.Terminalize(ctx, id, Terminal{
		Status:       StatusFailed,
		Source:       TerminalSourceSupervisor,
		Evidence:     EvidenceMissing,
		FailureClass: "supervisor_lost",
	})
	if err != nil {
		t.Fatalf("Terminalize: %v", err)
	}
	if rec.Source != TerminalSourceSupervisor || rec.FailureClass != "supervisor_lost" {
		t.Fatalf("record = %+v", rec)
	}
	assertOneTerminal(t, store, id)
}

// The store has one pooled connection and the writer holds it for the whole
// transaction, so any store or event call through the pool inside it would wait
// forever on a context without a deadline. The recorder, which does use the
// pool, must run after the connection is released.
func TestTerminalizeTxNoNestedStoreCalls(t *testing.T) {
	store := testStore(t)
	id := runningDispatch(t, store)

	var recorded atomic.Bool
	store.eventRecorder = func(dispatchID, runID, fromStatus, toStatus string) {
		var n int
		_ = store.db.QueryRowContext(context.Background(), "SELECT count(*) FROM dispatches").Scan(&n)
		recorded.Store(true)
	}

	done := make(chan error, 1)
	go func() {
		_, err := store.Terminalize(context.Background(), id, Terminal{
			Status:   StatusCompleted,
			Source:   TerminalSourceCollect,
			Evidence: EvidenceNotApplicable,
			Fields:   UpdateFields{"sandbox_effective": `{"mode":"workspace-write"}`},
		})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Terminalize: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Terminalize did not return within 5s: something inside the transaction waited on the pool")
	}
	if !recorded.Load() {
		t.Fatal("recorder did not run after commit")
	}
}

// Writers in separate processes race to record one dispatch's outcome. Exactly
// one wins; the others get its record back and write nothing.
func TestTerminalizeConcurrentWritersRecordOnce(t *testing.T) {
	const (
		dispatches = 20
		writers    = 4
	)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	primary, err := db.Open(path, 5*time.Second)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { primary.Close() })
	if err := primary.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	seed := New(primary.SqlDB(), nil)

	stores := make([]*Store, writers)
	for w := range stores {
		handle, err := db.Open(path, 5*time.Second)
		if err != nil {
			t.Fatalf("Open writer %d: %v", w, err)
		}
		t.Cleanup(func() { handle.Close() })
		stores[w] = New(handle.SqlDB(), nil)
	}
	statuses := []string{StatusCompleted, StatusFailed, StatusTimeout, StatusCancelled}

	for i := 0; i < dispatches; i++ {
		id := runningDispatch(t, seed)
		start := make(chan struct{})
		recs := make([]*TerminalRecord, writers)
		errs := make([]error, writers)
		var wg sync.WaitGroup
		for w, store := range stores {
			wg.Add(1)
			go func(w int, store *Store) {
				defer wg.Done()
				<-start
				recs[w], errs[w] = store.Terminalize(ctx, id, Terminal{Status: statuses[w], Source: TerminalSourceKill, Evidence: EvidenceNotApplicable})
			}(w, store)
		}
		close(start)
		wg.Wait()

		winners := 0
		for w, err := range errs {
			switch {
			case err == nil:
				winners++
			case errors.Is(err, ErrAlreadyTerminal):
			default:
				t.Fatalf("dispatch %s: writer %d: %v", id, w, err)
			}
		}
		if winners != 1 {
			t.Fatalf("dispatch %s: %d writers recorded an outcome, want 1", id, winners)
		}
		final, err := readTerminal(ctx, seed.db, id)
		if err != nil || final == nil {
			t.Fatalf("dispatch %s: read record: %v (%v)", id, final, err)
		}
		for w, rec := range recs {
			if rec == nil || rec.EventID != final.EventID || rec.Status != final.Status {
				t.Fatalf("dispatch %s: writer %d returned %+v, recorded %+v", id, w, rec, final)
			}
		}
		assertOneTerminal(t, seed, id)
		d, err := seed.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if d.Status != final.Status {
			t.Fatalf("dispatch %s: status %s, record says %s", id, d.Status, final.Status)
		}
	}
}

// A dispatch that ended before migration 040 has no record. The writer reports
// it already terminal, returns no record and writes nothing.
func TestTerminalizeDispatchEndedBeforeSchema40(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := runningDispatch(t, store)
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER dispatches_terminal_outbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE dispatches SET status = 'completed' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	events := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_events WHERE dispatch_id = ?", id)

	rec, err := store.Terminalize(ctx, id, Terminal{Status: StatusFailed, Source: TerminalSourceKill, Evidence: EvidenceMissing})
	if !errors.Is(err, ErrAlreadyTerminal) || rec != nil {
		t.Fatalf("Terminalize = %+v, %v; want nil, ErrAlreadyTerminal", rec, err)
	}
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_terminals WHERE dispatch_id = ?", id); n != 0 {
		t.Fatalf("terminal records = %d, want 0", n)
	}
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_events WHERE dispatch_id = ?", id); n != events {
		t.Fatalf("dispatch events = %d, want %d", n, events)
	}
}

func TestTerminalizeRejectsInvalidOutcome(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := runningDispatch(t, store)
	valid := Terminal{Status: StatusFailed, Source: TerminalSourceKill, Evidence: EvidenceMissing}

	for name, mutate := range map[string]func(*Terminal){
		"non-terminal status": func(o *Terminal) { o.Status = StatusRunning },
		"trigger source":      func(o *Terminal) { o.Source = "trigger" },
		"legacy source":       func(o *Terminal) { o.Source = "legacy" },
		"unrecorded evidence": func(o *Terminal) { o.Evidence = EvidenceUnrecorded },
		"empty evidence":      func(o *Terminal) { o.Evidence = "" },
		"unknown usage":       func(o *Terminal) { o.UsageStatus = "partial" },
		"status column":       func(o *Terminal) { o.Fields = UpdateFields{"status": StatusCompleted} },
	} {
		out := valid
		mutate(&out)
		if _, err := store.Terminalize(ctx, id, out); err == nil {
			t.Errorf("%s: Terminalize accepted it", name)
		}
	}
	if _, err := store.Terminalize(ctx, "missing", valid); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown dispatch: err = %v, want ErrNotFound", err)
	}

	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusRunning {
		t.Fatalf("status after rejected writes = %s, want running", d.Status)
	}
	if n := terminalRowCount(t, store, "SELECT count(*) FROM dispatch_terminals WHERE dispatch_id = ?", id); n != 0 {
		t.Fatalf("terminal records = %d, want 0", n)
	}
}

// Every field a writer supplies lands in the record, and the dispatch columns
// it names are written with the status.
func TestTerminalizeRecordsCallerFields(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	id := runningDispatch(t, store)

	exit, in, out, cacheRead, cacheWrite, decision := 137, int64(10), int64(20), int64(30), int64(40), int64(620)
	rec, err := store.Terminalize(ctx, id, Terminal{
		Status:             StatusFailed,
		Source:             TerminalSourceSupervisor,
		Evidence:           EvidenceMismatched,
		Reason:             "tree claim differs from observation",
		Fields:             UpdateFields{"error_message": "evidence mismatched", "exit_code": exit},
		ExitCode:           &exit,
		ExitSignal:         "SIGKILL",
		FailureClass:       "evidence_mismatched",
		UsageStatus:        UsageIncomplete,
		InputTokens:        &in,
		OutputTokens:       &out,
		CacheReadTokens:    &cacheRead,
		CacheWriteTokens:   &cacheWrite,
		ProducerBackend:    "claude",
		ProducerModel:      "claude-sonnet-5",
		RouteDecisionID:    &decision,
		ReportPath:         "/tmp/report.json",
		ReportSHA256:       "deadbeef",
		ArtifactsJSON:      `{"plan":{"path":"/tmp/plan.md","sha256":"abc"}}`,
		BaseCommit:         "base",
		BaseWorktreeDigest: "base-digest",
		HeadCommit:         "head",
		HeadWorktreeDigest: "head-digest",
		OrphansKilled:      2,
	})
	if err != nil {
		t.Fatalf("Terminalize: %v", err)
	}
	if rec.ExitCode == nil || *rec.ExitCode != exit || rec.ExitSignal != "SIGKILL" || rec.FailureClass != "evidence_mismatched" ||
		rec.UsageStatus != UsageIncomplete || rec.InputTokens == nil || *rec.InputTokens != in || rec.OutputTokens == nil || *rec.OutputTokens != out ||
		rec.CacheReadTokens == nil || *rec.CacheReadTokens != cacheRead || rec.CacheWriteTokens == nil || *rec.CacheWriteTokens != cacheWrite ||
		rec.ProducerBackend != "claude" || rec.ProducerModel != "claude-sonnet-5" || rec.RouteDecisionID == nil || *rec.RouteDecisionID != decision ||
		rec.ReportPath != "/tmp/report.json" || rec.ReportSHA256 != "deadbeef" || rec.ArtifactsJSON != `{"plan":{"path":"/tmp/plan.md","sha256":"abc"}}` ||
		rec.BaseCommit != "base" || rec.BaseWorktreeDigest != "base-digest" || rec.HeadCommit != "head" || rec.HeadWorktreeDigest != "head-digest" ||
		rec.OrphansKilled != 2 || rec.Contested || rec.CreatedAt == 0 {
		t.Fatalf("record = %+v", rec)
	}
	d, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if d.ErrorMessage == nil || *d.ErrorMessage != "evidence mismatched" || d.ExitCode == nil || *d.ExitCode != exit {
		t.Fatalf("dispatch error %v exit %v, want the writer's fields", d.ErrorMessage, d.ExitCode)
	}
}
