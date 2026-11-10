//go:build icsched_test

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/mistakeknot/intercore/internal/cli"
	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/scheduler"
	"github.com/mistakeknot/intercore/internal/scheduler/runner"
)

// cmdSchedulerRunner is `ic scheduler runner`, compiled only under the
// icsched_test build tag. It reports whether a runner serves this database:
// alive, hung (the process exists but its heartbeat is stale or it is not
// running) or absent. It takes no lock and writes nothing. It exits 0 once it
// has a verdict, whatever the verdict, and 2 when it cannot read the database.
func cmdSchedulerRunner(ctx context.Context, args []string) int {
	f := cli.ParseFlags(args)
	if bad := f.Unknown("stale-after"); len(bad) > 0 {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: unknown flag --%s\n", bad[0])
		return 3
	}
	stale, err := f.Duration("stale-after", runner.DefaultStaleAfter)
	if err != nil || stale <= 0 {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: invalid --stale-after\n")
		return 3
	}
	// Read-only probe: the database must already exist (a missing path is an
	// error, never a new empty file), the persistent journal mode is left as
	// it is, and the connection refuses writes.
	path, err := resolveDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: %v\n", err)
		return 2
	}
	d, err := db.OpenExisting(path, flagTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: %v\n", err)
		return 2
	}
	defer d.Close()
	if _, err := d.SqlDB().ExecContext(ctx, "PRAGMA query_only = ON"); err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: %v\n", err)
		return 2
	}
	if err := d.CheckSchemaVersion(); err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: %v\n", err)
		return 2
	}

	st, err := runner.Probe(ctx, scheduler.NewStore(d.SqlDB()), time.Now(), stale)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: scheduler runner: %v\n", err)
		return 2
	}
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"runner":          st.Liveness,
			"state":           st.State,
			"pid":             st.PID,
			"epoch":           st.Epoch,
			"version":         st.Version,
			"heartbeat_age_s": int64(st.HeartbeatAge / time.Second),
			"paused":          st.Paused,
			"in_flight":       st.InFlight,
			"queue":           st.Queue,
		})
		return 0
	}
	fmt.Println(st.Liveness)
	fmt.Printf("state: %s  pid: %d  epoch: %d  version: %s\n", st.State, st.PID, st.Epoch, st.Version)
	fmt.Printf("heartbeat age: %ds  paused: %v  in flight: %d\n", int64(st.HeartbeatAge/time.Second), st.Paused, st.InFlight)
	keys := make([]string, 0, len(st.Queue))
	for k := range st.Queue {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-10s %d\n", k, st.Queue[k])
	}
	return 0
}
