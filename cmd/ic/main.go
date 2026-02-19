package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/db"
	"github.com/mistakeknot/interverse/infra/intercore/internal/sentinel"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

const version = "0.2.0"

var (
	flagDB      string
	flagTimeout time.Duration
	flagVerbose bool
	flagJSON    bool
)

func main() {
	// Parse all args manually to support global flags before or after subcommand.
	// Go's flag package stops at the first non-flag arg, so "ic init --db=x" misses --db.
	var subcommand string
	var subArgs []string

	for i := 1; i < len(os.Args); i++ {
		arg := os.Args[i]
		switch {
		case strings.HasPrefix(arg, "--db="):
			flagDB = strings.TrimPrefix(arg, "--db=")
		case arg == "--db" && i+1 < len(os.Args):
			i++
			flagDB = os.Args[i]
		case strings.HasPrefix(arg, "--timeout="):
			val := strings.TrimPrefix(arg, "--timeout=")
			d, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: invalid timeout: %s\n", val)
				os.Exit(3)
			}
			flagTimeout = d
		case arg == "--verbose":
			flagVerbose = true
		case arg == "--json":
			flagJSON = true
		default:
			if subcommand == "" {
				subcommand = arg
			} else {
				subArgs = append(subArgs, arg)
			}
		}
	}

	if subcommand == "" {
		printUsage()
		os.Exit(0)
	}

	if flagTimeout == 0 {
		flagTimeout = 100 * time.Millisecond
	}

	ctx := context.Background()
	var exitCode int

	switch subcommand {
	case "init":
		exitCode = cmdInit(ctx)
	case "version":
		exitCode = cmdVersion(ctx)
	case "health":
		exitCode = cmdHealth(ctx)
	case "sentinel":
		exitCode = cmdSentinel(ctx, subArgs)
	case "state":
		exitCode = cmdState(ctx, subArgs)
	case "dispatch":
		exitCode = cmdDispatch(ctx, subArgs)
	case "run":
		exitCode = cmdRun(ctx, subArgs)
	case "gate":
		exitCode = cmdGate(ctx, subArgs)
	case "lock":
		exitCode = cmdLock(ctx, subArgs)
	case "compat":
		exitCode = cmdCompat(ctx, subArgs)
	default:
		fmt.Fprintf(os.Stderr, "ic: unknown command: %s\n", subcommand)
		printUsage()
		exitCode = 3
	}

	os.Exit(exitCode)
}

func printUsage() {
	fmt.Println(`ic — intercore state database CLI

Usage: ic [flags] <command> [args]

Commands:
  init                          Initialize the database
  version                       Print version info
  health                        Check database health
  sentinel check <n> <s> --interval=<sec>  Check/claim a sentinel
  sentinel reset <n> <s>        Reset a sentinel
  sentinel list                 List all sentinels
  sentinel prune --older-than=<dur>  Prune old sentinels
  state set <k> <s> [--ttl=<dur>]   Set state (reads JSON from stdin)
  state get <k> <s>             Get state
  state delete <k> <s>          Delete state
  state list <k>                List scope_ids for a key
  state prune                   Prune expired state
  dispatch spawn [opts]         Spawn an agent dispatch
  dispatch status <id>          Show dispatch status
  dispatch list [--active]      List dispatches
  dispatch poll <id>            Poll dispatch liveness
  dispatch wait <id> [--timeout=<dur>]  Wait for dispatch completion
  dispatch kill <id>            Kill a dispatch
  dispatch prune --older-than=<dur>  Prune old dispatches
  run create --project=<dir> --goal=<text> [opts]  Create a run
  run status <id>               Show run details
  run advance <id> [--priority=N]  Advance to next phase
  run phase <id>                Print current phase
  run current [--project=<dir>]  Print active run ID for project
  run list [--active]           List runs
  run events <id>               Show phase event audit trail
  run cancel <id>               Cancel a run
  run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]
  run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
  run agent list <run>          List agents for run
  run agent update <id> --status=<s>  Update agent status
  run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
  run artifact list <run> [--phase=<p>]  List artifacts for run
  gate check <run_id> [--priority=N]   Dry-run gate evaluation (0=pass, 1=fail)
  gate override <run_id> --reason=<s>  Force advance past failing gate
  gate rules [--phase=<from>]          Show gate conditions
  lock acquire <name> <scope> [--timeout=<dur>] [--owner=<s>]  Acquire a lock
  lock release <name> <scope> [--owner=<s>]  Release a lock
  lock list                     List active locks
  lock stale [--older-than=<dur>]  List stale locks
  lock clean [--older-than=<dur>]  Remove stale locks
  compat status                 Show migration status
  compat check <key>            Check if key has data in DB

Flags:
  --db=<path>       Database path (default: .clavain/intercore.db)
  --timeout=<dur>   SQLite busy timeout (default: 100ms)
  --verbose         Verbose output
  --json            JSON output`)
}

