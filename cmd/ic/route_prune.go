package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/routing"
)

// parseRetention accepts a Go duration ("720h") or a whole number of days ("30d").
func parseRetention(v string) (time.Duration, error) {
	if strings.HasSuffix(v, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(v, "d"))
		// A count past what a Duration holds would wrap to a short window and delete recent rows.
		if err != nil || n <= 0 || int64(n) > int64(math.MaxInt64/(24*time.Hour)) {
			return 0, fmt.Errorf("invalid retention %q: want a positive number of days like 30d", v)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid retention %q: want a positive duration like 720h or 30d", v)
	}
	return d, nil
}

// cmdRoutePrune moves routing decisions older than a window into a gzip JSONL
// archive and deletes them from the database. With --no-archive they are deleted
// for good, so that has to be asked for by name.
func cmdRoutePrune(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	if bad := f.Unknown("older-than", "archive-dir", "no-archive", "dry-run", "vacuum"); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "ic: route prune: unknown flag(s): --%s\n", strings.Join(bad, ", --"))
		return 3
	}
	if len(f.Positionals) > 0 {
		fmt.Fprintf(os.Stderr, "ic: route prune: unexpected argument %q\n", f.Positionals[0])
		return 3
	}
	window := routing.DefaultRetention
	if v := f.String("older-than", ""); v != "" {
		d, err := parseRetention(v)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: route prune: %v\n", err)
			return 3
		}
		window = d
	}
	noArchive := f.Bool("no-archive")
	if noArchive && f.Has("archive-dir") {
		fmt.Fprintf(os.Stderr, "ic: route prune: --no-archive and --archive-dir are mutually exclusive\n")
		return 3
	}
	dryRun := f.Bool("dry-run")

	dbPath, err := resolveDBPath()
	if err != nil {
		slog.Error("route prune failed", "error", err)
		return 2
	}
	archiveDir := ""
	if !noArchive {
		archiveDir = f.String("archive-dir", "")
		if archiveDir == "" {
			archiveDir = filepath.Join(filepath.Dir(dbPath), "routing-archive")
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("route prune failed", "error", err)
		return 2
	}
	defer d.Close()

	res, err := routing.PruneDecisions(ctx, d.SqlDB(), routing.PruneOpts{
		OlderThan:  window,
		ArchiveDir: archiveDir,
		DryRun:     dryRun,
	})
	if err != nil {
		slog.Error("route prune failed", "error", err)
		return 2
	}

	vacuumed := false
	if f.Bool("vacuum") && !dryRun && res.Deleted > 0 {
		// VACUUM rewrites the file (needs free space about the size of the database)
		// and returns the pages the prune freed to the filesystem. It is a separate,
		// explicit step because it takes an exclusive lock for its duration.
		if _, err := d.SqlDB().ExecContext(ctx, "VACUUM"); err != nil {
			slog.Error("route prune: vacuum failed (rows were pruned and archived)", "error", err)
			return 2
		}
		vacuumed = true
	}

	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			*routing.PruneResult
			Window   string `json:"window"`
			Vacuumed bool   `json:"vacuumed"`
		}{res, window.String(), vacuumed})
		return 0
	}
	verb := "pruned"
	if dryRun {
		verb = "would prune"
	}
	fmt.Printf("%s %d routing decision(s) older than %s", verb, res.Eligible, window)
	if res.Archived > 0 {
		fmt.Printf("; archived %d to %s", res.Archived, res.ArchiveDir)
	}
	if noArchive && res.Deleted > 0 {
		fmt.Printf("; NOT archived (--no-archive)")
	}
	if res.Protected > 0 {
		fmt.Printf("; kept %d still referenced", res.Protected)
	}
	if vacuumed {
		fmt.Printf("; vacuumed")
	}
	fmt.Println()
	return 0
}

// cmdRouteRestore re-inserts rows from archive files written by `route prune`.
func cmdRouteRestore(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	if bad := f.Unknown(); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "ic: route restore: unknown flag(s): --%s\n", strings.Join(bad, ", --"))
		return 3
	}
	if len(f.Positionals) == 0 {
		fmt.Fprintf(os.Stderr, "ic: route restore: usage: ic route restore <archive.jsonl.gz|dir>...\n")
		return 3
	}
	d, err := openDB()
	if err != nil {
		slog.Error("route restore failed", "error", err)
		return 2
	}
	defer d.Close()
	res, err := routing.RestoreDecisions(ctx, d.SqlDB(), f.Positionals)
	if err != nil {
		slog.Error("route restore failed", "error", err)
		return 2
	}
	if flagJSON {
		_ = json.NewEncoder(os.Stdout).Encode(res)
		return 0
	}
	fmt.Printf("restored %d routing decision(s); %d already present\n", res.Restored, res.Existing)
	return 0
}
