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
	"syscall"
	"time"
)

// SpawnOptions configures a dispatch spawn.
type SpawnOptions struct {
	AgentType        string // "codex" (default)
	ProjectDir       string // required: working directory for the agent
	PromptFile       string // required: path to prompt file
	OutputFile       string // optional: path for agent output
	Name             string // optional: human label
	Model            string // optional: codex model
	Sandbox          string // optional: sandbox mode (default: "workspace-write")
	TimeoutSec       int    // optional: agent timeout in seconds
	ScopeID          string // optional: grouping scope
	ParentID         string // optional: parent dispatch ID
	DispatchSH       string // optional: explicit path to dispatch.sh
	ParentDispatchID string // optional: parent dispatch for spawn depth tracking
	Policy           *SpawnPolicy   // optional: spawn policy to enforce
	BudgetQuerier    BudgetQuerier  // optional: budget checker (required if Policy.BudgetEnforce)
}

// SpawnResult holds the result of a spawn operation.
type SpawnResult struct {
	ID  string
	Cmd *exec.Cmd // retained for in-process callers; nil after ic exits
	PID int
}

// Spawn creates a new dispatch record and starts the agent process.
func Spawn(ctx context.Context, store *Store, opts SpawnOptions) (*SpawnResult, error) {
	if opts.ProjectDir == "" {
		return nil, fmt.Errorf("spawn: project_dir is required")
	}
	if opts.PromptFile == "" {
		return nil, fmt.Errorf("spawn: prompt_file is required")
	}
	if opts.AgentType == "" {
		opts.AgentType = "codex"
	}
	if opts.Sandbox == "" {
		opts.Sandbox = "workspace-write"
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
		AgentType:  opts.AgentType,
		ProjectDir: opts.ProjectDir,
		PromptFile: &opts.PromptFile,
		PromptHash: &promptHash,
		OutputFile: &outputFile,
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

	// Compute spawn depth from parent dispatch
	if opts.ParentDispatchID != "" {
		d.ParentDispatchID = opts.ParentDispatchID
		parent, err := store.Get(ctx, opts.ParentDispatchID)
		if err == nil {
			d.SpawnDepth = parent.SpawnDepth + 1
		}
		// If parent not found, depth stays 0 (best-effort)
	}

	// Check spawn policy before creating the record
	if opts.Policy != nil {
		if err := CheckPolicy(ctx, store, opts.BudgetQuerier, *opts.Policy, d); err != nil {
			return nil, err
		}
	}

	id, err := store.Create(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("spawn: create record: %w", err)
	}

	// Build and start the command
	cmd, err := buildCmd(opts, outputFile)
	if err != nil {
		store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{
			"error_message": fmt.Sprintf("build command: %v", err),
		})
		return nil, fmt.Errorf("spawn: %w", err)
	}

	if err := cmd.Start(); err != nil {
		store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{
			"error_message": fmt.Sprintf("start: %v", err),
		})
		return nil, fmt.Errorf("spawn: start process: %w", err)
	}

	pid := cmd.Process.Pid
	now := time.Now().Unix()
	store.UpdateStatus(ctx, id, StatusRunning, UpdateFields{
		"pid":        pid,
		"started_at": now,
	})

	return &SpawnResult{ID: id, Cmd: cmd, PID: pid}, nil
}

// buildCmd constructs the exec.Cmd for the agent.
func buildCmd(opts SpawnOptions, outputFile string) (*exec.Cmd, error) {
	dispatchSH := resolveDispatchSH(opts.DispatchSH)

	var cmd *exec.Cmd
	if dispatchSH != "" {
		// Use dispatch.sh wrapper
		args := []string{"-C", opts.ProjectDir, "-o", outputFile, "--prompt-file", opts.PromptFile}
		if opts.Name != "" {
			args = append(args, "-n", opts.Name)
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
		// Fallback: bare codex exec (no JSONL parsing, no verdict)
		args := []string{"exec", "--prompt-file", opts.PromptFile}
		if opts.Model != "" {
			args = append(args, "-m", opts.Model)
		}
		cmd = exec.Command("codex", args...)
		cmd.Dir = opts.ProjectDir
	}

	// New process group for clean signal propagation
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

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

	// Walk up from CWD looking for hub/clavain/scripts/dispatch.sh
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	for {
		candidate := filepath.Join(dir, "hub", "clavain", "scripts", "dispatch.sh")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
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
