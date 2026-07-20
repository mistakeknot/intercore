package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/goal"
)

func cmdGoal(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("goal: missing subcommand",
			"expected", "create, show, list, close, audit, lint-condition, successor")
		return 3
	}
	switch args[0] {
	case "create":
		return cmdGoalCreate(ctx, args[1:])
	case "show":
		return cmdGoalShow(ctx, args[1:])
	case "list":
		return cmdGoalList(ctx, args[1:])
	case "close":
		return cmdGoalClose(ctx, args[1:])
	case "audit":
		return cmdGoalAudit(ctx, args[1:])
	case "lint-condition":
		return cmdGoalLint(args[1:])
	case "successor":
		return cmdGoalSuccessor(ctx, args[1:])
	default:
		slog.Error("goal: unknown subcommand", "got", args[0])
		return 3
	}
}

func goalStore() (*goal.Store, func(), int) {
	d, err := openDB()
	if err != nil {
		slog.Error("goal: open db failed", "error", err)
		return nil, nil, 2
	}
	return goal.New(d.SqlDB()), func() { d.Close() }, 0
}

func cmdGoalCreate(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	title := f.String("title", "")
	project := f.String("project", "")
	condition := f.String("condition", "")
	conditionFile := f.String("condition-file", "")
	charter := f.String("charter", "")
	beadID := f.String("bead", "")
	force := f.Bool("force")
	complexity, err := f.Int("complexity", 3)
	if err != nil || complexity < 1 || complexity > 5 {
		slog.Error("goal create: invalid complexity")
		return 3
	}
	if title == "" || project == "" {
		slog.Error("goal create: --title and --project are required")
		return 3
	}
	if conditionFile != "" {
		b, readErr := os.ReadFile(conditionFile)
		if readErr != nil {
			slog.Error("goal create: read condition file", "error", readErr)
			return 2
		}
		condition = string(b)
	}

	// Tier-independent gate (KD 9): refuse error-level lint findings.
	problems := goal.LintCondition(condition)
	hasError := false
	for _, problem := range problems {
		fmt.Fprintf(os.Stderr, "lint %s: %s\n", problem.Severity, problem.Message)
		if problem.Severity == "error" {
			hasError = true
		}
	}
	if hasError && !force {
		slog.Error("goal create: condition failed lint (use --force to override)")
		return 3
	}

	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	g := &goal.Goal{
		ProjectDir:    project,
		Title:         title,
		ConditionText: condition,
		Complexity:    complexity,
	}
	if charter != "" {
		g.CharterPath = &charter
	}
	if beadID != "" {
		g.BeadID = &beadID
	}
	id, err := store.Create(ctx, g)
	if err != nil {
		slog.Error("goal create failed", "error", err)
		return 2
	}
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"id": id, "status": "open"})
	} else {
		fmt.Println(id)
	}
	return 0
}

func cmdGoalShow(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("goal show: missing id")
		return 3
	}
	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	g, err := store.Get(ctx, args[0])
	if err != nil {
		slog.Error("goal show failed", "error", err)
		return 2
	}
	json.NewEncoder(os.Stdout).Encode(g)
	return 0
}

func cmdGoalList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	goals, err := store.List(ctx, f.String("project", ""), f.String("status", ""))
	if err != nil {
		slog.Error("goal list failed", "error", err)
		return 2
	}
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(goals)
		return 0
	}
	for _, g := range goals {
		fmt.Printf("%s\t%s\t%s\n", g.ID, g.Status, g.Title)
	}
	return 0
}

// close subcommands: begin | step | finish | release
func cmdGoalClose(ctx context.Context, args []string) int {
	if len(args) < 2 {
		slog.Error("goal close: usage: close <begin|step|finish|release> <goal-id> [flags]")
		return 3
	}
	verb, id := args[0], args[1]
	f := cli.ParseFlags(args[2:])
	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	switch verb {
	case "begin":
		ttl, err := f.Int("ttl", 1800)
		if err != nil {
			slog.Error("goal close begin: invalid --ttl")
			return 3
		}
		fence, err := store.AcquireClose(ctx, id, f.String("run", ""), f.String("owner", ""), int64(ttl))
		if err != nil {
			slog.Error("goal close begin failed", "error", err)
			return 2
		}
		json.NewEncoder(os.Stdout).Encode(map[string]any{"fence": fence})
		return 0
	case "step":
		fence, err := f.Int("fence", 0)
		if err != nil || fence == 0 {
			slog.Error("goal close step: --fence required")
			return 3
		}
		if err := store.StampStep(ctx, id, f.String("name", ""), int64(fence)); err != nil {
			slog.Error("goal close step failed", "error", err)
			return 2
		}
		return 0
	case "finish":
		fence, err := f.Int("fence", 0)
		if err != nil || fence == 0 {
			slog.Error("goal close finish: --fence required")
			return 3
		}
		if err := store.FinishClose(ctx, id, int64(fence)); err != nil {
			slog.Error("goal close finish failed", "error", err)
			return 2
		}
		return 0
	case "release":
		fence, err := f.Int("fence", 0)
		if err != nil || fence == 0 {
			slog.Error("goal close release: --fence required")
			return 3
		}
		if err := store.ReleaseLease(ctx, id, int64(fence)); err != nil {
			slog.Error("goal close release failed", "error", err)
			return 2
		}
		return 0
	default:
		slog.Error("goal close: unknown verb", "got", verb)
		return 3
	}
}

func cmdGoalAudit(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	dormantAfter, err := f.Int("dormant-after", 604800) // 7 days
	if err != nil {
		slog.Error("goal audit: invalid --dormant-after")
		return 3
	}
	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	defects, err := store.Audit(ctx, f.String("project", ""), int64(dormantAfter))
	if err != nil {
		slog.Error("goal audit failed", "error", err)
		return 2
	}
	json.NewEncoder(os.Stdout).Encode(defects)
	if len(defects) > 0 {
		return 1 // check-style: findings present
	}
	return 0
}

func cmdGoalLint(args []string) int {
	f := cli.ParseFlags(args)
	text := f.String("text", "")
	file := f.String("file", "")
	if file != "" {
		b, err := os.ReadFile(file)
		if err != nil {
			slog.Error("goal lint-condition: read file", "error", err)
			return 2
		}
		text = string(b)
	}
	problems := goal.LintCondition(text)
	json.NewEncoder(os.Stdout).Encode(problems)
	for _, problem := range problems {
		if problem.Severity == "error" {
			return 1
		}
	}
	return 0
}

func cmdGoalSuccessor(ctx context.Context, args []string) int {
	if len(args) < 1 {
		slog.Error("goal successor: usage: successor <goal-id> --ref=<bead-or-goal-or-text>")
		return 3
	}
	f := cli.ParseFlags(args[1:])
	ref := f.String("ref", "")
	if ref == "" {
		slog.Error("goal successor: --ref required")
		return 3
	}
	store, closeDB, rc := goalStore()
	if rc != 0 {
		return rc
	}
	defer closeDB()

	if err := store.SetSuccessor(ctx, args[0], ref); err != nil {
		slog.Error("goal successor failed", "error", err)
		return 2
	}
	return 0
}
