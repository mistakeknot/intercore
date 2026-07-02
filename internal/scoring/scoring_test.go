package scoring

import (
	"fmt"
	"math"
	"testing"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestAssign_Empty(t *testing.T) {
	// No tasks or agents should return nil.
	if got := Assign(nil, []Agent{{ID: "a1"}}, StrategyBalanced, DefaultConfig()); got != nil {
		t.Errorf("expected nil for empty tasks, got %v", got)
	}
	if got := Assign([]Task{{ID: "t1"}}, nil, StrategyBalanced, DefaultConfig()); got != nil {
		t.Errorf("expected nil for empty agents, got %v", got)
	}
}

func TestAssign_SinglePair(t *testing.T) {
	tasks := []Task{{ID: "t1", Title: "test", Type: "task", Priority: 2, Score: 0.5}}
	agents := []Agent{{ID: "a1", Name: "claude-1", Type: "claude"}}

	assignments := Assign(tasks, agents, StrategyBalanced, DefaultConfig())
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}
	if assignments[0].TaskID != "t1" || assignments[0].AgentID != "a1" {
		t.Errorf("unexpected assignment: task=%s agent=%s", assignments[0].TaskID, assignments[0].AgentID)
	}
}

func TestEstimateComplexity(t *testing.T) {
	tests := []struct {
		name string
		task Task
		want float64
	}{
		{"epic high complexity", Task{Type: "epic", Priority: 3}, 0.9},
		{"chore low complexity", Task{Type: "chore", Priority: 0}, 0.2},
		{"task normal", Task{Type: "task", Priority: 2}, 0.4},
		{"feature normal", Task{Type: "feature", Priority: 2}, 0.7},
		{"bug with many unblocks", Task{Type: "bug", Priority: 2, UnblocksIDs: []string{"a", "b", "c", "d", "e"}}, 0.65},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateComplexity(&tt.task)
			if !almostEqual(got, tt.want) {
				t.Errorf("estimateComplexity() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEstimateComplexity_Clamped(t *testing.T) {
	// Very low should clamp to 0.
	task := Task{Type: "chore", Priority: 0}
	got := estimateComplexity(&task)
	if got < 0 || got > 1 {
		t.Errorf("complexity out of range [0,1]: %v", got)
	}
}

func TestAgentTypeBonus(t *testing.T) {
	highComplexity := Task{Type: "epic", Priority: 3} // ~0.9
	lowComplexity := Task{Type: "chore", Priority: 0} // ~0.2

	// Claude gets bonus for high complexity.
	if bonus := agentTypeBonus("claude", &highComplexity); bonus <= 0 {
		t.Errorf("claude should get bonus for high complexity, got %v", bonus)
	}
	// Codex gets bonus for low complexity.
	if bonus := agentTypeBonus("codex", &lowComplexity); bonus <= 0 {
		t.Errorf("codex should get bonus for low complexity, got %v", bonus)
	}
	// Claude gets penalty for low complexity.
	if bonus := agentTypeBonus("claude", &lowComplexity); bonus >= 0 {
		t.Errorf("claude should get penalty for low complexity, got %v", bonus)
	}
	// Codex gets penalty for high complexity.
	if bonus := agentTypeBonus("codex", &highComplexity); bonus >= 0 {
		t.Errorf("codex should get penalty for high complexity, got %v", bonus)
	}
}

func TestProfileTagBonus(t *testing.T) {
	// Full match.
	got := profileTagBonus([]string{"go", "backend"}, []string{"go", "backend"}, 0.15)
	if !almostEqual(got, 0.15) {
		t.Errorf("full match should give full weight, got %v", got)
	}

	// Partial match.
	got = profileTagBonus([]string{"go"}, []string{"go", "backend"}, 0.15)
	if !almostEqual(got, 0.075) {
		t.Errorf("half match should give half weight, got %v", got)
	}

	// No match.
	got = profileTagBonus([]string{"python"}, []string{"go", "backend"}, 0.15)
	if got != 0 {
		t.Errorf("no match should be 0, got %v", got)
	}

	// Empty task tags.
	got = profileTagBonus([]string{"go"}, nil, 0.15)
	if got != 0 {
		t.Errorf("empty task tags should be 0, got %v", got)
	}

	// Case insensitive.
	got = profileTagBonus([]string{"Go"}, []string{"go"}, 0.15)
	if !almostEqual(got, 0.15) {
		t.Errorf("case insensitive match should work, got %v", got)
	}
}

func TestFocusPatternBonus(t *testing.T) {
	// Pattern matches all files.
	got := focusPatternBonus([]string{"internal/"}, []string{"internal/foo.go", "internal/bar.go"}, 0.10)
	if !almostEqual(got, 0.10) {
		t.Errorf("all files match should give full weight, got %v", got)
	}

	// Pattern matches some files.
	got = focusPatternBonus([]string{"internal/"}, []string{"internal/foo.go", "cmd/main.go"}, 0.10)
	if !almostEqual(got, 0.05) {
		t.Errorf("half match should give half weight, got %v", got)
	}

	// No match.
	got = focusPatternBonus([]string{"pkg/"}, []string{"internal/foo.go"}, 0.10)
	if got != 0 {
		t.Errorf("no match should be 0, got %v", got)
	}

	// Empty inputs.
	if focusPatternBonus(nil, []string{"a.go"}, 0.10) != 0 {
		t.Error("empty patterns should be 0")
	}
	if focusPatternBonus([]string{"*.go"}, nil, 0.10) != 0 {
		t.Error("empty files should be 0")
	}
}

func TestMatchesPattern(t *testing.T) {
	tests := []struct {
		file, pattern string
		want          bool
	}{
		{"internal/foo.go", "internal/", true},
		{"internal/sub/bar.go", "internal/", true},
		{"cmd/main.go", "internal/", false},
		{"foo.go", "*.go", true},
		{"foo.ts", "*.go", false},
		{"internal/foo.go", "*.go", true},
		{"internal/foo.go", "internal/foo.go", true},
		{"foo_test.go", "test", true},
		{"", "*.go", false},
		{"foo.go", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.file+"~"+tt.pattern, func(t *testing.T) {
			got := matchesPattern(tt.file, tt.pattern)
			if got != tt.want {
				t.Errorf("matchesPattern(%q, %q) = %v, want %v", tt.file, tt.pattern, got, tt.want)
			}
		})
	}
}

func TestCriticalPathBonus(t *testing.T) {
	tests := []struct {
		name     string
		unblocks int
		wantMin  float64
	}{
		{"no unblocks", 0, 0},
		{"1 unblock", 1, 0.1},
		{"3 unblocks", 3, 0.15},
		{"5 unblocks", 5, 0.2},
		{"10 unblocks", 10, 0.2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := make([]string, tt.unblocks)
			for i := range ids {
				ids[i] = "x"
			}
			task := Task{UnblocksIDs: ids}
			got := criticalPathBonus(&task)
			if !almostEqual(got, tt.wantMin) {
				t.Errorf("criticalPathBonus(%d) = %v, want %v", tt.unblocks, got, tt.wantMin)
			}
		})
	}
}

func TestFileOverlapPenalty(t *testing.T) {
	reservations := map[string][]string{
		"a1": {"main.go", "config.go"},
		"a2": {"main.go"},
	}

	// Agent a1 assigning task touching main.go — only a2's reservation counts.
	got := fileOverlapPenalty("a1", []string{"main.go"}, reservations)
	if !almostEqual(got, 0.1) {
		t.Errorf("expected 0.1 penalty, got %v", got)
	}

	// Agent a2 — a1 has main.go reserved.
	got = fileOverlapPenalty("a2", []string{"main.go"}, reservations)
	if !almostEqual(got, 0.1) {
		t.Errorf("expected 0.1 penalty, got %v", got)
	}

	// No overlap.
	got = fileOverlapPenalty("a1", []string{"new.go"}, reservations)
	if got != 0 {
		t.Errorf("expected 0 penalty for non-overlapping files, got %v", got)
	}

	// Capped at 0.5.
	reservations["a2"] = []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}
	got = fileOverlapPenalty("a1", []string{"a.go", "b.go", "c.go", "d.go", "e.go", "f.go"}, reservations)
	if got > 0.5 {
		t.Errorf("penalty should be capped at 0.5, got %v", got)
	}
}

func TestContextPenalty(t *testing.T) {
	// Below threshold — no penalty.
	if got := contextPenalty(50, 80); got != 0 {
		t.Errorf("below threshold should be 0, got %v", got)
	}
	// At threshold — no penalty.
	if got := contextPenalty(80, 80); got != 0 {
		t.Errorf("at threshold should be 0, got %v", got)
	}
	// At 90% with 80% threshold — 50% excess → 0.15 penalty.
	got := contextPenalty(90, 80)
	if !almostEqual(got, 0.15) {
		t.Errorf("contextPenalty(90, 80) = %v, want 0.15", got)
	}
	// At 100% — full 0.3 penalty.
	got = contextPenalty(100, 80)
	if !almostEqual(got, 0.3) {
		t.Errorf("contextPenalty(100, 80) = %v, want 0.3", got)
	}
}

func TestStrategyQuality(t *testing.T) {
	tasks := []Task{
		{ID: "t1", Score: 0.5},
		{ID: "t2", Score: 0.8},
	}
	agents := []Agent{
		{ID: "a1", Type: "claude"},
		{ID: "a2", Type: "codex"},
	}

	cfg := DefaultConfig()
	cfg.UseAgentProfiles = false // Disable to test pure score-based assignment.
	cfg.PreferCriticalPath = false
	cfg.PenalizeFileOverlap = false
	cfg.BudgetAware = false

	assignments := Assign(tasks, agents, StrategyQuality, cfg)
	if len(assignments) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(assignments))
	}

	// Higher-scored task should be assigned first.
	if assignments[0].TaskID != "t2" {
		t.Errorf("first assignment should be highest scored task t2, got %s", assignments[0].TaskID)
	}
}

