package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mistakeknot/intercore/internal/publish"
)

func cmdPublish(ctx context.Context, args []string) int {
	if len(args) == 0 {
		printPublishUsage()
		return 3
	}

	switch args[0] {
	case "doctor":
		return cmdPublishDoctor(ctx, args[1:])
	case "clean":
		return cmdPublishClean(ctx, args[1:])
	case "status":
		return cmdPublishStatus(ctx, args[1:])
	case "init":
		return cmdPublishInit(ctx, args[1:])
	case "help", "--help", "-h":
		printPublishUsage()
		return 0
	default:
		// Treat as version or flag: ic publish 0.3.0, ic publish --patch
		return cmdPublishRun(ctx, args)
	}
}

func cmdPublishRun(ctx context.Context, args []string) int {
	opts := publish.PublishOpts{}

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--patch":
			opts.Mode = publish.BumpPatch
		case args[i] == "--minor":
			opts.Mode = publish.BumpMinor
		case args[i] == "--dry-run":
			opts.DryRun = true
		case args[i] == "--auto":
			opts.Auto = true
		case strings.HasPrefix(args[i], "--cwd="):
			opts.CWD = strings.TrimPrefix(args[i], "--cwd=")
		case args[i] == "--cwd" && i+1 < len(args):
			i++
			opts.CWD = args[i]
		case strings.HasPrefix(args[i], "--"):
			slog.Error("publish: unknown flag", "value", args[i])
			return 3
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) > 0 {
		opts.Mode = publish.BumpExact
		opts.Version = positional[0]
	}

	// For auto mode, default to patch bump
	if opts.Auto && opts.Mode == publish.BumpExact && opts.Version == "" {
		opts.Mode = publish.BumpPatch
	}

	if opts.Mode == publish.BumpExact && opts.Version == "" {
		slog.Error("publish: specify a version (e.g., 0.3.0) or use --patch/--minor")
		return 3
	}

	// Open DB for state tracking (optional — publish works without it)
	var engine *publish.Engine
	d, err := openDB()
	if err == nil {
		defer d.Close()
		engine = publish.NewEngine(d.SqlDB(), opts)
	} else {
		engine = publish.NewEngine(nil, opts)
	}

	if err := engine.Publish(ctx); err != nil {
		if errors.Is(err, publish.ErrVersionMatch) {
			if !opts.Auto {
				slog.Error("publish failed", "error", err)
			}
			return 0 // not an error
		}
		slog.Error("publish failed", "error", err)
		return 2
	}
	return 0
}

func cmdPublishDoctor(ctx context.Context, args []string) int {
	opts := publish.DoctorOpts{}
	// --json is a global flag (consumed by main.go), so check flagJSON too
	opts.JSON = flagJSON
	for _, arg := range args {
		switch arg {
		case "--fix":
			opts.Fix = true
		case "--json":
			opts.JSON = true
		}
	}

	result, err := publish.RunDoctor(ctx, opts)
	if err != nil {
		slog.Error("publish doctor failed", "error", err)
		return 2
	}

	if opts.JSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.Encode(result.Findings)
		return 0
	}

	if len(result.Findings) == 0 {
		fmt.Printf("All %d plugins healthy.\n", len(result.PluginDirs))
		return 0
	}

	errors := 0
	warnings := 0
	for _, f := range result.Findings {
		icon := "  "
		switch f.Severity {
		case "error":
			icon = "✗ "
			errors++
		case "warning":
			icon = "⚠ "
			warnings++
		case "info":
			icon = "✓ "
		}

		plugin := ""
		if f.Plugin != "" {
			plugin = f.Plugin + ": "
		}
		fmt.Printf("%s%s%s\n", icon, plugin, f.Message)
		if f.Fix != "" && !opts.Fix {
			fmt.Printf("    fix: %s\n", f.Fix)
		}
	}

	fmt.Printf("\n%d error(s), %d warning(s) across %d plugins\n", errors, warnings, len(result.PluginDirs))
	if errors > 0 && !opts.Fix {
		fmt.Println("Run 'ic publish doctor --fix' to auto-repair.")
	}

	if errors > 0 {
		return 1
	}
	return 0
}

