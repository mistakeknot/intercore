package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/routing"
)

// --- Route Commands ---

func cmdRoute(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, `ic route — cost-aware model routing

Usage: ic route <subcommand> [args]

Subcommands:
  model   --phase=<p> --category=<c> --agent=<a>   Resolve a single model
  batch   --phase=<p> <agent1> <agent2> ...         Resolve models for multiple agents
  dispatch --tier=<name>                            Resolve a dispatch tier to model
  dispatch --type=<name> [--phase=<p>]             Resolve subagent type to model
  table   [--phase=<p>]                             Show full routing table
  record  --agent=<a> --model=<m> --rule=<r> ...    Record a routing decision
  list    [--agent=<a>] [--model=<m>] [--limit=N]   List routing decisions
`)
		return 3
	}

	switch args[0] {
	case "model":
		return cmdRouteModel(ctx, args[1:])
	case "batch":
		return cmdRouteBatch(ctx, args[1:])
	case "dispatch":
		return cmdRouteDispatch(ctx, args[1:])
	case "table":
		return cmdRouteTable(ctx, args[1:])
	case "record":
		return cmdRouteRecord(ctx, args[1:])
	case "list":
		return cmdRouteList(ctx, args[1:])
	default:
		slog.Error("route: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

// findConfigFile searches for a config file by walking up from CWD.
func findConfigFile(name string) string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}

	for {
		// Check common config locations relative to project root
		candidates := []string{
			filepath.Join(dir, "os", "clavain", "config", name),
			filepath.Join(dir, "config", name),
		}
		for _, c := range candidates {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func loadRoutingConfig() (*routing.Config, error) {
	routingPath := findConfigFile("routing.yaml")
	if routingPath == "" {
		return nil, fmt.Errorf("routing.yaml not found (searched up from CWD)")
	}

	// Look for agent-roles.yaml near the routing.yaml (sibling or known paths)
	rolesPath := ""
	routingDir := filepath.Dir(routingPath)
	// Walk up from routingDir to find project root, then check known locations
	candidates := []string{
		filepath.Join(routingDir, "agent-roles.yaml"),
	}
	// From os/clavain/config/ → project root is ../../../
	// From config/ → project root is ../
	for _, rel := range []string{
		filepath.Join(routingDir, "..", "..", "..", "interverse", "interflux", "config", "flux-drive", "agent-roles.yaml"),
		filepath.Join(routingDir, "..", "interverse", "interflux", "config", "flux-drive", "agent-roles.yaml"),
	} {
		candidates = append(candidates, rel)
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			rolesPath = c
			break
		}
	}

	return routing.LoadConfig(routingPath, rolesPath)
}

func cmdRouteModel(ctx context.Context, args []string) int {
	var phase, category, agent string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--phase="):
			phase = strings.TrimPrefix(arg, "--phase=")
		case strings.HasPrefix(arg, "--category="):
			category = strings.TrimPrefix(arg, "--category=")
		case strings.HasPrefix(arg, "--agent="):
			agent = strings.TrimPrefix(arg, "--agent=")
		}
	}

	cfg, err := loadRoutingConfig()
	if err != nil {
		slog.Error("route model", "error", err)
		return 2
	}

	r := routing.NewResolver(cfg)
	model := r.ResolveModel(routing.ResolveOpts{
		Phase:    phase,
		Category: category,
		Agent:    agent,
	})

	if flagJSON {
		out := map[string]string{
			"model":    model,
			"phase":    phase,
			"category": category,
			"agent":    agent,
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	} else {
		fmt.Println(model)
	}
	return 0
}

func cmdRouteBatch(ctx context.Context, args []string) int {
	var phase string
	var agents []string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--phase=") {
			phase = strings.TrimPrefix(arg, "--phase=")
		} else if !strings.HasPrefix(arg, "--") {
			agents = append(agents, arg)
		}
	}

	if len(agents) == 0 {
		fmt.Fprintf(os.Stderr, "ic route batch: provide agent names as positional args\n")
		return 3
	}

	cfg, err := loadRoutingConfig()
	if err != nil {
		slog.Error("route batch", "error", err)
		return 2
	}

	r := routing.NewResolver(cfg)
	result := r.ResolveBatch(agents, phase)

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result)
	} else {
		for _, agent := range agents {
			fmt.Printf("%s\t%s\n", agent, result[agent])
		}
	}
	return 0
}

