package dispatch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
