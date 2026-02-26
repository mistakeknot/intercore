package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
			fmt.Fprintf(os.Stderr, "ic: publish: unknown flag: %s\n", args[i])
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
		fmt.Fprintf(os.Stderr, "ic: publish: specify a version (e.g., 0.3.0) or use --patch/--minor\n")
		return 3
	}

	// Open DB for state tracking (optional — publish works without it)
	var db *publish.Store
	d, err := openDB()
	if err == nil {
		defer d.Close()
		engine := publish.NewEngine(d.SqlDB(), opts)
		if err := engine.Publish(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "ic: publish: %v\n", err)
			return 2
		}
		return 0
	}

	// No DB — run without state tracking
	_ = db
	engine := publish.NewEngine(nil, opts)
	if err := engine.Publish(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish: %v\n", err)
		if err == publish.ErrVersionMatch {
			return 0 // not an error
		}
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
		fmt.Fprintf(os.Stderr, "ic: publish doctor: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "ic: publish clean: %v\n", err)
			return 2
		}

		orphaned := 0
		gitDirs := 0
		for _, versions := range entries {
			for _, v := range versions {
				if v.Orphaned {
					orphaned++
				}
			}
		}

		// Count .git dirs
		base := publish.CacheBase()
		if base != "" {
			fmt.Printf("Would clean:\n")
			fmt.Printf("  %d orphaned cache directories\n", orphaned)
			fmt.Printf("  .git directories (scanning...)\n")
		}
		_ = gitDirs
		return 0
	}

	count, bytes, err := publish.CleanOrphans()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish clean: orphans: %v\n", err)
	}
	if count > 0 {
		fmt.Printf("Cleaned %d orphaned directories (%.1f MB freed)\n", count, float64(bytes)/1024/1024)
	}

	count, bytes, err = publish.StripGitDirs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish clean: .git: %v\n", err)
	}
	if count > 0 {
		fmt.Printf("Stripped %d .git directories (%.1f MB freed)\n", count, float64(bytes)/1024/1024)
	}

	if count == 0 {
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
		fmt.Fprintf(os.Stderr, "ic: publish status: %v\n", err)
		return 2
	}

	plugin, err := publish.ReadPlugin(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish status: %v\n", err)
		return 2
	}

	marketRoot, err := publish.FindMarketplace(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish status: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: publish status: %v\n", err)
		return 2
	}

	mktVersions, err := publish.ListMarketplacePlugins(marketRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish status: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: publish init: %v\n", err)
		return 2
	}

	plugin, err := publish.ReadPlugin(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish init: %v\n", err)
		return 2
	}

	if name != "" {
		plugin.Name = name
	}

	marketRoot, err := publish.FindMarketplace(root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish init: %v\n", err)
		return 2
	}

	if err := publish.RegisterPlugin(marketRoot, plugin); err != nil {
		fmt.Fprintf(os.Stderr, "ic: publish init: %v\n", err)
		return 2
	}

	fmt.Printf("Registered %s v%s in marketplace.\n", plugin.Name, plugin.Version)
	fmt.Println("Next: commit and push the marketplace changes, then run 'ic publish --patch'")
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

  ic publish clean               Prune orphaned cache dirs, strip .git
  ic publish clean --dry-run     Show what would be cleaned

  ic publish status              Show publish state for current plugin
  ic publish status --all        Show all plugins' publish health

  ic publish init                Register new plugin in marketplace
  ic publish init --name=<name>  Register with explicit name`)
}
