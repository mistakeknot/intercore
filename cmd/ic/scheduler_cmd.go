package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

func cmdScheduler(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("scheduler: missing subcommand", "expected", "status, submit, stats, pause, resume, list, prune")
		return 3
	}

	switch args[0] {
	case "submit":
		return cmdSchedulerSubmit(ctx, args[1:])
	case "status":
		return cmdSchedulerStatus(ctx, args[1:])
	case "stats":
		return cmdSchedulerStats(ctx)
	case "list":
		return cmdSchedulerList(ctx, args[1:])
	case "pause":
		return cmdSchedulerPause(ctx)
	case "resume":
		return cmdSchedulerResume(ctx)
	case "cancel":
		return cmdSchedulerCancel(ctx, args[1:])
	case "prune":
		return cmdSchedulerPrune(ctx, args[1:])
	default:
		slog.Error("scheduler: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdSchedulerSubmit(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	promptFile := f.String("prompt-file", "")
	projectDir := f.String("project", "")
	agentType := f.String("type", "codex")
	sessionName := f.String("session", "")
	name := f.String("name", "")

	priority, err := f.Int("priority", int(scheduler.PriorityNormal))
	if err != nil {
		slog.Error("scheduler submit: invalid priority", "value", f.String("priority", ""))
		return 3
	}

	if promptFile == "" {
		slog.Error("scheduler submit: --prompt-file is required")
		return 3
	}
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("scheduler submit: cannot determine project dir", "error", err)
			return 2
		}
		projectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("scheduler submit failed", "error", err)
		return 2
	}
	defer d.Close()

	// Build spawn opts JSON for persistence.
	spawnOpts, err := scheduler.MarshalSpawnOpts(map[string]string{
		"prompt_file": promptFile,
		"project_dir": projectDir,
		"agent_type":  agentType,
		"name":        name,
	})
	if err != nil {
		slog.Error("scheduler submit failed", "error", err)
		return 2
	}

	job := scheduler.NewSpawnJob("", scheduler.JobTypeDispatch, sessionName)
	job.AgentType = agentType
	job.ProjectDir = projectDir
	job.Priority = scheduler.JobPriority(priority)
	job.SpawnOpts = spawnOpts

	store := scheduler.NewStore(d.SqlDB())
	if err := store.Create(ctx, job); err != nil {
		slog.Error("scheduler submit failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]string{
			"id":     job.ID,
			"status": string(job.Status),
		})
	} else {
		fmt.Println(job.ID)
	}
	return 0
}

func cmdSchedulerStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: scheduler status: usage: ic scheduler status <job-id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("scheduler status failed", "error", err)
		return 2
	}
	defer d.Close()

	store := scheduler.NewStore(d.SqlDB())
	job, err := store.Get(ctx, args[0])
	if err != nil {
		slog.Error("scheduler status failed", "error", err)
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(jobToMap(job))
	} else {
		printSchedulerJob(job)
	}
	return 0
}

func cmdSchedulerStats(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("scheduler stats failed", "error", err)
		return 2
	}
	defer d.Close()

	store := scheduler.NewStore(d.SqlDB())

	// Count by status from the DB.
	statusCounts := make(map[string]int)
	for _, status := range []string{"pending", "running", "completed", "failed", "cancelled"} {
		jobs, err := store.List(ctx, status, 10000)
		if err != nil {
			slog.Error("scheduler stats failed", "error", err)
			return 2
		}
		statusCounts[status] = len(jobs)
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(statusCounts)
	} else {
		fmt.Println("Scheduler Jobs:")
		for _, s := range []string{"pending", "running", "completed", "failed", "cancelled"} {
			fmt.Printf("  %-12s %d\n", s+":", statusCounts[s])
		}
	}
	return 0
}

