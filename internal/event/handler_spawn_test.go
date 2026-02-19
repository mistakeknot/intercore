package event

import (
	"bytes"
	"context"
	"errors"
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
	h := NewSpawnHandler(q, s, &buf)

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
	h := NewSpawnHandler(q, s, &buf)

	e := Event{Source: SourcePhase, Type: "advance", RunID: "run001", ToState: "executing"}
	h(context.Background(), e)

	if len(s.spawned) != 2 {
		t.Errorf("spawned %d, want 2 (ok1 and ok2)", len(s.spawned))
	}
	if !bytes.Contains(buf.Bytes(), []byte("fail1 failed")) {
		t.Error("expected failure log for fail1")
	}
}
