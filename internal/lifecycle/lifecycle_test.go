package lifecycle

import (
	"sync"
	"testing"
	"time"
)

func TestNewMachine_StartsWaiting(t *testing.T) {
	m := NewMachine("agent-1", DefaultDetectionConfig())
	if m.State() != StateWaiting {
		t.Errorf("expected Waiting, got %s", m.State())
	}
	if m.AgentID() != "agent-1" {
		t.Errorf("expected agent-1, got %s", m.AgentID())
	}
}

func TestTransition_ValidPath(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())

	// Waiting -> Generating -> Thinking -> Idle -> Completed
	steps := []State{StateGenerating, StateThinking, StateIdle, StateCompleted}
	for _, s := range steps {
		if err := m.Transition(s, "test"); err != nil {
			t.Fatalf("valid transition to %s failed: %v", s, err)
		}
	}

	if m.State() != StateCompleted {
		t.Errorf("expected Completed, got %s", m.State())
	}

	history := m.History()
	if len(history) != len(steps) {
		t.Errorf("expected %d history entries, got %d", len(steps), len(history))
	}
}

func TestTransition_InvalidPath(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())

	// Waiting -> Idle is not valid.
	err := m.Transition(StateIdle, "invalid")
	if err == nil {
		t.Fatal("expected error for invalid transition Waiting -> Idle")
	}
}

func TestTransition_SameState_NoOp(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())
	m.Transition(StateGenerating, "start")

	// Generating -> Generating should be a no-op.
	if err := m.Transition(StateGenerating, "same"); err != nil {
		t.Fatalf("same-state transition should be no-op, got: %v", err)
	}

	// Should only have 1 transition in history.
	if len(m.History()) != 1 {
		t.Errorf("expected 1 transition, got %d", len(m.History()))
	}
}

func TestTransition_CompletedIsTerminal(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())
	m.Transition(StateGenerating, "start")
	m.Transition(StateCompleted, "done")

	// No transitions allowed from Completed.
	for _, s := range []State{StateWaiting, StateGenerating, StateThinking, StateIdle, StateStalled, StateError} {
		if err := m.Transition(s, "attempt"); err == nil {
			t.Errorf("expected error transitioning from Completed to %s", s)
		}
	}
}

func TestTransition_ErrorCanRecover(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())
	m.Transition(StateGenerating, "start")
	m.Transition(StateError, "crash")

	// Error -> Waiting (retry).
	if err := m.Transition(StateWaiting, "retry"); err != nil {
		t.Fatalf("Error -> Waiting should be valid: %v", err)
	}

	// Error -> Idle (reassign).
	m.Transition(StateGenerating, "start")
	m.Transition(StateError, "crash")
	if err := m.Transition(StateIdle, "reassign"); err != nil {
		t.Fatalf("Error -> Idle should be valid: %v", err)
	}
}

func TestOnTransition_Callback(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())

	var called []Transition
	m.OnTransition(func(tr Transition) {
		called = append(called, tr)
	})

	m.Transition(StateGenerating, "start")
	m.Transition(StateThinking, "plan")

	if len(called) != 2 {
		t.Fatalf("expected 2 callbacks, got %d", len(called))
	}
	if called[0].From != StateWaiting || called[0].To != StateGenerating {
		t.Errorf("first callback: %s -> %s", called[0].From, called[0].To)
	}
	if called[1].From != StateGenerating || called[1].To != StateThinking {
		t.Errorf("second callback: %s -> %s", called[1].From, called[1].To)
	}
}

func TestRecordActivity_UpdatesLastActivity(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())
	before := m.LastActivity()

	time.Sleep(1 * time.Millisecond)
	m.RecordActivity(100)

	after := m.LastActivity()
	if !after.After(before) {
		t.Error("LastActivity should be updated after RecordActivity")
	}
}

func TestVelocity(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.VelocityWindow = 100 * time.Millisecond
	m := NewMachine("a1", cfg)

	// Record 100 tokens.
	m.RecordActivity(100)

	// Velocity = 100 tokens / (100ms in minutes) = 100 / (1/600) = 60000.
	vel := m.Velocity()
	if vel <= 0 {
		t.Errorf("expected positive velocity, got %v", vel)
	}
}

func TestCheckStall_Timeout(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.StallTimeout = 1 * time.Millisecond
	cfg.VelocityWindow = 1 * time.Millisecond
	cfg.MinTokensPerMin = 0 // Disable velocity check
	m := NewMachine("a1", cfg)

	m.Transition(StateGenerating, "start")
	time.Sleep(2 * time.Millisecond)

	if !m.CheckStall() {
		t.Error("expected stall after timeout")
	}
	if m.State() != StateStalled {
		t.Errorf("expected Stalled, got %s", m.State())
	}
}

func TestCheckStall_VelocityBased(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.StallTimeout = 1 * time.Hour // Disable timeout check
	cfg.VelocityWindow = 1 * time.Millisecond
	cfg.MinTokensPerMin = 1000000
	m := NewMachine("a1", cfg)

	m.Transition(StateGenerating, "start")
	// Record tiny activity to satisfy the "sinceActivity > VelocityWindow" guard.
	m.RecordActivity(1)
	time.Sleep(2 * time.Millisecond)

	if !m.CheckStall() {
		t.Error("expected velocity-based stall")
	}
}

