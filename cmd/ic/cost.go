package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/intercore/internal/budget"
	costpkg "github.com/mistakeknot/intercore/internal/cost"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
)

func cmdCost(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: cost: usage: ic cost <baseline|reconcile|list>\n")
		return 3
	}

	switch args[0] {
	case "baseline":
		return cmdCostBaseline(ctx, args[1:])
	case "reconcile":
		return cmdCostReconcile(ctx, args[1:])
	case "list":
		return cmdCostList(ctx, args[1:])
	default:
		slog.Error("cost: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdCostBaseline(ctx context.Context, args []string) int {
	opts := costpkg.BaselineOpts{
		LastN: 50,
		Days:  30,
	}

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--last="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--last="))
			if err != nil {
				slog.Error("cost baseline: invalid --last", "error", err)
				return 3
			}
			opts.LastN = v
		case strings.HasPrefix(arg, "--days="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--days="))
			if err != nil {
				slog.Error("cost baseline: invalid --days", "error", err)
				return 3
			}
			opts.Days = v
		case arg == "--by-phase":
			opts.ByPhase = true
		case arg == "--by-agent":
			opts.ByAgent = true
		case strings.HasPrefix(arg, "--script="):
			opts.InterstatScript = strings.TrimPrefix(arg, "--script=")
		default:
			slog.Error("cost baseline: unknown flag", "value", arg)
			return 3
		}
	}

	opts.JSON = flagJSON

	// Resolve interstat script at CLI layer (avoids L1→L3 coupling in internal/)
	if opts.InterstatScript == "" {
		opts.InterstatScript = costpkg.FindInterstatScript()
		if opts.InterstatScript == "" {
			slog.Error("cost baseline: interstat cost-query.sh not found", "hint", "install interstat plugin or use --script=<path>")
			return 2
		}
	}

	// Pass DB for direct landed_changes queries (primary denominator source)
	d, err := openDB()
	if err != nil {
		slog.Warn("cost baseline: could not open DB for landed_changes, falling back to bd", "error", err)
	} else {
		defer d.Close()
		opts.DB = d.SqlDB()
	}

	result, err := costpkg.ComputeBaseline(ctx, opts)
	if err != nil {
		slog.Error("cost baseline failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(result)
	} else {
		fmt.Print(costpkg.FormatText(result))
	}
	return 0
}

func cmdCostReconcile(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: cost reconcile: usage: ic cost reconcile <run_id> --billed-in=N --billed-out=N [--dispatch=<id>] [--source=manual]\n")
		return 3
	}
	runID := args[0]

	var billedIn, billedOut int64
	var dispatchID, source string
	var hasBilledIn, hasBilledOut bool

	for _, arg := range args[1:] {
		switch {
		case strings.HasPrefix(arg, "--billed-in="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--billed-in="), 10, 64)
			if err != nil {
				slog.Error("cost reconcile: invalid --billed-in", "error", err)
				return 3
			}
			billedIn = v
			hasBilledIn = true
		case strings.HasPrefix(arg, "--billed-out="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--billed-out="), 10, 64)
			if err != nil {
				slog.Error("cost reconcile: invalid --billed-out", "error", err)
				return 3
			}
			billedOut = v
			hasBilledOut = true
		case strings.HasPrefix(arg, "--dispatch="):
			dispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--source="):
			source = strings.TrimPrefix(arg, "--source=")
		default:
			slog.Error("cost reconcile: unknown flag", "value", arg)
			return 3
		}
	}

	if !hasBilledIn || !hasBilledOut {
		slog.Error("cost reconcile: --billed-in and --billed-out are required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("cost reconcile failed", "error", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	eStore := event.NewStore(d.SqlDB())
	rStore := budget.NewReconcileStore(d.SqlDB(), dStore)

	recorder := func(ctx context.Context, runID, eventType, reason string) error {
		return eStore.AddDispatchEvent(ctx, "", runID, "", "", eventType, reason, nil)
	}

	rec, err := rStore.Reconcile(ctx, runID, dispatchID, billedIn, billedOut, source, recorder)
	if err != nil {
		slog.Error("cost reconcile failed", "error", err)
		return 2
	}

	hasDiscrepancy := rec.DeltaIn != 0 || rec.DeltaOut != 0

	if flagJSON {
		out := map[string]interface{}{
			"run_id":       rec.RunID,
			"reported_in":  rec.ReportedIn,
			"reported_out": rec.ReportedOut,
			"billed_in":    rec.BilledIn,
			"billed_out":   rec.BilledOut,
			"delta_in":     rec.DeltaIn,
			"delta_out":    rec.DeltaOut,
			"source":       rec.Source,
			"discrepancy":  hasDiscrepancy,
		}
		if rec.DispatchID != "" {
			out["dispatch_id"] = rec.DispatchID
		}
		json.NewEncoder(os.Stdout).Encode(out)
	} else {
		fmt.Printf("Run: %s\n", rec.RunID)
		if rec.DispatchID != "" {
			fmt.Printf("  Dispatch:     %s\n", rec.DispatchID)
		}
		fmt.Printf("  Reported:     in=%d out=%d\n", rec.ReportedIn, rec.ReportedOut)
		fmt.Printf("  Billed:       in=%d out=%d\n", rec.BilledIn, rec.BilledOut)
		fmt.Printf("  Delta:        in=%+d out=%+d\n", rec.DeltaIn, rec.DeltaOut)
		fmt.Printf("  Source:       %s\n", rec.Source)
		if hasDiscrepancy {
			fmt.Printf("  Status:       DISCREPANCY\n")
		} else {
			fmt.Printf("  Status:       OK\n")
		}
	}

	if hasDiscrepancy {
		return 1
	}
	return 0
}

func cmdCostList(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: cost list: usage: ic cost list <run_id> [--limit=N]\n")
		return 3
	}
	runID := args[0]

	limit := 100
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--limit=") {
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				slog.Error("cost list: invalid --limit", "error", err)
				return 3
			}
			limit = v
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("cost list failed", "error", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	rStore := budget.NewReconcileStore(d.SqlDB(), dStore)

	recs, err := rStore.List(ctx, runID, limit)
	if err != nil {
		slog.Error("cost list failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(recs)
	} else {
		if len(recs) == 0 {
			fmt.Printf("No reconciliations for run %s\n", runID)
			return 0
		}
		for _, r := range recs {
			status := "OK"
			if r.DeltaIn != 0 || r.DeltaOut != 0 {
				status = "DISCREPANCY"
			}
			scope := "run"
			if r.DispatchID != "" {
				scope = r.DispatchID
			}
			fmt.Printf("[%s] %s  scope=%s  delta_in=%+d  delta_out=%+d  source=%s\n",
				r.CreatedAt.Format("2006-01-02 15:04"), status, scope, r.DeltaIn, r.DeltaOut, r.Source)
		}
	}
	return 0
}
