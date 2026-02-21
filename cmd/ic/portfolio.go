package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/portfolio"
)

func cmdPortfolio(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: portfolio: missing subcommand (dep, relay)\n")
		return 3
	}

	switch args[0] {
	case "dep":
		return cmdPortfolioDep(ctx, args[1:])
	case "relay":
		return cmdPortfolioRelay(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: portfolio: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdPortfolioDep(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep: missing subcommand (add, list, remove)\n")
		return 3
	}

	switch args[0] {
	case "add":
		return cmdPortfolioDepAdd(ctx, args[1:])
	case "list":
		return cmdPortfolioDepList(ctx, args[1:])
	case "remove":
		return cmdPortfolioDepRemove(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: portfolio dep: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdPortfolioDepAdd(ctx context.Context, args []string) int {
	var portfolioID, upstream, downstream string

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--upstream="):
			upstream = strings.TrimPrefix(args[i], "--upstream=")
		case strings.HasPrefix(args[i], "--downstream="):
			downstream = strings.TrimPrefix(args[i], "--downstream=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 || upstream == "" || downstream == "" {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: usage: ic portfolio dep add <portfolio-id> --upstream=<path> --downstream=<path>\n")
		return 3
	}
	portfolioID = positional[0]

	// Normalize paths to absolute to match child project_dir values
	upstream, err := filepath.Abs(upstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: resolve upstream: %v\n", err)
		return 2
	}
	downstream, err = filepath.Abs(downstream)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: resolve downstream: %v\n", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: %v\n", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	if err := depStore.Add(ctx, portfolioID, upstream, downstream); err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: %v\n", err)
		return 2
	}

	fmt.Println("added")
	return 0
}

func cmdPortfolioDepList(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep list: usage: ic portfolio dep list <portfolio-id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep list: %v\n", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	deps, err := depStore.List(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep list: %v\n", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(deps))
		for i, dep := range deps {
			items[i] = map[string]interface{}{
				"upstream":   dep.UpstreamProject,
				"downstream": dep.DownstreamProject,
				"created_at": dep.CreatedAt,
			}
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, dep := range deps {
			fmt.Printf("%s → %s\n", dep.UpstreamProject, dep.DownstreamProject)
		}
	}
	return 0
}

func cmdPortfolioDepRemove(ctx context.Context, args []string) int {
	var portfolioID, upstream, downstream string

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--upstream="):
			upstream = strings.TrimPrefix(args[i], "--upstream=")
		case strings.HasPrefix(args[i], "--downstream="):
			downstream = strings.TrimPrefix(args[i], "--downstream=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 || upstream == "" || downstream == "" {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep remove: usage: ic portfolio dep remove <portfolio-id> --upstream=<path> --downstream=<path>\n")
		return 3
	}
	portfolioID = positional[0]

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep remove: %v\n", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	if err := depStore.Remove(ctx, portfolioID, upstream, downstream); err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep remove: %v\n", err)
		return 2
	}

	fmt.Println("removed")
	return 0
}

func cmdPortfolioRelay(ctx context.Context, args []string) int {
	var portfolioID string
	interval := 2 * time.Second

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--interval="):
			val := strings.TrimPrefix(args[i], "--interval=")
			d, err := time.ParseDuration(val)
			if err != nil || d <= 0 {
				fmt.Fprintf(os.Stderr, "ic: portfolio relay: invalid interval: %s\n", val)
				return 3
			}
			interval = d
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: portfolio relay: usage: ic portfolio relay <portfolio-id> [--interval=2s]\n")
		return 3
	}
	portfolioID = positional[0]

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: portfolio relay: %v\n", err)
		return 2
	}
	defer d.Close()

	relay := portfolio.NewRelay(portfolioID, d.SqlDB(), interval)
	relay.SetLogWriter(os.Stderr)

	// Handle SIGINT/SIGTERM for clean shutdown
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintf(os.Stderr, "\n[relay] shutting down...\n")
		cancel()
	}()

	fmt.Fprintf(os.Stderr, "[relay] starting for portfolio %s (interval=%s)\n", portfolioID, interval)
	if err := relay.Run(relayCtx); err != nil && err != context.Canceled {
		fmt.Fprintf(os.Stderr, "ic: portfolio relay: %v\n", err)
		return 2
	}

	fmt.Fprintf(os.Stderr, "[relay] stopped\n")
	return 0
}
