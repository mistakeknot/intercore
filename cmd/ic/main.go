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
	"github.com/mistakeknot/interverse/infra/intercore/internal/dispatch"
	"github.com/mistakeknot/interverse/infra/intercore/internal/phase"
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
  run list [--active]           List runs
  run events <id>               Show phase event audit trail
  run cancel <id>               Cancel a run
  run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]
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

// --- Dispatch Commands ---

func cmdDispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: dispatch: missing subcommand (spawn, status, list, poll, wait, kill, prune)\n")
		return 3
	}

	switch args[0] {
	case "spawn":
		return cmdDispatchSpawn(ctx, args[1:])
	case "status":
		return cmdDispatchStatus(ctx, args[1:])
	case "list":
		return cmdDispatchList(ctx, args[1:])
	case "poll":
		return cmdDispatchPoll(ctx, args[1:])
	case "wait":
		return cmdDispatchWait(ctx, args[1:])
	case "kill":
		return cmdDispatchKill(ctx, args[1:])
	case "prune":
		return cmdDispatchPrune(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: dispatch: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdDispatchSpawn(ctx context.Context, args []string) int {
	opts := dispatch.SpawnOptions{}
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--type="):
			opts.AgentType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--prompt-file="):
			opts.PromptFile = strings.TrimPrefix(args[i], "--prompt-file=")
		case strings.HasPrefix(args[i], "--project="):
			opts.ProjectDir = strings.TrimPrefix(args[i], "--project=")
		case strings.HasPrefix(args[i], "--output="):
			opts.OutputFile = strings.TrimPrefix(args[i], "--output=")
		case strings.HasPrefix(args[i], "--name="):
			opts.Name = strings.TrimPrefix(args[i], "--name=")
		case strings.HasPrefix(args[i], "--model="):
			opts.Model = strings.TrimPrefix(args[i], "--model=")
		case strings.HasPrefix(args[i], "--sandbox="):
			opts.Sandbox = strings.TrimPrefix(args[i], "--sandbox=")
		case strings.HasPrefix(args[i], "--timeout="):
			val := strings.TrimPrefix(args[i], "--timeout=")
			dur, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: dispatch spawn: invalid timeout: %s\n", val)
				return 3
			}
			opts.TimeoutSec = int(dur.Seconds())
		case strings.HasPrefix(args[i], "--scope-id="):
			opts.ScopeID = strings.TrimPrefix(args[i], "--scope-id=")
		case strings.HasPrefix(args[i], "--parent-id="):
			opts.ParentID = strings.TrimPrefix(args[i], "--parent-id=")
		case strings.HasPrefix(args[i], "--dispatch-sh="):
			opts.DispatchSH = strings.TrimPrefix(args[i], "--dispatch-sh=")
		default:
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if opts.PromptFile == "" {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: --prompt-file is required\n")
		return 3
	}
	if opts.ProjectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch spawn: cannot determine project dir: %v\n", err)
			return 2
		}
		opts.ProjectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	result, err := dispatch.Spawn(ctx, store, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch spawn: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":  result.ID,
			"pid": result.PID,
		})
	} else {
		fmt.Println(result.ID)
	}
	return 0
}

func cmdDispatchStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch status: usage: ic dispatch status <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch status: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	disp, err := store.Get(ctx, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch status: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch status: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}
	return 0
}

func cmdDispatchList(ctx context.Context, args []string) int {
	var activeOnly bool
	var scopeFilter *string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--active":
			activeOnly = true
		case strings.HasPrefix(args[i], "--scope="):
			s := strings.TrimPrefix(args[i], "--scope=")
			scopeFilter = &s
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	var dispatches []*dispatch.Dispatch

	if activeOnly {
		dispatches, err = store.ListActive(ctx)
	} else {
		dispatches, err = store.List(ctx, scopeFilter)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch list: %v\n", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(dispatches))
		for i, disp := range dispatches {
			items[i] = dispatchToMap(disp)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, disp := range dispatches {
			name := ""
			if disp.Name != nil {
				name = *disp.Name
			}
			fmt.Printf("%s\t%s\t%s\t%s\n", disp.ID, disp.Status, disp.AgentType, name)
		}
	}
	return 0
}

func cmdDispatchPoll(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: usage: ic dispatch poll <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	disp, err := dispatch.Poll(ctx, store, args[0])
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch poll: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch poll: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}
	return 0
}