func cmdPublishClean(ctx context.Context, args []string) int {
	dryRun := false
	for _, arg := range args {
		if arg == "--dry-run" {
			dryRun = true
		}
	}

	if dryRun {
		// Just scan and report
		entries, err := publish.ListCacheEntries()
		if err != nil {
			slog.Error("publish clean failed", "error", err)
			return 2
		}

		orphaned := 0
		stale := 0
		for pluginName, versions := range entries {
			installedVer := publish.ReadInstalledVersion(pluginName)
			for _, v := range versions {
				if v.Orphaned {
					orphaned++
				} else if !v.IsSymlink && v.Version != installedVer {
					stale++
				}
			}
		}

		fmt.Printf("Would clean:\n")
		fmt.Printf("  %d orphaned cache directories\n", orphaned)
		fmt.Printf("  %d stale version directories\n", stale)
		fmt.Printf("  .git directories in cache entries\n")
		return 0
	}

	totalCount := 0

	count, bytes, err := publish.CleanOrphans()
	if err != nil {
		slog.Error("publish clean: orphans failed", "error", err)
	}
	if count > 0 {
		fmt.Printf("Cleaned %d orphaned directories (%.1f MB freed)\n", count, float64(bytes)/1024/1024)
		totalCount += count
	}

	count, bytes, err = publish.StripGitDirs()
	if err != nil {
		slog.Error("publish clean: .git failed", "error", err)
	}
	if count > 0 {
		fmt.Printf("Stripped %d .git directories (%.1f MB freed)\n", count, float64(bytes)/1024/1024)
		totalCount += count
	}

	count, bytes, err = publish.PruneStaleVersions(1)
	if err != nil {
		slog.Error("publish clean: stale versions failed", "error", err)
	}
	if count > 0 {
		fmt.Printf("Pruned %d stale version directories (%.1f MB freed)\n", count, float64(bytes)/1024/1024)
		totalCount += count
	}

	if totalCount == 0 {
		fmt.Println("Cache is clean.")
	}
	return 0
}

func cmdPublishStatus(ctx context.Context, args []string) int {
	showAll := false
	for _, arg := range args {
		if arg == "--all" {
			showAll = true
		}
	}

	if showAll {
		return cmdPublishStatusAll(ctx)
	}

	// Current plugin
	cwd, _ := os.Getwd()
	root, err := publish.FindPluginRoot(cwd)
	if err != nil {
		slog.Error("publish status failed", "error", err)
		return 2
	}

	plugin, err := publish.ReadPlugin(root)
	if err != nil {
		slog.Error("publish status failed", "error", err)
		return 2
	}

	marketRoot, err := publish.FindMarketplace(root)
	if err != nil {
		slog.Error("publish status failed", "error", err)
		return 2
	}

	mktVer, _ := publish.ReadMarketplaceVersion(marketRoot, plugin.Name)
	instVer := publish.ReadInstalledVersion(plugin.Name)

	fmt.Printf("Plugin:       %s\n", plugin.Name)
	fmt.Printf("plugin.json:  %s\n", plugin.Version)
	fmt.Printf("marketplace:  %s\n", mktVer)
	fmt.Printf("installed:    %s\n", instVer)

	derived := publish.DiscoverVersionFiles(root)
	if len(derived) > 0 {
		fmt.Println("Derived files:")
		for _, d := range derived {
			v, err := publish.ReadVersionFromFile(d)
			if err != nil {
				fmt.Printf("  ✗ %s: %v\n", d.Path, err)
			} else {
				icon := "✓"
				if v != plugin.Version {
					icon = "✗"
				}
				fmt.Printf("  %s %s: %s\n", icon, d.Path, v)
			}
		}
	}

	return 0
}

func cmdPublishStatusAll(ctx context.Context) int {
	cwd, _ := os.Getwd()
	marketRoot, err := publish.FindMarketplace(cwd)
	if err != nil {
		slog.Error("publish status failed", "error", err)
		return 2
	}

	mktVersions, err := publish.ListMarketplacePlugins(marketRoot)
	if err != nil {
		slog.Error("publish status failed", "error", err)
		return 2
	}

	fmt.Printf("%-20s %-10s %-10s %-10s %s\n", "PLUGIN", "PLUGIN.JSON", "MARKET", "INSTALLED", "STATUS")
	for name, mktVer := range mktVersions {
		instVer := publish.ReadInstalledVersion(name)
		status := "✓"
		if instVer != mktVer {
			status = "drift"
		}
		fmt.Printf("%-20s %-10s %-10s %-10s %s\n", name, "-", mktVer, instVer, status)
	}

	return 0
}

