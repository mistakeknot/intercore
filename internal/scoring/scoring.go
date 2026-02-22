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
	BaseScore         float64 `json:"base_score"`
	AgentTypeBonus    float64 `json:"agent_type_bonus"`
	CriticalPathBonus float64 `json:"critical_path_bonus"`
	ProfileTagBonus   float64 `json:"profile_tag_bonus"`
	FocusPatternBonus float64 `json:"focus_pattern_bonus"`
	FileOverlapPenalty float64 `json:"file_overlap_penalty"`
	ContextPenalty    float64 `json:"context_penalty"`
}

// Config controls scoring behavior.
type Config struct {
	PreferCriticalPath      bool    `json:"prefer_critical_path"`
	PenalizeFileOverlap     bool    `json:"penalize_file_overlap"`
	UseAgentProfiles        bool    `json:"use_agent_profiles"`
	BudgetAware             bool    `json:"budget_aware"`
	ContextThreshold        float64 `json:"context_threshold"`         // 0-100, default 80
	ProfileTagBoostWeight   float64 `json:"profile_tag_boost_weight"`  // default 0.15
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
	task      Task
	agent     Agent
	score     float64
	breakdown Breakdown
}

// Assign computes optimal assignments for the given tasks and agents.
func Assign(tasks []Task, agents []Agent, strategy Strategy, cfg Config) []Assignment {
	if len(tasks) == 0 || len(agents) == 0 {
		return nil
	}

	// Score all pairs.
	pairs := scoreAllPairs(tasks, agents, cfg)

	// Select assignments based on strategy.
	selected := selectAssignments(pairs, len(agents), len(tasks), strategy)

	// Build result.
	assignments := make([]Assignment, len(selected))
	for i, p := range selected {
		assignments[i] = Assignment{
			TaskID:    p.task.ID,
			AgentID:   p.agent.ID,
			Score:     p.score,
			Breakdown: p.breakdown,
			Reason:    buildReason(p, strategy),
		}
	}
	return assignments
}

func scoreAllPairs(tasks []Task, agents []Agent, cfg Config) []scoredPair {
	// Build reservation map: agent -> reserved files.
	reservations := make(map[string][]string)
	for _, a := range agents {
		reservations[a.ID] = a.Reservations
	}

	pairs := make([]scoredPair, 0, len(tasks)*len(agents))
	for _, task := range tasks {
		for _, agent := range agents {
			pair := scoreOne(agent, task, cfg, reservations)
			pairs = append(pairs, pair)
		}
	}
	return pairs
}

func scoreOne(agent Agent, task Task, cfg Config, reservations map[string][]string) scoredPair {
	b := Breakdown{
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
		b.FileOverlapPenalty = fileOverlapPenalty(agent.ID, task.Files, reservations)
	}

	if cfg.BudgetAware {
		threshold := cfg.ContextThreshold
		if threshold == 0 {
			threshold = 80
		}
		b.ContextPenalty = contextPenalty(agent.ContextUsage, threshold)
	}

	total := b.BaseScore +
		b.AgentTypeBonus +
		b.CriticalPathBonus +
		b.ProfileTagBonus +
		b.FocusPatternBonus -
		b.FileOverlapPenalty -
		b.ContextPenalty

	return scoredPair{
		task:      task,
		agent:     agent,
		score:     total,
		breakdown: b,
	}
}