// resolveDBPath finds the database path.
func resolveDBPath() (string, error) {
	if flagDB != "" {
		if err := validateDBPath(flagDB); err != nil {
			return "", err
		}
		return flagDB, nil
	}

	// Walk up from CWD looking for .clavain/intercore.db
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot determine working directory: %w", err)
	}

	for {
		candidate := filepath.Join(dir, ".clavain", "intercore.db")
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Default: create under CWD
	return filepath.Join(".clavain", "intercore.db"), nil
}

// validateDBPath prevents path traversal attacks.
func validateDBPath(path string) error {
	cleaned := filepath.Clean(path)
	if filepath.Ext(cleaned) != ".db" {
		return fmt.Errorf("ic: db path must have .db extension: %s", path)
	}
	if strings.Contains(cleaned, "..") {
		return fmt.Errorf("ic: db path must not contain '..': %s", path)
	}
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return fmt.Errorf("ic: db path: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("ic: cannot determine working directory: %w", err)
	}
	if !strings.HasPrefix(abs, cwd+string(filepath.Separator)) && abs != cwd {
		return fmt.Errorf("ic: db path must be under current directory: %s resolves to %s", path, abs)
	}
	return nil
}

func openDB() (*db.DB, error) {
	path, err := resolveDBPath()
	if err != nil {
		return nil, err
	}
	return db.Open(path, flagTimeout)
}

// --- Commands ---

func cmdInit(ctx context.Context) int {
	path, err := resolveDBPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: init: %v\n", err)
		return 2
	}

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "ic: init: cannot create directory %s: %v\n", dir, err)
		return 2
	}

	d, err := db.Open(path, flagTimeout)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: init: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: init: migration failed: %v\n", err)
		return 2
	}

	v, _ := d.SchemaVersion()
	fmt.Printf("initialized %s (schema v%d)\n", path, v)
	return 0
}

func cmdVersion(ctx context.Context) int {
	fmt.Printf("ic %s\n", version)

	d, err := openDB()
	if err != nil {
		fmt.Printf("schema: unknown (no database)\n")
		return 0
	}
	defer d.Close()

	v, err := d.SchemaVersion()
	if err != nil {
		fmt.Printf("schema: unknown\n")
	} else {
		fmt.Printf("schema: v%d\n", v)
	}
	return 0
}

func cmdHealth(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: health: %v\n", err)
		return 2
	}
	defer d.Close()

	if err := d.Health(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: health: %v\n", err)
		return 2
	}

	fmt.Println("ok")
	return 0
}

