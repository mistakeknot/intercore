package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/lock"
)

// --- Lock Commands ---

func cmdLock(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("lock: missing subcommand", "expected", "acquire, release, list, stale, clean")
		return 3
	}

	switch args[0] {
	case "acquire":
		return cmdLockAcquire(ctx, args[1:])
	case "release":
		return cmdLockRelease(ctx, args[1:])
	case "list":
		return cmdLockList(ctx)
	case "stale":
		return cmdLockStale(ctx, args[1:])
	case "clean":
		return cmdLockClean(ctx, args[1:])
	default:
		slog.Error("lock: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdLockAcquire(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	owner := f.String("owner", "")

	if len(f.Positionals) < 2 {
		fmt.Fprintf(os.Stderr, "ic: lock acquire: usage: ic lock acquire <name> <scope> [--timeout=<dur>] [--owner=<s>]\n")
		return 3
	}

	dur := time.Second
	if f.Has("timeout") {
		var err error
		dur, err = f.Duration("timeout", time.Second)
		if err != nil {
			slog.Error("lock acquire: invalid timeout", "value", f.String("timeout", ""))
			return 3
		}
	}

	if owner == "" {
		hostname, _ := os.Hostname()
		owner = fmt.Sprintf("%d:%s", os.Getpid(), hostname)
	}

	mgr := lock.NewManager("")
	err := mgr.Acquire(ctx, f.Positionals[0], f.Positionals[1], owner, dur)
	if err != nil {
		if errors.Is(err, lock.ErrTimeout) {
			if flagVerbose {
				slog.Error("lock acquire: timed out")
			}
			return 1
		}
		slog.Error("lock acquire failed", "error", err)
		return 2
	}

	if flagVerbose {
		fmt.Printf("acquired %s/%s\n", f.Positionals[0], f.Positionals[1])
	}
	return 0
}

func cmdLockRelease(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	owner := f.String("owner", "")

	if len(f.Positionals) < 2 {
		fmt.Fprintf(os.Stderr, "ic: lock release: usage: ic lock release <name> <scope> [--owner=<s>]\n")
		return 3
	}

	if owner == "" {
		hostname, _ := os.Hostname()
		owner = fmt.Sprintf("%d:%s", os.Getpid(), hostname)
	}

	mgr := lock.NewManager("")
	err := mgr.Release(ctx, f.Positionals[0], f.Positionals[1], owner)
	if err != nil {
		if errors.Is(err, lock.ErrNotFound) {
			if flagVerbose {
				slog.Error("lock release: not found")
			}
			return 1
		}
		if errors.Is(err, lock.ErrNotOwner) {
			if flagVerbose {
				slog.Error("lock release: not owner")
			}
			return 1
		}
		slog.Error("lock release failed", "error", err)
		return 2
	}

	if flagVerbose {
		fmt.Printf("released %s/%s\n", f.Positionals[0], f.Positionals[1])
	}
	return 0
}

func cmdLockList(ctx context.Context) int {
	mgr := lock.NewManager("")
	locks, err := mgr.List(ctx)
	if err != nil {
		slog.Error("lock list failed", "error", err)
		return 2
	}

	for _, l := range locks {
		age := ""
		if !l.Created.IsZero() {
			age = time.Since(l.Created).Truncate(time.Second).String()
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", l.Name, l.Scope, l.Owner, age)
	}
	return 0
}

func cmdLockStale(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	olderThan := f.String("older-than", "5s")

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		slog.Error("lock stale: invalid duration", "value", olderThan)
		return 3
	}

	mgr := lock.NewManager("")
	locks, err := mgr.Stale(ctx, dur)
	if err != nil {
		slog.Error("lock stale failed", "error", err)
		return 2
	}

	for _, l := range locks {
		age := ""
		if !l.Created.IsZero() {
			age = time.Since(l.Created).Truncate(time.Second).String()
		}
		fmt.Printf("%s\t%s\t%s\t%s\n", l.Name, l.Scope, l.Owner, age)
	}
	return 0
}

func cmdLockClean(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	olderThan := f.String("older-than", "5s")

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		slog.Error("lock clean: invalid duration", "value", olderThan)
		return 3
	}

	mgr := lock.NewManager("")
	count, err := mgr.Clean(ctx, dur)
	if err != nil {
		slog.Error("lock clean failed", "error", err)
		return 2
	}

	fmt.Printf("%d cleaned\n", count)
	return 0
}