func cmdRouteDispatch(ctx context.Context, args []string) int {
	var tier, subagentType, currentPhase string
	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--tier="):
			tier = strings.TrimPrefix(args[i], "--tier=")
		case strings.HasPrefix(args[i], "--type="):
			subagentType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--phase="):
			currentPhase = strings.TrimPrefix(args[i], "--phase=")
		default:
			if !strings.HasPrefix(args[i], "--") && tier == "" && subagentType == "" {
				tier = args[i]
			}
		}
	}

	// When --type is provided, resolve subagent type to model via dispatch tiers
	if subagentType != "" {
		cfg, err := loadRoutingConfig()
		if err != nil {
			slog.Error("route dispatch", "error", err)
			return 2
		}
		r := routing.NewResolver(cfg)

		// Try type:phase first, then type alone
		var model string
		if currentPhase != "" {
			model = r.ResolveDispatchTier(subagentType + ":" + currentPhase)
		}
		if model == "" {
			model = r.ResolveDispatchTier(subagentType)
		}

		if flagJSON {
			out := map[string]string{"type": subagentType, "phase": currentPhase, "model": model}
			enc := json.NewEncoder(os.Stdout)
			enc.Encode(out)
		} else {
			if model == "" {
				fmt.Fprintf(os.Stderr, "type %q: no dispatch tier found\n", subagentType)
				return 1
			}
			fmt.Println(model)
		}
		return 0
	}

	// Original --tier path (backward compat)
	if tier == "" {
		fmt.Fprintf(os.Stderr, "ic route dispatch: requires --tier=<name> or --type=<name>\n")
		return 3
	}

	cfg, err := loadRoutingConfig()
	if err != nil {
		slog.Error("route dispatch", "error", err)
		return 2
	}

	r := routing.NewResolver(cfg)
	model := r.ResolveDispatchTier(tier)

	if model == "" {
		fmt.Fprintf(os.Stderr, "tier %q: not found (checked fallbacks)\n", tier)
		return 1
	}

	if flagJSON {
		out := map[string]string{"tier": tier, "model": model}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(out)
	} else {
		fmt.Println(model)
	}
	return 0
}

func cmdRouteTable(ctx context.Context, args []string) int {
	var phase string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--phase=") {
			phase = strings.TrimPrefix(arg, "--phase=")
		}
	}

	cfg, err := loadRoutingConfig()
	if err != nil {
		slog.Error("route table", "error", err)
		return 2
	}

	r := routing.NewResolver(cfg)

	if flagJSON {
		table := buildRouteTable(r, cfg, phase)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(table)
	} else {
		printRouteTable(r, cfg, phase)
	}
	return 0
}

type routeTableEntry struct {
	Agent    string `json:"agent"`
	Category string `json:"category"`
	Model    string `json:"model"`
	Floor    string `json:"floor,omitempty"`
}

func buildRouteTable(r *routing.Resolver, cfg *routing.Config, phase string) []routeTableEntry {
	var entries []routeTableEntry

	// Collect all known agents from overrides and roles
	agents := map[string]bool{}
	for a := range cfg.Subagents.Overrides {
		agents[a] = true
	}
	for _, role := range cfg.Roles.Roles {
		for _, a := range role.Agents {
			agents[a] = true
		}
	}

	floors := cfg.SafetyFloors()

	for agent := range agents {
		category := routing.InferCategoryExported(agent)
		model := r.ResolveModel(routing.ResolveOpts{
			Phase:    phase,
			Category: category,
			Agent:    agent,
		})
		entry := routeTableEntry{
			Agent:    agent,
			Category: category,
			Model:    model,
		}
		// Check if floor was applied (try full name, then stripped short name)
		if f, ok := floors[agent]; ok {
			entry.Floor = f
		} else if strings.Contains(agent, ":") {
			parts := strings.Split(agent, ":")
			short := parts[len(parts)-1]
			if f, ok := floors[short]; ok {
				entry.Floor = f
			}
		}
		entries = append(entries, entry)
	}

	return entries
}

func printRouteTable(r *routing.Resolver, cfg *routing.Config, phase string) {
	entries := buildRouteTable(r, cfg, phase)

	if phase != "" {
		fmt.Printf("Phase: %s\n", phase)
	}
	fmt.Printf("%-40s %-12s %-8s %s\n", "AGENT", "CATEGORY", "MODEL", "FLOOR")
	fmt.Println(strings.Repeat("-", 72))

	for _, e := range entries {
		floor := e.Floor
		if floor == "" {
			floor = "-"
		}
		fmt.Printf("%-40s %-12s %-8s %s\n", e.Agent, e.Category, e.Model, floor)
	}
}

