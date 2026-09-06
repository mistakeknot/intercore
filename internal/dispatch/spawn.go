package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SpawnOptions configures a dispatch spawn.
type SpawnOptions struct {
	AgentType        string        // backend passed to dispatch.sh; "codex" (default) supports direct execution
	ProjectDir       string        // required: working directory for the agent
	PromptFile       string        // required: path to prompt file
	OutputFile       string        // optional: path for agent output
	Name             string        // optional: human label
	Model            string        // optional: codex model
	Sandbox          string        // optional: sandbox mode (default: "workspace-write")
	SandboxSpec      string        // optional: JSON sandbox specification
	TimeoutSec       int           // optional: agent timeout in seconds
	ScopeID          string        // optional: grouping scope
	RunID            string        // optional: strict existing-run binding; becomes ScopeID
	ParentID         string        // optional: parent dispatch ID
	DispatchSH       string        // optional: explicit path to dispatch.sh
	ParentDispatchID string        // optional: parent dispatch for spawn depth tracking
	Policy           *SpawnPolicy  // optional: spawn policy to enforce
	BudgetQuerier    BudgetQuerier // optional additional veto; never replaces persisted budget admission
	retry            bool          // internal retry admission preserves the original spawn depth
}

// SpawnResult holds the result of a spawn operation.
type SpawnResult struct {
	ID      string
	Cmd     *exec.Cmd // retained for in-process callers; nil after ic exits
	PID     int
	process processIdentity
}

// Terminate uses the birth identity captured before this spawn was detached.
func (r *SpawnResult) Terminate() error { return terminateRecordedProcess(r.process) }

// Spawn creates a new dispatch record and starts the agent process.
func Spawn(ctx context.Context, store *Store, opts SpawnOptions) (*SpawnResult, error) {
	if err := opts.Validate(); err != nil {
		return nil, err
	}
	if err := checkProcessInspection(); err != nil {
		return nil, err
	}
	if opts.RunID != "" {
		opts.ScopeID = opts.RunID
	}
	if opts.AgentType == "" {
		opts.AgentType = "codex"
	}
	if opts.Sandbox == "" {
		opts.Sandbox = "workspace-write"
		if opts.AgentType == "flere" {
			opts.Sandbox = "read-only"
		}
	}

	// Capture base repo commit (git HEAD) for write-set conflict detection
	baseCommit, _ := gitHeadCommit(opts.ProjectDir)

	// Hash the prompt file for dedup detection
	promptHash, err := hashFile(opts.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("spawn: hash prompt: %w", err)
	}

	// Determine output file path
	outputFile := opts.OutputFile
	if outputFile == "" {
		outputFile = filepath.Join(os.TempDir(), fmt.Sprintf("ic-dispatch-%d.md", time.Now().UnixNano()))
	}
	verdictFile := outputFile + ".verdict"

	// Build the dispatch record
	d := &Dispatch{
		AgentType:   opts.AgentType,
		ProjectDir:  opts.ProjectDir,
		PromptFile:  &opts.PromptFile,
		PromptHash:  &promptHash,
		OutputFile:  &outputFile,
		VerdictFile: &verdictFile,
	}
	if opts.Name != "" {
		d.Name = &opts.Name
	}
	if opts.Model != "" {
		d.Model = &opts.Model
	}
	if opts.Sandbox != "" {
		d.Sandbox = &opts.Sandbox
	}
	if opts.SandboxSpec != "" {
		d.SandboxSpec = &opts.SandboxSpec
	}
	if opts.TimeoutSec > 0 {
		d.TimeoutSec = &opts.TimeoutSec
	}
	if opts.ScopeID != "" {
		d.ScopeID = &opts.ScopeID
	}
	if opts.ParentID != "" {
		d.ParentID = &opts.ParentID
	}
	if baseCommit != "" {
		d.BaseRepoCommit = &baseCommit
	}

	d.ParentDispatchID = opts.ParentDispatchID
	id, err := store.admit(ctx, d, opts)
	if err != nil {
		return nil, fmt.Errorf("spawn: admission: %w", err)
	}

	// Build and start the command
	cmd, err := buildCmd(opts, outputFile)
	if err != nil {
		store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{
			"error_message": fmt.Sprintf("build command: %v", err),
		})
		return nil, fmt.Errorf("spawn: %w", err)
	}

	// Overwrite ambient identity only after this attempt has been admitted.
	// The dispatch ID is the attempt identity; the worker cannot create another.
	runID := ""
	if d.ScopeID != nil {
		runID = *d.ScopeID
	}
	cmd.Env = append(os.Environ(), "IC_RUN_ID="+runID, "IC_DISPATCH_ID="+id,
		"CLAVAIN_DISPATCH_ID="+id, "IC_DISPATCH_ATTEMPT="+fmt.Sprint(d.RetryCount), "IC_PROMPT_HASH="+promptHash)
	if err := cmd.Start(); err != nil {
		store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{
			"error_message": fmt.Sprintf("start: %v", err),
		})
		return nil, fmt.Errorf("spawn: start process: %w", err)
	}

	pid := cmd.Process.Pid
	identity, err := store.recordProcessIdentity(ctx, id, pid)
	if err != nil {
		// No wait/reaper has started, so this is still our unreaped child and its
		// PID cannot have been recycled. Never leave an ungovernable live attempt.
		killProcess(pid)
		_ = cmd.Wait()
		_ = store.UpdateStatus(context.Background(), id, StatusFailed, UpdateFields{
			"error_message":     "record process identity: " + err.Error(),
			"quarantine_reason": WorkerOutcomeIndeterminate,
		})
		return nil, fmt.Errorf("spawn: process identity: %w", err)
	}
	return &SpawnResult{ID: id, Cmd: cmd, PID: pid, process: identity}, nil
}

