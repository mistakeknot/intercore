package dispatch

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"
)

const WorkerOutcomeIndeterminate = "worker_outcome_indeterminate"

type WorkerArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type WorkerUsage struct {
	Input      int `json:"input"`
	Output     int `json:"output"`
	CacheRead  int `json:"cache_read"`
	CacheWrite int `json:"cache_write"`
}
type WorkerReceipt struct {
	Schema                string                    `json:"schema"`
	DispatchID            string                    `json:"dispatch_id"`
	RunID                 string                    `json:"run_id"`
	AttemptID             string                    `json:"attempt_id"`
	Attempt               int                       `json:"attempt"`
	SessionID             string                    `json:"session_id"`
	SessionFile           string                    `json:"session_file"`
	PromptHash            string                    `json:"prompt_hash"`
	SubmittedPromptSHA256 string                    `json:"submitted_prompt_sha256"`
	Outcome               string                    `json:"outcome"`
	FailureClass          string                    `json:"failure_class"`
	ErrorMessage          string                    `json:"error_message"`
	Provider              string                    `json:"provider"`
	Model                 string                    `json:"model"`
	UserEntryID           string                    `json:"user_entry_id"`
	FinalAssistantEntryID string                    `json:"final_assistant_entry_id"`
	FinalLeafID           string                    `json:"final_leaf_id"`
	StopReason            string                    `json:"stop_reason"`
	PromptAccepted        bool                      `json:"prompt_accepted"`
	RetryAllowed          bool                      `json:"retry_allowed"`
	IndependentAcceptance bool                      `json:"independent_acceptance"`
	UsageSemantics        string                    `json:"usage_semantics"`
	Usage                 *WorkerUsage              `json:"usage"`
	SandboxEffective      json.RawMessage           `json:"sandbox_effective"`
	Artifacts             map[string]WorkerArtifact `json:"artifacts"`
}

func readWorkerArtifact(a WorkerArtifact) ([]byte, error) {
	if a.Path == "" || len(a.SHA256) != 64 || !filepath.IsAbs(a.Path) {
		return nil, errors.New("missing worker artifact identity")
	}
	content, err := os.ReadFile(a.Path)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(content)
	if fmt.Sprintf("%x", sum) != a.SHA256 {
		return nil, errors.New("worker artifact hash mismatch")
	}
	return content, nil
}

