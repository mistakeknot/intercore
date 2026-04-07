package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/scheduler"
	"github.com/mistakeknot/intercore/internal/state"
)

// --- Dispatch Commands ---

func cmdDispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("dispatch: missing subcommand", "expected", "spawn, status, list, poll, wait, kill, prune, tokens")
		return 3
	}

	switch args[0] {
	case "spawn":
		return cmdDispatchSpawn(ctx, args[1:])
	case "status":
		return cmdDispatchStatus(ctx, args[1:])
	case "list":
		return cmdDispatchList(ctx, args[1:])
	case "poll":
		return cmdDispatchPoll(ctx, args[1:])
	case "wait":
		return cmdDispatchWait(ctx, args[1:])
	case "kill":
		return cmdDispatchKill(ctx, args[1:])
	case "prune":
		return cmdDispatchPrune(ctx, args[1:])
	case "tokens":
		return cmdDispatchTokens(ctx, args[1:])
	case "retry":
		return cmdDispatchRetry(ctx, args[1:])
	default:
		slog.Error("dispatch: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdDispatchSpawn(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	opts := dispatch.SpawnOptions{
		AgentType:  f.String("type", ""),
		PromptFile: f.String("prompt-file", ""),
		ProjectDir: f.String("project", ""),
		OutputFile: f.String("output", ""),
		Name:       f.String("name", ""),
		Model:      f.String("model", ""),
		Sandbox:    f.String("sandbox", ""),
		SandboxSpec: f.String("sandbox-spec", ""),
		ScopeID:    f.String("scope-id", ""),
		ParentID:   f.String("parent-id", ""),
		DispatchSH: f.String("dispatch-sh", ""),
	}
	scheduled := f.Bool("scheduled")
	schedulerSession := f.String("scheduler-session", "")

	if f.Has("timeout") {
		dur, err := f.Duration("timeout", 0)
		if err != nil {
			slog.Error("dispatch spawn: invalid timeout", "value", f.String("timeout", ""))
			return 3
		}
		opts.TimeoutSec = int(dur.Seconds())
	}

	if opts.PromptFile == "" {
		slog.Error("dispatch spawn: --prompt-file is required")
		return 3
	}
	if opts.ProjectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("dispatch spawn: cannot determine project dir", "error", err)
			return 2
		}
		opts.ProjectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch spawn failed", "error", err)
		return 2
	}
	defer d.Close()

	// --scheduled: submit to scheduler instead of direct exec.
	if scheduled {
		spawnJSON, err := scheduler.MarshalSpawnOpts(opts)
		if err != nil {
			slog.Error("dispatch spawn: marshal opts", "error", err)
			return 2
		}

		agentType := opts.AgentType
		if agentType == "" {
			agentType = "codex"
		}

		job := scheduler.NewSpawnJob("", scheduler.JobTypeDispatch, schedulerSession)
		job.AgentType = agentType
		job.ProjectDir = opts.ProjectDir
		job.SpawnOpts = spawnJSON

		schedStore := scheduler.NewStore(d.SqlDB())
		if err := schedStore.Create(ctx, job); err != nil {
			slog.Error("dispatch spawn: scheduler submit failed", "error", err)
			return 2
		}

		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"job_id":    job.ID,
				"scheduled": true,
			})
		} else {
			fmt.Println(job.ID)
		}
		return 0
	}

	// Portfolio dispatch limit check (best-effort, relay-maintained cache).
	// Note: this is advisory, not atomic — concurrent spawns may exceed the limit.
	if opts.ScopeID != "" {
		if limited, msg := checkPortfolioDispatchLimit(ctx, d.SqlDB(), opts.ScopeID); limited {
			slog.Error("dispatch spawn: rejected", "reason", msg)
			return 1
		}
	}

	store := dispatch.New(d.SqlDB(), nil)
	result, err := dispatch.Spawn(ctx, store, opts)
	if err != nil {
		slog.Error("dispatch spawn failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":  result.ID,
			"pid": result.PID,
		})
	} else {
		fmt.Println(result.ID)
	}
	return 0
}

func cmdDispatchStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch status: usage: ic dispatch status <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch status failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := store.Get(ctx, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			slog.Error("dispatch status: not found", "id", args[0])
			return 1
		}
		slog.Error("dispatch status failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}
	return 0
}

