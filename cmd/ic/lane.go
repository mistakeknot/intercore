package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/lane"
)

func cmdLane(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: lane: missing subcommand (create, list, status, close, events, sync, members, velocity)\n")
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
	default:
		fmt.Fprintf(os.Stderr, "ic: lane: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdLaneCreate(ctx context.Context, args []string) int {
	var name, laneType, description string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--name="):
			name = strings.TrimPrefix(arg, "--name=")
		case strings.HasPrefix(arg, "--type="):
			laneType = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "--description="):
			description = strings.TrimPrefix(arg, "--description=")
		}
	}

	if name == "" {
		fmt.Fprintf(os.Stderr, "ic: lane create: --name is required\n")
		return 3
	}
	if laneType == "" {
		laneType = "standing"
	}
	if laneType != "standing" && laneType != "arc" {
		fmt.Fprintf(os.Stderr, "ic: lane create: --type must be 'standing' or 'arc'\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane create: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane create: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	id, err := store.Create(ctx, name, laneType, description)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane create: %v\n", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
			"id":        id,
			"name":      name,
			"lane_type": laneType,
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane create: encode: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: lane list: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane list: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	lanes, err := store.List(ctx, status)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane list: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: lane list: encode: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: lane status: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane status: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	// Try by ID first, fall back to name
	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane status: %v\n", err)
			return 2
		}
	}

	members, err := store.GetMembers(ctx, l.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane status: members: %v\n", err)
		return 2
	}

	events, err := store.Events(ctx, l.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane status: events: %v\n", err)
		return 2
	}

	if flagJSON {
		out := map[string]interface{}{
			"id":           l.ID,
			"name":         l.Name,
			"lane_type":    l.LaneType,
			"status":       l.Status,
			"description":  l.Description,
			"metadata":     l.Metadata,
			"member_count": len(members),
			"members":      members,
			"event_count":  len(events),
			"created_at":   l.CreatedAt,
			"updated_at":   l.UpdatedAt,
		}
		if err := json.NewEncoder(os.Stdout).Encode(out); err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane status: encode: %v\n", err)
			return 2
		}
	} else {
		fmt.Printf("Lane: %s (%s)\n", l.Name, l.ID)
		fmt.Printf("Type: %s  Status: %s\n", l.LaneType, l.Status)
		if l.Description != "" {
			fmt.Printf("Description: %s\n", l.Description)
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
		fmt.Fprintf(os.Stderr, "ic: lane close: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane close: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	if err := store.Close(ctx, args[0]); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane close: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: lane events: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane events: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	events, err := store.Events(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane events: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: lane events: encode: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: lane sync: --bead-ids is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane sync: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane sync: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	// Resolve name to ID if needed
	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane sync: %v\n", err)
			return 2
		}
	}

	if err := store.SnapshotMembers(ctx, l.ID, beadIDs); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane sync: %v\n", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"lane_id":      l.ID,
			"member_count": len(beadIDs),
		}); err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane sync: encode: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: lane members: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane members: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())

	l, err := store.Get(ctx, idOrName)
	if err != nil {
		l, err = store.GetByName(ctx, idOrName)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane members: %v\n", err)
			return 2
		}
	}

	members, err := store.GetMembers(ctx, l.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane members: %v\n", err)
		return 2
	}

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(members); err != nil {
			fmt.Fprintf(os.Stderr, "ic: lane members: encode: %v\n", err)
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
				fmt.Fprintf(os.Stderr, "ic: lane velocity: invalid --days value: %s\n", val)
				return 3
			}
			if days < 1 {
				fmt.Fprintf(os.Stderr, "ic: lane velocity: --days must be >= 1, got %d\n", days)
				return 3
			}
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane velocity: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane velocity: migrate: %v\n", err)
		return 2
	}

	store := lane.New(d.SqlDB())
	v := lane.NewVelocityCalculator(store)
	scores, err := v.ComputeStarvationFromDB(ctx, days)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lane velocity: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: lane velocity: encode: %v\n", err)
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
