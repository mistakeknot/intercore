package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/observation"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

func cmdSituation(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("situation: missing subcommand", "expected", "snapshot")
		return 3
	}
	switch args[0] {
	case "snapshot":
		return cmdSituationSnapshot(ctx, args[1:])
	default:
		slog.Error("situation: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdSituationSnapshot(ctx context.Context, args []string) int {
	var runID string
	var eventLimit int = 20

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--run="):
			runID = strings.TrimPrefix(arg, "--run=")
		case arg == "--run" && i+1 < len(args):
			i++
			runID = args[i]
		case strings.HasPrefix(arg, "--events="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--events="))
			if err != nil {
				slog.Error("situation snapshot: invalid --events", "value", arg)
				return 3
			}
			eventLimit = n
		default:
			if runID == "" {
				runID = arg
			}
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("situation snapshot: open db", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	dStore := dispatch.New(d.SqlDB(), nil)
	eStore := event.NewStore(d.SqlDB())
	sStore := scheduler.NewStore(d.SqlDB())

	collector := observation.NewCollector(pStore, dStore, eStore, sStore)

	snap, err := collector.Collect(ctx, observation.CollectOptions{
		RunID:      runID,
		EventLimit: eventLimit,
	})
	if err != nil {
		slog.Error("situation snapshot", "error", err)
		return 2
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(snap); err != nil {
		slog.Error("situation snapshot: encode", "error", err)
		return 2
	}
	return 0
}