func cmdSentinel(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: sentinel: missing subcommand (check, reset, list, prune)\n")
		return 3
	}

	switch args[0] {
	case "check":
		return cmdSentinelCheck(ctx, args[1:])
	case "reset":
		return cmdSentinelReset(ctx, args[1:])
	case "list":
		return cmdSentinelList(ctx)
	case "prune":
		return cmdSentinelPrune(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: sentinel: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdSentinelCheck(ctx context.Context, args []string) int {
	// Parse mixed positional + flag args: <name> <scope_id> --interval=<sec>
	intervalSec := -1
	var positional []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--interval=") {
			val := strings.TrimPrefix(args[i], "--interval=")
			var err error
			intervalSec, err = strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: sentinel check: invalid interval: %s\n", val)
				return 3
			}
		} else if args[i] == "--interval" && i+1 < len(args) {
			i++
			var err error
			intervalSec, err = strconv.Atoi(args[i])
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: sentinel check: invalid interval: %s\n", args[i])
				return 3
			}
		} else {
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 || intervalSec < 0 {
		fmt.Fprintf(os.Stderr, "ic: sentinel check: usage: ic sentinel check <name> <scope_id> --interval=<seconds>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel check: %v\n", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB())
	allowed, err := store.Check(ctx, positional[0], positional[1], intervalSec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel check: %v\n", err)
		return 2
	}

	if allowed {
		fmt.Println("allowed")
		return 0
	}
	fmt.Println("throttled")
	return 1
}

func cmdSentinelReset(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "ic: sentinel reset: usage: ic sentinel reset <name> <scope_id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel reset: %v\n", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB())
	if err := store.Reset(ctx, args[0], args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel reset: %v\n", err)
		return 2
	}

	fmt.Println("reset")
	return 0
}

func cmdSentinelList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB())
	sentinels, err := store.List(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel list: %v\n", err)
		return 2
	}

	for _, s := range sentinels {
		fmt.Printf("%s\t%s\t%d\n", s.Name, s.ScopeID, s.LastFired)
	}
	return 0
}