func cmdDispatchWait(ctx context.Context, args []string) int {
	var id string
	var timeoutStr string
	var pollStr string

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--timeout="):
			timeoutStr = strings.TrimPrefix(args[i], "--timeout=")
		case strings.HasPrefix(args[i], "--poll="):
			pollStr = strings.TrimPrefix(args[i], "--poll=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: usage: ic dispatch wait <id> [--timeout=<dur>] [--poll=<dur>]\n")
		return 3
	}
	id = positional[0]

	var timeout time.Duration
	if timeoutStr != "" {
		var err error
		timeout, err = time.ParseDuration(timeoutStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: invalid timeout: %s\n", timeoutStr)
			return 3
		}
	}

	var pollInterval time.Duration
	if pollStr != "" {
		var err error
		pollInterval, err = time.ParseDuration(pollStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: invalid poll interval: %s\n", pollStr)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	disp, err := dispatch.Wait(ctx, store, id, pollInterval, timeout)
	if err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch wait: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch wait: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(dispatchToMap(disp))
	} else {
		printDispatch(disp)
	}

	if disp.Status == dispatch.StatusFailed || disp.Status == dispatch.StatusTimeout {
		return 1
	}
	return 0
}

func cmdDispatchKill(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: usage: ic dispatch kill <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	if err := dispatch.Kill(ctx, store, args[0]); err != nil {
		if err == dispatch.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: dispatch kill: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: dispatch kill: %v\n", err)
		return 2
	}

	fmt.Println("killed")
	return 0
}

func cmdDispatchPrune(ctx context.Context, args []string) int {
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
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: usage: ic dispatch prune --older-than=<duration>\n")
		return 3
	}

	dur, err := time.ParseDuration(olderThan)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: invalid duration: %v\n", err)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: %v\n", err)
		return 2
	}
	defer d.Close()

	store := dispatch.New(d.SqlDB())
	count, err := store.Prune(ctx, dur)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: dispatch prune: %v\n", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

// --- dispatch output helpers ---

func dispatchToMap(d *dispatch.Dispatch) map[string]interface{} {
	m := map[string]interface{}{
		"id":          d.ID,
		"agent_type":  d.AgentType,
		"status":      d.Status,
		"project_dir": d.ProjectDir,
		"turns":       d.Turns,
		"commands":    d.Commands,
		"messages":    d.Messages,
		"in_tokens":   d.InputTokens,
		"out_tokens":  d.OutputTokens,
		"created_at":  d.CreatedAt,
	}
	if d.PromptFile != nil {
		m["prompt_file"] = *d.PromptFile
	}
	if d.OutputFile != nil {
		m["output_file"] = *d.OutputFile
	}
	if d.PID != nil {
		m["pid"] = *d.PID
	}
	if d.ExitCode != nil {
		m["exit_code"] = *d.ExitCode
	}
	if d.Name != nil {
		m["name"] = *d.Name
	}
	if d.Model != nil {
		m["model"] = *d.Model
	}
	if d.StartedAt != nil {
		m["started_at"] = *d.StartedAt
	}
	if d.CompletedAt != nil {
		m["completed_at"] = *d.CompletedAt
	}
	if d.VerdictStatus != nil {
		m["verdict_status"] = *d.VerdictStatus
	}
	if d.VerdictSummary != nil {
		m["verdict_summary"] = *d.VerdictSummary
	}
	if d.ErrorMessage != nil {
		m["error_message"] = *d.ErrorMessage
	}
	if d.ScopeID != nil {
		m["scope_id"] = *d.ScopeID
	}
	if d.ParentID != nil {
		m["parent_id"] = *d.ParentID
	}
	return m
}

