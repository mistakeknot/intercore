package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/landed"
)

func cmdLanded(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: landed: usage: ic landed <record|list|revert|summary>\n")
		return 3
	}

	switch args[0] {
	case "record":
		return cmdLandedRecord(ctx, args[1:])
	case "list":
		return cmdLandedList(ctx, args[1:])
	case "revert":
		return cmdLandedRevert(ctx, args[1:])
	case "summary":
		return cmdLandedSummary(ctx, args[1:])
	default:
		slog.Error("landed: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdLandedRecord(ctx context.Context, args []string) int {
	var opts landed.RecordOpts

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--commit="):
			opts.CommitSHA = strings.TrimPrefix(arg, "--commit=")
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--branch="):
			opts.Branch = strings.TrimPrefix(arg, "--branch=")
		case strings.HasPrefix(arg, "--dispatch="):
			opts.DispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--run="):
			opts.RunID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--bead="):
			opts.BeadID = strings.TrimPrefix(arg, "--bead=")
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--files="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--files="))
			if err != nil {
				slog.Error("landed record: invalid --files", "error", err)
				return 3
			}
			opts.FilesChanged = v
		case strings.HasPrefix(arg, "--insertions="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--insertions="))
			if err != nil {
				slog.Error("landed record: invalid --insertions", "error", err)
				return 3
			}
			opts.Insertions = v
		case strings.HasPrefix(arg, "--deletions="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--deletions="))
			if err != nil {
				slog.Error("landed record: invalid --deletions", "error", err)
				return 3
			}
			opts.Deletions = v
		default:
			slog.Error("landed record: unknown flag", "value", arg)
			return 3
		}
	}

	if opts.CommitSHA == "" || opts.ProjectDir == "" {
		slog.Error("landed record: --commit and --project are required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("landed record failed", "error", err)
		return 2
	}
	defer d.Close()

	store := landed.NewStore(d.SqlDB())
	id, err := store.Record(ctx, opts)
	if err != nil {
		slog.Error("landed record failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":         id,
			"commit_sha": opts.CommitSHA,
			"project":    opts.ProjectDir,
		})
	} else {
		fmt.Printf("Recorded landed change: %s (id=%d)\n", opts.CommitSHA[:minInt(12, len(opts.CommitSHA))], id)
	}
	return 0
}

func cmdLandedList(ctx context.Context, args []string) int {
	var opts landed.ListOpts
	opts.Limit = 100

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--bead="):
			opts.BeadID = strings.TrimPrefix(arg, "--bead=")
		case strings.HasPrefix(arg, "--run="):
			opts.RunID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--since="):
			t, err := time.Parse(time.RFC3339, strings.TrimPrefix(arg, "--since="))
			if err != nil {
				slog.Error("landed list: invalid --since (use RFC3339)", "error", err)
				return 3
			}
			opts.Since = t.Unix()
		case arg == "--include-reverted":
			opts.IncludeReverted = true
		case strings.HasPrefix(arg, "--limit="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				slog.Error("landed list: invalid --limit", "error", err)
				return 3
			}
			opts.Limit = v
		default:
			slog.Error("landed list: unknown flag", "value", arg)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("landed list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := landed.NewStore(d.SqlDB())
	changes, err := store.List(ctx, opts)
	if err != nil {
		slog.Error("landed list failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(changes)
	} else {
		if len(changes) == 0 {
			fmt.Println("No landed changes found.")
			return 0
		}
		for _, c := range changes {
			sha := c.CommitSHA[:minInt(12, len(c.CommitSHA))]
			t := time.Unix(c.LandedAt, 0).Format("2006-01-02 15:04")
			bead := ""
			if c.BeadID != nil {
				bead = " bead=" + *c.BeadID
			}
			reverted := ""
			if c.RevertedAt != nil {
				reverted = " [REVERTED]"
			}
			fmt.Printf("[%s] %s  %s%s%s\n", t, sha, c.ProjectDir, bead, reverted)
		}
		fmt.Printf("\n%d landed change(s)\n", len(changes))
	}
	return 0
}

func cmdLandedRevert(ctx context.Context, args []string) int {
	var commitSHA, projectDir, revertedBy string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--commit="):
			commitSHA = strings.TrimPrefix(arg, "--commit=")
		case strings.HasPrefix(arg, "--project="):
			projectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--reverted-by="):
			revertedBy = strings.TrimPrefix(arg, "--reverted-by=")
		default:
			slog.Error("landed revert: unknown flag", "value", arg)
			return 3
		}
	}

	if commitSHA == "" || projectDir == "" || revertedBy == "" {
		slog.Error("landed revert: --commit, --project, and --reverted-by are required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("landed revert failed", "error", err)
		return 2
	}
	defer d.Close()

	store := landed.NewStore(d.SqlDB())
	if err := store.MarkReverted(ctx, commitSHA, projectDir, revertedBy); err != nil {
		slog.Error("landed revert failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"commit":      commitSHA,
			"project":     projectDir,
			"reverted_by": revertedBy,
		})
	} else {
		fmt.Printf("Marked %s as reverted by %s\n",
			commitSHA[:minInt(12, len(commitSHA))],
			revertedBy[:minInt(12, len(revertedBy))])
	}
	return 0
}

func cmdLandedSummary(ctx context.Context, args []string) int {
	var opts landed.ListOpts

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--bead="):
			opts.BeadID = strings.TrimPrefix(arg, "--bead=")
		case strings.HasPrefix(arg, "--since="):
			t, err := time.Parse(time.RFC3339, strings.TrimPrefix(arg, "--since="))
			if err != nil {
				slog.Error("landed summary: invalid --since (use RFC3339)", "error", err)
				return 3
			}
			opts.Since = t.Unix()
		case strings.HasPrefix(arg, "--days="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--days="))
			if err != nil {
				slog.Error("landed summary: invalid --days", "error", err)
				return 3
			}
			opts.Since = time.Now().AddDate(0, 0, -v).Unix()
		default:
			slog.Error("landed summary: unknown flag", "value", arg)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("landed summary failed", "error", err)
		return 2
	}
	defer d.Close()

	store := landed.NewStore(d.SqlDB())
	summary, err := store.Summary(ctx, opts)
	if err != nil {
		slog.Error("landed summary failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(summary)
	} else {
		fmt.Printf("Landed Changes Summary\n")
		fmt.Printf("══════════════════════\n")
		fmt.Printf("  Total:      %d\n", summary.Total)
		fmt.Printf("  Reverted:   %d\n", summary.Reverted)
		fmt.Printf("  Active:     %d\n", summary.Total-summary.Reverted)
		if summary.FirstLanding > 0 {
			fmt.Printf("  First:      %s\n", time.Unix(summary.FirstLanding, 0).Format("2006-01-02 15:04"))
			fmt.Printf("  Last:       %s\n", time.Unix(summary.LastLanding, 0).Format("2006-01-02 15:04"))
		}
		if len(summary.ByBead) > 0 {
			fmt.Printf("\n  By Bead (%d):\n", len(summary.ByBead))
			for bead, count := range summary.ByBead {
				fmt.Printf("    %-20s %d commits\n", bead, count)
			}
		}
		if len(summary.ByRun) > 0 {
			fmt.Printf("\n  By Run (%d):\n", len(summary.ByRun))
			for run, count := range summary.ByRun {
				fmt.Printf("    %-20s %d commits\n", run[:minInt(12, len(run))], count)
			}
		}
	}
	return 0
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
