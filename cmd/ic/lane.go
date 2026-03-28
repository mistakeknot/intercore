package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/lane"
)

func cmdLane(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("lane: missing subcommand", "expected", "create, list, status, close, events, sync, members, velocity")
		return 3
	}

	switch args[0] {
	case "create":
		return cmdLaneCreate(ctx, args[1:])
	case "list":
		return cmdLaneList(ctx, args[1:])
	case "status":
		return cmdLaneStatus(ctx, args[1:])
	case "close":
		return cmdLaneClose(ctx, args[1:])
	case "events":
		return cmdLaneEvents(ctx, args[1:])
	case "sync":
		return cmdLaneSync(ctx, args[1:])
	case "members":
		return cmdLaneMembers(ctx, args[1:])
	case "velocity":
		return cmdLaneVelocity(ctx, args[1:])
	case "update":
		return cmdLaneUpdate(ctx, args[1:])
	default:
		slog.Error("lane: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdLaneCreate(ctx context.Context, args []string) int {
	var name, laneType, description, intent string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--name="):
			name = strings.TrimPrefix(arg, "--name=")
		case strings.HasPrefix(arg, "--type="):
			laneType = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "--description="):
			description = strings.TrimPrefix(arg, "--description=")
		case strings.HasPrefix(arg, "--intent="):
			intent = strings.TrimPrefix(arg, "--intent=")
		}
	}

	if name == "" {
		slog.Error("lane create: --name is required")
		return 3
	}
	if laneType == "" {
		laneType = "standing"
	}
	if laneType != "standing" && laneType != "arc" {
		slog.Error("lane create: --type must be 'standing' or 'arc'")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane create failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane create: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	id, err := store.Create(ctx, name, laneType, description, intent)
	if err != nil {
		slog.Error("lane create failed", "error", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
			"id":        id,
			"name":      name,
			"lane_type": laneType,
		}); err != nil {
			slog.Error("lane create: encode failed", "error", err)
			return 2
		}
	} else {
		fmt.Printf("%s\n", id)
	}
	return 0
}

func cmdLaneList(ctx context.Context, args []string) int {
	status := ""
	for _, arg := range args {
		switch {
		case arg == "--active":
			status = "active"
		case strings.HasPrefix(arg, "--status="):
			status = strings.TrimPrefix(arg, "--status=")
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane list failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane list: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	lanes, err := store.List(ctx, status)
	if err != nil {
		slog.Error("lane list failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(lanes))
		for i, l := range lanes {
			items[i] = map[string]interface{}{
				"id":          l.ID,
				"name":        l.Name,
				"lane_type":   l.LaneType,
				"status":      l.Status,
				"description": l.Description,
				"created_at":  l.CreatedAt,
				"updated_at":  l.UpdatedAt,
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(items); err != nil {
			slog.Error("lane list: encode failed", "error", err)
			return 2
		}
	} else {
		if len(lanes) == 0 {
			fmt.Println("no lanes")
			return 0
		}
		fmt.Printf("%-12s %-10s %-8s %s\n", "NAME", "TYPE", "STATUS", "ID")
		for _, l := range lanes {
			fmt.Printf("%-12s %-10s %-8s %s\n", l.Name, l.LaneType, l.Status, l.ID)
		}
	}
	return 0
}

func cmdLaneStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: lane status: usage: ic lane status <id-or-name>\n")
		return 3
	}
	idOrName := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("lane status failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane status: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	// Try by ID first, fall back to name
	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			slog.Error("lane status failed", "error", err)
			return 2
		}
	}

	members, err := store.GetMembers(ctx, l.ID)
	if err != nil {
		slog.Error("lane status: members failed", "error", err)
		return 2
	}

	events, err := store.Events(ctx, l.ID)
	if err != nil {
		slog.Error("lane status: events failed", "error", err)
		return 2
	}

	if flagJSON {
		out := map[string]interface{}{
			"id":           l.ID,
			"name":         l.Name,
			"lane_type":    l.LaneType,
			"status":       l.Status,
			"description":  l.Description,
			"intent":       l.Intent,
			"metadata":     l.Metadata,
			"member_count": len(members),
			"members":      members,
			"event_count":  len(events),
			"created_at":   l.CreatedAt,
			"updated_at":   l.UpdatedAt,
		}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			slog.Error("lane status: encode failed", "error", err)
			return 2
		}
	} else {
		fmt.Printf("Lane: %s (%s)\n", l.Name, l.ID)
		fmt.Printf("Type: %s  Status: %s\n", l.LaneType, l.Status)
		if l.Description != "" {
			fmt.Printf("Description: %s\n", l.Description)
		}
		if l.Intent != "" {
			fmt.Printf("Intent: %s\n", l.Intent)
		}
		fmt.Printf("Members: %d  Events: %d\n", len(members), len(events))
		fmt.Printf("Created: %s\n", time.Unix(l.CreatedAt, 0).Format(time.RFC3339))
		if len(members) > 0 {
			fmt.Printf("Members (%d): %s\n", len(members), strings.Join(members, ", "))
		}
	}
	return 0
}

func cmdLaneClose(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: lane close: usage: ic lane close <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane close failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane close: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	if err := store.Close(ctx, args[0]); err != nil {
		slog.Error("lane close failed", "error", err)
		return 2
	}

	fmt.Println("closed")
	return 0
}

func cmdLaneEvents(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: lane events: usage: ic lane events <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane events failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane events: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	events, err := store.Events(ctx, args[0])
	if err != nil {
		slog.Error("lane events failed", "error", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(events))
		for i, e := range events {
			items[i] = map[string]interface{}{
				"id":         e.ID,
				"lane_id":    e.LaneID,
				"event_type": e.EventType,
				"payload":    e.Payload,
				"created_at": e.CreatedAt,
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(items); err != nil {
			slog.Error("lane events: encode failed", "error", err)
			return 2
		}
	} else {
		for _, e := range events {
			fmt.Printf("%s  %-14s  %s\n",
				time.Unix(e.CreatedAt, 0).Format("2006-01-02 15:04"),
				e.EventType, e.Payload)
		}
	}
	return 0
}

func cmdLaneSync(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: lane sync: usage: ic lane sync <id-or-name> [--bead-ids=id1,id2,...]\n")
		return 3
	}
	idOrName := args[0]

	var beadIDs []string
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--bead-ids=") {
			beadIDs = strings.Split(strings.TrimPrefix(arg, "--bead-ids="), ",")
		}
	}

	if len(beadIDs) == 0 {
		slog.Error("lane sync: --bead-ids is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane sync failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane sync: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	// Resolve name to ID if needed
	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			slog.Error("lane sync failed", "error", err)
			return 2
		}
	}

	if err := store.SnapshotMembers(ctx, l.ID, beadIDs); err != nil {
		slog.Error("lane sync failed", "error", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"lane_id":      l.ID,
			"member_count": len(beadIDs),
		}); err != nil {
			slog.Error("lane sync: encode failed", "error", err)
			return 2
		}
	} else {
		fmt.Printf("synced %d members to lane %s\n", len(beadIDs), l.Name)
	}
	return 0
}

