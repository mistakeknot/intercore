package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/observability"
	"github.com/mistakeknot/intercore/internal/sentinel"
	"github.com/mistakeknot/intercore/internal/state"
)

const version = "0.3.2"

var (
	flagDB      string
	flagTimeout time.Duration
	flagVerbose bool
	flagJSON    bool
	flagVV      bool // double-verbose for debug level
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
		case arg == "-vv":
			flagVerbose = true
			flagVV = true
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

	// Initialize structured logging
	logLevel := slog.LevelWarn
	if envLevel := os.Getenv("IC_LOG_LEVEL"); envLevel != "" {
		logLevel = observability.ParseLevel(envLevel)
	} else if flagVV {
		logLevel = slog.LevelDebug
	} else if flagVerbose {
		logLevel = slog.LevelInfo
	}
	slog.SetDefault(slog.New(observability.NewHandler(os.Stderr, logLevel)))

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
	case "events":
		exitCode = cmdEvents(ctx, subArgs)
	case "gate":
		exitCode = cmdGate(ctx, subArgs)
	case "lock":
		exitCode = cmdLock(ctx, subArgs)
	case "interspect":
		exitCode = cmdInterspect(ctx, subArgs)
	case "discovery":
		exitCode = cmdDiscovery(ctx, subArgs)
	case "portfolio":
		exitCode = cmdPortfolio(ctx, subArgs)
	case "lane":
		exitCode = cmdLane(ctx, subArgs)
	case "config":
		exitCode = cmdConfig(ctx, subArgs)
	case "agency":
		exitCode = cmdAgency(ctx, subArgs)
	case "compat":
		exitCode = cmdCompat(ctx, subArgs)
	case "route":
		exitCode = cmdRoute(ctx, subArgs)
	case "cost":
		exitCode = cmdCost(ctx, subArgs)
	case "scheduler":
		exitCode = cmdScheduler(ctx, subArgs)
	case "coordination":
		exitCode = cmdCoordination(ctx, subArgs)
	case "publish":
		exitCode = cmdPublish(ctx, subArgs)
	case "landed":
		exitCode = cmdLanded(ctx, subArgs)
	case "session":
		exitCode = cmdSession(ctx, subArgs)
	case "situation":
		exitCode = cmdSituation(ctx, subArgs)
	default:
		slog.Error("unknown command", "command", subcommand)
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
  dispatch spawn [opts]         Spawn an agent dispatch (--parent-dispatch=<id>)
  dispatch status <id>          Show dispatch status
  dispatch list [--active]      List dispatches
  dispatch poll <id>            Poll dispatch liveness
  dispatch wait <id> [--timeout=<dur>]  Wait for dispatch completion
  dispatch kill <id>            Kill a dispatch
  dispatch prune --older-than=<dur>  Prune old dispatches
  run create --project=<dir> --goal=<text> [opts]  Create a run
  run create --projects=<p1>,<p2> --goal=<text>    Create portfolio run
  run status <id>               Show run details
  run advance <id> [--priority=N]  Advance to next phase
  run phase <id>                Print current phase
  run current [--project=<dir>]  Print active run ID for project
  run list [--active] [--portfolio]  List runs (--portfolio = portfolio only)
  run events <id>               Show phase event audit trail
  run skip <id> <phase> --reason=<text>  Pre-skip a phase
  run rollback <id> --to-phase=<p> [--reason=<text>] [--dry-run]
  run rollback <id> --layer=code [--phase=<p>] [--format=json|text]
  run replay <id> [--mode=simulate|reexecute] [--allow-live] [--limit=N]
  run replay inputs <id> [--limit=N]   List recorded nondeterministic inputs
  run replay record <id> --kind=<kind> [--key=<k>] [--payload=<json>] [--artifact-ref=<ref>]
  run tokens <id>               Token aggregation across dispatches
  run budget <id>               Check budget thresholds (exit 1=exceeded)
  run cancel <id>               Cancel a run
  run set <id> [--complexity=N] [--auto-advance=bool] [--force-full=bool] [--max-dispatches=N]
  run create ... [--budget-enforce] [--max-agents=N]  Enable budget enforcement
  run agent add <run> --type=<t> [--name=<n>] [--dispatch-id=<id>]
  run agent list <run>          List agents for run
  run agent update <id> --status=<s>  Update agent status
  run artifact add <run> --phase=<p> --path=<f> [--type=<t>]
  run artifact list <run> [--phase=<p>]  List artifacts for run
  events tail <run_id|--all> [--follow] [--consumer=<name>]
  events tail ... [--since-phase=N] [--since-dispatch=N] [--limit=N]
  events record --source=<s> --type=<t> --payload=<json> [opts]  Record event
  events cursor list             List named cursors
  events cursor reset <name>     Reset a named cursor
  gate check <run_id> [--priority=N]   Dry-run gate evaluation (0=pass, 1=fail)
  gate override <run_id> --reason=<s>  Force advance past failing gate
  gate rules [--phase=<from>]          Show gate conditions
  lock acquire <name> <scope> [--timeout=<dur>] [--owner=<s>]  Acquire a lock
  lock release <name> <scope> [--owner=<s>]  Release a lock
  lock list                     List active locks
  lock stale [--older-than=<dur>]  List stale locks
  lock clean [--older-than=<dur>]  Remove stale locks
  interspect record --agent=<name> --type=<type> [opts]  Record evidence event
  interspect query [--agent=<name>] [--since=<id>]      Query evidence events
  discovery submit [opts]       Submit a discovery (--source, --source-id, --title required)
  discovery status <id>         Show discovery details
  discovery list [opts]         List discoveries (--source, --status, --tier, --limit)
  discovery score <id> --score=<0.0-1.0>  Score a discovery
  discovery promote <id> --bead-id=<bid> [--force]  Promote to bead
  discovery dismiss <id>        Dismiss a discovery
  discovery feedback <id> --signal=<type> [--data=@file] [--actor=<name>]
  discovery profile             Show interest profile
  discovery profile update --keyword-weights=<json> --source-weights=<json>
  discovery decay --rate=<0.0-1.0> [--min-age=<sec>]  Decay old scores
  discovery rollback --source=<s> --since=<ts>  Rollback source discoveries
  discovery search --embedding=@<file> [opts]  Semantic search
  portfolio dep add <id> --upstream=<p> --downstream=<p>  Add dependency
  portfolio dep list <id>       List dependencies for portfolio
  portfolio dep remove <id> --upstream=<p> --downstream=<p>  Remove dependency
  portfolio relay <id> [--interval=2s]  Run event relay for portfolio
  config set <key> <value>      Set a kernel config value
  config get <key>              Get a kernel config value
  config list [--verbose]       List kernel config values
  route model --phase=<p> --category=<c> --agent=<a>  Resolve a single model
  route batch --phase=<p> <agents...>  Resolve models for multiple agents
  route dispatch --tier=<name>  Resolve dispatch tier to model
  route table [--phase=<p>]     Show full routing table
  route record --agent=<a> --model=<m> --rule=<r>  Record a routing decision
  route list [--agent=<a>] [--model=<m>]  List routing decisions
  cost reconcile <run> --billed-in=N --billed-out=N [--dispatch=<id>] [--source=<s>]
  cost list <run> [--limit=N]   List past reconciliations
  publish <ver>                  Publish plugin (bump + push + sync)
  publish --patch                Auto-increment patch version
  publish doctor [--fix]         Detect/fix drift and health issues
  publish clean                  Prune orphaned cache dirs
  publish status [--all]         Show publish health
  publish init                   Register new plugin in marketplace
  session start --session=<id> --project=<dir> [opts]  Register a session
  session attribute --session=<id> [--bead=<id>] [opts]  Record attribution
  session end --session=<id>    End a session
  session current --session=<id> [--project=<dir>]  Show latest attribution
  session list [--project=<dir>] [--active-only]    List sessions
  situation snapshot [opts]      Unified observation layer (OODAR)
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
		slog.Error("init failed", "error", err)
		return 2
	}

	// Ensure parent directory exists with restricted permissions
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		slog.Error("init: cannot create directory", "value", dir, "error", err)
		return 2
	}

	d, err := db.Open(path, flagTimeout)
	if err != nil {
		slog.Error("init failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Migrate(ctx); err != nil {
		slog.Error("init: migration failed", "error", err)
		return 2
	}

	// Ensure DB file has restricted permissions (owner read/write only).
	// SQLite creates files with the process umask, which may be too permissive.
	if err := os.Chmod(path, 0600); err != nil {
		slog.Warn("init: could not set file permissions", "error", err)
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
		slog.Error("health failed", "error", err)
		return 2
	}
	defer d.Close()

	if err := d.Health(ctx); err != nil {
		slog.Error("health failed", "error", err)
		return 2
	}

	fmt.Println("ok")
	return 0
}