func printDispatch(d *dispatch.Dispatch) {
	fmt.Printf("ID:      %s\n", d.ID)
	fmt.Printf("Status:  %s\n", d.Status)
	fmt.Printf("Type:    %s\n", d.AgentType)
	if d.Name != nil {
		fmt.Printf("Name:    %s\n", *d.Name)
	}
	if d.PID != nil {
		fmt.Printf("PID:     %d\n", *d.PID)
	}
	fmt.Printf("Project: %s\n", d.ProjectDir)
	if d.PromptFile != nil {
		fmt.Printf("Prompt:  %s\n", *d.PromptFile)
	}
	if d.OutputFile != nil {
		fmt.Printf("Output:  %s\n", *d.OutputFile)
	}
	if d.Turns > 0 || d.Commands > 0 || d.Messages > 0 {
		fmt.Printf("Stats:   %d turns, %d commands, %d messages\n", d.Turns, d.Commands, d.Messages)
	}
	if d.InputTokens > 0 || d.OutputTokens > 0 {
		fmt.Printf("Tokens:  %d in / %d out\n", d.InputTokens, d.OutputTokens)
	}
	if d.VerdictStatus != nil {
		fmt.Printf("Verdict: %s\n", *d.VerdictStatus)
	}
	if d.VerdictSummary != nil {
		fmt.Printf("Summary: %s\n", *d.VerdictSummary)
	}
	if d.ExitCode != nil {
		fmt.Printf("Exit:    %d\n", *d.ExitCode)
	}
	if d.ErrorMessage != nil {
		fmt.Printf("Error:   %s\n", *d.ErrorMessage)
	}
}

// --- Run Commands ---

func cmdRun(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: run: missing subcommand (create, status, advance, phase, list, events, cancel, set)\n")
		return 3
	}

	switch args[0] {
	case "create":
		return cmdRunCreate(ctx, args[1:])
	case "status":
		return cmdRunStatus(ctx, args[1:])
	case "advance":
		return cmdRunAdvance(ctx, args[1:])
	case "phase":
		return cmdRunPhase(ctx, args[1:])
	case "list":
		return cmdRunList(ctx, args[1:])
	case "events":
		return cmdRunEvents(ctx, args[1:])
	case "cancel":
		return cmdRunCancel(ctx, args[1:])
	case "set":
		return cmdRunSet(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: run: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdRunCreate(ctx context.Context, args []string) int {
	var project, goal, scopeID string
	complexity := 3

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--project="):
			project = strings.TrimPrefix(args[i], "--project=")
		case strings.HasPrefix(args[i], "--goal="):
			goal = strings.TrimPrefix(args[i], "--goal=")
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				fmt.Fprintf(os.Stderr, "ic: run create: invalid complexity (1-5): %s\n", val)
				return 3
			}
			complexity = c
		case strings.HasPrefix(args[i], "--scope-id="):
			scopeID = strings.TrimPrefix(args[i], "--scope-id=")
		default:
			fmt.Fprintf(os.Stderr, "ic: run create: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if project == "" {
		cwd, err := os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: run create: cannot determine project dir: %v\n", err)
			return 2
		}
		project = cwd
	}

	if goal == "" {
		fmt.Fprintf(os.Stderr, "ic: run create: --goal is required\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run create: %v\n", err)
		return 2
	}
	defer d.Close()

	run := &phase.Run{
		ProjectDir:  project,
		Goal:        goal,
		Complexity:  complexity,
		AutoAdvance: true,
	}
	if scopeID != "" {
		run.ScopeID = &scopeID
	}

	store := phase.New(d.SqlDB())
	id, err := store.Create(ctx, run)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run create: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":    id,
			"phase": phase.PhaseBrainstorm,
		})
	} else {
		fmt.Println(id)
	}
	return 0
}

func cmdRunStatus(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run status: usage: ic run status <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run status: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run status: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run status: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(runToMap(run))
	} else {
		printRun(run)
	}
	return 0
}

