package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/cli"
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
	f := cli.ParseFlags(args)
	runID := f.String("run", "")
	eventLimit, err := f.Int("events", 20)
	if err != nil {
		slog.Error("situation snapshot: invalid --events", "value", f.String("events", ""))
		return 3
	}
	if runID == "" && len(f.Positionals) > 0 {
		runID = f.Positionals[0]
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