func cmdLaneMembers(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: lane members: usage: ic lane members <id-or-name>\n")
		return 3
	}
	idOrName := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("lane members failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane members: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			slog.Error("lane members failed", "error", err)
			return 2
		}
	}

	members, err := store.GetMembers(ctx, l.ID)
	if err != nil {
		slog.Error("lane members failed", "error", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(members); err != nil {
			slog.Error("lane members: encode failed", "error", err)
			return 2
		}
	} else {
		if len(members) == 0 {
			fmt.Println("no members")
			return 0
		}
		for _, m := range members {
			fmt.Println(m)
		}
	}
	return 0
}

func cmdLaneVelocity(ctx context.Context, args []string) int {
	days := 7
	for _, arg := range args {
		if strings.HasPrefix(arg, "--days=") {
			val := strings.TrimPrefix(arg, "--days=")
			n, err := fmt.Sscanf(val, "%d", &days)
			if err != nil || n != 1 {
				slog.Error("lane velocity: invalid --days value", "value", val)
				return 3
			}
			if days < 1 {
				slog.Error("lane velocity: --days must be >= 1", "days", days)
				return 3
			}
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane velocity failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane velocity: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	v := lane.NewVelocityCalculator(store)
	scores, err := v.ComputeStarvationFromDB(ctx, days)
	if err != nil {
		slog.Error("lane velocity failed", "error", err)
		return 2
	}

	sorted := lane.SortedByStarvation(scores)

	if flagJSON {
		items := make([]map[string]interface{}, len(sorted))
		for i, s := range sorted {
			items[i] = map[string]interface{}{
				"lane_id":    s.LaneID,
				"name":       s.LaneName,
				"open_beads": s.OpenBeads,
				"closed":     s.ClosedLast,
				"throughput": s.Throughput,
				"starvation": s.Starvation,
			}
		}
		if err := json.NewEncoder(os.Stdout).Encode(items); err != nil {
			slog.Error("lane velocity: encode failed", "error", err)
			return 2
		}
	} else {
		if len(sorted) == 0 {
			fmt.Println("no active lanes")
			return 0
		}
		fmt.Printf("%-12s %5s %6s %8s %10s\n", "LANE", "OPEN", "CLOSED", "THRPUT", "STARV")
		for _, s := range sorted {
			fmt.Printf("%-12s %5d %6d %8.1f %10.1f\n",
				s.LaneName, s.OpenBeads, s.ClosedLast, s.Throughput, s.Starvation)
		}
	}
	return 0
}

func cmdLaneUpdate(ctx context.Context, args []string) int {
	var idOrName, intent string
	intentSet := false

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--name="):
			idOrName = strings.TrimPrefix(arg, "--name=")
		case strings.HasPrefix(arg, "--intent="):
			intent = strings.TrimPrefix(arg, "--intent=")
			intentSet = true
		}
	}

	if idOrName == "" {
		slog.Error("lane update: --name is required")
		return 3
	}
	if !intentSet {
		slog.Error("lane update: --intent is required (only supported field)")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("lane update failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("lane update: migrate failed", "error", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	// Resolve by ID or name
	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			slog.Error("lane update: not found", "name", idOrName, "error", err)
			return 2
		}
	}

	if err := store.SetIntent(ctx, l.ID, intent); err != nil {
		slog.Error("lane update failed", "error", err)
		return 2
	}

	fmt.Printf("updated %s intent\n", l.Name)
	return 0
}
