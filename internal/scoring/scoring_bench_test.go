package scoring

import (
	"fmt"
	"testing"
)

func makeAgents(n int) []Agent {
	agents := make([]Agent, n)
	for i := range agents {
		agents[i] = Agent{
			ID:           fmt.Sprintf("agent-%d", i),
			Name:         fmt.Sprintf("Agent %d", i),
			Type:         []string{"claude", "codex", "gemini"}[i%3],
			ContextUsage: float64(20 + i*5),
			Tags:         []string{"go", "review", "test"}[:1+(i%3)],
			FilePatterns: []string{"src/*.go", "internal/**/*.go", "cmd/*.go"}[:1+(i%3)],
			Reservations: []string{fmt.Sprintf("src/file%d.go", i)},
		}
	}
	return agents
}

func makeTasks(n int) []Task {
	tasks := make([]Task, n)
	for i := range tasks {
		tasks[i] = Task{
			ID:       fmt.Sprintf("task-%d", i),
			Title:    fmt.Sprintf("Task %d", i),
			Type:     []string{"feature", "bug", "chore", "epic"}[i%4],
			Priority: i % 5,
			Score:    float64(10-i%5) / 10.0,
			Tags:     []string{"go", "backend", "api"}[:1+(i%3)],
			Files:    []string{fmt.Sprintf("src/handler%d.go", i), fmt.Sprintf("internal/svc%d.go", i)},
		}
	}
	return tasks
}

func BenchmarkAssign5x3(b *testing.B) {
	tasks := makeTasks(5)
	agents := makeAgents(3)
	cfg := DefaultConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Assign(tasks, agents, StrategyQuality, cfg)
	}
}

func BenchmarkAssign20x8(b *testing.B) {
	tasks := makeTasks(20)
	agents := makeAgents(8)
	cfg := DefaultConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Assign(tasks, agents, StrategyQuality, cfg)
	}
}

func BenchmarkAssign50x15(b *testing.B) {
	tasks := makeTasks(50)
	agents := makeAgents(15)
	cfg := DefaultConfig()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = Assign(tasks, agents, StrategyBalanced, cfg)
	}
}

func BenchmarkScoreAllPairs10x5(b *testing.B) {
	tasks := makeTasks(10)
	agents := makeAgents(5)
	cfg := DefaultConfig()

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

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = scoreAllPairs(ctx, cfg)
	}
}
