package dispatch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/mistakeknot/intercore/internal/routing"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// SpawnOptions configures a dispatch spawn.
type SpawnOptions struct {
	Decision         *routing.ReasoningDecision // complete contract, serialized with scheduled jobs
	AgentType        string                     // backend passed to dispatch.sh; "codex" (default) supports direct execution
	ProjectDir       string                     // required: working directory for the agent
	PromptFile       string                     // required: path to prompt file
	OutputFile       string                     // optional: path for agent output
	Name             string                     // optional: human label
	Model            string                     // optional: codex model
	Sandbox          string                     // optional: sandbox mode (default: "workspace-write")
	SandboxSpec      string                     // optional: JSON sandbox specification
	TimeoutSec       int                        // optional: agent timeout in seconds
	ScopeID          string                     // optional: grouping scope
	RunID            string                     // optional: strict existing-run binding; becomes ScopeID
	ParentID         string                     // optional: parent dispatch ID
	DispatchSH       string                     // optional: explicit path to dispatch.sh
	ParentDispatchID string                     // optional: parent dispatch for spawn depth tracking
	Policy           *SpawnPolicy               // optional: spawn policy to enforce
	BudgetQuerier    BudgetQuerier              // optional additional veto; never replaces persisted budget admission
	retry            bool                       // internal retry admission preserves the original spawn depth
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
	if opts.Decision != nil {
		if err := validateReasoningDecision(opts.Decision); err != nil {
			return nil, fmt.Errorf("spawn: reasoning contract: %w", err)
		}
		opts.AgentType = opts.Decision.Profile.Backend
		opts.Model = opts.Decision.Profile.Model
	}
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
	if opts.Decision != nil {
		if err := recordReasoningDecision(ctx, store, id, opts.ProjectDir, opts.Decision); err != nil {
			_ = store.UpdateStatus(ctx, id, StatusFailed, UpdateFields{"error_message": err.Error()})
			return nil, err
		}
	}
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

func validateReasoningDecision(d *routing.ReasoningDecision) error {
	if d.PolicySource == "" || d.PolicyHash == "" || d.RequestedRole == "" {
		return fmt.Errorf("missing source, hash or role")
	}
	cfg, err := routing.LoadConfig(d.PolicySource, "")
	if err != nil {
		return err
	}
	if cfg.PolicyHash != d.PolicyHash {
		return fmt.Errorf("selected policy changed; reclassify before admission")
	}
	resolved, err := routing.NewResolver(cfg).ResolveDecision(d.RequestedRole, d.ProducerIdentity, d.PolicyProfile, d.Context)
	if err != nil {
		return err
	}
	// Compare the serialized contract. JSON round trips normalize optional empty
	// slices (for example a profile's fallbacks: []) to nil, without changing
	// the policy meaning. Semantically distinct context fields retain their tags.
	want, err := json.Marshal(resolved)
	if err != nil {
		return err
	}
	got, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if !bytes.Equal(want, got) {
		return fmt.Errorf("decision does not match selected policy; resolve again")
	}
	return nil
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
		if d := opts.Decision; d != nil {
			raw, _ := json.Marshal(d)
			candidate, _ := json.Marshal(routing.DispatchCandidate{ProfileRef: d.ProfileRef, Profile: d.Profile})
			args = append(args, "--role-resolved", "--role", d.RequestedRole, "--resolved-profile-ref", d.ProfileRef, "--resolved-route-json", string(raw), "--resolved-profile-json", string(candidate), "--reasoning-effort", d.Profile.ReasoningEffort, "--service-tier", d.Profile.ServiceTier)
			if d.ProducerIdentity != "" {
				args = append(args, "--producer-identity", d.ProducerIdentity, "--validator-relationship", d.ValidatorRelationship)
			}
			if d.Profile.MinimumCodexVersion != "" {
				args = append(args, "--minimum-codex-version", d.Profile.MinimumCodexVersion)
			}
		}
		if opts.Sandbox != "" {
			args = append(args, "--sandbox", opts.Sandbox)
		}
		if opts.TimeoutSec > 0 {
			args = append(args, "--timeout", fmt.Sprintf("%d", opts.TimeoutSec))
		}
		cmd = exec.Command("bash", append([]string{dispatchSH}, args...)...)
	} else {
		if opts.Decision != nil {
			return nil, fmt.Errorf("governed dispatch requires dispatch.sh; refusing bare model fallback")
		}
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

func recordReasoningDecision(ctx context.Context, store *Store, id, project string, decision *routing.ReasoningDecision) error {
	raw, err := json.Marshal(decision)
	if err != nil {
		return err
	}
	excluded, _ := json.Marshal(decision.Excluded)
	_, err = routing.NewDecisionStore(store.db).Record(ctx, routing.RecordDecisionOpts{DispatchID: id, Agent: decision.RequestedRole, SelectedModel: decision.Profile.Model, RuleMatched: "reasoning-contract", PolicyHash: decision.PolicyHash, ContextJSON: string(raw), Excluded: string(excluded), ProjectDir: project})
	return err
}

func loadReasoningDecision(ctx context.Context, store *Store, id string) (*routing.ReasoningDecision, error) {
	var raw string
	err := store.db.QueryRowContext(ctx, "SELECT context_json FROM routing_decisions WHERE dispatch_id=? AND rule_matched='reasoning-contract' ORDER BY id DESC LIMIT 1", id).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var d routing.ReasoningDecision
	if err = json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, err
	}
	return &d, nil
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
