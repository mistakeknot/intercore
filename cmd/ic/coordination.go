package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/coordination"
	"github.com/mistakeknot/intercore/internal/event"
)

func cmdCoordination(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("coordination: missing subcommand", "expected", "reserve, release, check, list, sweep, transfer")
		return 3
	}

	switch args[0] {
	case "reserve":
		return cmdCoordReserve(ctx, args[1:])
	case "release":
		return cmdCoordRelease(ctx, args[1:])
	case "check":
		return cmdCoordCheck(ctx, args[1:])
	case "list":
		return cmdCoordList(ctx, args[1:])
	case "sweep":
		return cmdCoordSweep(ctx, args[1:])
	case "transfer":
		return cmdCoordTransfer(ctx, args[1:])
	default:
		slog.Error("coordination: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func coordStore(ctx context.Context) (*coordination.Store, func(), int) {
	d, err := openDB()
	if err != nil {
		slog.Error("coordination failed", "error", err)
		return nil, nil, 2
	}

	if err := d.Migrate(ctx); err != nil {
		d.Close()
		slog.Error("coordination: migrate failed", "error", err)
		return nil, nil, 2
	}

	store := coordination.NewStore(d.SqlDB())

	// Wire event emission
	evStore := event.NewStore(d.SqlDB())
	notifier := event.NewNotifier()
	store.SetEventFunc(func(ctx context.Context, eventType, lockID, owner, pattern, scope, reason, runID string) error {
		if err := evStore.AddCoordinationEvent(ctx, eventType, lockID, owner, pattern, scope, reason, runID, nil); err != nil {
			return err
		}
		return notifier.Notify(ctx, event.Event{
			Source: event.SourceCoordination,
			Type:   eventType,
			RunID:  runID,
		})
	})

	return store, func() { d.Close() }, 0
}

func cmdCoordReserve(ctx context.Context, args []string) int {
	var lock coordination.Lock
	lock.Type = coordination.TypeFileReservation
	lock.Exclusive = true

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--owner="):
			lock.Owner = strings.TrimPrefix(arg, "--owner=")
		case strings.HasPrefix(arg, "--scope="):
			lock.Scope = strings.TrimPrefix(arg, "--scope=")
		case strings.HasPrefix(arg, "--pattern="):
			lock.Pattern = strings.TrimPrefix(arg, "--pattern=")
		case strings.HasPrefix(arg, "--reason="):
			lock.Reason = strings.TrimPrefix(arg, "--reason=")
		case strings.HasPrefix(arg, "--type="):
			lock.Type = strings.TrimPrefix(arg, "--type=")
		case strings.HasPrefix(arg, "--ttl="):
			val := strings.TrimPrefix(arg, "--ttl=")
			var ttl int
			fmt.Sscanf(val, "%d", &ttl)
			lock.TTLSeconds = ttl
		case strings.HasPrefix(arg, "--dispatch="):
			lock.DispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--run="):
			lock.RunID = strings.TrimPrefix(arg, "--run=")
		case arg == "--exclusive=false":
			lock.Exclusive = false
		case arg == "--exclusive":
			lock.Exclusive = true
		}
	}

	if lock.Owner == "" || lock.Scope == "" || lock.Pattern == "" {
		slog.Error("coordination reserve: --owner, --scope, and --pattern are required")
		return 3
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	result, err := store.Reserve(ctx, lock)
	if err != nil {
		slog.Error("coordination reserve failed", "error", err)
		return 2
	}

	if result.Conflict != nil {
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(result)
		} else {
			slog.Warn("coordination conflict", "pattern", result.Conflict.BlockerPattern, "owner", result.Conflict.BlockerOwner, "reason", result.Conflict.BlockerReason)
		}
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(result)
	} else {
		fmt.Printf("%s\n", result.Lock.ID)
	}
	return 0
}

