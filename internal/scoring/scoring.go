// Package scoring provides multi-factor assignment scoring for agent-task pairs.
//
// The scorer evaluates every possible (agent, task) combination using weighted
// factors to find the optimal assignment. This replaces manual or round-robin
// work distribution with intelligent routing.
//
// Factors:
//   - Base task score (priority, dependency graph centrality)
//   - Agent type bonus (match agent capabilities to task complexity)
//   - Profile tag affinity (agent specialization match)
//   - File focus overlap (agent's file history vs task's target files)
//   - Context penalty (penalize agents near context exhaustion)
//   - File reservation penalty (avoid conflicts with existing locks)
//
// Inspired by github.com/Dicklesworthstone/ntm/internal/coordinator/assign.go.
package scoring

import (
	"sort"
	"strings"
)

// Task represents a unit of work to be assigned.
type Task struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Type        string   `json:"type"`     // "epic", "feature", "bug", "task", "chore"
	Priority    int      `json:"priority"` // 0=critical, 4=backlog
	Score       float64  `json:"score"`    // Base score from triage/centrality
	Tags        []string `json:"tags"`
	Files       []string `json:"files"`    // Target files this task will touch
	UnblocksIDs []string `json:"unblocks"` // IDs of tasks this unblocks
}

// Agent represents an available agent that can be assigned work.
type Agent struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Type         string   `json:"type"`          // "claude", "codex", "gemini"
	ContextUsage float64  `json:"context_usage"` // 0-100 percentage
	Tags         []string `json:"tags"`          // Agent specialization tags
	FilePatterns []string `json:"file_patterns"` // Glob patterns agent has worked on
	Reservations []string `json:"reservations"`  // Currently reserved file paths
}

// Assignment represents a scored agent-task pairing.
type Assignment struct {
	TaskID    string    `json:"task_id"`
	AgentID   string    `json:"agent_id"`
	Score     float64   `json:"score"`
	Breakdown Breakdown `json:"breakdown"`
	Reason    string    `json:"reason"`
}

// Breakdown shows how each factor contributed to the score.
type Breakdown struct {
	BaseScore          float64 `json:"base_score"`
	AgentTypeBonus     float64 `json:"agent_type_bonus"`
	CriticalPathBonus  float64 `json:"critical_path_bonus"`
	ProfileTagBonus    float64 `json:"profile_tag_bonus"`
	FocusPatternBonus  float64 `json:"focus_pattern_bonus"`
	FileOverlapPenalty float64 `json:"file_overlap_penalty"`
	ContextPenalty     float64 `json:"context_penalty"`
}

// Config controls scoring behavior.
type Config struct {
	PreferCriticalPath      bool    `json:"prefer_critical_path"`
	PenalizeFileOverlap     bool    `json:"penalize_file_overlap"`
	UseAgentProfiles        bool    `json:"use_agent_profiles"`
	BudgetAware             bool    `json:"budget_aware"`
	ContextThreshold        float64 `json:"context_threshold"`          // 0-100, default 80
	ProfileTagBoostWeight   float64 `json:"profile_tag_boost_weight"`   // default 0.15
	FocusPatternBoostWeight float64 `json:"focus_pattern_boost_weight"` // default 0.10
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		PreferCriticalPath:      true,
		PenalizeFileOverlap:     true,
		UseAgentProfiles:        true,
		BudgetAware:             true,
		ContextThreshold:        80,
		ProfileTagBoostWeight:   0.15,
		FocusPatternBoostWeight: 0.10,
	}
}

// Strategy controls how assignments are selected from scored pairs.
type Strategy string

const (
	StrategyBalanced   Strategy = "balanced"    // Spread work evenly
	StrategySpeed      Strategy = "speed"       // Assign to any available agent
	StrategyQuality    Strategy = "quality"     // Best-scoring agent per task
	StrategyDependency Strategy = "dependency"  // Prioritize blockers
	StrategyRoundRobin Strategy = "round-robin" // Deterministic even distribution
)

type scoredPair struct {
	taskIdx   int
	agentIdx  int
	score     float64
	breakdown Breakdown
}

// scoringContext holds shared state for a single Assign call to avoid repeated allocations.
type scoringContext struct {
	tasks        []Task
	agents       []Agent
	reservations map[string][]string
}

