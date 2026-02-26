package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

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
	var runID, agent, eventType, reason, contextJSON, session, project string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--run="):
			runID = strings.TrimPrefix(args[i], "--run=")
		case strings.HasPrefix(args[i], "--agent="):
			agent = strings.TrimPrefix(args[i], "--agent=")
		case strings.HasPrefix(args[i], "--type="):
			eventType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		case strings.HasPrefix(args[i], "--context="):
			contextJSON = strings.TrimPrefix(args[i], "--context=")
		case strings.HasPrefix(args[i], "--session="):
			session = strings.TrimPrefix(args[i], "--session=")
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		default:
			slog.Error("interspect record: unknown flag", "value", args[i])
			return 3
		}
	}

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
	var agent string
	var since int64
	limit := 100

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--agent="):
			agent = strings.TrimPrefix(args[i], "--agent=")
		case strings.HasPrefix(args[i], "--since="):
			val := strings.TrimPrefix(args[i], "--since=")
			if _, err := fmt.Sscanf(val, "%d", &since); err != nil {
				slog.Error("interspect query: invalid --since", "value", val)
				return 3
			}
		case strings.HasPrefix(args[i], "--limit="):
			val := strings.TrimPrefix(args[i], "--limit=")
			if _, err := fmt.Sscanf(val, "%d", &limit); err != nil {
				slog.Error("interspect query: invalid --limit", "value", val)
				return 3
			}
		default:
			slog.Error("interspect query: unknown flag", "value", args[i])
			return 3
		}
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
