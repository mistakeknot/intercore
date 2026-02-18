package dispatch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// stateFileData matches the JSON written by dispatch.sh's _dispatch_write_state_files.
type stateFileData struct {
	Name     string `json:"name"`
	Workdir  string `json:"workdir"`
	Started  int64  `json:"started"`
	Activity string `json:"activity"`
	Turns    int    `json:"turns"`
	Commands int    `json:"commands"`
	Messages int    `json:"messages"`
}

// Poll checks a dispatch's liveness and updates its stats from the state file.
// Returns the updated dispatch.
func Poll(ctx context.Context, store *Store, id string) (*Dispatch, error) {
	d, err := store.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if d.IsTerminal() {
		return d, nil
	}

	if d.PID == nil {
		return d, nil
	}
	pid := *d.PID

	alive := isProcessAlive(pid)

	if alive {
		// Read state file for live stats
		stateFile := fmt.Sprintf("/tmp/clavain-dispatch-%d.json", pid)
		if state, err := readStateFile(stateFile); err == nil {
			store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
				"turns":    state.Turns,
				"commands": state.Commands,
				"messages": state.Messages,
			})
		}
		return store.Get(ctx, id)
	}

	// Process is dead — collect results
	if err := Collect(ctx, store, id); err != nil {
		return nil, fmt.Errorf("poll: collect: %w", err)
	}
	return store.Get(ctx, id)
}

// Collect reads output files and updates the dispatch with final results.
// Called when the process has exited.
func Collect(ctx context.Context, store *Store, id string) error {
	d, err := store.Get(ctx, id)
	if err != nil {
		return err
	}

	if d.IsTerminal() {
		return nil // already collected
	}

	fields := UpdateFields{
		"completed_at": time.Now().Unix(),
	}

	// Parse verdict sidecar
	if d.VerdictFile != nil {
		if vs, vsum, err := parseVerdictFile(*d.VerdictFile); err == nil {
			fields["verdict_status"] = vs
			fields["verdict_summary"] = vsum
		}
	}

	// Parse summary file for token counts
	if d.OutputFile != nil {
		summaryFile := *d.OutputFile + ".summary"
		if inTok, outTok, err := parseSummaryFile(summaryFile); err == nil {
			fields["input_tokens"] = inTok
			fields["output_tokens"] = outTok
		}
	}

	// Infer exit code from verdict
	exitCode := inferExitCode(d, fields)
	fields["exit_code"] = exitCode

	// Determine final status
	status := StatusCompleted
	if exitCode != 0 {
		status = StatusFailed
	}

	return store.UpdateStatus(ctx, id, status, fields)
}

// Wait polls until the dispatch reaches a terminal state or timeout.
func Wait(ctx context.Context, store *Store, id string, pollInterval, timeout time.Duration) (*Dispatch, error) {
	if pollInterval <= 0 {
		pollInterval = 2 * time.Second
	}

	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		d, err := Poll(ctx, store, id)
		if err != nil {
			return nil, err
		}
		if d.IsTerminal() {
			return d, nil
		}

		select {
		case <-ctx.Done():
			// Timeout — kill the process
			if d.PID != nil {
				killProcess(*d.PID)
			}
			store.UpdateStatus(context.Background(), id, StatusTimeout, UpdateFields{
				"completed_at":  time.Now().Unix(),
				"error_message": "timeout waiting for dispatch",
			})
			return store.Get(context.Background(), id)
		case <-ticker.C:
			// continue polling
		}
	}
}

// Kill sends SIGTERM then SIGKILL to a dispatch's process.
func Kill(ctx context.Context, store *Store, id string) error {
	d, err := store.Get(ctx, id)
	if err != nil {
		return err
	}
	if d.IsTerminal() {
		return nil
	}
	if d.PID == nil {
		return store.UpdateStatus(ctx, id, StatusCancelled, UpdateFields{
			"completed_at":  time.Now().Unix(),
			"error_message": "no PID to kill",
		})
	}

	killProcess(*d.PID)

	return store.UpdateStatus(ctx, id, StatusCancelled, UpdateFields{
		"completed_at":  time.Now().Unix(),
		"error_message": "killed by user",
	})
}

// --- internal helpers ---

func isProcessAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil
}

func killProcess(pid int) {
	// Try SIGTERM first
	syscall.Kill(pid, syscall.SIGTERM)

	// Wait up to 5 seconds for graceful shutdown
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		if !isProcessAlive(pid) {
			return
		}
	}

	// Escalate to SIGKILL
	syscall.Kill(pid, syscall.SIGKILL)
}

func readStateFile(path string) (*stateFileData, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state stateFileData
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// parseVerdictFile reads the verdict sidecar written by dispatch.sh.
// Format:
//
//	--- VERDICT ---
//	STATUS: pass|warn|fail
//	FILES: ...
//	FINDINGS: ...
//	SUMMARY: ...
//	---
func parseVerdictFile(path string) (status, summary string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "STATUS: ") {
			status = strings.TrimPrefix(line, "STATUS: ")
		}
		if strings.HasPrefix(line, "SUMMARY: ") {
			summary = strings.TrimPrefix(line, "SUMMARY: ")
		}
	}
	if status == "" {
		return "", "", fmt.Errorf("no STATUS line in verdict")
	}
	return status, summary, nil
}

// parseSummaryFile reads the summary written by dispatch.sh.
// Format: "Tokens: 1234 in / 5678 out"
func parseSummaryFile(path string) (inTokens, outTokens int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "Tokens: ") {
			return parseTokenLine(line)
		}
	}
	return 0, 0, fmt.Errorf("no Tokens line in summary")
}

// parseTokenLine parses "Tokens: 1234 in / 5678 out"
func parseTokenLine(line string) (int, int, error) {
	line = strings.TrimPrefix(line, "Tokens: ")
	parts := strings.Split(line, " / ")
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("unexpected token format: %s", line)
	}

	inStr := strings.TrimSuffix(strings.TrimSpace(parts[0]), " in")
	outStr := strings.TrimSuffix(strings.TrimSpace(parts[1]), " out")

	inTok, err := strconv.Atoi(strings.TrimSpace(inStr))
	if err != nil {
		return 0, 0, fmt.Errorf("parse input tokens: %w", err)
	}
	outTok, err := strconv.Atoi(strings.TrimSpace(outStr))
	if err != nil {
		return 0, 0, fmt.Errorf("parse output tokens: %w", err)
	}
	return inTok, outTok, nil
}

func inferExitCode(d *Dispatch, fields UpdateFields) int {
	if vs, ok := fields["verdict_status"]; ok {
		switch vs {
		case "pass":
			return 0
		case "fail":
			return 1
		case "warn":
			return 0
		}
	}
	// No verdict — process disappeared without writing sidecars
	return -1
}
