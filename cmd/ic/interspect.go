package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/event"
)

func cmdInterspect(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("interspect: missing subcommand", "expected", "record, query")
		return 3
	}

	switch args[0] {
	case "record":
		return cmdInterspectRecord(ctx, args[1:])
	case "query":
		return cmdInterspectQuery(ctx, args[1:])
	default:
		slog.Error("interspect: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdInterspectRecord(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	runID := f.String("run", "")
	agent := f.String("agent", "")
	eventType := f.String("type", "")
	reason := f.String("reason", "")
	contextJSON := f.String("context", "")
	session := f.String("session", "")
	project := f.String("project", "")

	if agent == "" {
		slog.Error("interspect record: --agent is required")
		return 3
	}
	if eventType == "" {
		slog.Error("interspect record: --type is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("interspect record failed", "error", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	id, err := evStore.AddInterspectEvent(ctx, runID, agent, eventType, reason, contextJSON, session, project)
	if err != nil {
		slog.Error("interspect record failed", "error", err)
		return 2
	}

	fmt.Printf("%d\n", id)
	return 0
}

func cmdInterspectQuery(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	agent := f.String("agent", "")

	since, err := f.Int64("since", 0)
	if err != nil {
		slog.Error("interspect query: invalid --since", "value", f.String("since", ""))
		return 3
	}
	limit, err2 := f.Int("limit", 100)
	if err2 != nil {
		slog.Error("interspect query: invalid --limit", "value", f.String("limit", ""))
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("interspect query failed", "error", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	events, err := evStore.ListInterspectEvents(ctx, agent, since, limit)
	if err != nil {
		slog.Error("interspect query failed", "error", err)
		return 2
	}

	enc := json.NewEncoder(os.Stdout)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			slog.Error("interspect query: write failed", "error", err)
			return 2
		}
	}
	return 0
}