func cmdCoordRelease(ctx context.Context, args []string) int {
	var id, owner, scope string
	var positional []string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--owner="):
			owner = strings.TrimPrefix(arg, "--owner=")
		case strings.HasPrefix(arg, "--scope="):
			scope = strings.TrimPrefix(arg, "--scope=")
		default:
			positional = append(positional, arg)
		}
	}

	if len(positional) > 0 {
		id = positional[0]
	}
	if id == "" && (owner == "" || scope == "") {
		slog.Error("coordination release: provide <id> or --owner + --scope")
		return 3
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	n, err := store.Release(ctx, id, owner, scope)
	if err != nil {
		slog.Error("coordination release failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]int64{"released": n})
	} else {
		fmt.Printf("%d released\n", n)
	}
	return 0
}

func cmdCoordCheck(ctx context.Context, args []string) int {
	var scope, pattern, excludeOwner string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--scope="):
			scope = strings.TrimPrefix(arg, "--scope=")
		case strings.HasPrefix(arg, "--pattern="):
			pattern = strings.TrimPrefix(arg, "--pattern=")
		case strings.HasPrefix(arg, "--exclude-owner="):
			excludeOwner = strings.TrimPrefix(arg, "--exclude-owner=")
		}
	}

	if scope == "" || pattern == "" {
		slog.Error("coordination check: --scope and --pattern are required")
		return 3
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	conflicts, err := store.Check(ctx, scope, pattern, excludeOwner)
	if err != nil {
		slog.Error("coordination check failed", "error", err)
		return 2
	}

	if len(conflicts) > 0 {
		if flagJSON {
			json.NewEncoder(os.Stdout).Encode(conflicts)
		} else {
			for _, c := range conflicts {
				fmt.Printf("%s\t%s\t%s\n", c.Owner, c.Pattern, c.Reason)
			}
		}
		return 1 // conflict
	}

	if flagJSON {
		fmt.Println("[]")
	}
	return 0 // clear
}

func cmdCoordList(ctx context.Context, args []string) int {
	var f coordination.ListFilter

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--scope="):
			f.Scope = strings.TrimPrefix(arg, "--scope=")
		case strings.HasPrefix(arg, "--owner="):
			f.Owner = strings.TrimPrefix(arg, "--owner=")
		case strings.HasPrefix(arg, "--type="):
			f.Type = strings.TrimPrefix(arg, "--type=")
		case arg == "--active":
			f.Active = true
		}
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	locks, err := store.List(ctx, f)
	if err != nil {
		slog.Error("coordination list failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(locks)
	} else {
		for _, l := range locks {
			age := time.Since(time.Unix(l.CreatedAt, 0)).Truncate(time.Second)
			active := "active"
			if l.ReleasedAt != nil {
				active = "released"
			}
			fmt.Printf("%s\t%s\t%s\t%s\t%s\t%s\n", l.ID, l.Type, l.Owner, l.Pattern, active, age)
		}
	}
	return 0
}

func cmdCoordSweep(ctx context.Context, args []string) int {
	var olderThan string
	var dryRun bool

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--older-than="):
			olderThan = strings.TrimPrefix(arg, "--older-than=")
		case arg == "--dry-run":
			dryRun = true
		}
	}

	var dur time.Duration
	if olderThan != "" {
		var err error
		dur, err = time.ParseDuration(olderThan)
		if err != nil {
			slog.Error("coordination sweep: invalid duration", "value", olderThan)
			return 3
		}
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	result, err := store.Sweep(ctx, dur, dryRun)
	if err != nil {
		slog.Error("coordination sweep failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(result)
	} else {
		prefix := ""
		if dryRun {
			prefix = "(dry-run) "
		}
		fmt.Printf("%s%d expired, %d total\n", prefix, result.Expired, result.Total)
	}
	return 0
}

func cmdCoordTransfer(ctx context.Context, args []string) int {
	var from, to, scope string
	var force bool

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--from="):
			from = strings.TrimPrefix(arg, "--from=")
		case strings.HasPrefix(arg, "--to="):
			to = strings.TrimPrefix(arg, "--to=")
		case strings.HasPrefix(arg, "--scope="):
			scope = strings.TrimPrefix(arg, "--scope=")
		case arg == "--force":
			force = true
		}
	}

	if from == "" || to == "" || scope == "" {
		slog.Error("coordination transfer: --from, --to, and --scope are required")
		return 3
	}

	store, cleanup, code := coordStore(ctx)
	if code != 0 {
		return code
	}
	defer cleanup()

	n, err := store.Transfer(ctx, from, to, scope, force)
	if err != nil {
		slog.Error("coordination transfer failed", "error", err)
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]int64{"transferred": n})
	} else {
		fmt.Printf("%d transferred\n", n)
	}
	return 0
}
