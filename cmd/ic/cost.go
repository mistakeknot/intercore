package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/intercore/internal/budget"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
)

func cmdCost(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: cost: usage: ic cost <reconcile|list>\n")
		return 3
	}

	switch args[0] {
	case "reconcile":
		return cmdCostReconcile(ctx, args[1:])
	case "list":
		return cmdCostList(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: cost: unknown subcommand: %s\n", args[0])
		return 3
	}
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
				fmt.Fprintf(os.Stderr, "ic: cost reconcile: invalid --billed-in: %v\n", err)
				return 3
			}
			billedIn = v
			hasBilledIn = true
		case strings.HasPrefix(arg, "--billed-out="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--billed-out="), 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: cost reconcile: invalid --billed-out: %v\n", err)
				return 3
			}
			billedOut = v
			hasBilledOut = true
		case strings.HasPrefix(arg, "--dispatch="):
			dispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--source="):
			source = strings.TrimPrefix(arg, "--source=")
		default:
			fmt.Fprintf(os.Stderr, "ic: cost reconcile: unknown flag: %s\n", arg)
			return 3
		}
	}

	if !hasBilledIn || !hasBilledOut {
		fmt.Fprintf(os.Stderr, "ic: cost reconcile: --billed-in and --billed-out are required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: cost reconcile: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: cost reconcile: %v\n", err)
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
				fmt.Fprintf(os.Stderr, "ic: cost list: invalid --limit: %v\n", err)
				return 3
			}
			limit = v
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: cost list: %v\n", err)
		return 2
	}
	defer d.Close()

	dStore := dispatch.New(d.SqlDB(), nil)
	rStore := budget.NewReconcileStore(d.SqlDB(), dStore)

	recs, err := rStore.List(ctx, runID, limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: cost list: %v\n", err)
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
