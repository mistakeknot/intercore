package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/portfolio"
)

func cmdPortfolio(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("portfolio: missing subcommand", "expected", "dep, relay, order, status")
		return 3
	}

	switch args[0] {
	case "dep":
		return cmdPortfolioDep(ctx, args[1:])
	case "relay":
		return cmdPortfolioRelay(ctx, args[1:])
	case "order":
		return cmdPortfolioOrder(ctx, args[1:])
	case "status":
		return cmdPortfolioStatus(ctx, args[1:])
	default:
		slog.Error("portfolio: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdPortfolioDep(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("portfolio dep: missing subcommand", "expected", "add, list, remove")
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
		slog.Error("portfolio dep: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdPortfolioDepAdd(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	upstream := f.String("upstream", "")
	downstream := f.String("downstream", "")

	if len(f.Positionals) < 1 || upstream == "" || downstream == "" {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep add: usage: ic portfolio dep add <portfolio-id> --upstream=<path> --downstream=<path>\n")
		return 3
	}
	portfolioID := f.Positionals[0]

	// Normalize paths to absolute to match child project_dir values
	upstream, err := filepath.Abs(upstream)
	if err != nil {
		slog.Error("portfolio dep add: resolve upstream failed", "error", err)
		return 2
	}
	downstream, err = filepath.Abs(downstream)
	if err != nil {
		slog.Error("portfolio dep add: resolve downstream failed", "error", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		slog.Error("portfolio dep add failed", "error", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	if err := depStore.Add(ctx, portfolioID, upstream, downstream); err != nil {
		slog.Error("portfolio dep add failed", "error", err)
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
		slog.Error("portfolio dep list failed", "error", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	deps, err := depStore.List(ctx, args[0])
	if err != nil {
		slog.Error("portfolio dep list failed", "error", err)
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
	f := cli.ParseFlags(args)
	upstream := f.String("upstream", "")
	downstream := f.String("downstream", "")

	if len(f.Positionals) < 1 || upstream == "" || downstream == "" {
		fmt.Fprintf(os.Stderr, "ic: portfolio dep remove: usage: ic portfolio dep remove <portfolio-id> --upstream=<path> --downstream=<path>\n")
		return 3
	}
	portfolioID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("portfolio dep remove failed", "error", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	if err := depStore.Remove(ctx, portfolioID, upstream, downstream); err != nil {
		slog.Error("portfolio dep remove failed", "error", err)
		return 2
	}

	fmt.Println("removed")
	return 0
}

func cmdPortfolioRelay(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	interval := 2 * time.Second

	if f.Has("interval") {
		d, err := f.Duration("interval", 2*time.Second)
		if err != nil || d <= 0 {
			slog.Error("portfolio relay: invalid interval", "value", f.String("interval", ""))
			return 3
		}
		interval = d
	}

	if len(f.Positionals) < 1 {
		fmt.Fprintf(os.Stderr, "ic: portfolio relay: usage: ic portfolio relay <portfolio-id> [--interval=2s]\n")
		return 3
	}
	portfolioID := f.Positionals[0]

	d, err := openDB()
	if err != nil {
		slog.Error("portfolio relay failed", "error", err)
		return 2
	}
	defer d.Close()

	relay := portfolio.NewRelay(portfolioID, d.SqlDB(), interval)
	relay.SetLogger(slog.Default())

	// Handle SIGINT/SIGTERM for clean shutdown
	relayCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		slog.Info("relay shutting down")
		cancel()
	}()

	slog.Info("relay starting", "portfolio", portfolioID, "interval", interval.String())
	if err := relay.Run(relayCtx); err != nil && err != context.Canceled {
		slog.Error("portfolio relay failed", "error", err)
		return 2
	}

	slog.Info("relay stopped")
	return 0
}

func cmdPortfolioOrder(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: portfolio order: usage: ic portfolio order <portfolio-id>\n")
		return 3
	}
	portfolioID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("portfolio order failed", "error", err)
		return 2
	}
	defer d.Close()

	depStore := portfolio.NewDepStore(d.SqlDB())
	deps, err := depStore.List(ctx, portfolioID)
	if err != nil {
		slog.Error("portfolio order failed", "error", err)
		return 2
	}

	order, err := portfolio.TopologicalSort(deps)
	if err != nil {
		slog.Error("portfolio order failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(order)
	} else {
		for i, p := range order {
			fmt.Printf("%d\t%s\n", i+1, p)
		}
	}
	return 0
}

func cmdPortfolioStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: portfolio status: usage: ic portfolio status <portfolio-id>\n")
		return 3
	}
	portfolioID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("portfolio status failed", "error", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	depStore := portfolio.NewDepStore(d.SqlDB())

	children, err := store.GetChildren(ctx, portfolioID)
	if err != nil {
		slog.Error("portfolio status failed", "error", err)
		return 2
	}

	deps, err := depStore.List(ctx, portfolioID)
	if err != nil {
		slog.Error("portfolio status failed", "error", err)
		return 2
	}

	// Build upstream map: project → []upstream projects
	upstreamMap := make(map[string][]string)
	for _, dep := range deps {
		upstreamMap[dep.DownstreamProject] = append(upstreamMap[dep.DownstreamProject], dep.UpstreamProject)
	}

	// Index children by project dir
	childByProject := make(map[string]*phase.Run)
	for _, c := range children {
		if _, exists := childByProject[c.ProjectDir]; !exists {
			childByProject[c.ProjectDir] = c
		}
	}

	type childStatus struct {
		Project   string   `json:"project"`
		Phase     string   `json:"phase"`
		Status    string   `json:"status"`
		Ready     bool     `json:"ready"`
		BlockedBy []string `json:"blocked_by,omitempty"`
	}

	var statuses []childStatus
	for _, child := range children {
		cs := childStatus{
			Project: child.ProjectDir,
			Phase:   child.Phase,
			Status:  child.Status,
			Ready:   true,
		}

		if child.Status == phase.StatusCompleted || child.Status == phase.StatusCancelled || child.Status == phase.StatusFailed {
			statuses = append(statuses, cs)
			continue
		}

		for _, upstream := range upstreamMap[child.ProjectDir] {
			upRun, ok := childByProject[upstream]
			if !ok {
				continue
			}
			if upRun.Status == phase.StatusCompleted || upRun.Status == phase.StatusCancelled || upRun.Status == phase.StatusFailed {
				continue
			}
			// Check if upstream has reached child's current phase by name
			// (not by index, since chains may differ)
			upChain := phase.ResolveChain(upRun)
			targetIdx := phase.ChainPhaseIndex(upChain, child.Phase)
			if targetIdx < 0 {
				continue // upstream chain doesn't have this phase
			}
			upIdx := phase.ChainPhaseIndex(upChain, upRun.Phase)
			if upIdx < targetIdx {
				cs.Ready = false
				cs.BlockedBy = append(cs.BlockedBy, fmt.Sprintf("%s (at %s)", upstream, upRun.Phase))
			}
		}

		statuses = append(statuses, cs)
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(statuses)
	} else {
		fmt.Printf("%-40s %-20s %-12s %-6s %s\n", "PROJECT", "PHASE", "STATUS", "READY", "BLOCKED BY")
		for _, cs := range statuses {
			blockedBy := ""
			if len(cs.BlockedBy) > 0 {
				blockedBy = strings.Join(cs.BlockedBy, ", ")
			}
			ready := "yes"
			if !cs.Ready {
				ready = "no"
			}
			fmt.Printf("%-40s %-20s %-12s %-6s %s\n", cs.Project, cs.Phase, cs.Status, ready, blockedBy)
		}
	}
	return 0
}
