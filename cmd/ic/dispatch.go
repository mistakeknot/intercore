package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/routing"
	"github.com/mistakeknot/intercore/internal/scheduler"
	"github.com/mistakeknot/intercore/internal/state"
)

// --- Dispatch Commands ---

func dispatchMutationError(operation string, err error) int {
	var rejection *dispatch.SpawnRejection
	if errors.Is(err, dispatch.ErrStaleStatus) {
		rejection = &dispatch.SpawnRejection{Reason: "stale_status"}
	} else if errors.Is(err, dispatch.ErrNotFound) {
		rejection = &dispatch.SpawnRejection{Reason: "not_found"}
	} else {
		_ = errors.As(err, &rejection)
	}
	if rejection != nil {
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(rejection)
		} else {
			slog.Error("dispatch "+operation+": rejected", "error", rejection)
		}
		return 1
	}
	slog.Error("dispatch "+operation+" failed", "error", err)
	return 2
}

func cmdDispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("dispatch: missing subcommand", "expected", "spawn, status, list, poll, wait, kill, prune, tokens, retry")
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
	case "reconcile":
		if len(args) != 2 {
			return 3
		}
		d, err := openDB()
		if err != nil {
			slog.Error("dispatch reconcile", "error", err)
			return 2
		}
		defer d.Close()
		receipt, err := dispatch.New(d.SqlDB(), nil).ReconcileWorker(ctx, args[1])
		if err != nil {
			slog.Error("dispatch reconcile", "error", err)
			return 1
		}
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(receipt)
		} else {
			fmt.Println("Reconciliation appended; terminal attempt retained")
		}
		return 0
	default:
		slog.Error("dispatch: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdDispatchSpawn(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	opts := dispatch.SpawnOptions{
		AgentType:        f.String("type", ""),
		PromptFile:       f.String("prompt-file", ""),
		ProjectDir:       f.String("project", ""),
		OutputFile:       f.String("output", ""),
		Name:             f.String("name", ""),
		Model:            f.String("model", ""),
		Sandbox:          f.String("sandbox", ""),
		SandboxSpec:      f.String("sandbox-spec", ""),
		ScopeID:          f.String("scope-id", ""),
		RunID:            f.String("run-id", ""),
		ParentID:         f.String("parent-id", ""),
		ParentDispatchID: f.String("parent-dispatch-id", ""),
		DispatchSH:       f.String("dispatch-sh", ""),
	}
	if role := f.String("role", ""); role != "" {
		if opts.Model != "" || opts.AgentType != "" {
			slog.Error("role cannot be combined with model or type")
			return 3
		}
		cfg, err := loadSelectedRoutingConfig(f.String("policy", ""))
		if err != nil {
			slog.Error("dispatch policy", "error", err)
			return 2
		}
		c, err := readDecisionContext(f.String("context-file", ""))
		if err != nil {
			slog.Error("dispatch context", "error", err)
			return 3
		}
		decision, err := routing.NewResolver(cfg).ResolveDecision(role, f.String("producer-identity", ""), f.String("policy-profile", ""), c)
		if err != nil {
			slog.Error("dispatch reasoning", "error", err)
			return 1
		}
		opts.Decision = &decision
		opts.Model = decision.Profile.Model
		opts.AgentType = decision.Profile.Backend
		if opts.DispatchSH == "" {
			opts.DispatchSH = filepath.Join(filepath.Dir(cfg.PolicySource), "..", "scripts", "dispatch.sh")
		}
	}
	scheduled := f.Bool("scheduled")
	schedulerSession := f.String("scheduler-session", "")
	for _, name := range []string{"run-id", "scope-id", "parent-dispatch-id"} {
		if f.Has(name) && f.String(name, "") == "" {
			slog.Error("dispatch spawn: binding requires a nonempty value", "flag", name)
			return 3
		}
	}
	policy := dispatch.SpawnPolicy{}
	for name, target := range map[string]*int{
		"max-active-per-run": &policy.MaxActivePerRun,
		"max-active-global":  &policy.MaxActiveGlobal,
		"max-agents-per-run": &policy.MaxAgentsPerRun,
		"max-spawn-depth":    &policy.MaxSpawnDepth,
	} {
		if !f.Has(name) {
			continue
		}
		raw, hasValue := f.Raw(name)
		value, err := strconv.Atoi(raw)
		if !hasValue || err != nil || value < 0 {
			slog.Error("dispatch spawn: limit requires a nonnegative integer", "flag", name)
			return 3
		}
		*target = value
	}
	if f.Has("budget-enforce") {
		policy.BudgetEnforce = true
		if raw, hasValue := f.Raw("budget-enforce"); hasValue {
			value, err := strconv.ParseBool(raw)
			if err != nil {
				slog.Error("dispatch spawn: invalid budget-enforce boolean", "value", raw)
				return 3
			}
			policy.BudgetEnforce = value
		}
	}
	opts.Policy = &policy

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
	if err := opts.Validate(); err != nil {
		slog.Error("dispatch spawn: invalid options", "error", err)
		return 3
	}
	if opts.RunID != "" {
		opts.ScopeID = opts.RunID
	}

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch spawn failed", "error", err)
		return 2
	}
	defer d.Close()

	// --scheduled: submit to scheduler instead of direct exec.
	if scheduled {
		if opts.AgentType == "" {
			opts.AgentType = "codex"
		}
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
		if rejection := checkPortfolioDispatchLimit(ctx, d.SqlDB(), opts.ScopeID); rejection != nil {
			if flagJSON {
				json.NewEncoder(os.Stdout).Encode(rejection)
			} else {
				slog.Error("dispatch spawn: rejected", "error", rejection)
			}
			return 1
		}
	}

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
	result, err := dispatch.Spawn(ctx, store, opts)
	if err != nil {
		var rejection *dispatch.SpawnRejection
		if errors.As(err, &rejection) {
			if flagJSON {
				json.NewEncoder(os.Stdout).Encode(rejection)
			} else {
				slog.Error("dispatch spawn: rejected", "error", rejection)
			}
			return 1
		}
		var input *dispatch.InputError
		if errors.As(err, &input) {
			slog.Error("dispatch spawn: invalid options", "error", input)
			return 3
		}
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
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
		json.NewEncoder(os.Stdout).Encode(dispatch.ToOutput(disp))
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
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
		items := make([]dispatch.DispatchOutput, len(dispatches))
		for i, disp := range dispatches {
			items[i] = dispatch.ToOutput(disp)
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
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
		json.NewEncoder(os.Stdout).Encode(dispatch.ToOutput(disp))
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
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
		json.NewEncoder(os.Stdout).Encode(dispatch.ToOutput(disp))
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
	if err := dispatch.Kill(ctx, store, args[0]); err != nil {
		return dispatchMutationError("kill", err)
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

	store := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
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

	dStore := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))
	if err := dStore.UpdateTokens(ctx, id, fields); err != nil {
		return dispatchMutationError("tokens", err)
	}

	// Budget check: if this dispatch belongs to a run, check budget thresholds
	if disp, err := dStore.Get(ctx, id); err == nil && disp.ScopeID != nil {
		pStore := phase.New(d.SqlDB())
		sStore := state.New(d.SqlDB())
		checker := budget.New(pStore, dStore, sStore, newBudgetRecorder(d.SqlDB()))
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

// cmdDispatchRetry: ic dispatch retry <dispatch-id> [--escalate] [--chain-key=K]
//
//	[--failure-mode=timeout|error|verdict-fail|criteria-fail] [--failure-detail=TXT] [--json]
//
// Creates the retry record (does not start the process — same contract as the
// library). With --escalate, applies the two-strikes ladder; records an
// escalation provenance row in routing_decisions (rule=escalation,
// floor_from=old model, floor_to=new model).
func cmdDispatchRetry(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch retry: usage: ic dispatch retry <dispatch-id> [--escalate] [--chain-key=K] [--failure-mode=timeout|error|verdict-fail|criteria-fail] [--failure-detail=TXT]\n")
		return 3
	}
	id := f.Positionals[0]
	escalate := f.Bool("escalate")
	chainKey := f.String("chain-key", "")
	failureModeStr := f.String("failure-mode", "")
	failureDetail := f.String("failure-detail", "")

	d, err := openDB()
	if err != nil {
		slog.Error("dispatch retry failed", "error", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), newDispatchRecorder(d.SqlDB()))

	if !escalate {
		result, err := dispatch.Retry(ctx, dStore, id, dispatch.DefaultRetryPolicy())
		if err != nil {
			return dispatchMutationError("retry", err)
		}
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
				"new_id":     result.NewID,
				"attempt":    result.Attempt,
				"backoff_ms": result.BackoffMs,
			})
		} else {
			fmt.Printf("%s\tattempt=%d\tbackoff_ms=%d\n", result.NewID, result.Attempt, result.BackoffMs)
		}
		return 0
	}

	// --escalate path
	orig, err := dStore.Get(ctx, id)
	if err != nil {
		return dispatchMutationError("retry", err)
	}
	origModel := ""
	if orig.Model != nil {
		origModel = *orig.Model
	}
	nameOrType := orig.AgentType
	if orig.Name != nil && *orig.Name != "" {
		nameOrType = *orig.Name
	}
	projectDir := orig.ProjectDir

	stateStore := state.New(d.SqlDB())
	mode := dispatch.ParseFailureMode(failureModeStr)
	cfg, err := loadSelectedRoutingConfig(f.String("policy", ""))
	if err != nil {
		slog.Error("escalation policy", "error", err)
		return 2
	}
	ep := dispatch.EscalationPolicyFromConfig(cfg)
	ep.PolicyProfile = f.String("policy-profile", "")
	ep.Context, err = readDecisionContext(f.String("context-file", ""))
	if err != nil {
		slog.Error("escalation context", "error", err)
		return 3
	}
	result, err := dispatch.RetryWithEscalation(ctx, dStore, stateStore, id, ep, chainKey, mode, failureDetail)
	if err != nil {
		if result != nil && result.Exhausted {
			if flagJSON {
				json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
					"exhausted":   true,
					"lesson_file": result.LessonFile,
					"error":       err.Error(),
				})
			} else {
				fmt.Fprintf(os.Stderr, "escalation chain exhausted: %s\nlesson chain: %s\n", err.Error(), result.LessonFile)
			}
			return 1
		}
		return dispatchMutationError("retry --escalate", err)
	}

	if result.Escalated {
		dstore := routing.NewDecisionStore(d.SqlDB())
		_, _ = dstore.Record(ctx, routing.RecordDecisionOpts{
			DispatchID:    result.NewID,
			Agent:         nameOrType,
			SelectedModel: result.Model,
			RuleMatched:   "escalation",
			FloorApplied:  true,
			FloorFrom:     origModel,
			FloorTo:       result.Model,
			ContextJSON:   fmt.Sprintf(`{"chain_key":%q,"failure_mode":%q,"attempt":%d}`, chainKey, mode, result.Attempt),
			ProjectDir:    projectDir,
		})
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"decision":   result.Decision,
			"new_id":     result.NewID,
			"model":      result.Model,
			"escalated":  result.Escalated,
			"attempt":    result.Attempt,
			"backoff_ms": result.BackoffMs,
		})
	} else {
		fmt.Printf("%s\tmodel=%s\tescalated=%v\tbackoff_ms=%d\n", result.NewID, result.Model, result.Escalated, result.BackoffMs)
	}
	return 0
}

// --- dispatch output helpers ---

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
func checkPortfolioDispatchLimit(ctx context.Context, db *sql.DB, scopeID string) *dispatch.SpawnRejection {
	phaseStore := phase.New(db)
	stateStore := state.New(db)

	run, err := phaseStore.Get(ctx, scopeID)
	if err != nil || run.ParentRunID == nil {
		return nil
	}

	parent, err := phaseStore.Get(ctx, *run.ParentRunID)
	if err != nil || parent.MaxDispatches <= 0 {
		return nil
	}

	payload, err := stateStore.Get(ctx, "active-dispatch-count", *run.ParentRunID)
	if err != nil {
		slog.Warn("dispatch spawn: no relay data for portfolio, dispatch limit not enforced", "portfolio_id", *run.ParentRunID)
		return nil
	}

	var countStr string
	if err := json.Unmarshal(payload, &countStr); err != nil {
		return nil
	}
	count, err := strconv.Atoi(countStr)
	if err != nil {
		return nil
	}

	if count >= parent.MaxDispatches {
		return &dispatch.SpawnRejection{Reason: "portfolio_dispatch_limit", RunID: scopeID, Current: int64(count), Limit: int64(parent.MaxDispatches)}
	}
	return nil
}
