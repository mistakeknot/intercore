package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Receipt publication races with cancellation and collection. Missing or torn
// receipt writes must stay indeterminate; later proof is appended, never used
// to rewrite the authoritative attempt or silently start a replacement.
func TestWorkerGeneratedReceiptRecovery(t *testing.T) {
	for _, order := range recoveryOrders("publish", "collect", "cancel") {
		for _, boundary := range []string{"missing", "torn", "foreign-attempt"} {
			t.Run(strings.Join(order, "-")+"/"+boundary, func(t *testing.T) {
				ctx := context.Background()
				store := testStore(t)
				id, output, receipt := workerReceiptFixture(t, store)
				other, _, foreign := workerReceiptFixture(t, store)
				otherBefore, err := store.Get(ctx, other)
				if err != nil {
					t.Fatal(err)
				}
				switch boundary {
				case "torn":
					if err := os.WriteFile(output+".receipt.json", []byte(`{"schema":"flere.dispatch-result.v1",`), 0600); err != nil {
						t.Fatal(err)
					}
				case "foreign-attempt":
					saveWorkerReceipt(t, output, foreign)
				}
				published := false
				var first *TerminalRecord
				wantInput, wantOutput := 0, 0
				for _, action := range append(append([]string{}, order...), "collect", "cancel") {
					switch action {
					case "publish":
						saveWorkerReceipt(t, output, receipt)
						published = true
					case "collect":
						if err := Collect(ctx, store, id); err != nil {
							t.Fatal(err)
						}
					case "cancel":
						if err := Kill(ctx, store, id); err != nil {
							t.Fatal(err)
						}
					}
					rec, err := readTerminal(ctx, store.db, id)
					if err != nil {
						t.Fatal(err)
					}
					if rec == nil {
						continue
					}
					if first == nil {
						wantStatus, wantSource := StatusFailed, TerminalSourceKill
						if action == "collect" {
							wantSource = TerminalSourceCollect
							if published {
								wantStatus, wantInput, wantOutput = StatusCompleted, 13, 2
							}
						}
						if rec.Status != wantStatus || rec.Source != wantSource {
							t.Fatalf("first terminal = %+v", rec)
						}
						first = rec
					} else if !reflect.DeepEqual(first, rec) {
						t.Fatal("receipt/cancel replay rewrote terminal")
					}
					assertOneTerminal(t, store, id)
				}
				verified, err := store.ReconcileWorker(ctx, id)
				if err != nil || verified == nil || verified.Usage == nil || *verified.Usage != (WorkerUsage{Input: 10, Output: 2, CacheRead: 3}) {
					t.Fatalf("late usage proof lost: %+v, %v", verified, err)
				}
				var raw string
				if err := store.db.QueryRowContext(ctx, "SELECT envelope_json FROM dispatch_events WHERE dispatch_id=? AND event_type='worker_reconciliation'", id).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var evidence WorkerReceipt
				if err := json.Unmarshal([]byte(raw), &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence.DispatchID != id || evidence.Usage == nil || *evidence.Usage != *verified.Usage {
					t.Fatalf("persisted reconciliation lost binding/usage: %s", raw)
				}
				d, err := store.Get(ctx, id)
				if err != nil || d.InputTokens != wantInput || d.OutputTokens != wantOutput || ShouldRetry(d, DefaultRetryPolicy()) {
					t.Fatalf("accounting or retry changed: %+v, %v", d, err)
				}
				if wantInput == 0 && (d.QuarantineReason == nil || *d.QuarantineReason != WorkerOutcomeIndeterminate) {
					t.Fatalf("uncertain acceptance was not quarantined: %+v", d)
				}
				after, err := readTerminal(ctx, store.db, id)
				if err != nil || !reflect.DeepEqual(first, after) {
					t.Fatal("reconciliation changed terminal")
				}
				otherAfter, err := store.Get(ctx, other)
				if err != nil || !reflect.DeepEqual(otherBefore, otherAfter) {
					t.Fatal("recovery changed another attempt")
				}
				_, err = store.admit(ctx, &Dispatch{AgentType: "codex", ProjectDir: t.TempDir()}, SpawnOptions{Policy: &SpawnPolicy{MaxActiveGlobal: 1}})
				requireRejection(t, err, "concurrency_limit_global")
			})
		}
	}
}

func TestFlereVerdictCannotEstablishCompletion(t *testing.T) {
	store := testStore(t)
	output := filepath.Join(t.TempDir(), "output")
	verdict := output + ".verdict"
	if err := os.WriteFile(verdict, []byte("STATUS: pass\nSUMMARY: fabricated success\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id, err := store.Create(context.Background(), &Dispatch{AgentType: "flere", ProjectDir: ".", OutputFile: &output, VerdictFile: &verdict})
	if err != nil {
		t.Fatal(err)
	}
	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	d, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusFailed || d.QuarantineReason == nil || *d.QuarantineReason != "worker_outcome_indeterminate" {
		t.Fatalf("fabricated verdict accepted: %+v", d)
	}
	if ShouldRetry(d, DefaultRetryPolicy()) {
		t.Fatal("indeterminate worker may not automatically retry")
	}
	if ToOutput(d).FailureClass == nil || *ToOutput(d).FailureClass != WorkerOutcomeIndeterminate {
		t.Fatal("worker failure class must be public")
	}
}

func workerReceiptFixture(t *testing.T, store *Store) (string, string, map[string]any) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output")
	scope, model, promptHash := "run", "provider/model", "hash"
	id, err := store.Create(context.Background(), &Dispatch{AgentType: "flere", ProjectDir: dir, OutputFile: &output, ScopeID: &scope, Model: &model, PromptHash: &promptHash})
	if err != nil {
		t.Fatal(err)
	}
	write := func(path string, value []byte) WorkerArtifact {
		t.Helper()
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(value)
		return WorkerArtifact{Path: path, SHA256: fmt.Sprintf("%x", sum)}
	}
	prompt := write(filepath.Join(dir, "prompt"), []byte("inspect"))
	receipt := map[string]any{"schema": "flere.dispatch-result.v1", "dispatch_id": id, "attempt_id": id, "run_id": scope, "attempt": 0, "session_id": "session", "prompt_hash": promptHash, "submitted_prompt_sha256": prompt.SHA256, "outcome": "success", "prompt_accepted": true, "provider": "provider", "model": "model", "user_entry_id": "user", "final_assistant_entry_id": "assistant", "final_leaf_id": "assistant", "stop_reason": "stop", "usage_semantics": "fresh_session_cumulative", "usage": WorkerUsage{Input: 10, Output: 2, CacheRead: 3}}
	start, _ := json.Marshal(receipt)
	native := write(filepath.Join(dir, "native.jsonl"), []byte("{\"type\":\"session\",\"id\":\"session\"}\n{\"type\":\"message\",\"id\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"inspect\"}}\n{\"type\":\"message\",\"id\":\"assistant\",\"message\":{\"role\":\"assistant\",\"provider\":\"provider\",\"model\":\"model\",\"stopReason\":\"stop\",\"content\":[{\"type\":\"text\",\"text\":\"done\"}]}}\n"))
	receipt["session_file"] = native.Path
	receipt["artifacts"] = map[string]WorkerArtifact{"started": write(filepath.Join(dir, "started.json"), start), "transcript": native, "prompt": prompt, "events": write(filepath.Join(dir, "events"), []byte("{}\n")), "output": write(output, []byte("done"))}
	return id, output, receipt
}

func saveWorkerReceipt(t *testing.T, output string, receipt map[string]any) {
	t.Helper()
	content, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output+".receipt.json", content, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkerReceiptProofAndIdempotentAccounting(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	saveWorkerReceipt(t, output, receipt)
	for i := 0; i < 2; i++ {
		if err := Collect(context.Background(), store, id); err != nil {
			t.Fatal(err)
		}
	}
	d, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if d.Status != StatusCompleted || d.InputTokens != 13 || d.OutputTokens != 2 {
		t.Fatalf("incorrect worker collection %+v", d)
	}
}

func TestWorkerFailedAttemptRetainsVerifiedUsage(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	receipt["outcome"], receipt["failure_class"], receipt["stop_reason"] = "error", "worker_provider_error", "error"
	artifacts := receipt["artifacts"].(map[string]WorkerArtifact)
	native, err := os.ReadFile(artifacts["transcript"].Path)
	if err != nil {
		t.Fatal(err)
	}
	native = []byte(strings.ReplaceAll(string(native), `"stopReason":"stop"`, `"stopReason":"error"`))
	if err := os.WriteFile(artifacts["transcript"].Path, native, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(native)
	artifacts["transcript"] = WorkerArtifact{Path: artifacts["transcript"].Path, SHA256: fmt.Sprintf("%x", sum)}
	saveWorkerReceipt(t, output, receipt)
	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	d, _ := store.Get(context.Background(), id)
	if d.Status != StatusFailed || d.InputTokens != 13 || d.OutputTokens != 2 || ShouldRetry(d, DefaultRetryPolicy()) {
		t.Fatalf("failed attempt usage lost: %+v", d)
	}
}

func TestWorkerReceiptRejectsOverflow(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	receipt["usage"] = WorkerUsage{Input: math.MaxInt, CacheRead: 1}
	saveWorkerReceipt(t, output, receipt)
	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	d, _ := store.Get(context.Background(), id)
	if d.Status != StatusFailed || d.InputTokens != 0 {
		t.Fatalf("overflow accepted: %+v", d)
	}
}

func TestWorkerReceiptRejectsForgedOrIncompleteProof(t *testing.T) {
	for _, field := range []string{"dispatch_id", "attempt_id", "run_id", "session_id", "prompt_hash", "submitted_prompt_sha256", "final_assistant_entry_id", "final_leaf_id", "provider", "model", "stop_reason", "usage_semantics", "session_file"} {
		t.Run(field, func(t *testing.T) {
			store := testStore(t)
			id, output, receipt := workerReceiptFixture(t, store)
			receipt[field] = "forged"
			saveWorkerReceipt(t, output, receipt)
			if err := Collect(context.Background(), store, id); err != nil {
				t.Fatal(err)
			}
			d, _ := store.Get(context.Background(), id)
			if d.Status != StatusFailed || d.QuarantineReason == nil || *d.QuarantineReason != WorkerOutcomeIndeterminate {
				t.Fatalf("accepted forged %s: %+v", field, d)
			}
		})
	}
}

func TestWorkerReconciliationDoesNotRewriteTerminalAttempt(t *testing.T) {
	store := testStore(t)
	id, output, receipt := workerReceiptFixture(t, store)
	if err := Collect(context.Background(), store, id); err != nil {
		t.Fatal(err)
	}
	saveWorkerReceipt(t, output, receipt)
	if _, err := store.ReconcileWorker(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, _ := store.Get(context.Background(), id)
	if d.Status != StatusFailed || *d.QuarantineReason != WorkerOutcomeIndeterminate || d.InputTokens != 0 {
		t.Fatalf("reconciliation rewrote attempt: %+v", d)
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM dispatch_events WHERE dispatch_id=? AND event_type='worker_reconciliation'", id).Scan(&count); err != nil || count != 1 {
		t.Fatalf("reconciliation count %d: %v", count, err)
	}
}