func TestCheckStall_OnlyActiveStates(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.StallTimeout = 1 * time.Millisecond
	m := NewMachine("a1", cfg)

	time.Sleep(2 * time.Millisecond)

	// Waiting state should not trigger stall.
	if m.CheckStall() {
		t.Error("Waiting state should not become stalled")
	}

	// Idle state should not trigger stall.
	m.Transition(StateGenerating, "start")
	m.Transition(StateIdle, "pause")
	time.Sleep(2 * time.Millisecond)
	if m.CheckStall() {
		t.Error("Idle state should not become stalled")
	}
}

func TestCheckStall_RecoveryFromStalled(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.StallTimeout = 1 * time.Millisecond
	m := NewMachine("a1", cfg)

	m.Transition(StateGenerating, "start")
	time.Sleep(2 * time.Millisecond)
	m.CheckStall()

	if m.State() != StateStalled {
		t.Fatalf("expected Stalled, got %s", m.State())
	}

	// Recovery: Stalled -> Generating.
	if err := m.Transition(StateGenerating, "resumed"); err != nil {
		t.Fatalf("recovery from stalled should work: %v", err)
	}
}

func TestSnapshot(t *testing.T) {
	m := NewMachine("a1", DefaultDetectionConfig())
	m.Transition(StateGenerating, "start")
	m.RecordActivity(50)

	snap := m.Snapshot()
	if snap.AgentID != "a1" {
		t.Errorf("expected a1, got %s", snap.AgentID)
	}
	if snap.State != StateGenerating {
		t.Errorf("expected Generating, got %s", snap.State)
	}
	if snap.TransitionCount != 1 {
		t.Errorf("expected 1 transition, got %d", snap.TransitionCount)
	}
}

func TestRegistry_GetCreatesNew(t *testing.T) {
	r := NewRegistry(DefaultDetectionConfig())

	m1 := r.Get("a1")
	m2 := r.Get("a1")

	if m1 != m2 {
		t.Error("Get should return same machine for same agent")
	}

	m3 := r.Get("a2")
	if m1 == m3 {
		t.Error("different agents should get different machines")
	}

	agents := r.Agents()
	if len(agents) != 2 {
		t.Errorf("expected 2 agents, got %d", len(agents))
	}
}

func TestRegistry_Remove(t *testing.T) {
	r := NewRegistry(DefaultDetectionConfig())
	r.Get("a1")
	r.Remove("a1")

	if len(r.Agents()) != 0 {
		t.Error("expected 0 agents after remove")
	}
}

func TestRegistry_Snapshots(t *testing.T) {
	r := NewRegistry(DefaultDetectionConfig())
	r.Get("a1").Transition(StateGenerating, "start")
	r.Get("a2").Transition(StateGenerating, "start")

	snaps := r.Snapshots()
	if len(snaps) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(snaps))
	}
}

func TestRegistry_CheckStalls(t *testing.T) {
	cfg := DefaultDetectionConfig()
	cfg.StallTimeout = 1 * time.Millisecond
	cfg.MinTokensPerMin = 0
	r := NewRegistry(cfg)

	r.Get("a1").Transition(StateGenerating, "start")
	r.Get("a2").Transition(StateThinking, "plan")
	r.Get("a3") // Waiting — should not stall

	time.Sleep(2 * time.Millisecond)

	stalled := r.CheckStalls()
	if len(stalled) != 2 {
		t.Errorf("expected 2 stalled agents, got %d: %v", len(stalled), stalled)
	}
}

func TestRegistry_ConcurrentAccess(t *testing.T) {
	r := NewRegistry(DefaultDetectionConfig())
	var wg sync.WaitGroup

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			agentID := "agent-concurrent"
			m := r.Get(agentID)
			m.RecordActivity(10)
			_ = m.Snapshot()
		}(i)
	}

	wg.Wait()

	agents := r.Agents()
	if len(agents) != 1 {
		t.Errorf("expected 1 agent, got %d", len(agents))
	}
}

func TestAllTransitionPaths(t *testing.T) {
	// Verify every documented valid transition works.
	for from, targets := range validTransitions {
		for _, to := range targets {
			m := NewMachine("test", DefaultDetectionConfig())
			// Force the machine to the 'from' state.
			// We use a helper to skip if we can't reach 'from' via valid paths.
			if !reachState(m, from) {
				t.Logf("skip: cannot reach %s", from)
				continue
			}
			if err := m.Transition(to, "test"); err != nil {
				t.Errorf("transition %s -> %s should be valid: %v", from, to, err)
			}
		}
	}
}

// reachState tries to get the machine to a target state via valid transitions.
func reachState(m *Machine, target State) bool {
	if m.State() == target {
		return true
	}
	// Simple BFS-like path.
	paths := map[State][]State{
		StateWaiting:    {},
		StateGenerating: {StateGenerating},
		StateThinking:   {StateGenerating, StateThinking},
		StateIdle:       {StateGenerating, StateIdle},
		StateStalled:    {StateGenerating, StateStalled},
		StateError:      {StateGenerating, StateError},
		StateCompleted:  {StateGenerating, StateCompleted},
	}

	path, ok := paths[target]
	if !ok {
		return false
	}
	for _, s := range path {
		if err := m.Transition(s, "reach"); err != nil {
			return false
		}
	}
	return m.State() == target
}