// agentTypeBonus returns a bonus based on agent-task complexity match.
func agentTypeBonus(agentType string, task Task) float64 {
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

func estimateComplexity(task Task) float64 {
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

	agentSet := make(map[string]bool, len(agentTags))
	for _, t := range agentTags {
		agentSet[strings.ToLower(t)] = true
	}

	matches := 0
	for _, t := range taskTags {
		if agentSet[strings.ToLower(t)] {
			matches++
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

func criticalPathBonus(task Task) float64 {
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

func selectAssignments(pairs []scoredPair, numAgents, numTasks int, strategy Strategy) []scoredPair {
	switch strategy {
	case StrategyQuality:
		return selectQuality(pairs, numAgents, numTasks)
	case StrategyDependency:
		return selectDependency(pairs, numAgents, numTasks)
	case StrategyRoundRobin:
		return selectRoundRobin(pairs, numAgents, numTasks)
	case StrategySpeed:
		return selectSpeed(pairs, numAgents, numTasks)
	default: // balanced
		return selectBalanced(pairs, numAgents, numTasks)
	}
}

// selectQuality assigns each task to its highest-scoring agent.
func selectQuality(pairs []scoredPair, _, _ int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[string]bool)
	var result []scoredPair
	for _, p := range pairs {
		if assigned[p.task.ID] {
			continue
		}
		assigned[p.task.ID] = true
		result = append(result, p)
	}
	return result
}

// selectBalanced assigns tasks to agents, ensuring even distribution.
func selectBalanced(pairs []scoredPair, numAgents, _ int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assignedTasks := make(map[string]bool)
	agentLoad := make(map[string]int)

	// Count unique tasks.
	taskSet := make(map[string]bool)
	for _, p := range pairs {
		taskSet[p.task.ID] = true
	}
	numTasks := len(taskSet)
	maxPerAgent := (numTasks + numAgents - 1) / numAgents
	if maxPerAgent < 1 {
		maxPerAgent = 1
	}

	var result []scoredPair
	for _, p := range pairs {
		if assignedTasks[p.task.ID] {
			continue
		}
		if agentLoad[p.agent.ID] >= maxPerAgent {
			continue
		}
		assignedTasks[p.task.ID] = true
		agentLoad[p.agent.ID]++
		result = append(result, p)
	}
	return result
}

// selectDependency prioritizes tasks that unblock the most other tasks.
func selectDependency(pairs []scoredPair, _, _ int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		// Primary sort: more unblocks first
		ui := len(pairs[i].task.UnblocksIDs)
		uj := len(pairs[j].task.UnblocksIDs)
		if ui != uj {
			return ui > uj
		}
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[string]bool)
	var result []scoredPair
	for _, p := range pairs {
		if assigned[p.task.ID] {
			continue
		}
		assigned[p.task.ID] = true
		result = append(result, p)
	}
	return result
}

// selectSpeed assigns each task to any available agent, highest score first.
func selectSpeed(pairs []scoredPair, _, _ int) []scoredPair {
	sort.Slice(pairs, func(i, j int) bool {
		return pairs[i].score > pairs[j].score
	})

	assigned := make(map[string]bool)
	var result []scoredPair
	for _, p := range pairs {
		if assigned[p.task.ID] {
			continue
		}
		assigned[p.task.ID] = true
		result = append(result, p)
	}
	return result
}

// selectRoundRobin distributes tasks evenly across agents in order.
func selectRoundRobin(pairs []scoredPair, numAgents, _ int) []scoredPair {
	// Collect unique tasks and agents.
	taskOrder := make([]string, 0)
	taskSeen := make(map[string]bool)
	agentOrder := make([]string, 0)
	agentSeen := make(map[string]bool)

	for _, p := range pairs {
		if !taskSeen[p.task.ID] {
			taskSeen[p.task.ID] = true
			taskOrder = append(taskOrder, p.task.ID)
		}
		if !agentSeen[p.agent.ID] {
			agentSeen[p.agent.ID] = true
			agentOrder = append(agentOrder, p.agent.ID)
		}
	}

	// Build lookup.
	pairMap := make(map[string]map[string]scoredPair)
	for _, p := range pairs {
		if pairMap[p.task.ID] == nil {
			pairMap[p.task.ID] = make(map[string]scoredPair)
		}
		pairMap[p.task.ID][p.agent.ID] = p
	}

	var result []scoredPair
	agentIdx := 0
	for _, taskID := range taskOrder {
		if agentIdx >= len(agentOrder) {
			agentIdx = 0
		}
		agentID := agentOrder[agentIdx]
		if p, ok := pairMap[taskID][agentID]; ok {
			result = append(result, p)
		}
		agentIdx++
	}
	return result
}

func buildReason(p scoredPair, strategy Strategy) string {
	parts := make([]string, 0, 4)

	if p.breakdown.AgentTypeBonus > 0 {
		parts = append(parts, "good type match")
	}
	if p.breakdown.CriticalPathBonus > 0 {
		parts = append(parts, "on critical path")
	}
	if p.breakdown.ProfileTagBonus > 0 {
		parts = append(parts, "tag affinity")
	}
	if p.breakdown.FocusPatternBonus > 0 {
		parts = append(parts, "file focus match")
	}
	if p.breakdown.ContextPenalty > 0 {
		parts = append(parts, "context pressure")
	}
	if p.breakdown.FileOverlapPenalty > 0 {
		parts = append(parts, "file conflict")
	}

	if len(parts) == 0 {
		return "default assignment"
	}
	return strings.Join(parts, ", ")
}