func TestStrategyRoundRobin(t *testing.T) {
	tasks := []Task{
		{ID: "t1", Score: 0.5},
		{ID: "t2", Score: 0.5},
		{ID: "t3", Score: 0.5},
	}
	agents := []Agent{
		{ID: "a1"},
		{ID: "a2"},
	}

	cfg := DefaultConfig()
	cfg.UseAgentProfiles = false
	cfg.PreferCriticalPath = false
	cfg.PenalizeFileOverlap = false
	cfg.BudgetAware = false

	assignments := Assign(tasks, agents, StrategyRoundRobin, cfg)
	if len(assignments) != 3 {
		t.Fatalf("expected 3 assignments, got %d", len(assignments))
	}

	// Round-robin should alternate agents.
	if assignments[0].AgentID != "a1" {
		t.Errorf("assignment 0 should be a1, got %s", assignments[0].AgentID)
	}
	if assignments[1].AgentID != "a2" {
		t.Errorf("assignment 1 should be a2, got %s", assignments[1].AgentID)
	}
	if assignments[2].AgentID != "a1" {
		t.Errorf("assignment 2 should wrap around to a1, got %s", assignments[2].AgentID)
	}
}

func TestStrategyDependency(t *testing.T) {
	tasks := []Task{
		{ID: "t1", Score: 0.9, UnblocksIDs: nil},
		{ID: "t2", Score: 0.3, UnblocksIDs: []string{"t1", "t3", "t4"}},
	}
	agents := []Agent{{ID: "a1"}}

	cfg := DefaultConfig()

	assignments := Assign(tasks, agents, StrategyDependency, cfg)
	if len(assignments) != 2 {
		t.Fatalf("expected 2 assignments, got %d", len(assignments))
	}

	// Task t2 unblocks more, should be first despite lower base score.
	if assignments[0].TaskID != "t2" {
		t.Errorf("dependency strategy should prioritize t2 (3 unblocks), got %s", assignments[0].TaskID)
	}
}