// Assign computes optimal assignments for the given tasks and agents.
func Assign(tasks []Task, agents []Agent, strategy Strategy, cfg Config) []Assignment {
	if len(tasks) == 0 || len(agents) == 0 {
		return nil
	}

	// Build reservation map once.
	reservations := make(map[string][]string, len(agents))
	for i := range agents {
		if len(agents[i].Reservations) > 0 {
			reservations[agents[i].ID] = agents[i].Reservations
		}
	}

	ctx := &scoringContext{
		tasks:        tasks,
		agents:       agents,
		reservations: reservations,
	}

	// Score all pairs.
	pairs := scoreAllPairs(ctx, cfg)

	// Select assignments based on strategy.
	selected := selectAssignments(pairs, ctx, len(agents), len(tasks), strategy)

	// Build result.
	assignments := make([]Assignment, len(selected))
	for i, p := range selected {
		assignments[i] = Assignment{
			TaskID:    ctx.tasks[p.taskIdx].ID,
			AgentID:   ctx.agents[p.agentIdx].ID,
			Score:     p.score,
			Breakdown: p.breakdown,
			Reason:    buildReason(p, ctx, strategy),
		}
	}
	return assignments
}

func scoreAllPairs(ctx *scoringContext, cfg Config) []scoredPair {
	n := len(ctx.tasks) * len(ctx.agents)
	pairs := make([]scoredPair, n)
	idx := 0
	for ti := range ctx.tasks {
		for ai := range ctx.agents {
			scoreOne(&pairs[idx], ti, ai, ctx, cfg)
			idx++
		}
	}
	return pairs
}

func scoreOne(out *scoredPair, taskIdx, agentIdx int, ctx *scoringContext, cfg Config) {
	task := &ctx.tasks[taskIdx]
	agent := &ctx.agents[agentIdx]

	b := &out.breakdown
	*b = Breakdown{
		BaseScore: task.Score,
	}

	if cfg.UseAgentProfiles {
		b.AgentTypeBonus = agentTypeBonus(agent.Type, task)
	}

	if cfg.UseAgentProfiles && len(agent.Tags) > 0 {
		w := cfg.ProfileTagBoostWeight
		if w == 0 {
			w = 0.15
		}
		b.ProfileTagBonus = profileTagBonus(agent.Tags, task.Tags, w)
	}

	if cfg.UseAgentProfiles && len(agent.FilePatterns) > 0 {
		w := cfg.FocusPatternBoostWeight
		if w == 0 {
			w = 0.10
		}
		b.FocusPatternBonus = focusPatternBonus(agent.FilePatterns, task.Files, w)
	}

	if cfg.PreferCriticalPath {
		b.CriticalPathBonus = criticalPathBonus(task)
	}

	if cfg.PenalizeFileOverlap {
		b.FileOverlapPenalty = fileOverlapPenalty(agent.ID, task.Files, ctx.reservations)
	}

	if cfg.BudgetAware {
		threshold := cfg.ContextThreshold
		if threshold == 0 {
			threshold = 80
		}
		b.ContextPenalty = contextPenalty(agent.ContextUsage, threshold)
	}

	out.taskIdx = taskIdx
	out.agentIdx = agentIdx
	out.score = b.BaseScore +
		b.AgentTypeBonus +
		b.CriticalPathBonus +
		b.ProfileTagBonus +
		b.FocusPatternBonus -
		b.FileOverlapPenalty -
		b.ContextPenalty
}

// agentTypeBonus returns a bonus based on agent-task complexity match.
func agentTypeBonus(agentType string, task *Task) float64 {
	complexity := estimateComplexity(task)

	switch strings.ToLower(agentType) {
	case "claude", "cc":
		if complexity >= 0.7 {
			return 0.15
		} else if complexity <= 0.3 {
			return -0.05
		}
	case "codex", "cod":
		if complexity <= 0.3 {
			return 0.15
		} else if complexity >= 0.7 {
			return -0.1
		}
	case "gemini", "gmi":
		if complexity >= 0.4 && complexity <= 0.6 {
			return 0.05
		}
	}
	return 0
}

func estimateComplexity(task *Task) float64 {
	c := 0.5

	switch task.Type {
	case "epic":
		c += 0.3
	case "feature":
		c += 0.2
	case "bug":
		// varies
	case "task":
		c -= 0.1
	case "chore":
		c -= 0.2
	}

	if task.Priority == 0 {
		c -= 0.1
	} else if task.Priority >= 3 {
		c += 0.1
	}

	if len(task.UnblocksIDs) >= 5 {
		c += 0.15
	} else if len(task.UnblocksIDs) >= 3 {
		c += 0.1
	}

	if c < 0 {
		c = 0
	} else if c > 1 {
		c = 1
	}
	return c
}

