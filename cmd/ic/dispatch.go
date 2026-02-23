package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/budget"
	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
	"github.com/mistakeknot/interverse/infra/intercore/internal/scheduler"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

// --- Dispatch Commands ---

func cmdDispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: dispatch: missing subcommand (spawn, status, list, poll, wait, kill, prune, tokens)\n")
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
	default:
		fmt.Fprintf(os.Stderr, "ic: dispatch: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdDispatchSpawn(ctx context.Context, args []string) int {
	opts := dispatch.SpawnOptions{}
	var scheduled bool
	var schedulerSession string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--scheduled":
			scheduled = true
		case strings.HasPrefix(args[i], "--scheduler-session="):
			schedulerSession = strings.TrimPrefix(args[i], "--scheduler-session=")
		case strings.HasPrefix(args[i], "--type="):
			opts.AgentType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--prompt-file="):
			opts.PromptFile = strings.TrimPrefix(args[i], "--prompt-file=")
		case strings.HasPrefix(args[i], "--project="):
			opts.ProjectDir = strings.TrimPrefix(args[i], "--project=")
		case strings.HasPrefix(args[i], "--output="):
			opts.OutputFile = strings.TrimPrefix(args[i], "--output=")
		case strings.HasPrefix(args[i], "--name="):
			opts.Name = strings.TrimPrefix(args[i], "--name=")
		case strings.HasPrefix(args[i], "--model="):
			opts.Model = strings.TrimPrefix(args[i], "--model=")
		case strings.HasPrefix(args[i], "--sandbox="):
			opts.Sandbox = strings.TrimPrefix(args[i], "--sandbox=")
		case strings.HasPrefix(args[i], "--sandbox-spec="):
			opts.SandboxSpec = strings.TrimPrefix(args[i], "--sandbox-spec=")
		case strings.HasPrefix(args[i], "--timeout="):
			val := strings.TrimPrefix(args[i], "--timeout=")
			dur, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: dispatch spawn: invalid timeout: %s\n", val)
				return 3
			}
			opts.TimeoutSec = int(dur.Seconds())
		case strings.HasPrefix(args[i], "--scope-id="):
			opts.ScopeID = strings.TrimPrefix(args[i], "--scope-id=")
		case strings.HasPrefix(args[i], "--parent-id="):
			opts.ParentID = strings.TrimPrefix(args[i], "--parent-id=")
		case strings.HasPrefix(args[i], "--dispatch-sh="):
			opts.DispatchSH = strings.TrimPrefix(args[i], "--dispatch-sh=")
		default:
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if opts.PromptFile == "" {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: --prompt-file is required\n")
		return 3
	}
	if opts.ProjectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: cannot determine project dir: %v\n", err)
			return 2
		}
		opts.ProjectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: %v\n", err)
		return 2
	}
	defer d.Close()

	// --scheduled: submit to scheduler instead of direct exec.
	if scheduled {
		spawnJSON, err := scheduler.MarshalSpawnOpts(opts)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: marshal opts: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: scheduler submit: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: %s\n", msg)
			return 1
		}
	}

	store := dispatch.New(d.SqlDB(), nil)
	result, err := dispatch.Spawn(ctx, store, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: dispatch status: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := store.Get(ctx, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch status: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch status: %v\n", err)
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
	var activeOnly bool
	var scopeFilter *string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--active":
			activeOnly = true
		case strings.HasPrefix(args[i], "--scope="):
			s := strings.TrimPrefix(args[i], "--scope=")
			scopeFilter = &s
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch list: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: dispatch list: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := dispatch.Poll(ctx, store, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch poll: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: %v\n", err)
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
	var id string
	var timeoutStr string
	var pollStr string

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--timeout="):
			timeoutStr = strings.TrimPrefix(args[i], "--timeout=")
		case strings.HasPrefix(args[i], "--poll="):
			pollStr = strings.TrimPrefix(args[i], "--poll=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: usage: ic dispatch wait <id> [--timeout=<dur>] [--poll=<dur>]\n")
		return 3
	}
	id = positional[0]

	var timeout time.Duration
	if timeoutStr != "" {
		var err error
		timeout, err = time.ParseDuration(timeoutStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: invalid timeout: %s\n", timeoutStr)
			return 3
		}
	}

	var pollInterval time.Duration
	if pollStr != "" {
		var err error
		pollInterval, err = time.ParseDuration(pollStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: invalid poll interval: %s\n", pollStr)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	disp, err := dispatch.Wait(ctx, store, id, pollInterval, timeout)
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	if err := dispatch.Kill(ctx, store, args[0]); err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch kill: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: %v\n", err)
		return 2
	}

	fmt.Println("killed")
	return 0
}

func cmdDispatchPrune(ctx context.Context, args []string) int {
	var olderThan string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--older-than=") {
			olderThan = strings.TrimPrefix(args[i], "--older-than=")
		} else if args[i] == "--older-than" && i+1 < len(args) {
			i++
			olderThan = args[i]
		}
	}
	if olderThan == "" {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: usage: ic dispatch prune --older-than=<duration>\n")
		return 3
	}

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: invalid duration: %v\n", err)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB(), nil)
	count, err := store.Prune(ctx, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: %v\n", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdDispatchTokens(ctx context.Context, args []string) int {
	var positional []string
	fields := dispatch.UpdateFields{}

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--in="):
			v, err := strconv.Atoi(strings.TrimPrefix(args[i], "--in="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: dispatch tokens: invalid --in: %s\n", args[i])
				return 3
			}
			fields["input_tokens"] = v
		case strings.HasPrefix(args[i], "--out="):
			v, err := strconv.Atoi(strings.TrimPrefix(args[i], "--out="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: dispatch tokens: invalid --out: %s\n", args[i])
				return 3
			}
			fields["output_tokens"] = v
		case strings.HasPrefix(args[i], "--cache="):
			v, err := strconv.Atoi(strings.TrimPrefix(args[i], "--cache="))
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: dispatch tokens: invalid --cache: %s\n", args[i])
				return 3
			}
			fields["cache_hits"] = v
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch tokens: usage: ic dispatch tokens <id> [--in=N] [--out=N] [--cache=N]\n")
		return 3
	}
	id := positional[0]

	if len(fields) == 0 {
		fmt.Fprintf(os.Stderr, "ic: dispatch tokens: at least one of --in, --out, --cache is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch tokens: %v\n", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	if err := dStore.UpdateTokens(ctx, id, fields); err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch tokens: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch tokens: %v\n", err)
		return 2
	}

	// Budget check: if this dispatch belongs to a run, check budget thresholds
	if disp, err := dStore.Get(ctx, id); err == nil && disp.ScopeID != nil {
		pStore := phase.New(d.SqlDB())
		sStore := state.New(d.SqlDB())
		checker := budget.New(pStore, dStore, sStore, nil)
		result, err := checker.Check(ctx, *disp.ScopeID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[budget] check: %v\n", err)
		} else if result != nil {
			if result.Exceeded {
				fmt.Fprintf(os.Stderr, "[budget] EXCEEDED: %d/%d tokens\n", result.Used, result.Budget)
			} else if result.Warning {
				fmt.Fprintf(os.Stderr, "[budget] WARNING: %d/%d tokens (%d%% threshold)\n", result.Used, result.Budget, result.WarnPct)
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
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: warning: no relay data for portfolio %s, dispatch limit not enforced\n", *run.ParentRunID)
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