func cmdRunAdvance(ctx context.Context, args []string) int {
	var id string
	priority := 4
	disableGates := false
	skipReason := ""

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--priority="):
			val := strings.TrimPrefix(args[i], "--priority=")
			p, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run advance: invalid priority: %s\n", val)
				return 3
			}
			priority = p
		case args[i] == "--disable-gates":
			disableGates = true
		case strings.HasPrefix(args[i], "--skip-reason="):
			skipReason = strings.TrimPrefix(args[i], "--skip-reason=")
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run advance: usage: ic run advance <id> [--priority=N] [--disable-gates] [--skip-reason=S]\n")
		return 3
	}
	id = positional[0]

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	result, err := phase.Advance(ctx, store, id, phase.GateConfig{
		Priority:   priority,
		DisableAll: disableGates,
		SkipReason: skipReason,
	})
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run advance: not found: %s\n", id)
			return 1
		}
		if err == phase.ErrTerminalRun || err == phase.ErrTerminalPhase {
			fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run advance: %v\n", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"from_phase":  result.FromPhase,
			"to_phase":    result.ToPhase,
			"event_type":  result.EventType,
			"gate_result": result.GateResult,
			"gate_tier":   result.GateTier,
			"advanced":    result.Advanced,
			"reason":      result.Reason,
		})
	} else {
		if result.Advanced {
			fmt.Printf("%s → %s\n", result.FromPhase, result.ToPhase)
		} else {
			fmt.Printf("%s (blocked: %s)\n", result.FromPhase, result.EventType)
		}
	}

	if result.Advanced {
		return 0
	}
	return 1
}

func cmdRunPhase(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run phase: usage: ic run phase <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run phase: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run phase: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run phase: %v\n", err)
		return 2
	}

	fmt.Println(run.Phase)
	return 0
}

func cmdRunList(ctx context.Context, args []string) int {
	activeOnly := false
	var scopeFilter *string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--active":
			activeOnly = true
		case strings.HasPrefix(args[i], "--scope="):
			s := strings.TrimPrefix(args[i], "--scope=")
			scopeFilter = &s
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	var runs []*phase.Run

	if activeOnly {
		runs, err = store.ListActive(ctx)
	} else {
		runs, err = store.List(ctx, scopeFilter)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run list: %v\n", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(runs))
		for i, r := range runs {
			items[i] = runToMap(r)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, r := range runs {
			fmt.Printf("%s\t%s\t%s\t%s\n", r.ID, r.Status, r.Phase, r.Goal)
		}
	}
	return 0
}

func cmdRunEvents(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run events: usage: ic run events <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run events: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())
	events, err := store.Events(ctx, args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run events: %v\n", err)
		return 2
	}

	if flagJSON {
		items := make([]map[string]interface{}, len(events))
		for i, e := range events {
			items[i] = eventToMap(e)
		}
		json.NewEncoder(os.Stdout).Encode(items)
	} else {
		for _, e := range events {
			gate := ""
			if e.GateResult != nil {
				gate = *e.GateResult
			}
			reason := ""
			if e.Reason != nil {
				reason = *e.Reason
			}
			fmt.Printf("%s → %s\t%s\t%s\t%s\n", e.FromPhase, e.ToPhase, e.EventType, gate, reason)
		}
	}
	return 0
}

func cmdRunCancel(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run cancel: usage: ic run cancel <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, args[0])
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run cancel: not found: %s\n", args[0])
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
		return 2
	}

	if phase.IsTerminalStatus(run.Status) {
		fmt.Fprintf(os.Stderr, "ic: run cancel: run already %s\n", run.Status)
		return 1
	}

	if err := store.UpdateStatus(ctx, args[0], phase.StatusCancelled); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run cancel: %v\n", err)
		return 2
	}

	// Record cancel event
	store.AddEvent(ctx, &phase.PhaseEvent{
		RunID:     args[0],
		FromPhase: run.Phase,
		ToPhase:   run.Phase,
		EventType: phase.EventCancel,
	})

	fmt.Println("cancelled")
	return 0
}

