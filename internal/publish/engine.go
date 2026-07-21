package publish

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/pkg/authz"
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

	// Check if the developer already bumped (marketplace is stale).
	// If plugin.json version > marketplace version, a bump is unnecessary —
	// the remaining work is syncing marketplace + local.
	mktVer, _ := ReadMarketplaceVersion(marketRoot, plugin.Name)
	marketBehind := mktVer != "" && mktVer != plugin.Version && CompareVersions(plugin.Version, mktVer) > 0

	// Auto mode (hooks) always takes the sync-only path when behind.
	syncOnly := e.opts.Auto && marketBehind

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
			if marketBehind {
				// Explicit target equals the already-bumped plugin.json and the
				// marketplace is behind — sync instead of erroring, mirroring
				// the --auto path. ErrVersionMatch is reserved for the true
				// no-op where every location already matches.
				syncOnly = true
			} else {
				return ErrVersionMatch
			}
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
		// Run cheap, side-effect-free validation in dry-run so authors
		// can preview frontmatter health before committing to a publish.
		if fmErr := ValidateFrontmatter(pluginRoot); fmErr != nil {
			return fmt.Errorf("frontmatter validation: %w", fmErr)
		}
		if releaseErr := verifyReleaseArtifacts(pluginRoot); releaseErr != nil {
			return fmt.Errorf("release validation: %w", releaseErr)
		}
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
		if active != nil {
			switch {
			case e.opts.Auto || active.IsStale(time.Now().Unix()):
				// Clear and proceed. Auto mode (hooks) always clears; interactive
				// mode clears only records that are provably dead (failed or
				// abandoned), so a genuinely-running publish in another terminal
				// is never stomped. A failed attempt thus self-heals on the next
				// plain `ic publish` — no --auto required.
				if !e.opts.Auto {
					reason := "no update in over an hour"
					if active.Error != "" {
						reason = "previous attempt failed: " + active.Error
					}
					e.out("Clearing stale publish state %s (phase %s; %s)\n", active.ID, active.Phase, reason)
				}
				if delErr := e.store.Delete(ctx, active.ID); delErr != nil {
					return fmt.Errorf("clear stale publish state %s: %w", active.ID, delErr)
				}
			default:
				// Genuinely in flight — a publish is actively running elsewhere.
				return fmt.Errorf("%w: %s at phase %s (id: %s) — another publish is running; wait for it to finish. If it has crashed, run 'ic publish unlock' to clear the lock now (or 'ic publish --auto'); it also self-clears once the state goes stale (1h of inactivity)",
					ErrActivePublish, active.PluginName, active.Phase, active.ID)
			}
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

	// Lock cleanup: every failure path below returns early without reaching the
	// PhaseDone Complete() call, which historically left the publish_state row
	// behind at its failed phase. A leaked row blocks the next publish until it
	// ages past the stale threshold (or an --auto run clears it). Track success
	// and, on any non-success exit, delete the row so the lock self-clears
	// immediately. See sylveste-2uhz.
	succeeded := false
	defer func() {
		if !succeeded && e.store != nil && stateID != "" {
			if delErr := e.store.Delete(ctx, stateID); delErr != nil {
				e.out("warning: cannot clear publish lock %s: %v\n", stateID, delErr)
			}
		}
	}()

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

	var releaseDirtyFiles []string
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

		// Frontmatter YAML check — clavain 0.6.245 shipped a broken description
		// (unquoted colon) that silently took the entire plugin out of
		// skill_listing. See sylveste-ulp8.
		if fmErr := ValidateFrontmatter(pluginRoot); fmErr != nil {
			setError(PhaseValidation, fmErr)
			return fmt.Errorf("frontmatter validation: %w", fmErr)
		}

		// Human approval gate: block auto-publish of agent-mutated plugins.
		// Assembles explicit deps for RequiresApproval — token string +
		// caller agent id come from PublishOpts (composition root), the
		// shared DB handle from the store (or nil if no state tracking),
		// and the pub key is loaded from the project root on the fly.
		if e.opts.Auto {
			var db *sql.DB
			if e.store != nil {
				db = e.store.DB()
			}
			var pub []byte
			if projectRoot := projectRootForPlugin(pluginRoot); projectRoot != "" {
				if loaded, pubErr := authz.LoadPubKey(projectRoot); pubErr == nil {
					pub = loaded
				}
			}
			needs, via := RequiresApproval(
				pluginRoot,
				e.opts.AuthzTokenStr,
				e.opts.AuthzCallerAgentID,
				db, pub, time.Now().Unix(),
			)
			if needs {
				err := ErrApprovalRequired
				setError(PhaseValidation, err)
				return err
			}
			e.out("  Approval granted via %s\n", via)
		}

		// Managed release artifacts are part of the publish transaction. Verify
		// first, rebuild only when stale, and stage every rebuilt tracked file
		// with the version commit below.
		var rebuilt bool
		releaseDirtyFiles, rebuilt, err = prepareReleaseArtifacts(pluginRoot, true)
		if err != nil {
			setError(PhaseValidation, err)
			return fmt.Errorf("release preparation: %w", err)
		}
		if rebuilt {
			e.out("  Rebuilt and verified release artifacts\n")
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
		filesToAdd = append(filesToAdd, releaseDirtyFiles...)
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

	// Release artifacts are a hard boundary: re-run the verifier after the
	// plugin commit/push and immediately before marketplace or cache mutation.
	// Sync-only publishes cannot create an unversioned artifact repair commit,
	// so stale artifacts fail here without changing either downstream surface.
	if releaseErr := verifyReleaseArtifacts(pluginRoot); releaseErr != nil {
		setError(PhaseValidation, releaseErr)
		return fmt.Errorf("release validation before marketplace update: %w", releaseErr)
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

	// Prune stale cache versions across ALL marketplaces, not just interagency.
	// This is a multi-marketplace sweep so plugins from claude-plugins-official,
	// arouth-plugins, etc. don't accumulate stale versions either. The version
	// we JUST published is passed as explicit protection: RefreshCCMarketplace
	// above can rewrite installed_plugins.json concurrently, and the prune must
	// not depend on that file to know this version is live (Sylveste-0lt).
	justPublished := map[string]string{plugin.Name + "@interagency-marketplace": targetVersion}
	if pruned, freed, err := PruneStaleVersionsAcrossMarketplaces(1, justPublished); err != nil {
		e.out("  warning: stale version prune: %v\n", err)
	} else if pruned > 0 {
		e.out("  Pruned %d stale cache version(s) (%.1f MB freed)\n", pruned, float64(freed)/1024/1024)
	}

	// The version prune skips dirs carrying an .orphaned_at marker, and until
	// now nothing on the publish path ever removed them — they persisted until
	// someone manually ran `ic publish clean`, tripping version-drift checks
	// downstream. Clean them here once past the session-continuity grace window.
	if cleaned, freed, err := CleanOrphansOlderThan(24 * time.Hour); err != nil {
		e.out("  warning: orphan clean: %v\n", err)
	} else if cleaned > 0 {
		e.out("  Cleaned %d orphaned cache dir(s) (%.1f MB freed)\n", cleaned, float64(freed)/1024/1024)
	}

	// Bridge symlinks whose targets the prune already removed can never
	// resolve again — retire them so version listings stay truthful.
	if removed, err := PruneDanglingSymlinks(); err != nil {
		e.out("  warning: dangling symlink prune: %v\n", err)
	} else if removed > 0 {
		e.out("  Removed %d dangling version symlink(s)\n", removed)
	}

	// Post-publish assertion (Sylveste-0lt): the path installed_plugins.json
	// points at must exist NOW — a silent miss here surfaces as "plugin failed
	// to load" at the next session start, with no clue it was the publish.
	if ip2, err := ReadInstalled(); err == nil {
		if rec, ok := ip2.Plugins[plugin.Name+"@interagency-marketplace"]; ok && len(rec) > 0 {
			if _, statErr := os.Stat(rec[0].InstallPath); statErr != nil {
				e.out("  ERROR: published cache path missing after prune: %s — plugin will fail to load; run ic publish doctor --fix\n", rec[0].InstallPath)
			}
		}
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

	// Phase 7c: Regenerate interchart ecosystem diagram (best-effort — non-fatal)
	if interchartRoot, err := FindInterchart(pluginRoot); err == nil {
		e.out("  Regenerating ecosystem diagram...\n")
		if err := RegenerateInterchart(interchartRoot, pluginRoot); err != nil {
			e.out("  warning: interchart: %v\n", err)
		}
	}

	// Phase 8: Done
	succeeded = true
	if e.store != nil && stateID != "" {
		e.store.Complete(ctx, stateID)
	}

	// Consume approval marker if present (single-use)
	ConsumeApproval(pluginRoot)

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