// buildCmd constructs the exec.Cmd for the agent.
func buildCmd(opts SpawnOptions, outputFile string) (*exec.Cmd, error) {
	dispatchSH := resolveDispatchSH(opts.DispatchSH)

	var cmd *exec.Cmd
	if dispatchSH != "" {
		// Use dispatch.sh wrapper
		args := []string{"--to", opts.AgentType, "-C", opts.ProjectDir, "-o", outputFile, "--prompt-file", opts.PromptFile}
		if opts.Name != "" {
			args = append(args, "--name", opts.Name)
		}
		if opts.Model != "" {
			args = append(args, "-m", opts.Model)
		}
		if opts.Sandbox != "" {
			args = append(args, "--sandbox", opts.Sandbox)
		}
		if opts.TimeoutSec > 0 {
			args = append(args, "--timeout", fmt.Sprintf("%d", opts.TimeoutSec))
		}
		cmd = exec.Command("bash", append([]string{dispatchSH}, args...)...)
	} else {
		if opts.AgentType != "codex" {
			return nil, fmt.Errorf("backend %q requires dispatch.sh; direct execution only supports codex", opts.AgentType)
		}
		// Fallback: bare codex exec (no JSONL parsing, no verdict)
		args := []string{"exec", "--prompt-file", opts.PromptFile}
		if opts.Model != "" {
			args = append(args, "-m", opts.Model)
		}
		cmd = exec.Command("codex", args...)
		cmd.Dir = opts.ProjectDir
	}

	// New process group for clean signal propagation
	cmd.SysProcAttr = platformSysProcAttr()

	// Detach stdin, let stdout/stderr go to /dev/null
	// (dispatch.sh handles its own output redirection)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil

	return cmd, nil
}

// resolveDispatchSH finds dispatch.sh in order of precedence:
// 1. explicit path, 2. CLAVAIN_DISPATCH_SH env, 3. monorepo walk-up
func resolveDispatchSH(explicit string) string {
	if explicit != "" {
		if _, err := os.Stat(explicit); err == nil {
			return explicit
		}
	}

	if envPath := os.Getenv("CLAVAIN_DISPATCH_SH"); envPath != "" {
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}

	// Walk up from CWD, preferring the current monorepo layout. Older
	// installations used hub/clavain and still support the wrapper contract.
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		for _, layout := range []string{filepath.Join("os", "Clavain"), filepath.Join("hub", "clavain")} {
			candidate := filepath.Join(dir, layout, "scripts", "dispatch.sh")
			if _, err := os.Stat(candidate); err == nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	return "" // fallback to bare codex
}

// gitHeadCommit runs git rev-parse HEAD in the given directory.
// Returns empty string on any error (not a git repo, git not installed, etc.).
func gitHeadCommit(dir string) (string, error) {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "HEAD")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h[:8]), nil // 16-char hex prefix
}