func cmdDispatchList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	activeOnly := f.Bool("active")
	scopeFilter := f.StringPtr("scope")

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	var dispatches []*dispatch.Dispatch

	if activeOnly {
		dispatches, err = store.ListActive(ctx)
	} else {
		dispatches, err = store.List(ctx, scopeFilter)
	}
	if err != nil {
		slog.Error("dispatch list failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(dispatches))
		for i, disp := range dispatches {
			items[i] = dispatchToMap(disp)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, disp := range dispatches {
			name := ""
			if disp.Name != nil {
				name = *disp.Name
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", disp.ID, disp.Status, disp.AgentType, name)
		}
	}
	return 0
}

func cmdDispatchPoll(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: usage: ic dispatch poll <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch poll failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := dispatch.Poll(ctx, store, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			slog.Error("dispatch poll: not found", "id", args[0])
			return 1
		}
		slog.Error("dispatch poll failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}
	return 0
}

func cmdDispatchWait(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: usage: ic dispatch wait <id> [--timeout=<dur>] [--poll=<dur>]\n")
		return 3
	}
	id := f.Positionals[0]

	timeout, err := f.Duration("timeout", 0)
	if err != nil {
		slog.Error("dispatch wait: invalid timeout", "value", f.String("timeout", ""))
		return 3
	}

	pollInterval, err := f.Duration("poll", 0)
	if err != nil {
		slog.Error("dispatch wait: invalid poll interval", "value", f.String("poll", ""))
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch wait failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := dispatch.Wait(ctx, store, id, pollInterval, timeout)
	if err != nil {
		if err == dispatch.ErrNotFound {
			slog.Error("dispatch wait: not found", "id", id)
			return 1
		}
		slog.Error("dispatch wait failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}

	if disp.Status == dispatch.StatusFailed || disp.Status == dispatch.StatusTimeout {
		return 1
	}
	return 0
}

func cmdDispatchKill(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: usage: ic dispatch kill <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch kill failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	if err := dispatch.Kill(ctx, store, args[0]); err != nil {
		if err == dispatch.ErrNotFound {
			slog.Error("dispatch kill: not found", "id", args[0])
			return 1
		}
		slog.Error("dispatch kill failed", "error", err)
		return 2
	}

	fmt.Println("killed")
	return 0
}

func cmdDispatchPrune(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	if !f.Has("older-than") {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: usage: ic dispatch prune --older-than=<duration>\n")
		return 3
	}

	dur, err := f.Duration("older-than", 0)
	if err != nil {
		slog.Error("dispatch prune: invalid duration", "error", err)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch prune failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	count, err := store.Prune(ctx, dur)
	if err != nil {
		slog.Error("dispatch prune failed", "error", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdDispatchTokens(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	fields := dispatch.UpdateFields{}

	if f.Has("in") {
		v, err := f.Int("in", 0)
		if err != nil {
			slog.Error("dispatch tokens: invalid --in", "value", f.String("in", ""))
			return 3
		}
		fields["input_tokens"] = v
	}

	if f.Has("out") {
		v, err := f.Int("out", 0)
		if err != nil {
			slog.Error("dispatch tokens: invalid --out", "value", f.String("out", ""))
			return 3
		}
		fields["output_tokens"] = v
	}

	if f.Has("cache") {
		v, err := f.Int("cache", 0)
		if err != nil {
			slog.Error("dispatch tokens: invalid --cache", "value", f.String("cache", ""))
			return 3
		}
		fields["cache_hits"] = v
	}

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch tokens: usage: ic dispatch tokens <id> [--in=N] [--out=N] [--cache=N]\n")
		return 3
	}
	id := f.Positionals[0]

	if len(fields) == 0 {
		slog.Error("dispatch tokens: at least one of --in, --out, --cache is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch tokens failed", "error", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	if err := dStore.UpdateTokens(ctx, id, fields); err != nil {
		if err == dispatch.ErrNotFound {
			slog.Error("dispatch tokens: not found", "id", id)
			return 1
		}
		slog.Error("dispatch tokens failed", "error", err)
		return 2
	}

	// Budget check: if this dispatch belongs to a run, check budget thresholds
	if disp, err := dStore.Get(ctx, id); err == nil && disp.ScopeID != nil {
		pStore := phase.New(d.SqlDB())
		sStore := state.New(d.SqlDB())
		checker := budget.New(pStore, dStore, sStore, nil)
		result, err := checker.Check(ctx, *disp.ScopeID)
		if err != nil {
			slog.Debug("budget: check", "error", err)
		} else if result != nil {
			if result.Exceeded {
				slog.Warn("budget exceeded", "used", result.Used, "budget", result.Budget)
			} else if result.Warning {
				slog.Warn("budget warning", "used", result.Used, "budget", result.Budget, "warn_pct", result.WarnPct)
			}
		}
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]string{"status": "updated"})
	} else {
		fmt.Println("updated")
	}
	return 0
}

// --- dispatch output helpers ---

func dispatchToMap(d *dispatch.Dispatch) map[string]interface{} {
	m := map[string]interface{}{
		"id":          d.ID,
		"agent_type":  d.AgentType,
		"status":      d.Status,
		"project_dir": d.ProjectDir,
		"turns":       d.Turns,
		"commands":    d.Commands,
		"messages":    d.Messages,
		"in_tokens":   d.InputTokens,
		"out_tokens":  d.OutputTokens,
		"created_at":  d.CreatedAt,
	}
	if d.PromptFile != nil {
		m["prompt_file"] = *d.PromptFile
	}
	if d.OutputFile != nil {
		m["output_file"] = *d.OutputFile
	}
	if d.PID != nil {
		m["pid"] = *d.PID
	}
	if d.ExitCode != nil {
		m["exit_code"] = *d.ExitCode
	}
	if d.Name != nil {
		m["name"] = *d.Name
	}
	if d.Model != nil {
		m["model"] = *d.Model
	}
	if d.StartedAt != nil {
		m["started_at"] = *d.StartedAt
	}
	if d.CompletedAt != nil {
		m["completed_at"] = *d.CompletedAt
	}
	if d.VerdictStatus != nil {
		m["verdict_status"] = *d.VerdictStatus
	}
	if d.VerdictSummary != nil {
		m["verdict_summary"] = *d.VerdictSummary
	}
	if d.ErrorMessage != nil {
		m["error_message"] = *d.ErrorMessage
	}
	if d.CacheHits != nil {
		m["cache_hits"] = *d.CacheHits
	}
	if d.SandboxSpec != nil {
		m["sandbox_spec"] = json.RawMessage(*d.SandboxSpec)
	}
	if d.SandboxEffective != nil {
		m["sandbox_effective"] = json.RawMessage(*d.SandboxEffective)
	}
	if d.ScopeID != nil {
		m["scope_id"] = *d.ScopeID
	}
	if d.ParentID != nil {
		m["parent_id"] = *d.ParentID
	}
	return m
}

func printDispatch(d *dispatch.Dispatch) {
	fmt.Printf("ID:      %s\n", d.ID)
	fmt.Printf("Status:  %s\n", d.Status)
	fmt.Printf("Type:    %s\n", d.AgentType)
	if d.Name != nil {
		fmt.Printf("Name:    %s\n", *d.Name)
	}
	if d.PID != nil {
		fmt.Printf("PID:     %d\n", *d.PID)
	}
	fmt.Printf("Project: %s\n", d.ProjectDir)
	if d.PromptFile != nil {
		fmt.Printf("Prompt:  %s\n", *d.PromptFile)
	}
	if d.OutputFile != nil {
		fmt.Printf("Output:  %s\n", *d.OutputFile)
	}
	if d.Turns > 0 || d.Commands > 0 || d.Messages > 0 {
		fmt.Printf("Stats:   %d turns, %d commands, %d messages\n", d.Turns, d.Commands, d.Messages)
	}
	if d.InputTokens > 0 || d.OutputTokens > 0 {
		if d.CacheHits != nil && *d.CacheHits > 0 {
			fmt.Printf("Tokens:  %d in / %d out / %d cache\n", d.InputTokens, d.OutputTokens, *d.CacheHits)
		} else {
			fmt.Printf("Tokens:  %d in / %d out\n", d.InputTokens, d.OutputTokens)
		}
	}
	if d.VerdictStatus != nil {
		fmt.Printf("Verdict: %s\n", *d.VerdictStatus)
	}
	if d.VerdictSummary != nil {
		fmt.Printf("Summary: %s\n", *d.VerdictSummary)
	}
	if d.SandboxSpec != nil {
		fmt.Printf("Sandbox Spec: %s\n", *d.SandboxSpec)
	}
	if d.SandboxEffective != nil {
		fmt.Printf("Sandbox Eff:  %s\n", *d.SandboxEffective)
	}
	if d.ExitCode != nil {
		fmt.Printf("Exit:    %d\n", *d.ExitCode)
	}
	if d.ErrorMessage != nil {
		fmt.Printf("Error:   %s\n", *d.ErrorMessage)
	}
}

func cmdDispatchRetry(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	policy := dispatch.DefaultRetryPolicy()

	if f.Has("max-retries") {
		v, err := f.Int("max-retries", 0)
		if err != nil {
			slog.Error("dispatch retry: invalid --max-retries", "value", f.String("max-retries", ""))
			return 3
		}
		policy.MaxRetries = v
	}
	if f.Has("base-backoff") {
		dur, err := f.Duration("base-backoff", 0)
		if err != nil {
			slog.Error("dispatch retry: invalid --base-backoff", "value", f.String("base-backoff", ""))
			return 3
		}
		policy.BaseBackoff = dur
	}
	if f.Has("max-backoff") {
		dur, err := f.Duration("max-backoff", 0)
		if err != nil {
			slog.Error("dispatch retry: invalid --max-backoff", "value", f.String("max-backoff", ""))
			return 3
		}
		policy.MaxBackoff = dur
	}
	if f.Bool("no-retry-timeout") {
		policy.RetryOnTimeout = false
	}
	waitBackoff := f.Bool("wait")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch retry: usage: ic dispatch retry <id> [--max-retries=N] [--base-backoff=<dur>] [--max-backoff=<dur>] [--no-retry-timeout] [--wait]\n")
		return 3
	}
	id := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch retry failed", "error", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)

	var result *dispatch.RetryResult
	if waitBackoff {
		result, err = dispatch.RetryWithBackoff(ctx, store, id, policy)
	} else {
		result, err = dispatch.Retry(ctx, store, id, policy)
	}
	if err != nil {
		slog.Error("dispatch retry failed", "error", err)
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"original_id": result.OriginalID,
			"new_id":      result.NewID,
			"attempt":     result.Attempt,
			"backoff_ms":  result.BackoffMs,
		})
	} else {
		fmt.Printf("Retry %d: %s → %s (backoff %dms)\n", result.Attempt, result.OriginalID, result.NewID, result.BackoffMs)
	}
	return 0
}

// checkPortfolioDispatchLimit checks if the dispatch limit for a portfolio run is exceeded.
// Returns (true, message) if the limit is reached, (false, "") otherwise.
// Degrades gracefully: returns false if any lookup fails (no relay, no parent, etc.).
func checkPortfolioDispatchLimit(ctx context.Context, db *sql.DB, scopeID string) (bool, string) {
	phaseStore := phase.New(db)
	stateStore := state.New(db)

	run, err := phaseStore.Get(ctx, scopeID)
	if err != nil || run.ParentRunID == nil {
		return false, ""
	}

	parent, err := phaseStore.Get(ctx, *run.ParentRunID)
	if err != nil || parent.MaxDispatches <= 0 {
		return false, ""
	}

	payload, err := stateStore.Get(ctx, "active-dispatch-count", *run.ParentRunID)
	if err != nil {
		slog.Warn("dispatch spawn: no relay data for portfolio, dispatch limit not enforced", "portfolio_id", *run.ParentRunID)
		return false, ""
	}

	var countStr string
	if err := json.Unmarshal(payload, &countStr); err != nil {
		return false, ""
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return false, ""
	}

	if count >= parent.MaxDispatches {
		return true, fmt.Sprintf("portfolio dispatch limit reached (%d/%d)", count, parent.MaxDispatches)
	}
	return false, ""
}