func cmdSentinel(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("sentinel: missing subcommand", "expected", "check, reset, list, prune")
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
		slog.Error("sentinel: unknown subcommand", "subcommand", args[0])
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
				slog.Error("sentinel check: invalid interval", "value", val)
				return 3
			}
		} else if args[i] == "--interval" && i+1 < len(args) {
			i++
			var err error
			intervalSec, err = strconv.Atoi(args[i])
			if err != nil {
				slog.Error("sentinel check: invalid interval", "value", args[i])
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
		slog.Error("sentinel check failed", "error", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB(), nil)
	allowed, err := store.Check(ctx, positional[0], positional[1], intervalSec)
	if err != nil {
		slog.Error("sentinel check failed", "error", err)
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
		slog.Error("sentinel reset failed", "error", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB(), nil)
	if err := store.Reset(ctx, args[0], args[1]); err != nil {
		slog.Error("sentinel reset failed", "error", err)
		return 2
	}

	fmt.Println("reset")
	return 0
}

func cmdSentinelList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("sentinel list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB(), nil)
	sentinels, err := store.List(ctx)
	if err != nil {
		slog.Error("sentinel list failed", "error", err)
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
		slog.Error("sentinel prune: invalid duration", "error", err)
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("sentinel prune failed", "error", err)
		return 2
	}
	defer d.Close()

	store := sentinel.New(d.SqlDB(), nil)
	count, err := store.Prune(ctx, dur)
	if err != nil {
		slog.Error("sentinel prune failed", "error", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdState(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("state: missing subcommand", "expected", "set, get, delete, list, prune")
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
		slog.Error("state: unknown subcommand", "subcommand", args[0])
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

	if err := state.ValidateKey(key); err != nil {
		slog.Error("state set failed", "error", err)
		return 2
	}

	var ttl time.Duration
	if ttlStr != "" {
		var err error
		ttl, err = time.ParseDuration(ttlStr)
		if err != nil {
			slog.Error("state set: invalid TTL", "error", err)
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
			slog.Error("state set: invalid file path", "error", err)
			return 2
		}
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("state set: cannot determine working directory", "error", err)
			return 2
		}
		if !strings.HasPrefix(absFile, cwd+string(filepath.Separator)) {
			slog.Error("state set: file path must be under current directory", "path", filePath)
			return 2
		}
		payload, err = os.ReadFile(absFile)
		if err != nil {
			slog.Error("state set: cannot read file", "value", filePath, "error", err)
			return 2
		}
	} else {
		payload, err = io.ReadAll(os.Stdin)
		if err != nil {
			slog.Error("state set: cannot read stdin", "error", err)
			return 2
		}
	}

	payload = []byte(strings.TrimSpace(string(payload)))

	if err := state.ValidatePayload(payload); err != nil {
		slog.Error("state set failed", "error", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		slog.Error("state set failed", "error", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	if err := store.Set(ctx, key, scopeID, json.RawMessage(payload), ttl); err != nil {
		slog.Error("state set failed", "error", err)
		return 2
	}

	return 0
}

func cmdStateGet(ctx context.Context, args []string) int {
	if len(args) < 2 {
		fmt.Fprintf(os.Stderr, "ic: state get: usage: ic state get <key> <scope_id>\n")
		return 3
	}

	if err := state.ValidateKey(args[0]); err != nil {
		slog.Error("state get failed", "error", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		slog.Error("state get failed", "error", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	payload, err := store.Get(ctx, args[0], args[1])
	if err != nil {
		if err == state.ErrNotFound {
			return 1
		}
		slog.Error("state get failed", "error", err)
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

	if err := state.ValidateKey(args[0]); err != nil {
		slog.Error("state delete failed", "error", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		slog.Error("state delete failed", "error", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	deleted, err := store.Delete(ctx, args[0], args[1])
	if err != nil {
		slog.Error("state delete failed", "error", err)
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

	if err := state.ValidateKey(args[0]); err != nil {
		slog.Error("state list failed", "error", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		slog.Error("state list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	ids, err := store.List(ctx, args[0])
	if err != nil {
		slog.Error("state list failed", "error", err)
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
		slog.Error("state prune failed", "error", err)
		return 2
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	count, err := store.Prune(ctx)
	if err != nil {
		slog.Error("state prune failed", "error", err)
		return 2
	}

	fmt.Printf("%d pruned\n", count)
	return 0
}

func cmdCompat(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("compat: missing subcommand", "expected", "status, check")
		return 3
	}

	switch args[0] {
	case "status":
		return cmdCompatStatus(ctx)
	case "check":
		return cmdCompatCheck(ctx, args[1:])
	default:
		slog.Error("compat: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

// legacyPatterns maps intercore keys to their legacy temp file glob patterns.
var legacyPatterns = map[string]string{
	"dispatch":          "/tmp/clavain-dispatch-*.json",
	"stop":              "/tmp/clavain-stop-*",
	"compound_throttle": "/tmp/clavain-compound-last-*",
	"drift_throttle":    "/tmp/clavain-drift-last-*",
	"handoff":           "/tmp/clavain-handoff-*",
	"autopub":           "/tmp/clavain-autopub*.lock",
	"catalog_remind":    "/tmp/clavain-catalog-remind-*.lock",
	"discovery_brief":   "/tmp/clavain-discovery-brief-*.cache",
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
		slog.Error("compat check failed", "error", err)
		return 1
	}
	defer d.Close()

	store := state.New(d.SqlDB())
	ids, err := store.List(ctx, args[0])
	if err != nil {
		slog.Error("compat check failed", "error", err)
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
