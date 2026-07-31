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
		if err := json.NewEncoder(os.Stdout).Encode(autonomyStatusJSON{
			Resolution: res,
			Ops:        autonomy.Rulings(),
		}); err != nil {
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

	printOpFloors(res)
	return 0
}

// autonomyStatusJSON embeds Resolution so its fields stay at the top level of
// the object. gen-autonomy-position.py already reads `level`, `name`,
// `declared` and `derives_auto_advance` from there; adding `ops` alongside them
// extends the payload without moving what an existing reader depends on.
type autonomyStatusJSON struct {
	autonomy.Resolution
	Ops []autonomy.OpRuling `json:"ops"`
}

// printOpFloors renders the floor table, marking which operations the level in
// force actually clears.
//
// The point of printing this is that a refusal should be predictable before it
// happens. Knowing the level alone does not tell you what it will withhold —
// that needs the floors next to it, which is why this prints in `status` rather
// than hiding behind a separate subcommand.
func printOpFloors(res autonomy.Resolution) {
	rulings := autonomy.Rulings()
	if len(rulings) == 0 {
		return
	}

	// Width is measured, not hardcoded: a fixed %-18s silently misaligns the
	// whole table the first time someone rules on an op with a longer name,
	// and the table is the deliverable here.
	width := 0
	for _, r := range rulings {
		if n := len(r.Op); n > width {
			width = n
		}
	}

	fmt.Printf("\noperations gated by this level:\n")
	for _, r := range rulings {
		if r.Floor == 0 {
			fmt.Printf("  %-*s  no floor  %-7s — %s\n", width, r.Op, "", r.Reason)
			continue
		}
		verdict := "BLOCKED"
		if res.Level >= r.Floor {
			verdict = "allowed"
		}
		fmt.Printf("  %-*s  needs L%d  %-7s — %s\n", width, r.Op, r.Floor, verdict, r.Reason)
	}
	fmt.Printf("\n\"no floor\" is a recorded exemption, not an omission — an operation\n")
	fmt.Printf("absent from this list has never been ruled on.\n")
}