func TestStrategyBalanced_EvenDistribution(t *testing.T) {
	tasks := make([]Task, 6)
	for i := range tasks {
		tasks[i] = Task{ID: "t" + string(rune('1'+i)), Score: 0.5}
	}
	agents := []Agent{
		{ID: "a1"},
		{ID: "a2"},
		{ID: "a3"},
	}

	cfg := DefaultConfig()
	cfg.UseAgentProfiles = false
	cfg.PreferCriticalPath = false
	cfg.PenalizeFileOverlap = false
	cfg.BudgetAware = false

	assignments := Assign(tasks, agents, StrategyBalanced, cfg)

	// Count assignments per agent.
	load := make(map[string]int)
	for _, a := range assignments {
		load[a.AgentID]++
	}

	// No agent should have zero assignments.
	for _, agent := range agents {
		if load[agent.ID] == 0 {
			t.Errorf("agent %s got no assignments in balanced mode", agent.ID)
		}
	}
}

func TestBreakdown_Populated(t *testing.T) {
	tasks := []Task{{
		ID: "t1", Type: "epic", Priority: 3, Score: 0.7,
		Tags:        []string{"go", "backend"},
		Files:       []string{"internal/foo.go"},
		UnblocksIDs: []string{"t2", "t3", "t4"},
	}}
	agents := []Agent{{
		ID: "a1", Type: "claude",
		Tags:         []string{"go"},
		FilePatterns: []string{"internal/"},
		ContextUsage: 90,
	}}

	assignments := Assign(tasks, agents, StrategyQuality, DefaultConfig())
	if len(assignments) != 1 {
		t.Fatalf("expected 1 assignment, got %d", len(assignments))
	}

	b := assignments[0].Breakdown
	if b.BaseScore != 0.7 {
		t.Errorf("base score should be 0.7, got %v", b.BaseScore)
	}
	if b.AgentTypeBonus <= 0 {
		t.Errorf("claude should get agent type bonus for epic, got %v", b.AgentTypeBonus)
	}
	if b.CriticalPathBonus <= 0 {
		t.Errorf("3 unblocks should give critical path bonus, got %v", b.CriticalPathBonus)
	}
	if b.ProfileTagBonus <= 0 {
		t.Errorf("matching 'go' tag should give profile bonus, got %v", b.ProfileTagBonus)
	}
	if b.FocusPatternBonus <= 0 {
		t.Errorf("matching 'internal/' pattern should give focus bonus, got %v", b.FocusPatternBonus)
	}
	if b.ContextPenalty <= 0 {
		t.Errorf("90%% context usage should incur penalty, got %v", b.ContextPenalty)
	}
}