func cmdRunSet(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: run set: usage: ic run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool]\n")
		return 3
	}

	id := args[0]
	var complexity *int
	var autoAdvance *bool
	var forceFull *bool

	for i := 1; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--complexity="):
			val := strings.TrimPrefix(args[i], "--complexity=")
			c, err := strconv.Atoi(val)
			if err != nil || c < 1 || c > 5 {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid complexity (1-5): %s\n", val)
				return 3
			}
			complexity = &c
		case strings.HasPrefix(args[i], "--auto-advance="):
			val := strings.TrimPrefix(args[i], "--auto-advance=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid bool: %s\n", val)
				return 3
			}
			autoAdvance = &b
		case strings.HasPrefix(args[i], "--force-full="):
			val := strings.TrimPrefix(args[i], "--force-full=")
			b, err := strconv.ParseBool(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: run set: invalid bool: %s\n", val)
				return 3
			}
			forceFull = &b
		default:
			fmt.Fprintf(os.Stderr, "ic: run set: unknown flag: %s\n", args[i])
			return 3
		}
	}

	if complexity == nil && autoAdvance == nil && forceFull == nil {
		fmt.Fprintf(os.Stderr, "ic: run set: no settings to update\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
	}
	defer d.Close()

	store := phase.New(d.SqlDB())

	// Get current state for the event record
	run, err := store.Get(ctx, id)
	if err != nil {
		if err == phase.ErrNotFound {
			fmt.Fprintf(os.Stderr, "ic: run set: not found: %s\n", id)
			return 1
		}
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
	}

	if err := store.UpdateSettings(ctx, id, complexity, autoAdvance, forceFull); err != nil {
		fmt.Fprintf(os.Stderr, "ic: run set: %v\n", err)
		return 2
	}

	// Record set event
	store.AddEvent(ctx, &phase.PhaseEvent{
		RunID:     id,
		FromPhase: run.Phase,
		ToPhase:   run.Phase,
		EventType: phase.EventSet,
	})

	fmt.Println("updated")
	return 0
}

// --- run output helpers ---

func runToMap(r *phase.Run) map[string]interface{} {
	m := map[string]interface{}{
		"id":           r.ID,
		"project_dir":  r.ProjectDir,
		"goal":         r.Goal,
		"status":       r.Status,
		"phase":        r.Phase,
		"complexity":   r.Complexity,
		"force_full":   r.ForceFull,
		"auto_advance": r.AutoAdvance,
		"created_at":   r.CreatedAt,
		"updated_at":   r.UpdatedAt,
	}
	if r.CompletedAt != nil {
		m["completed_at"] = *r.CompletedAt
	}
	if r.ScopeID != nil {
		m["scope_id"] = *r.ScopeID
	}
	if r.Metadata != nil {
		m["metadata"] = *r.Metadata
	}
	return m
}

func eventToMap(e *phase.PhaseEvent) map[string]interface{} {
	m := map[string]interface{}{
		"id":         e.ID,
		"run_id":     e.RunID,
		"from_phase": e.FromPhase,
		"to_phase":   e.ToPhase,
		"event_type": e.EventType,
		"created_at": e.CreatedAt,
	}
	if e.GateResult != nil {
		m["gate_result"] = *e.GateResult
	}
	if e.GateTier != nil {
		m["gate_tier"] = *e.GateTier
	}
	if e.Reason != nil {
		m["reason"] = *e.Reason
	}
	return m
}

func printRun(r *phase.Run) {
	fmt.Printf("ID:         %s\n", r.ID)
	fmt.Printf("Status:     %s\n", r.Status)
	fmt.Printf("Phase:      %s\n", r.Phase)
	fmt.Printf("Goal:       %s\n", r.Goal)
	fmt.Printf("Project:    %s\n", r.ProjectDir)
	fmt.Printf("Complexity: %d\n", r.Complexity)
	if r.ForceFull {
		fmt.Printf("ForceFull:  true\n")
	}
	if !r.AutoAdvance {
		fmt.Printf("AutoAdv:    false\n")
	}
	if r.ScopeID != nil {
		fmt.Printf("Scope:      %s\n", *r.ScopeID)
	}
}