func profileTagBonus(agentTags, taskTags []string, weight float64) float64 {
	if len(taskTags) == 0 {
		return 0
	}

	// O(n*m) with small slices is faster than map allocation.
	matches := 0
	for _, tt := range taskTags {
		ttLower := strings.ToLower(tt)
		for _, at := range agentTags {
			if strings.ToLower(at) == ttLower {
				matches++
				break
			}
		}
	}

	if matches == 0 {
		return 0
	}

	// Proportion of task tags matched, scaled by weight.
	return float64(matches) / float64(len(taskTags)) * weight
}

func focusPatternBonus(filePatterns, taskFiles []string, weight float64) float64 {
	if len(taskFiles) == 0 || len(filePatterns) == 0 {
		return 0
	}

	matches := 0
	for _, tf := range taskFiles {
		for _, pat := range filePatterns {
			if matchesPattern(tf, pat) {
				matches++
				break
			}
		}
	}

	if matches == 0 {
		return 0
	}

	return float64(matches) / float64(len(taskFiles)) * weight
}

// matchesPattern does simple prefix/suffix matching for file patterns.
func matchesPattern(file, pattern string) bool {
	if pattern == "" {
		return false
	}
	// Exact match
	if file == pattern {
		return true
	}
	// Directory prefix: "internal/" matches "internal/foo.go"
	if strings.HasSuffix(pattern, "/") && strings.HasPrefix(file, pattern) {
		return true
	}
	// Extension match: "*.go" matches "foo.go"
	if strings.HasPrefix(pattern, "*.") {
		ext := pattern[1:]
		return strings.HasSuffix(file, ext)
	}
	// Contains
	return strings.Contains(file, pattern)
}

func criticalPathBonus(task *Task) float64 {
	n := len(task.UnblocksIDs)
	switch {
	case n >= 5:
		return 0.2
	case n >= 3:
		return 0.15
	case n >= 1:
		return 0.1
	default:
		return 0
	}
}

func fileOverlapPenalty(agentID string, taskFiles []string, reservations map[string][]string) float64 {
	if len(taskFiles) == 0 {
		return 0
	}

	// Check if any OTHER agent has reservations on the task's files.
	conflicts := 0
	for otherID, files := range reservations {
		if otherID == agentID {
			continue
		}
		for _, reserved := range files {
			for _, target := range taskFiles {
				if reserved == target {
					conflicts++
				}
			}
		}
	}

	if conflicts == 0 {
		return 0
	}

	// Scale penalty: 0.1 per conflicting file, capped at 0.5.
	penalty := float64(conflicts) * 0.1
	if penalty > 0.5 {
		penalty = 0.5
	}
	return penalty
}

func contextPenalty(usage, threshold float64) float64 {
	if usage <= threshold {
		return 0
	}
	// Linear ramp from threshold to 100.
	excess := (usage - threshold) / (100 - threshold)
	return excess * 0.3
}

func selectAssignments(pairs []scoredPair, ctx *scoringContext, numAgents, numTasks int, strategy Strategy) []scoredPair {
	switch strategy {
	case StrategyQuality:
		return selectQuality(pairs, ctx, numAgents, numTasks)
	case StrategyDependency:
		return selectDependency(pairs, ctx, numAgents, numTasks)
	case StrategyRoundRobin:
		return selectRoundRobin(pairs, ctx, numAgents, numTasks)
	case StrategySpeed:
		return selectSpeed(pairs, ctx, numAgents, numTasks)
	default: // balanced
		return selectBalanced(pairs, ctx, numAgents, numTasks)
	}
}

// selectQuality assigns each task to its highest-scoring agent.
func selectQuality(pairs []scoredPair, ctx *scoringContext, _, numTasks int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[int]bool, numTasks)
	result := make([]scoredPair, 0, numTasks)
	for i := range pairs {
		if assigned[pairs[i].taskIdx] {
			continue
		}
		assigned[pairs[i].taskIdx] = true
		result = append(result, pairs[i])
	}
	return result
}

// selectBalanced assigns tasks to agents, ensuring even distribution.
func selectBalanced(pairs []scoredPair, ctx *scoringContext, numAgents, numTasks int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assignedTasks := make(map[int]bool, numTasks)
	agentLoad := make(map[int]int, numAgents)

	maxPerAgent := (numTasks + numAgents - 1) / numAgents
	if maxPerAgent < 1 {
		maxPerAgent = 1
	}

	result := make([]scoredPair, 0, numTasks)
	for _, p := range pairs {
		if assignedTasks[p.taskIdx] {
			continue
		}
		if agentLoad[p.agentIdx] >= maxPerAgent {
			continue
		}
		assignedTasks[p.taskIdx] = true
		agentLoad[p.agentIdx]++
		result = append(result, p)
	}
	return result
}