// readWorkerReceipt verifies host evidence against the exact admitted attempt.
// A verdict, output text or process quiescence never substitutes for this proof.
func readWorkerReceipt(d *Dispatch) (*WorkerReceipt, error) {
	if d.OutputFile == nil {
		return nil, errors.New("missing worker output identity")
	}
	data, err := os.ReadFile(*d.OutputFile + ".receipt.json")
	if err != nil {
		return nil, err
	}
	var receipt WorkerReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return nil, err
	}
	if receipt.Schema != "flere.dispatch-result.v1" || receipt.DispatchID != d.ID || receipt.AttemptID != d.ID || receipt.Attempt != d.RetryCount || d.ScopeID == nil || receipt.RunID != *d.ScopeID || d.PromptHash == nil || receipt.PromptHash != *d.PromptHash || receipt.SessionID == "" || receipt.RetryAllowed || receipt.IndependentAcceptance {
		return nil, errors.New("worker receipt admission identity mismatch")
	}
	started, err := readWorkerArtifact(receipt.Artifacts["started"])
	if err != nil {
		return nil, err
	}
	var start WorkerReceipt
	if err := json.Unmarshal(started, &start); err != nil {
		return nil, err
	}
	if start.DispatchID != receipt.DispatchID || start.AttemptID != receipt.AttemptID || start.RunID != receipt.RunID || start.SessionID != receipt.SessionID || start.Attempt != receipt.Attempt || start.PromptHash != receipt.PromptHash || start.SubmittedPromptSHA256 != receipt.SubmittedPromptSHA256 {
		return nil, errors.New("worker started receipt mismatch")
	}
	if receipt.Outcome != "success" {
		if receipt.Outcome != "error" || receipt.FailureClass == "" {
			return nil, errors.New("invalid worker failure outcome")
		}
		if receipt.Usage == nil {
			return &receipt, nil
		}
	}
	if !receipt.PromptAccepted || receipt.StopReason == "" || (receipt.Outcome == "success" && receipt.StopReason != "stop") || receipt.FinalAssistantEntryID == "" || receipt.FinalLeafID != receipt.FinalAssistantEntryID || receipt.UserEntryID == "" || receipt.UsageSemantics != "fresh_session_cumulative" || receipt.Usage == nil || d.Model == nil || *d.Model != receipt.Provider+"/"+receipt.Model {
		return nil, errors.New("incomplete worker terminal proof")
	}
	u := receipt.Usage
	if u.Input < 0 || u.Output < 0 || u.CacheRead < 0 || u.CacheWrite < 0 {
		return nil, errors.New("invalid worker usage")
	}
	if u.Input > math.MaxInt-u.CacheRead || u.Input+u.CacheRead > math.MaxInt-u.CacheWrite || (receipt.Outcome == "success" && receipt.FailureClass != "") {
		return nil, errors.New("invalid worker totals or success classification")
	}
	names := []string{"events", "prompt", "transcript"}
	if receipt.Outcome == "success" {
		names = append(names, "output")
	}
	for _, name := range names {
		if _, err := readWorkerArtifact(receipt.Artifacts[name]); err != nil {
			return nil, err
		}
	}
	if receipt.Outcome == "success" {
		outputPath, err := filepath.EvalSymlinks(*d.OutputFile)
		if err != nil {
			return nil, err
		}
		outputPath, err = filepath.Abs(outputPath)
		if err != nil {
			return nil, err
		}
		if receipt.Artifacts["output"].Path != outputPath {
			return nil, errors.New("worker output path identity mismatch")
		}
	}
	if receipt.Artifacts["transcript"].Path != receipt.SessionFile || receipt.Artifacts["prompt"].SHA256 != receipt.SubmittedPromptSHA256 {
		return nil, errors.New("worker artifact path identity mismatch")
	}
	native, err := os.Open(receipt.SessionFile)
	if err != nil {
		return nil, err
	}
	defer native.Close()
	scanner := bufio.NewScanner(native)
	scanner.Buffer(make([]byte, 4096), 16*1024*1024)
	foundUser, foundAssistant, foundHeader := false, false, false
	seen := map[string]bool{}
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
				Provider   string `json:"provider"`
				Model      string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, err
		}
		if entry.ID == "" || seen[entry.ID] {
			return nil, errors.New("invalid or duplicate native entry identity")
		}
		seen[entry.ID] = true
		if entry.Type == "session" && entry.ID == receipt.SessionID {
			foundHeader = true
		}
		if entry.ID == receipt.UserEntryID && entry.Message.Role == "user" {
			foundUser = true
		}
		if entry.ID == receipt.FinalAssistantEntryID && entry.Message.Role == "assistant" && entry.Message.StopReason == receipt.StopReason && entry.Message.Provider == receipt.Provider && entry.Message.Model == receipt.Model {
			foundAssistant = true
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if !foundHeader || !foundUser || !foundAssistant {
		return nil, errors.New("worker native terminal entries missing")
	}
	return &receipt, nil
}

func collectWorker(ctx context.Context, store *Store, d *Dispatch) error {
	fields := UpdateFields{"completed_at": time.Now().Unix(), "exit_code": -1}
	receipt, err := readWorkerReceipt(d)
	status := StatusFailed
	if err == nil && receipt.Usage != nil {
		fields["input_tokens"] = receipt.Usage.Input + receipt.Usage.CacheRead + receipt.Usage.CacheWrite
		fields["output_tokens"] = receipt.Usage.Output
		fields["cache_hits"] = receipt.Usage.CacheRead
	}
	if err != nil {
		fields["error_message"] = err.Error()
		fields["quarantine_reason"] = WorkerOutcomeIndeterminate
	} else if receipt.Outcome != "success" {
		fields["error_message"] = receipt.ErrorMessage
		fields["quarantine_reason"] = receipt.FailureClass
	} else {
		status = StatusCompleted
		fields["exit_code"] = 0
		fields["input_tokens"] = receipt.Usage.Input + receipt.Usage.CacheRead + receipt.Usage.CacheWrite
		fields["output_tokens"] = receipt.Usage.Output
		fields["cache_hits"] = receipt.Usage.CacheRead
		fields["sandbox_effective"] = string(receipt.SandboxEffective)
	}
	return store.UpdateStatus(ctx, d.ID, status, fields)
}

// ReconcileWorker appends later proof without rewriting a terminal attempt or
// reopening it for automatic replay. Consumers can inspect the reconciliation.
func (s *Store) ReconcileWorker(ctx context.Context, id string) (*WorkerReceipt, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.AgentType != "flere" || !d.IsTerminal() {
		return nil, errors.New("reconciliation requires a terminal Flere attempt")
	}
	receipt, err := readWorkerReceipt(d)
	if err != nil {
		return nil, err
	}
	envelope, err := json.Marshal(receipt)
	if err != nil {
		return nil, err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO dispatch_events(dispatch_id,run_id,from_status,to_status,event_type,reason,envelope_json) VALUES(?,?,?,?,?,?,?)`, d.ID, d.ScopeID, d.Status, d.Status, "worker_reconciliation", receipt.Outcome, string(envelope))
	return receipt, err
}