func cmdSentinelPrune(ctx context.Context, args []string) int {
	var olderThan string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--older-than=") {
			olderThan = strings.TrimPrefix(args[i], "--older-than=")
		} else if args[i] == "--older-than" && i+1 < len(args) {
			i++
			olderThan = args[i]
		}
	}
	if olderThan == "" {
		fmt.Fprintf(os.Stderr, "ic: sentinel prune: usage: ic sentinel prune --older-than=<duration>\n")
		return 3
	}

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel prune: invalid duration: %v\n", err)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel prune: %v\n", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB())
	count, err := store.Prune(ctx, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: sentinel prune: %v\n", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdState(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: state: missing subcommand (set, get, delete, list, prune)\n")
		return 3
	}

	switch args[0] {
	case "set":
		return cmdStateSet(ctx, args[1:])
	case "get":
		return cmdStateGet(ctx, args[1:])
	case "delete":
		return cmdStateDelete(ctx, args[1:])
	case "list":
		return cmdStateList(ctx, args[1:])
	case "prune":
		return cmdStatePrune(ctx)
	default:
		fmt.Fprintf(os.Stderr, "ic: state: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdStateSet(ctx context.Context, args []string) int {
	// Parse mixed positional + flag args: <key> <scope_id> [--ttl=<dur>] [@filepath]
	var ttlStr string
	var positional []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--ttl=") {
			ttlStr = strings.TrimPrefix(args[i], "--ttl=")
		} else if args[i] == "--ttl" && i+1 < len(args) {
			i++
			ttlStr = args[i]
		} else {
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 2 {
		fmt.Fprintf(os.Stderr, "ic: state set: usage: ic state set <key> <scope_id> [--ttl=<duration>] [@filepath]\n")
		return 3
	}

	key := positional[0]
	scopeID := positional[1]

	var ttl time.Duration
	if ttlStr != "" {
		var err error
		ttl, err = time.ParseDuration(ttlStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: state set: invalid TTL: %v\n", err)
			return 3
		}
	}

	// Read payload from file or stdin
	var payload []byte
	var err error
	if len(positional) >= 3 && strings.HasPrefix(positional[2], "@") {
		filePath := positional[2][1:]
		// Validate file path is under CWD (same as --db validation)
		absFile, err := filepath.Abs(filePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: state set: invalid file path: %v\n", err)
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: state set: cannot determine working directory: %v\n", err)
			return 2
		}
		if !strings.HasPrefix(absFile, cwd+string(filepath.Separator)) {
			fmt.Fprintf(os.Stderr, "ic: state set: file path must be under current directory: %s\n", filePath)
			return 2
		}
		payload, err = os.ReadFile(absFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: state set: cannot read file %s: %v\n", filePath, err)
			return 2
		}
	} else {
		payload, err = io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: state set: cannot read stdin: %v\n", err)
			return 2
		}
	}

	payload = []byte(strings.TrimSpace(string(payload)))

	if err := state.ValidatePayload(payload); err != nil {
		fmt.Fprintf(os.Stderr, "ic: state set: %v\n", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state set: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	if err := store.Set(ctx, key, scopeID, json.RawMessage(payload), ttl); err != nil {
		fmt.Fprintf(os.Stderr, "ic: state set: %v\n", err)
		return 2
	}

	return 0
}

func cmdStateGet(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "ic: state get: usage: ic state get <key> <scope_id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state get: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	payload, err := store.Get(ctx, args[0], args[1])
	if err != nil {
		if err == state.ErrNotFound {
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: state get: %v\n", err)
		return 2
	}

	fmt.Println(string(payload))
	return 0
}

func cmdStateDelete(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "ic: state delete: usage: ic state delete <key> <scope_id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state delete: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	deleted, err := store.Delete(ctx, args[0], args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state delete: %v\n", err)
		return 2
	}

	if deleted {
		fmt.Println("deleted")
	} else {
		fmt.Println("not found")
	}
	return 0
}

func cmdStateList(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: state list: usage: ic state list <key>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	ids, err := store.List(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state list: %v\n", err)
		return 2
	}

	for _, id := range ids {
		fmt.Println(id)
	}
	return 0
}

func cmdStatePrune(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state prune: %v\n", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	count, err := store.Prune(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: state prune: %v\n", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdCompat(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: compat: missing subcommand (status, check)\n")
		return 3
	}

	switch args[0] {
	case "status":
		return cmdCompatStatus(ctx)
	case "check":
		return cmdCompatCheck(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: compat: unknown subcommand: %s\n", args[0])
		return 3
	}
}

// legacyPatterns maps intercore keys to their legacy temp file glob patterns.
var legacyPatterns = map[string]string{
	"dispatch":         "/tmp/clavain-dispatch-*.json",
	"stop":             "/tmp/clavain-stop-*",
	"compound_throttle": "/tmp/clavain-compound-last-*",
	"drift_throttle":   "/tmp/clavain-drift-last-*",
	"handoff":          "/tmp/clavain-handoff-*",
	"autopub":          "/tmp/clavain-autopub*.lock",
	"catalog_remind":   "/tmp/clavain-catalog-remind-*.lock",
	"discovery_brief":  "/tmp/clavain-discovery-brief-*.cache",
}

func cmdCompatStatus(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		// DB not available — show legacy-only status
		fmt.Println("KEY\t\t\tLEGACY\tDB")
		for key, pattern := range legacyPatterns {
			matches, _ := filepath.Glob(pattern)
			legacyExists := len(matches) > 0
			fmt.Printf("%-20s\t%s\t%s\n", key, boolStr(legacyExists), "n/a")
		}
		return 0
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	fmt.Println("KEY\t\t\tLEGACY\tDB")
	for key, pattern := range legacyPatterns {
		matches, _ := filepath.Glob(pattern)
		legacyExists := len(matches) > 0
		ids, _ := store.List(ctx, key)
		dbExists := len(ids) > 0
		fmt.Printf("%-20s\t%s\t%s\n", key, boolStr(legacyExists), boolStr(dbExists))
	}
	return 0
}

func cmdCompatCheck(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: compat check: usage: ic compat check <key>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: compat check: %v\n", err)
		return 1
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	ids, err := store.List(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: compat check: %v\n", err)
		return 2
	}

	if len(ids) > 0 {
		fmt.Printf("%s: %s scope(s) in DB\n", args[0], strconv.Itoa(len(ids)))
		return 0
	}
	fmt.Printf("%s: not in DB\n", args[0])
	return 1
}

func boolStr(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}