// selectDependency prioritizes tasks that unblock the most other tasks.
func selectDependency(pairs []scoredPair, ctx *scoringContext, _, numTasks int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		// Primary sort: more unblocks first
		ui := len(ctx.tasks[pairs[i].taskIdx].UnblocksIDs)
		uj := len(ctx.tasks[pairs[j].taskIdx].UnblocksIDs)
		if ui != uj {
			return ui > uj
		}
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[int]bool, numTasks)
	result := make([]scoredPair, 0, numTasks)
	for _, p := range pairs {
		if assigned[p.taskIdx] {
			continue
		}
		assigned[p.taskIdx] = true
		result = append(result, p)
	}
	return result
}

// selectSpeed assigns each task to any available agent, highest score first.
func selectSpeed(pairs []scoredPair, ctx *scoringContext, _, numTasks int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[int]bool, numTasks)
	result := make([]scoredPair, 0, numTasks)
	for _, p := range pairs {
		if assigned[p.taskIdx] {
			continue
		}
		assigned[p.taskIdx] = true
		result = append(result, p)
	}
	return result
}

// selectRoundRobin distributes tasks evenly across agents in order.
func selectRoundRobin(pairs []scoredPair, ctx *scoringContext, numAgents, numTasks int) []scoredPair {
	// Collect unique task and agent indices in order.
	taskOrder := make([]int, 0, numTasks)
	taskSeen := make(map[int]bool, numTasks)
	agentOrder := make([]int, 0, numAgents)
	agentSeen := make(map[int]bool, numAgents)

	for i := range pairs {
		if !taskSeen[pairs[i].taskIdx] {
			taskSeen[pairs[i].taskIdx] = true
			taskOrder = append(taskOrder, pairs[i].taskIdx)
		}
		if !agentSeen[pairs[i].agentIdx] {
			agentSeen[pairs[i].agentIdx] = true
			agentOrder = append(agentOrder, pairs[i].agentIdx)
		}
	}

	// Build lookup: pairLookup[taskIdx*numAgents + agentIdx] for O(1) access.
	// Use a flat map keyed by (taskIdx, agentIdx) encoded as a single int.
	type pairKey struct{ t, a int }
	pairMap := make(map[pairKey]scoredPair, len(pairs))
	for i := range pairs {
		pairMap[pairKey{pairs[i].taskIdx, pairs[i].agentIdx}] = pairs[i]
	}

	result := make([]scoredPair, 0, numTasks)
	agentIdx := 0
	for _, ti := range taskOrder {
		if agentIdx >= len(agentOrder) {
			agentIdx = 0
		}
		ai := agentOrder[agentIdx]
		if p, ok := pairMap[pairKey{ti, ai}]; ok {
			result = append(result, p)
		}
		agentIdx++
	}
	return result
}

// reasonTable precomputes all 64 possible reason strings (6 binary flags).
// Indexed by flags: bit0=typeMatch, bit1=critPath, bit2=tagAffinity,
// bit3=fileFocus, bit4=contextPressure, bit5=fileConflict.
var reasonTable [64]string

func init() {
	labels := [6]string{
		"good type match",
		"on critical path",
		"tag affinity",
		"file focus match",
		"context pressure",
		"file conflict",
	}
	reasonTable[0] = "default assignment"
	for flags := uint8(1); flags < 64; flags++ {
		var parts []string
		for bit := uint8(0); bit < 6; bit++ {
			if flags&(1<<bit) != 0 {
				parts = append(parts, labels[bit])
			}
		}
		reasonTable[flags] = strings.Join(parts, ", ")
	}
}

func buildReason(p scoredPair, ctx *scoringContext, strategy Strategy) string {
	var flags uint8
	if p.breakdown.AgentTypeBonus > 0 {
		flags |= 1
	}
	if p.breakdown.CriticalPathBonus > 0 {
		flags |= 2
	}
	if p.breakdown.ProfileTagBonus > 0 {
		flags |= 4
	}
	if p.breakdown.FocusPatternBonus > 0 {
		flags |= 8
	}
	if p.breakdown.ContextPenalty > 0 {
		flags |= 16
	}
	if p.breakdown.FileOverlapPenalty > 0 {
		flags |= 32
	}
	return reasonTable[flags]
}
