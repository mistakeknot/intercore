package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"

	"github.com/mistakeknot/intercore/pkg/autonomy"
)

func cmdAutonomy(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("autonomy: missing subcommand", "expected", "status")
		return 3
	}

	switch args[0] {
	case "status":
		return cmdAutonomyStatus(ctx)
	default:
		slog.Error("autonomy: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

// cmdAutonomyStatus reports the declared human-delegation level and what it
// derives. It deliberately does NOT report A/B/C track levels or capability
// mesh maturity: those are OS-layer concerns aggregated from evidence, and the
// kernel is mechanism, not policy. Conflating them here is exactly the
// ambiguity docs/canon/autonomy.md exists to prevent.
func cmdAutonomyStatus(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("autonomy status failed", "error", err)
		return 2
	}
	defer d.Close()

	res := autonomy.ResolveDB(ctx, d.SqlDB())

	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(res); err != nil {
			slog.Error("autonomy status failed", "error", err)
			return 2
		}
		return 0
	}

	fmt.Printf("delegation: L%d  (%s)\n", res.Level, res.Name)
	fmt.Printf("derives:    auto_advance=%t on new runs\n", res.AutoAdvance)
	fmt.Printf("source:     %s\n", res.Source)
	if !res.Declared {
		fmt.Printf("\nNot declared. Set it with:\n  ic config set %s <%d-%d>\n",
			autonomy.ConfigKey, autonomy.MinLevel, autonomy.MaxLevel)
	}
	return 0
}
