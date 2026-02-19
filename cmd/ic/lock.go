package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/lock"
)

// --- Lock Commands ---

func cmdLock(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: lock: missing subcommand (acquire, release, list, stale, clean)\n")
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
		fmt.Fprintf(os.Stderr, "ic: lock: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdLockAcquire(ctx context.Context, args []string) int {
	var timeout string
	var owner string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--timeout="):
			timeout = strings.TrimPrefix(args[i], "--timeout=")
		case strings.HasPrefix(args[i], "--owner="):
			owner = strings.TrimPrefix(args[i], "--owner=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 {
		fmt.Fprintf(os.Stderr, "ic: lock acquire: usage: ic lock acquire <name> <scope> [--timeout=<dur>] [--owner=<s>]\n")
		return 3
	}

	dur := time.Second
	if timeout != "" {
		var err error
		dur, err = time.ParseDuration(timeout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: lock acquire: invalid timeout: %s\n", timeout)
			return 3
		}
	}

	if owner == "" {
		hostname, _ := os.Hostname()
		owner = fmt.Sprintf("%d:%s", os.Getpid(), hostname)
	}

	mgr := lock.NewManager("")
	err := mgr.Acquire(ctx, positional[0], positional[1], owner, dur)
	if err != nil {
		if errors.Is(err, lock.ErrTimeout) {
			if flagVerbose {
				fmt.Fprintf(os.Stderr, "ic: lock acquire: timed out\n")
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: lock acquire: %v\n", err)
		return 2
	}

	if flagVerbose {
		fmt.Printf("acquired %s/%s\n", positional[0], positional[1])
	}
	return 0
}

func cmdLockRelease(ctx context.Context, args []string) int {
	var owner string
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--owner="):
			owner = strings.TrimPrefix(args[i], "--owner=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 {
		fmt.Fprintf(os.Stderr, "ic: lock release: usage: ic lock release <name> <scope> [--owner=<s>]\n")
		return 3
	}

	if owner == "" {
		hostname, _ := os.Hostname()
		owner = fmt.Sprintf("%d:%s", os.Getpid(), hostname)
	}

	mgr := lock.NewManager("")
	err := mgr.Release(ctx, positional[0], positional[1], owner)
	if err != nil {
		if errors.Is(err, lock.ErrNotFound) {
			if flagVerbose {
				fmt.Fprintf(os.Stderr, "ic: lock release: not found\n")
			}
			return 1
		}
		if errors.Is(err, lock.ErrNotOwner) {
			if flagVerbose {
				fmt.Fprintf(os.Stderr, "ic: lock release: not owner\n")
			}
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: lock release: %v\n", err)
		return 2
	}

	if flagVerbose {
		fmt.Printf("released %s/%s\n", positional[0], positional[1])
	}
	return 0
}

func cmdLockList(ctx context.Context) int {
	mgr := lock.NewManager("")
	locks, err := mgr.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lock list: %v\n", err)
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
	olderThan := "5s"
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--older-than=") {
			olderThan = strings.TrimPrefix(args[i], "--older-than=")
		}
	}

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lock stale: invalid duration: %s\n", olderThan)
		return 3
	}

	mgr := lock.NewManager("")
	locks, err := mgr.Stale(ctx, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lock stale: %v\n", err)
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
	olderThan := "5s"
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--older-than=") {
			olderThan = strings.TrimPrefix(args[i], "--older-than=")
		}
	}

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lock clean: invalid duration: %s\n", olderThan)
		return 3
	}

	mgr := lock.NewManager("")
	count, err := mgr.Clean(ctx, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: lock clean: %v\n", err)
		return 2
	}

	fmt.Printf("%d cleaned\n", count)
	return 0
}
