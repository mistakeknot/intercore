package agency

// Spec represents a complete agency spec for a single macro-stage.
// Each spec declares the full configuration for one stage: which agents
// to dispatch, what models to use, tool restrictions, expected artifacts,
// gate conditions, budget allocation, and capability declarations.
type Spec struct {
	Meta         Meta                      `yaml:"meta" json:"meta"`
	Agents       []AgentEntry              `yaml:"agents" json:"agents"`
	Models       map[string]ModelConfig    `yaml:"models,omitempty" json:"models,omitempty"`
	Tools        map[string]ToolConfig     `yaml:"tools,omitempty" json:"tools,omitempty"`
	Artifacts    ArtifactConfig            `yaml:"artifacts,omitempty" json:"artifacts,omitempty"`
	Gates        GateConfig                `yaml:"gates,omitempty" json:"gates,omitempty"`
	Budget       BudgetConfig              `yaml:"budget,omitempty" json:"budget,omitempty"`
	Capabilities map[string]CapabilitySet  `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
}

// Meta identifies which macro-stage this spec covers and its kernel phases.
type Meta struct {
	Stage       string   `yaml:"stage" json:"stage"`
	Description string   `yaml:"description" json:"description"`
	Phases      []string `yaml:"phases" json:"phases"`
}

// AgentEntry is one agent dispatch registration for a specific phase.
type AgentEntry struct {
	Phase       string   `yaml:"phase" json:"phase"`
	Command     string   `yaml:"command" json:"command"`
	Args        []string `yaml:"args,omitempty" json:"args,omitempty"`
	Mode        string   `yaml:"mode" json:"mode"`
	Priority    int      `yaml:"priority" json:"priority"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
}

// ModelConfig specifies the default model and per-category overrides for a phase.
type ModelConfig struct {
	Default    string            `yaml:"default" json:"default"`
	Categories map[string]string `yaml:"categories,omitempty" json:"categories,omitempty"`
}

// ToolConfig specifies tool allow/deny lists for an agent type.
type ToolConfig struct {
	Allow []string `yaml:"allow,omitempty" json:"allow,omitempty"`
	Deny  []string `yaml:"deny,omitempty" json:"deny,omitempty"`
}

// ArtifactConfig declares required inputs and expected outputs for a stage.
type ArtifactConfig struct {
	Required []ArtifactEntry `yaml:"required,omitempty" json:"required,omitempty"`
	Produces []ArtifactEntry `yaml:"produces,omitempty" json:"produces,omitempty"`
}

// ArtifactEntry is a single artifact reference (input or output).
type ArtifactEntry struct {
	Type        string `yaml:"type" json:"type"`
	Phase       string `yaml:"phase" json:"phase"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// GateConfig declares entry and exit conditions for a stage.
type GateConfig struct {
	Entry []GateRule `yaml:"entry,omitempty" json:"entry,omitempty"`
	Exit  []GateRule `yaml:"exit,omitempty" json:"exit,omitempty"`
}

// GateRule is a single gate check condition.
type GateRule struct {
	Check       string `yaml:"check" json:"check"`
	Phase       string `yaml:"phase,omitempty" json:"phase,omitempty"`
	Tier        string `yaml:"tier" json:"tier"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// BudgetConfig declares the budget allocation for a stage.
type BudgetConfig struct {
	Allocation    float64 `yaml:"allocation" json:"allocation"`
	MaxAgents     int     `yaml:"max_agents" json:"max_agents"`
	WarnThreshold float64 `yaml:"warn_threshold" json:"warn_threshold"`
}

// CapabilitySet declares what operations an agent type may perform (shadow mode).
type CapabilitySet struct {
	Kernel     []string `yaml:"kernel,omitempty" json:"kernel,omitempty"`
	Filesystem []string `yaml:"filesystem,omitempty" json:"filesystem,omitempty"`
	Dispatch   []string `yaml:"dispatch,omitempty" json:"dispatch,omitempty"`
}

// KnownStages is the set of valid macro-stage names.
var KnownStages = map[string]bool{
	"discover": true,
	"design":   true,
	"build":    true,
	"ship":     true,
	"reflect":  true,
}