func cmdSchedulerList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	statusFilter := f.String("status", "")
	limit, err := f.Int("limit", 50)
	if err != nil {
		slog.Error("scheduler list: invalid limit", "value", f.String("limit", ""))
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("scheduler list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := scheduler.NewStore(d.SqlDB())
	jobs, err := store.List(ctx, statusFilter, limit)
	if err != nil {
		slog.Error("scheduler list failed", "error", err)
		return 2
	}

	if flagJSON {
		var items []map[string]interface{}
		for _, j := range jobs {
			items = append(items, jobToMap(j))
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		if len(jobs) == 0 {
			fmt.Println("No scheduler jobs found.")
			return 0
		}
		for _, j := range jobs {
			fmt.Printf("%-36s  %-10s  %-8s  %s\n",
				j.ID,
				j.Status,
				j.AgentType,
				j.CreatedAt.Format("15:04:05"),
			)
		}
	}
	return 0
}

func cmdSchedulerPause(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("scheduler pause failed", "error", err)
		return 2
	}
	defer d.Close()

	// Set a state key indicating paused status.
	sqlDB := d.SqlDB()
	_, err = sqlDB.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at) VALUES ('scheduler_paused', 'global', 'true', unixepoch())`)
	if err != nil {
		slog.Error("scheduler pause failed", "error", err)
		return 2
	}

	fmt.Println("Scheduler paused.")
	return 0
}

func cmdSchedulerResume(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("scheduler resume failed", "error", err)
		return 2
	}
	defer d.Close()

	sqlDB := d.SqlDB()
	_, err = sqlDB.ExecContext(ctx,
		`INSERT OR REPLACE INTO state (key, scope_id, payload, updated_at) VALUES ('scheduler_paused', 'global', 'false', unixepoch())`)
	if err != nil {
		slog.Error("scheduler resume failed", "error", err)
		return 2
	}

	fmt.Println("Scheduler resumed.")
	return 0
}

func cmdSchedulerCancel(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: scheduler cancel: usage: ic scheduler cancel <job-id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("scheduler cancel failed", "error", err)
		return 2
	}
	defer d.Close()

	store := scheduler.NewStore(d.SqlDB())
	job, err := store.Get(ctx, args[0])
	if err != nil {
		slog.Error("scheduler cancel failed", "error", err)
		return 1
	}

	if job.Status == scheduler.StatusCompleted || job.Status == scheduler.StatusFailed || job.Status == scheduler.StatusCancelled {
		slog.Error("scheduler cancel: job already in terminal state", "status", job.Status)
		return 1
	}

	job.Status = scheduler.StatusCancelled
	job.CompletedAt = time.Now()
	if err := store.Update(ctx, job); err != nil {
		slog.Error("scheduler cancel failed", "error", err)
		return 2
	}

	fmt.Printf("Cancelled: %s\n", job.ID)
	return 0
}

func cmdSchedulerPrune(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	olderThan := f.String("older-than", "24h")

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		slog.Error("scheduler prune: invalid duration", "value", olderThan)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("scheduler prune failed", "error", err)
		return 2
	}
	defer d.Close()

	store := scheduler.NewStore(d.SqlDB())
	pruned, err := store.Prune(ctx, dur)
	if err != nil {
		slog.Error("scheduler prune failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]int64{"pruned": pruned})
	} else {
		fmt.Printf("Pruned %d completed scheduler jobs.\n", pruned)
	}
	return 0
}

// --- Helpers ---

func jobToMap(j *scheduler.SpawnJob) map[string]interface{} {
	m := map[string]interface{}{
		"id":          j.ID,
		"status":      string(j.Status),
		"priority":    int(j.Priority),
		"agent_type":  j.AgentType,
		"max_retries": j.MaxRetries,
		"retry_count": j.RetryCount,
		"created_at":  j.CreatedAt.Unix(),
	}
	if j.SessionName != "" {
		m["session_name"] = j.SessionName
	}
	if j.BatchID != "" {
		m["batch_id"] = j.BatchID
	}
	if j.DispatchID != "" {
		m["dispatch_id"] = j.DispatchID
	}
	if j.Error != "" {
		m["error"] = j.Error
	}
	if !j.StartedAt.IsZero() {
		m["started_at"] = j.StartedAt.Unix()
	}
	if !j.CompletedAt.IsZero() {
		m["completed_at"] = j.CompletedAt.Unix()
	}
	return m
}

func printSchedulerJob(j *scheduler.SpawnJob) {
	fmt.Printf("ID:         %s\n", j.ID)
	fmt.Printf("Status:     %s\n", j.Status)
	fmt.Printf("Priority:   %d\n", j.Priority)
	fmt.Printf("Agent:      %s\n", j.AgentType)
	if j.SessionName != "" {
		fmt.Printf("Session:    %s\n", j.SessionName)
	}
	if j.DispatchID != "" {
		fmt.Printf("Dispatch:   %s\n", j.DispatchID)
	}
	fmt.Printf("Retries:    %d/%d\n", j.RetryCount, j.MaxRetries)
	fmt.Printf("Created:    %s\n", j.CreatedAt.Format("2006-01-02 15:04:05"))
	if !j.StartedAt.IsZero() {
		fmt.Printf("Started:    %s\n", j.StartedAt.Format("2006-01-02 15:04:05"))
	}
	if !j.CompletedAt.IsZero() {
		fmt.Printf("Completed:  %s\n", j.CompletedAt.Format("2006-01-02 15:04:05"))
	}
	if j.Error != "" {
		fmt.Printf("Error:      %s\n", j.Error)
	}
}