func cmdRouteRecord(ctx context.Context, args []string) int {
	var opts routing.RecordDecisionOpts
	var floorApplied bool
	var complexity string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--dispatch="):
			opts.DispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--run="):
			opts.RunID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--bead="):
			opts.BeadID = strings.TrimPrefix(arg, "--bead=")
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--phase="):
			opts.Phase = strings.TrimPrefix(arg, "--phase=")
		case strings.HasPrefix(arg, "--agent="):
			opts.Agent = strings.TrimPrefix(arg, "--agent=")
		case strings.HasPrefix(arg, "--category="):
			opts.Category = strings.TrimPrefix(arg, "--category=")
		case strings.HasPrefix(arg, "--model="):
			opts.SelectedModel = strings.TrimPrefix(arg, "--model=")
		case strings.HasPrefix(arg, "--rule="):
			opts.RuleMatched = strings.TrimPrefix(arg, "--rule=")
		case arg == "--floor-applied":
			floorApplied = true
		case strings.HasPrefix(arg, "--floor-from="):
			opts.FloorFrom = strings.TrimPrefix(arg, "--floor-from=")
		case strings.HasPrefix(arg, "--floor-to="):
			opts.FloorTo = strings.TrimPrefix(arg, "--floor-to=")
		case strings.HasPrefix(arg, "--candidates="):
			opts.Candidates = strings.TrimPrefix(arg, "--candidates=")
		case strings.HasPrefix(arg, "--excluded="):
			opts.Excluded = strings.TrimPrefix(arg, "--excluded=")
		case strings.HasPrefix(arg, "--policy-hash="):
			opts.PolicyHash = strings.TrimPrefix(arg, "--policy-hash=")
		case strings.HasPrefix(arg, "--override-id="):
			opts.OverrideID = strings.TrimPrefix(arg, "--override-id=")
		case strings.HasPrefix(arg, "--complexity="):
			complexity = strings.TrimPrefix(arg, "--complexity=")
		case strings.HasPrefix(arg, "--context="):
			opts.ContextJSON = strings.TrimPrefix(arg, "--context=")
		}
	}

	opts.FloorApplied = floorApplied
	if complexity != "" {
		if v, err := strconv.Atoi(complexity); err == nil {
			opts.Complexity = v
		}
	}

	if opts.Agent == "" || opts.SelectedModel == "" || opts.RuleMatched == "" {
		slog.Error("route record: --agent, --model, and --rule are required")
		return 3
	}
	if opts.ProjectDir == "" {
		if wd, err := os.Getwd(); err == nil {
			opts.ProjectDir = wd
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("route record failed", "error", err)
		return 2
	}
	defer d.Close()

	store := routing.NewDecisionStore(d.SqlDB())
	id, err := store.Record(ctx, opts)
	if err != nil {
		slog.Error("route record failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":    id,
			"agent": opts.Agent,
			"model": opts.SelectedModel,
			"rule":  opts.RuleMatched,
		})
	} else {
		fmt.Printf("Routing decision recorded: id=%d agent=%s model=%s rule=%s\n",
			id, opts.Agent, opts.SelectedModel, opts.RuleMatched)
	}
	return 0
}

func cmdRouteList(ctx context.Context, args []string) int {
	var opts routing.ListDecisionOpts
	var limit string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--agent="):
			opts.Agent = strings.TrimPrefix(arg, "--agent=")
		case strings.HasPrefix(arg, "--model="):
			opts.Model = strings.TrimPrefix(arg, "--model=")
		case strings.HasPrefix(arg, "--dispatch="):
			opts.DispatchID = strings.TrimPrefix(arg, "--dispatch=")
		case strings.HasPrefix(arg, "--since="):
			if v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--since="), 10, 64); err == nil {
				opts.Since = v
			}
		case strings.HasPrefix(arg, "--until="):
			if v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--until="), 10, 64); err == nil {
				opts.Until = v
			}
		case strings.HasPrefix(arg, "--limit="):
			limit = strings.TrimPrefix(arg, "--limit=")
		}
	}

	if limit != "" {
		if v, err := strconv.Atoi(limit); err == nil {
			opts.Limit = v
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("route list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := routing.NewDecisionStore(d.SqlDB())
	decisions, err := store.List(ctx, opts)
	if err != nil {
		slog.Error("route list failed", "error", err)
		return 2
	}

	if flagJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(decisions)
	} else {
		if len(decisions) == 0 {
			fmt.Println("No routing decisions found.")
			return 0
		}
		fmt.Printf("%-6s %-35s %-8s %-18s %s\n", "ID", "AGENT", "MODEL", "RULE", "DECIDED")
		fmt.Println(strings.Repeat("-", 80))
		for _, dec := range decisions {
			fmt.Printf("%-6d %-35s %-8s %-18s %s\n",
				dec.ID, dec.Agent, dec.SelectedModel, dec.RuleMatched,
				time.Unix(dec.DecidedAt, 0).Format("2006-01-02 15:04"))
		}
	}
	return 0
}
