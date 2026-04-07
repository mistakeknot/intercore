package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
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
	f := cli.ParseFlags(args)
	var opts landed.RecordOpts
	opts.CommitSHA = f.String("commit", "")
	opts.ProjectDir = f.String("project", "")
	opts.Branch = f.String("branch", "")
	opts.DispatchID = f.String("dispatch", "")
	opts.RunID = f.String("run", "")
	opts.BeadID = f.String("bead", "")
	opts.SessionID = f.String("session", "")

	var err error
	opts.FilesChanged, err = f.Int("files", 0)
	if err != nil {
		slog.Error("landed record: invalid --files", "error", err)
		return 3
	}
	opts.Insertions, err = f.Int("insertions", 0)
	if err != nil {
		slog.Error("landed record: invalid --insertions", "error", err)
		return 3
	}
	opts.Deletions, err = f.Int("deletions", 0)
	if err != nil {
		slog.Error("landed record: invalid --deletions", "error", err)
		return 3
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
	f := cli.ParseFlags(args)
	var opts landed.ListOpts
	opts.ProjectDir = f.String("project", "")
	opts.BeadID = f.String("bead", "")
	opts.RunID = f.String("run", "")
	opts.SessionID = f.String("session", "")
	opts.IncludeReverted = f.Bool("include-reverted")

	if sinceStr := f.String("since", ""); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			slog.Error("landed list: invalid --since (use RFC3339)", "error", err)
			return 3
		}
		opts.Since = t.Unix()
	}

	limit, err := f.Int("limit", 100)
	if err != nil {
		slog.Error("landed list: invalid --limit", "error", err)
		return 3
	}
	opts.Limit = limit

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
	f := cli.ParseFlags(args)
	commitSHA := f.String("commit", "")
	projectDir := f.String("project", "")
	revertedBy := f.String("reverted-by", "")

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
	f := cli.ParseFlags(args)
	var opts landed.ListOpts
	opts.ProjectDir = f.String("project", "")
	opts.BeadID = f.String("bead", "")

	if sinceStr := f.String("since", ""); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			slog.Error("landed summary: invalid --since (use RFC3339)", "error", err)
			return 3
		}
		opts.Since = t.Unix()
	}
	if f.Has("days") {
		v, err := f.Int("days", 0)
		if err != nil {
			slog.Error("landed summary: invalid --days", "error", err)
			return 3
		}
		opts.Since = time.Now().AddDate(0, 0, -v).Unix()
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
