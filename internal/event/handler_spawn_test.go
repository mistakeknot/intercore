package event

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type mockQuerier struct {
	agents []string
	err    error
}

func (m *mockQuerier) ListPendingAgentIDs(ctx context.Context, runID string) ([]string, error) {
	return m.agents, m.err
}

type mockSpawner struct {
	spawned []string
	failIDs map[string]bool
}

func (m *mockSpawner) SpawnByAgentID(ctx context.Context, agentID string) error {
	if m.failIDs[agentID] {
		return errors.New("spawn failed")
	}
	m.spawned = append(m.spawned, agentID)
	return nil
}

func TestSpawnHandler_TriggersOnExecuting(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1", "agent2"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewSpawnHandler(q, s, logger)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "planned",
		ToState:   "executing",
		Timestamp: time.Now(),
	}

	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}

	if len(s.spawned) != 2 {
		t.Errorf("spawned %d agents, want 2", len(s.spawned))
	}
}

func TestSpawnHandler_IgnoresNonExecuting(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	e := Event{Source: SourcePhase, Type: "advance", ToState: "strategized"}
	h(context.Background(), e)

	if len(s.spawned) != 0 {
		t.Errorf("should not spawn for non-executing phase, spawned %d", len(s.spawned))
	}
}

func TestSpawnHandler_IgnoresDispatchEvents(t *testing.T) {
	q := &mockQuerier{agents: []string{"agent1"}}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	e := Event{Source: SourceDispatch, Type: "status_change", ToState: "executing"}
	h(context.Background(), e)

	if len(s.spawned) != 0 {
		t.Errorf("should not spawn for dispatch events")
	}
}

func TestSpawnHandler_NoAgents(t *testing.T) {
	q := &mockQuerier{agents: nil}
	s := &mockSpawner{failIDs: map[string]bool{}}
	h := NewSpawnHandler(q, s, nil)

	e := Event{Source: SourcePhase, Type: "advance", RunID: "run001", ToState: "executing"}
	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.spawned) != 0 {
		t.Errorf("should not spawn with no agents")
	}
}

func TestSpawnHandler_PartialFailure(t *testing.T) {
	q := &mockQuerier{agents: []string{"ok1", "fail1", "ok2"}}
	s := &mockSpawner{failIDs: map[string]bool{"fail1": true}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewSpawnHandler(q, s, logger)

	e := Event{Source: SourcePhase, Type: "advance", RunID: "run001", ToState: "executing"}
	h(context.Background(), e)

	if len(s.spawned) != 2 {
		t.Errorf("spawned %d, want 2 (ok1 and ok2)", len(s.spawned))
	}
	logOut := buf.String()
	if !strings.Contains(logOut, "auto-spawn failed") {
		t.Errorf("expected failure log for fail1, got: %s", logOut)
	}
	if !strings.Contains(logOut, "fail1") {
		t.Errorf("expected agent_id fail1 in log, got: %s", logOut)
	}
}

// TestSpawnWiringIntegration tests the full Notifier -> SpawnHandler -> mock chain,
// mirroring how cmdRunAdvance wires the spawn handler at run.go.
func TestSpawnWiringIntegration(t *testing.T) {
	// 1. Create notifier (same as cmdRunAdvance)
	notifier := NewNotifier()

	// 2. Create mocks matching existing patterns
	q := &mockQuerier{agents: []string{"agent-1", "agent-2"}}
	var spawned []string
	spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
		spawned = append(spawned, agentID)
		return nil
	})

	// 3. Subscribe spawn handler (same pattern as cmdRunAdvance)
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	notifier.Subscribe("spawn", NewSpawnHandler(q, spawner, logger))

	// 4. Fire a phase event to "executing" (simulates Advance callback)
	ctx := context.Background()
	if err := notifier.Notify(ctx, Event{
		RunID:     "test-run-1",
		Source:    SourcePhase,
		Type:      "advance",
		FromState: "planned",
		ToState:   "executing",
		Timestamp: time.Now(),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// 5. Assert spawns happened
	if len(spawned) != 2 {
		t.Fatalf("spawned %d agents, want 2", len(spawned))
	}
	if spawned[0] != "agent-1" || spawned[1] != "agent-2" {
		t.Errorf("spawned = %v, want [agent-1 agent-2]", spawned)
	}

	// 6. Assert log output contains structured spawn messages
	logStr := logBuf.String()
	if !strings.Contains(logStr, "auto-spawn started") {
		t.Errorf("missing spawn log for agent-1 in: %s", logStr)
	}
	if !strings.Contains(logStr, "agent-1") {
		t.Errorf("missing agent-1 in log: %s", logStr)
	}
	if !strings.Contains(logStr, "agent-2") {
		t.Errorf("missing agent-2 in log: %s", logStr)
	}
}

// TestSpawnWiringIntegration_MultipleHandlers verifies that the spawn handler
// coexists with other handlers on the same notifier without interference.
func TestSpawnWiringIntegration_MultipleHandlers(t *testing.T) {
	notifier := NewNotifier()

	// Subscribe a log handler first (like cmdRunAdvance does)
	var logEvents []Event
	notifier.Subscribe("log", func(ctx context.Context, e Event) error {
		logEvents = append(logEvents, e)
		return nil
	})

	// Subscribe spawn handler
	q := &mockQuerier{agents: []string{"agent-1"}}
	var spawned []string
	spawner := AgentSpawnerFunc(func(ctx context.Context, agentID string) error {
		spawned = append(spawned, agentID)
		return nil
	})
	notifier.Subscribe("spawn", NewSpawnHandler(q, spawner, nil))

	// Fire event
	ctx := context.Background()
	if err := notifier.Notify(ctx, Event{
		RunID:   "run-1",
		Source:  SourcePhase,
		Type:    "advance",
		ToState: "executing",
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	// Both handlers should have fired
	if len(logEvents) != 1 {
		t.Errorf("log handler received %d events, want 1", len(logEvents))
	}
	if len(spawned) != 1 {
		t.Errorf("spawn handler spawned %d agents, want 1", len(spawned))
	}

	// Verify handler count
	if notifier.HandlerCount() != 2 {
		t.Errorf("handler count = %d, want 2", notifier.HandlerCount())
	}
}