func TestBuildReason(t *testing.T) {
	// All factors active.
	p := scoredPair{
		breakdown: Breakdown{
			AgentTypeBonus:     0.1,
			CriticalPathBonus:  0.15,
			ProfileTagBonus:    0.05,
			FocusPatternBonus:  0.03,
			ContextPenalty:     0.1,
			FileOverlapPenalty: 0.05,
		},
	}
	reason := buildReason(p, nil, StrategyBalanced)
	for _, substr := range []string{"type match", "critical path", "tag affinity", "file focus", "context pressure", "file conflict"} {
		if !contains(reason, substr) {
			t.Errorf("reason %q missing %q", reason, substr)
		}
	}

	// No factors.
	p2 := scoredPair{breakdown: Breakdown{}}
	if buildReason(p2, nil, StrategyBalanced) != "default assignment" {
		t.Errorf("empty breakdown should give 'default assignment'")
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.PreferCriticalPath {
		t.Error("PreferCriticalPath should default to true")
	}
	if !cfg.PenalizeFileOverlap {
		t.Error("PenalizeFileOverlap should default to true")
	}
	if !cfg.UseAgentProfiles {
		t.Error("UseAgentProfiles should default to true")
	}
	if !cfg.BudgetAware {
		t.Error("BudgetAware should default to true")
	}
	if cfg.ContextThreshold != 80 {
		t.Errorf("ContextThreshold should be 80, got %v", cfg.ContextThreshold)
	}
	if cfg.ProfileTagBoostWeight != 0.15 {
		t.Errorf("ProfileTagBoostWeight should be 0.15, got %v", cfg.ProfileTagBoostWeight)
	}
	if cfg.FocusPatternBoostWeight != 0.10 {
		t.Errorf("FocusPatternBoostWeight should be 0.10, got %v", cfg.FocusPatternBoostWeight)
	}
}

func TestScoreOne_AllDisabled(t *testing.T) {
	cfg := Config{} // All false/zero
	agents := []Agent{{ID: "a1", ContextUsage: 95}}
	tasks := []Task{{ID: "t1", Score: 0.5, UnblocksIDs: []string{"t2"}, Files: []string{"main.go"}}}

	ctx := &scoringContext{
		tasks:        tasks,
		agents:       agents,
		reservations: map[string][]string{},
	}
	var pair scoredPair
	scoreOne(&pair, 0, 0, ctx, cfg)

	// Only base score should remain.
	if !almostEqual(pair.score, 0.5) {
		t.Errorf("with all features disabled, score should equal base score 0.5, got %v", pair.score)
	}
	if pair.breakdown.AgentTypeBonus != 0 {
		t.Error("agent type bonus should be 0 when disabled")
	}
	if pair.breakdown.CriticalPathBonus != 0 {
		t.Error("critical path bonus should be 0 when disabled")
	}
	if pair.breakdown.ContextPenalty != 0 {
		t.Error("context penalty should be 0 when disabled")
	}
}

func TestFileOverlapPenalty_OwnReservationsIgnored(t *testing.T) {
	reservations := map[string][]string{
		"a1": {"main.go"},
	}
	// Agent a1's own reservation on main.go should not penalize it.
	got := fileOverlapPenalty("a1", []string{"main.go"}, reservations)
	if got != 0 {
		t.Errorf("agent's own reservations should not cause penalty, got %v", got)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsSubstr(s, substr))
}

func containsSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestSelectQuality_PerAgentCap(t *testing.T) {
	// 10 tasks, 5 agents. Cap = ceil(10/5)+1 = 3.
	// No agent should get more than 3 even if it scores highest on all tasks.
	tasks := make([]Task, 10)
	for i := range tasks {
		tasks[i] = Task{ID: fmt.Sprintf("t%d", i), Score: 0.5}
	}
	agents := make([]Agent, 5)
	for i := range agents {
		agents[i] = Agent{ID: fmt.Sprintf("a%d", i), Type: "claude"}
	}

	cfg := DefaultConfig()
	cfg.UseAgentProfiles = false
	cfg.PreferCriticalPath = false
	cfg.PenalizeFileOverlap = false
	cfg.BudgetAware = false

	assignments := Assign(tasks, agents, StrategyQuality, cfg)

	// All tasks must be assigned (no silent drops).
	if len(assignments) != 10 {
		t.Fatalf("expected 10 assignments, got %d", len(assignments))
	}

	// No agent gets more than 3.
	load := make(map[string]int)
	for _, a := range assignments {
		load[a.AgentID]++
	}
	for agentID, count := range load {
		if count > 3 {
			t.Errorf("agent %s got %d tasks, want ≤3", agentID, count)
		}
	}
}

func TestSelectQuality_ZeroAgents(t *testing.T) {
	tasks := []Task{{ID: "t1", Score: 0.5}}
	var agents []Agent

	cfg := DefaultConfig()
	assignments := Assign(tasks, agents, StrategyQuality, cfg)
	if len(assignments) != 0 {
		t.Fatalf("expected 0 assignments with no agents, got %d", len(assignments))
	}
}

func TestSelectBalanced_ZeroAgents(t *testing.T) {
	tasks := []Task{{ID: "t1", Score: 0.5}}
	var agents []Agent

	cfg := DefaultConfig()
	assignments := Assign(tasks, agents, StrategyBalanced, cfg)
	if len(assignments) != 0 {
		t.Fatalf("expected 0 assignments with no agents, got %d", len(assignments))
	}
}
