package publish

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Engine orchestrates the publish pipeline.
type Engine struct {
	store *Store
	opts  PublishOpts
	out   func(string, ...interface{}) // output function
}

// NewEngine creates a publish engine. Pass nil for db to run without state tracking.
func NewEngine(db *sql.DB, opts PublishOpts) *Engine {
	e := &Engine{
		opts: opts,
		out: func(format string, args ...interface{}) {
			fmt.Fprintf(os.Stdout, format, args...)
		},
	}
	if db != nil {
		e.store = NewStore(db)
	}
	return e
}

// SetOutput overrides the output function (for testing or piping).
func (e *Engine) SetOutput(fn func(string, ...interface{})) {
	e.out = fn
}

// Publish runs the full publish pipeline.
func (e *Engine) Publish(ctx context.Context) error {
	// Phase 1: Discovery
	cwd := e.opts.CWD
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return fmt.Errorf("getwd: %w", err)
		}
	}

	pluginRoot, err := FindPluginRoot(cwd)
	if err != nil {
		return err
	}

	plugin, err := ReadPlugin(pluginRoot)
	if err != nil {
		return err
	}

	marketRoot, err := FindMarketplace(pluginRoot)
	if err != nil {
		return err
	}

	// In auto mode, check if the developer already bumped (marketplace is stale).
	// If plugin.json version > marketplace version, skip bump and just sync.
	syncOnly := false
	if e.opts.Auto {
		mktVer, _ := ReadMarketplaceVersion(marketRoot, plugin.Name)
		if mktVer != "" && mktVer != plugin.Version && CompareVersions(plugin.Version, mktVer) > 0 {
			// Developer already bumped — just sync marketplace + local
			syncOnly = true
		}
	}

	// Determine target version
	var targetVersion string
	if syncOnly {
		targetVersion = plugin.Version
	} else {
		targetVersion, err = e.resolveTargetVersion(plugin.Version)
		if err != nil {
			return err
		}

		if targetVersion == plugin.Version {
			return ErrVersionMatch
		}
	}

	derived := DiscoverVersionFiles(pluginRoot)

	if syncOnly {
		e.out("Syncing %s v%s to marketplace\n", plugin.Name, targetVersion)
	} else {
		e.out("Publishing %s: %s → %s\n", plugin.Name, plugin.Version, targetVersion)
	}
	if e.opts.DryRun {
		e.out("  Plugin root:  %s\n", pluginRoot)
		e.out("  Marketplace:  %s\n", marketRoot)
		e.out("  Version files: plugin.json")
		for _, d := range derived {
			rel, _ := filepath.Rel(pluginRoot, d.Path)
			e.out(", %s", rel)
		}
		e.out("\n")
		e.out("Dry run — no changes made.\n")
		return nil
	}

	// Check for active publish (resume scenario)
	if e.store != nil {
		if err := e.store.EnsureTable(ctx); err != nil {
			// Non-fatal — continue without state tracking
			e.out("warning: cannot create publish_state table: %v\n", err)
			e.store = nil
		}
	}

	if e.store != nil {
		active, err := e.store.GetActive(ctx, plugin.Name)
		if err != nil {
			return fmt.Errorf("check active: %w", err)
		}
		if active != nil && !e.opts.Auto {
			return fmt.Errorf("%w: %s at phase %s (id: %s) — use 'ic publish status' to inspect, or re-run to force",
				ErrActivePublish, active.PluginName, active.Phase, active.ID)
		}
		if active != nil && e.opts.Auto {
			// Auto mode: delete stale state and proceed
			e.store.Delete(ctx, active.ID)
		}
	}

	// Create publish state
	var stateID string
	if e.store != nil {
		st := &PublishState{
			PluginName:  plugin.Name,
			FromVersion: plugin.Version,
			ToVersion:   targetVersion,
			Phase:       PhaseDiscovery,
			PluginRoot:  pluginRoot,
			MarketRoot:  marketRoot,
		}
		if err := e.store.Create(ctx, st); err != nil {
			e.out("warning: cannot track publish state: %v\n", err)
		} else {
			stateID = st.ID
		}
	}

	// Helper to update state
	setPhase := func(phase Phase) {
		if e.store != nil && stateID != "" {
			e.store.Update(ctx, stateID, phase, "")
		}
	}
	setError := func(phase Phase, err error) {
		if e.store != nil && stateID != "" {
			e.store.Update(ctx, stateID, phase, err.Error())
		}
	}

	if !syncOnly {
		// Phase 2: Validation
		setPhase(PhaseValidation)
		e.out("  Validating...\n")

		clean, err := GitStatus(pluginRoot)
		if err != nil {
			setError(PhaseValidation, err)
			return fmt.Errorf("check plugin worktree: %w", err)
		}
		if !clean {
			err := ErrDirtyWorktree
			setError(PhaseValidation, err)
			return fmt.Errorf("plugin repo: %w", err)
		}

		clean, err = GitStatus(marketRoot)
		if err != nil {
			setError(PhaseValidation, err)
			return fmt.Errorf("check marketplace worktree: %w", err)
		}
		if !clean {
			err := fmt.Errorf("marketplace repo: %w", ErrDirtyWorktree)
			setError(PhaseValidation, err)
			return err
		}

		if err := GitRemoteReachable(pluginRoot); err != nil {
			setError(PhaseValidation, err)
			return err
		}
		if err := GitRemoteReachable(marketRoot); err != nil {
			setError(PhaseValidation, err)
			return err
		}

		// Run structural plugin validator (if available in the monorepo)
		if validatorErr := RunPluginValidator(pluginRoot); validatorErr != nil {
			setError(PhaseValidation, validatorErr)
			return fmt.Errorf("plugin validation: %w", validatorErr)
		}

		// Run post-bump hook if present (legacy: runs before bump despite the name).
		// Collect any files the hook modifies so they get staged in the commit phase.
		var hookDirtyFiles []string
		postBump := filepath.Join(pluginRoot, "scripts", "post-bump.sh")
		if _, err := os.Stat(postBump); err == nil {
			e.out("  Running post-bump hook...\n")
			if err := runHook(postBump, targetVersion); err != nil {
				setError(PhaseValidation, err)
				return fmt.Errorf("post-bump hook: %w", err)
			}
			hookDirtyFiles, _ = GitDirtyFiles(pluginRoot)
		}

		// Phase 3: Bump
		setPhase(PhaseBump)
		e.out("  Bumping versions...\n")

		if err := WritePluginVersion(plugin.PluginJSON, targetVersion); err != nil {
			setError(PhaseBump, err)
			return fmt.Errorf("write plugin.json: %w", err)
		}

		if err := WriteDerivedVersions(derived, targetVersion); err != nil {
			setError(PhaseBump, err)
			return fmt.Errorf("write derived versions: %w", err)
		}

		// Verify
		mismatches := VerifyVersions(plugin.PluginJSON, derived, targetVersion)
		if len(mismatches) > 0 {
			err := fmt.Errorf("version verification failed:\n  %s", strings.Join(mismatches, "\n  "))
			setError(PhaseBump, err)
			return err
		}

		// Phase 4: Commit plugin
		setPhase(PhaseCommitPlugin)
		e.out("  Committing plugin...\n")

		filesToAdd := []string{filepath.Join(".claude-plugin", "plugin.json")}
		for _, d := range derived {
			rel, err := filepath.Rel(pluginRoot, d.Path)
			if err != nil {
				continue
			}
			filesToAdd = append(filesToAdd, rel)
		}
		// Include files modified by the post-bump hook
		filesToAdd = append(filesToAdd, hookDirtyFiles...)

		if err := GitAdd(pluginRoot, filesToAdd...); err != nil {
			setError(PhaseCommitPlugin, err)
			return err
		}

		commitMsg := fmt.Sprintf("chore: bump version to %s", targetVersion)
		if err := GitCommit(pluginRoot, commitMsg); err != nil {
			setError(PhaseCommitPlugin, err)
			return err
		}

		if err := GitPullRebase(pluginRoot); err != nil {
			setError(PhaseCommitPlugin, err)
			return fmt.Errorf("pull --rebase (plugin): %w", err)
		}

		// Phase 5: Push plugin
		setPhase(PhasePushPlugin)
		e.out("  Pushing plugin...\n")

		if err := GitPush(pluginRoot); err != nil {
			setError(PhasePushPlugin, err)
			return err
		}
	}

	// Phase 6: Update marketplace
	setPhase(PhaseUpdateMarket)
	e.out("  Updating marketplace...\n")

	if err := UpdateMarketplaceVersion(marketRoot, plugin.Name, targetVersion); err != nil {
		setError(PhaseUpdateMarket, err)
		return err
	}

	marketplaceJSON := filepath.Join(".claude-plugin", "marketplace.json")
	if err := GitAdd(marketRoot, marketplaceJSON); err != nil {
		setError(PhaseUpdateMarket, err)
		return err
	}

	mktCommitMsg := fmt.Sprintf("chore: bump %s to v%s", plugin.Name, targetVersion)
	if err := GitCommit(marketRoot, mktCommitMsg); err != nil {
		setError(PhaseUpdateMarket, err)
		return err
	}

	if err := GitPullRebase(marketRoot); err != nil {
		setError(PhaseUpdateMarket, err)
		return fmt.Errorf("pull --rebase (marketplace): %w", err)
	}

	if err := GitPush(marketRoot); err != nil {
		setError(PhaseUpdateMarket, err)
		return err
	}

	// Phase 7: Sync local
	setPhase(PhaseSyncLocal)
	e.out("  Syncing local state...\n")

	// Rebuild cache (without .git)
	if err := RebuildCache(plugin.Name, targetVersion, pluginRoot); err != nil {
		e.out("  warning: cache rebuild: %v\n", err)
	}

	// Pre-build Go binary if this is a Go MCP plugin
	cachePath := filepath.Join(CacheBase(), plugin.Name, targetVersion)
	if err := BuildGoMCPBinary(plugin.Name, pluginRoot, cachePath); err != nil {
		e.out("  warning: Go binary pre-build: %v\n", err)
	}

	// Update installed_plugins.json (with git SHA for Claude Code plugin resolution)
	gitSha, _ := GitHeadCommit(pluginRoot)
	if err := UpdateInstalled(plugin.Name, targetVersion, cachePath, gitSha); err != nil {
		e.out("  warning: installed_plugins.json: %v\n", err)
	}

	// Sync CC marketplace checkout
	if err := SyncCCMarketplace(marketRoot, plugin.Name, targetVersion); err != nil {
		e.out("  warning: CC marketplace sync: %v\n", err)
	}

	// Refresh CC's in-memory marketplace index
	if err := RefreshCCMarketplace(); err != nil {
		e.out("  warning: CC marketplace refresh: %v\n", err)
	}

	// Prune stale cache versions (keep only the newly published one)
	if pruned, freed, err := PruneStaleVersions(1); err != nil {
		e.out("  warning: stale version prune: %v\n", err)
	} else if pruned > 0 {
		e.out("  Pruned %d stale cache version(s) (%.1f MB freed)\n", pruned, float64(freed)/1024/1024)
	}

	// Create hook symlinks
	hasHooks := false
	for _, hookPath := range []string{
		filepath.Join(pluginRoot, ".claude-plugin", "hooks", "hooks.json"),
		filepath.Join(pluginRoot, "hooks", "hooks.json"),
	} {
		if _, err := os.Stat(hookPath); err == nil {
			hasHooks = true
			break
		}
	}
	if hasHooks {
		CreateSymlinks(plugin.Name, plugin.Version, targetVersion)
	}

	// Phase 7b: Sync agent-rig.json (best-effort — non-fatal)
	if rigPath, err := FindAgentRig(pluginRoot); err == nil {
		result, err := SyncRig(rigPath, plugin.Name, plugin.Description(), "interagency-marketplace")
		if err != nil {
			e.out("  warning: rig sync: %v\n", err)
		} else if result.Added {
			e.out("  Added %s to agent-rig.json\n", plugin.Name)
			if err := CommitAndPushRig(result.ClavRoot, plugin.Name, targetVersion); err != nil {
				e.out("  warning: rig commit/push: %v\n", err)
			}
		}
	}

	// Phase 8: Done
	if e.store != nil && stateID != "" {
		e.store.Complete(ctx, stateID)
	}

	if syncOnly {
		e.out("  Synced %s v%s to marketplace\n", plugin.Name, targetVersion)
	} else {
		e.out("  Published %s v%s\n", plugin.Name, targetVersion)
	}
	e.out("  Restart Claude Code sessions to pick up the new version.\n")
	return nil
}