func cmdPublishInit(ctx context.Context, args []string) int {
	var name string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--name=") {
			name = strings.TrimPrefix(args[i], "--name=")
		} else if args[i] == "--name" && i+1 < len(args) {
			i++
			name = args[i]
		}
	}

	cwd, _ := os.Getwd()
	root, err := publish.FindPluginRoot(cwd)
	if err != nil {
		slog.Error("publish init failed", "error", err)
		return 2
	}

	plugin, err := publish.ReadPlugin(root)
	if err != nil {
		slog.Error("publish init failed", "error", err)
		return 2
	}

	if name != "" {
		plugin.Name = name
	}

	marketRoot, err := publish.FindMarketplace(root)
	if err != nil {
		slog.Error("publish init failed", "error", err)
		return 2
	}

	if err := publish.RegisterPlugin(marketRoot, plugin, root); err != nil {
		slog.Error("publish init failed", "error", err)
		return 2
	}

	fmt.Printf("Registered %s v%s in marketplace.\n", plugin.Name, plugin.Version)

	// Phase 2: Commit and push marketplace
	fmt.Println("  Committing marketplace...")
	marketplaceJSON := filepath.Join(".claude-plugin", "marketplace.json")
	if err := publish.GitAdd(marketRoot, marketplaceJSON); err != nil {
		slog.Warn("publish init: marketplace commit failed", "error", err)
	} else {
		msg := fmt.Sprintf("feat: add %s to marketplace (v%s)", plugin.Name, plugin.Version)
		if err := publish.GitCommit(marketRoot, msg); err != nil {
			slog.Warn("publish init: marketplace commit failed", "error", err)
		} else {
			fmt.Println("  Pushing marketplace...")
			if err := publish.GitPush(marketRoot); err != nil {
				slog.Warn("publish init: marketplace push failed", "error", err)
			}
		}
	}

	// Phase 3: Rebuild cache
	fmt.Println("  Rebuilding cache...")
	if err := publish.RebuildCache(plugin.Name, plugin.Version, root); err != nil {
		slog.Warn("publish init: cache rebuild failed", "error", err)
	}

	// Phase 4: Update installed_plugins.json (with git SHA for Claude Code plugin resolution)
	cachePath := filepath.Join(publish.CacheBase(), plugin.Name, plugin.Version)
	gitSha, _ := publish.GitHeadCommit(root)
	if err := publish.UpdateInstalled(plugin.Name, plugin.Version, cachePath, gitSha); err != nil {
		slog.Warn("publish init: installed_plugins.json update failed", "error", err)
	}

	// Phase 5: Enable in settings.json
	fmt.Println("  Enabling in settings...")
	if err := publish.EnableInSettings(plugin.Name); err != nil {
		slog.Warn("publish init: settings.json update failed", "error", err)
	}

	// Phase 6: Sync CC marketplace checkout
	if err := publish.SyncCCMarketplace(marketRoot, plugin.Name, plugin.Version); err != nil {
		slog.Warn("publish init: CC marketplace sync failed", "error", err)
	}

	// Phase 6b: Refresh CC's in-memory marketplace index
	if err := publish.RefreshCCMarketplace(); err != nil {
		slog.Warn("publish init: CC marketplace refresh failed", "error", err)
	}

	// Phase 7: Create hook symlinks if applicable
	for _, hookPath := range []string{
		filepath.Join(root, ".claude-plugin", "hooks", "hooks.json"),
		filepath.Join(root, "hooks", "hooks.json"),
	} {
		if _, err := os.Stat(hookPath); err == nil {
			publish.CreateSymlinks(plugin.Name, "", plugin.Version)
			break
		}
	}

	fmt.Printf("Done. %s v%s is registered, cached, and enabled.\n", plugin.Name, plugin.Version)
	fmt.Println("Restart your Claude Code session to load the plugin.")
	return 0
}

func printPublishUsage() {
	fmt.Println(`ic publish — plugin publishing pipeline

Usage:
  ic publish <version>           Bump to exact version and publish
  ic publish --patch             Auto-increment patch version
  ic publish --minor             Auto-increment minor version
  ic publish --auto [--cwd=<d>]  Auto mode (for hooks): patch bump, no prompts
  ic publish --dry-run           Show what would happen

  ic publish doctor              Detect all drift and health issues
  ic publish doctor --fix        Auto-repair everything
  ic publish doctor --json       Machine-readable output

  ic publish clean               Prune orphans, stale versions, strip .git
  ic publish clean --dry-run     Show what would be cleaned

  ic publish status              Show publish state for current plugin
  ic publish status --all        Show all plugins' publish health

  ic publish init                Register new plugin in marketplace
  ic publish init --name=<name>  Register with explicit name`)
}
