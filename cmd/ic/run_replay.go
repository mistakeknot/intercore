package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/replay"
)

type replayGate struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

type replayOutput struct {
	RunID     string            `json:"run_id"`
	Mode      string            `json:"mode"`
	RunStatus string            `json:"run_status"`
	Decisions []replay.Decision `json:"decisions"`
	Inputs    []*replay.Input   `json:"inputs"`
	Reexecute replayGate        `json:"reexecute"`
}

func cmdRunReplay(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: run replay: usage: ic run replay <run_id> [--mode=simulate|reexecute] [--allow-live] [--limit=N]\n")
		fmt.Fprintf(os.Stderr, "                ic run replay inputs <run_id> [--limit=N]\n")
		fmt.Fprintf(os.Stderr, "                ic run replay record <run_id> --kind=<kind> [--key=<k>] [--payload=<json>] [--artifact-ref=<ref>] [--event-source=<src>] [--event-id=<id>]\n")
		return 3
	}

	switch args[0] {
	case "inputs":
		return cmdRunReplayInputs(ctx, args[1:])
	case "record":
		return cmdRunReplayRecord(ctx, args[1:])
	}

	runID := args[0]
	rf := cli.ParseFlags(args[1:])
	mode := rf.String("mode", "simulate")
	allowLive := rf.Bool("allow-live")

	limit, err := rf.Int("limit", 2000)
	if err != nil || (rf.Has("limit") && limit <= 0) {
		slog.Error("run replay: invalid --limit", "value", rf.String("limit", ""))
		return 3
	}

	if mode != "simulate" && mode != "reexecute" {
		slog.Error("run replay: invalid --mode", "value", mode)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	run, err := pStore.Get(ctx, runID)
	if err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run replay: run not found", "id", runID)
			return 1
		}
		slog.Error("run replay failed", "error", err)
		return 2
	}

	if run.Status != phase.StatusCompleted {
		slog.Error("run replay: run must be completed for deterministic replay", "status", run.Status)
		return 1
	}

	evStore := event.NewStore(d.SqlDB())
	events, err := evStore.ListEvents(ctx, runID, 0, 0, 0, 0, limit)
	if err != nil {
		slog.Error("run replay: list events failed", "error", err)
		return 2
	}

	replayStore := replay.New(d.SqlDB())
	inputs, err := replayStore.ListInputs(ctx, runID, limit*2)
	if err != nil {
		slog.Error("run replay: list inputs failed", "error", err)
		return 2
	}

	out := replayOutput{
		RunID:     runID,
		Mode:      mode,
		RunStatus: run.Status,
		Inputs:    inputs,
		Reexecute: replayGate{
			Allowed: false,
		},
	}
	out.Decisions = replay.BuildTimeline(events, inputs)

	if mode == "simulate" {
		out.Reexecute.Reason = "simulate mode has no side effects"
	} else {
		if !allowLive {
			out.Reexecute.Reason = "live reexecute is gated: pass --allow-live to request it"
		} else {
			out.Reexecute.Reason = "live reexecute is currently disallowed by kernel policy"
		}
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			slog.Error("run replay: write failed", "error", err)
			return 2
		}
	} else {
		fmt.Printf("Run: %s (%s)\n", out.RunID, out.RunStatus)
		fmt.Printf("Mode: %s\n", out.Mode)
		fmt.Printf("Decisions: %d\n", len(out.Decisions))
		fmt.Printf("Recorded inputs: %d\n", len(out.Inputs))
		for _, d := range out.Decisions {
			fmt.Printf("  [%s#%d] %s %s -> %s\n", d.Source, d.EventID, d.Type, d.FromState, d.ToState)
		}
		fmt.Printf("Reexecute: allowed=%t (%s)\n", out.Reexecute.Allowed, out.Reexecute.Reason)
	}

	if mode == "reexecute" {
		return 1
	}
	return 0
}

func cmdRunReplayInputs(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run replay inputs: usage: ic run replay inputs <run_id> [--limit=N]\n")
		return 3
	}
	runID := f.Positionals[0]

	limit, err := f.Int("limit", 1000)
	if err != nil || (f.Has("limit") && limit <= 0) {
		slog.Error("run replay inputs: invalid --limit", "value", f.String("limit", ""))
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay inputs failed", "error", err)
		return 2
	}
	defer d.Close()

	replayStore := replay.New(d.SqlDB())
	inputs, err := replayStore.ListInputs(ctx, runID, limit)
	if err != nil {
		slog.Error("run replay inputs failed", "error", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(inputs); err != nil {
			slog.Error("run replay inputs: write failed", "error", err)
			return 2
		}
	} else {
		for _, in := range inputs {
			fmt.Printf("%d\t%s\t%s\t%s\n", in.ID, in.Kind, in.Key, in.Payload)
		}
	}
	return 0
}

func cmdRunReplayRecord(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run replay record: usage: ic run replay record <run_id> --kind=<kind> [--key=<k>] [--payload=<json>] [--artifact-ref=<ref>] [--event-source=<src>] [--event-id=<id>]\n")
		return 3
	}
	runID := f.Positionals[0]
	kind := f.String("kind", "")
	key := f.String("key", "")
	payload := f.String("payload", "")
	artifactRef := f.String("artifact-ref", "")
	eventSource := f.String("event-source", "")

	var eventID *int64
	if f.Has("event-id") {
		v, err := f.Int64("event-id", 0)
		if err != nil || v <= 0 {
			slog.Error("run replay record: invalid --event-id", "value", f.String("event-id", ""))
			return 3
		}
		eventID = &v
	}
	if kind == "" {
		slog.Error("run replay record: --kind is required")
		return 3
	}
	if payload == "" {
		payload = "{}"
	}

	d, err := openDB()
	if err != nil {
		slog.Error("run replay record failed", "error", err)
		return 2
	}
	defer d.Close()

	pStore := phase.New(d.SqlDB())
	if _, err := pStore.Get(ctx, runID); err != nil {
		if err == phase.ErrNotFound {
			slog.Error("run replay record: run not found", "id", runID)
			return 1
		}
		slog.Error("run replay record failed", "error", err)
		return 2
	}

	replayStore := replay.New(d.SqlDB())
	in := &replay.Input{
		RunID:       runID,
		Kind:        kind,
		Key:         key,
		Payload:     payload,
		EventSource: eventSource,
		EventID:     eventID,
	}
	if artifactRef != "" {
		in.ArtifactRef = &artifactRef
	}
	id, err := replayStore.AddInput(ctx, in)
	if err != nil {
		slog.Error("run replay record failed", "error", err)
		return 2
	}
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{"id": id})
	} else {
		fmt.Printf("%d\n", id)
	}
	return 0
}
