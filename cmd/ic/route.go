package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

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
  table   [--phase=<p>]                             Show full routing table
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
	var tier string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--tier=") {
			tier = strings.TrimPrefix(arg, "--tier=")
		} else if !strings.HasPrefix(arg, "--") && tier == "" {
			tier = arg
		}
	}

	if tier == "" {
		fmt.Fprintf(os.Stderr, "ic route dispatch: provide --tier=<name> or tier name as arg\n")
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
