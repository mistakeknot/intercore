// Package publish implements the plugin publishing pipeline for ic publish.
//
// It replaces the shell-based interbump.sh and auto-publish.sh with a Go
// implementation that provides a SQLite-backed state machine for recovery,
// single-source versioning (plugin.json), and comprehensive drift detection.
package publish

import "errors"

// Phase represents a step in the publish pipeline.
type Phase string

const (
	PhaseIdle         Phase = "idle"
	PhaseDiscovery    Phase = "discovery"
	PhaseValidation   Phase = "validation"
	PhaseBump         Phase = "bump"
	PhaseCommitPlugin Phase = "commit_plugin"
	PhasePushPlugin   Phase = "push_plugin"
	PhaseUpdateMarket Phase = "update_marketplace"
	PhaseSyncLocal    Phase = "sync_local"
	PhaseDone         Phase = "done"
)

// phaseOrder defines the execution sequence. Used for resume logic.
var phaseOrder = []Phase{
	PhaseDiscovery,
	PhaseValidation,
	PhaseBump,
	PhaseCommitPlugin,
	PhasePushPlugin,
	PhaseUpdateMarket,
	PhaseSyncLocal,
	PhaseDone,
}

// PhaseIndex returns the position of a phase in the pipeline, or -1.
func PhaseIndex(p Phase) int {
	for i, phase := range phaseOrder {
		if phase == p {
			return i
		}
	}
	return -1
}

// BumpMode controls how the version is incremented.
type BumpMode int

const (
	BumpExact BumpMode = iota // use an explicit version string
	BumpPatch                 // X.Y.Z → X.Y.(Z+1)
	BumpMinor                 // X.Y.Z → X.(Y+1).0
)

// PublishOpts configures a publish run.
type PublishOpts struct {
	Mode    BumpMode
	Version string // only used when Mode == BumpExact
	DryRun  bool
	Auto    bool   // suppress prompts, for hook usage
	CWD     string // override working directory
}

// Plugin represents a discovered plugin.
type Plugin struct {
	Name        string // from plugin.json .name
	Version     string // current version from plugin.json .version
	Root        string // absolute path to plugin root (parent of .claude-plugin/)
	PluginJSON  string // absolute path to plugin.json
	description string // from plugin.json .description (lazy-loaded)
}

// Description returns the plugin's description from plugin.json.
func (p *Plugin) Description() string {
	return p.description
}

// VersionFile is a file that contains a derived version string.
type VersionFile struct {
	Path    string // absolute path
	Type    string // "json", "toml", "cargo-toml"
	JSONKey string // jq-style key path, e.g. "version" for package.json
}

// PublishState tracks an in-flight publish for recovery.
type PublishState struct {
	ID          string
	PluginName  string
	FromVersion string
	ToVersion   string
	Phase       Phase
	PluginRoot  string
	MarketRoot  string
	StartedAt   int64
	UpdatedAt   int64
	Error       string // last error, empty if clean
}

// Errors
var (
	ErrNotPlugin         = errors.New("not a plugin directory (no .claude-plugin/plugin.json found)")
	ErrNoMarketplace     = errors.New("marketplace not found")
	ErrNotInMarketplace  = errors.New("plugin not registered in marketplace")
	ErrVersionMatch      = errors.New("already at target version")
	ErrDirtyWorktree     = errors.New("git worktree has uncommitted changes")
	ErrRemoteUnreachable = errors.New("git remote is unreachable")
	ErrActivePublish     = errors.New("another publish is in progress")
	ErrNoActivePublish   = errors.New("no active publish to resume")
	ErrApprovalRequired  = errors.New("agent-mutated plugin requires human approval — create .publish-approved or run 'ic publish' manually")
)