// resolveTargetVersion determines the version to publish.
func (e *Engine) resolveTargetVersion(current string) (string, error) {
	switch e.opts.Mode {
	case BumpExact:
		if e.opts.Version == "" {
			return "", fmt.Errorf("no version specified")
		}
		if _, _, _, _, err := ParseVersion(e.opts.Version); err != nil {
			return "", err
		}
		return e.opts.Version, nil
	case BumpPatch:
		return BumpVersion(current, BumpPatch)
	case BumpMinor:
		return BumpVersion(current, BumpMinor)
	default:
		return "", fmt.Errorf("unknown bump mode: %d", e.opts.Mode)
	}
}

// runHook executes a shell script hook.
func runHook(script, version string) error {
	cmd := execCommand("bash", script, version)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// RunPluginValidator runs scripts/validate-plugin.sh on a plugin directory.
// Returns an error if the validator finds errors (exit code 1).
// Returns nil if the script is not found (best-effort) or passes.
func RunPluginValidator(pluginRoot string) error {
	script := findMonorepoScript(pluginRoot, filepath.Join("scripts", "validate-plugin.sh"))
	if script == "" {
		return nil // not found — skip
	}

	cmd := execCommand("bash", script)
	cmd.Dir = pluginRoot
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err == nil {
		return nil // passed (warnings are OK)
	}

	// Exit code 1 = validation errors found
	output := strings.TrimSpace(stdout.String())
	if output == "" {
		output = strings.TrimSpace(stderr.String())
	}
	return fmt.Errorf("validate-plugin.sh found errors:\n%s", output)
}

// findMonorepoScript walks up from a directory to find a script path relative to the monorepo root.
func findMonorepoScript(from, relPath string) string {
	abs, _ := filepath.Abs(from)
	current := abs
	for i := 0; i < 5; i++ {
		candidate := filepath.Join(current, relPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
	return ""
}
