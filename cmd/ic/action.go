package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/action"
	"github.com/mistakeknot/intercore/internal/cli"
)

func cmdRunAction(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("run action: missing subcommand", "expected", "add, list, update, delete")
		return 3
	}

	switch args[0] {
	case "add":
		return cmdRunActionAdd(ctx, args[1:])
	case "list":
		return cmdRunActionList(ctx, args[1:])
	case "update":
		return cmdRunActionUpdate(ctx, args[1:])
	case "delete":
		return cmdRunActionDelete(ctx, args[1:])
	default:
		slog.Error("run action: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdRunActionAdd(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	phase := f.String("phase", "")
	command := f.String("command", "")
	argsJSON := f.String("args", "")
	mode := f.String("mode", "")
	actionType := f.String("type", "")

	priority, err := f.Int("priority", 0)
	if err != nil {
		slog.Error("run action add: invalid --priority", "value", f.String("priority", ""))
		return 3
	}

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run action add: usage: ic run action add <run_id> --phase=<p> --command=<c>\n")
		return 3
	}
	runID := f.Positionals[0]

	if phase == "" || command == "" {
		slog.Error("run action add: --phase and --command are required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run action add failed", "error", err)
		return 2
	}
	defer d.Close()

	s := action.New(d.SqlDB())
	a := &action.Action{
		RunID:      runID,
		Phase:      phase,
		ActionType: actionType,
		Command:    command,
		Mode:       mode,
		Priority:   priority,
	}
	if argsJSON != "" {
		a.Args = &argsJSON
	}

	id, err := s.Add(ctx, a)
	if err != nil {
		slog.Error("run action add failed", "error", err)
		if errors.Is(err, action.ErrDuplicate) {
			return 1
		}
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"id": id})
	} else {
		fmt.Printf("Added action %d: %s → %s\n", id, phase, command)
	}
	return 0
}

func cmdRunActionList(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	phase := f.String("phase", "")

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run action list: usage: ic run action list <run_id> [--phase=<p>]\n")
		return 3
	}
	runID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run action list failed", "error", err)
		return 2
	}
	defer d.Close()

	s := action.New(d.SqlDB())

	var actions []*action.Action
	if phase != "" {
		actions, err = s.ListForPhase(ctx, runID, phase)
	} else {
		actions, err = s.ListAll(ctx, runID)
	}
	if err != nil {
		slog.Error("run action list failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(actions))
		for i, a := range actions {
			items[i] = actionToMap(a)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		if len(actions) == 0 {
			fmt.Println("No actions registered.")
			return 0
		}
		for _, a := range actions {
			argsStr := ""
			if a.Args != nil {
				argsStr = " " + *a.Args
			}
			fmt.Printf("  %s → %s%s  [%s, priority=%d]\n", a.Phase, a.Command, argsStr, a.Mode, a.Priority)
		}
	}
	return 0
}

func cmdRunActionUpdate(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	phase := f.String("phase", "")
	command := f.String("command", "")
	argsJSON := f.String("args", "")
	mode := f.String("mode", "")

	priority := -1
	if f.Has("priority") {
		v, err := f.Int("priority", -1)
		if err != nil {
			slog.Error("run action update: invalid --priority", "value", f.String("priority", ""))
			return 3
		}
		priority = v
	}

	if len(f.Positionals) < 1 || phase == "" || command == "" {
		fmt.Fprintf(os.Stderr, "ic: run action update: usage: ic run action update <run_id> --phase=<p> --command=<c> [--args=...] [--mode=...]\n")
		return 3
	}
	runID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run action update failed", "error", err)
		return 2
	}
	defer d.Close()

	s := action.New(d.SqlDB())
	upd := &action.ActionUpdate{}
	if argsJSON != "" {
		upd.Args = &argsJSON
	}
	if mode != "" {
		upd.Mode = &mode
	}
	if priority >= 0 {
		upd.Priority = &priority
	}

	if err := s.Update(ctx, runID, phase, command, upd); err != nil {
		slog.Error("run action update failed", "error", err)
		if errors.Is(err, action.ErrNotFound) {
			return 1
		}
		return 2
	}

	fmt.Printf("Updated: %s → %s\n", phase, command)
	return 0
}

func cmdRunActionDelete(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	phase := f.String("phase", "")
	command := f.String("command", "")

	if len(f.Positionals) < 1 || phase == "" || command == "" {
		fmt.Fprintf(os.Stderr, "ic: run action delete: usage: ic run action delete <run_id> --phase=<p> --command=<c>\n")
		return 3
	}
	runID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("run action delete failed", "error", err)
		return 2
	}
	defer d.Close()

	s := action.New(d.SqlDB())
	if err := s.Delete(ctx, runID, phase, command); err != nil {
		slog.Error("run action delete failed", "error", err)
		if errors.Is(err, action.ErrNotFound) {
			return 1
		}
		return 2
	}

	fmt.Printf("Deleted: %s → %s\n", phase, command)
	return 0
}

func actionToMap(a *action.Action) map[string]interface{} {
	m := map[string]interface{}{
		"id":          a.ID,
		"run_id":      a.RunID,
		"phase":       a.Phase,
		"action_type": a.ActionType,
		"command":     a.Command,
		"mode":        a.Mode,
		"priority":    a.Priority,
	}
	if a.Args != nil {
		var parsed interface{}
		if err := json.Unmarshal([]byte(*a.Args), &parsed); err == nil {
			m["args"] = parsed
		} else {
			m["args"] = *a.Args
		}
	}
	return m
}
