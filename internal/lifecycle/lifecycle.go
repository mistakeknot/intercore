// Package lifecycle provides a formalized agent state machine with monitoring.
//
// States: Waiting -> Generating -> Thinking -> (Idle | Error | Stalled)
//
// The machine tracks agent lifecycle transitions, emits events on state change,
// and supports activity velocity monitoring for stall detection.
//
// Inspired by ntm's internal/coordinator/coordinator.go, adapted for
// Intercore's dispatch model.
package lifecycle

import (
	"fmt"
	"sync"
	"time"
)

// State represents an agent's current lifecycle state.
type State string

const (
	StateWaiting    State = "waiting"    // Queued, not yet started
	StateGenerating State = "generating" // Actively producing output
	StateThinking   State = "thinking"   // Planning / reasoning
	StateIdle       State = "idle"       // Between tasks, awaiting assignment
	StateStalled    State = "stalled"    // No progress detected
	StateError      State = "error"      // Failed / crashed
	StateCompleted  State = "completed"  // Work finished successfully
)

// validTransitions defines the allowed state transitions.
// A state maps to the set of states it can transition to.
var validTransitions = map[State][]State{
	StateWaiting:    {StateGenerating, StateThinking, StateError, StateCompleted},
	StateGenerating: {StateThinking, StateIdle, StateStalled, StateError, StateCompleted},
	StateThinking:   {StateGenerating, StateIdle, StateStalled, StateError, StateCompleted},
	StateIdle:       {StateGenerating, StateThinking, StateStalled, StateError, StateCompleted},
	StateStalled:    {StateGenerating, StateThinking, StateIdle, StateError, StateCompleted},
	StateError:      {StateWaiting, StateIdle}, // Can retry from error
	StateCompleted:  {},                        // Terminal state
}

// Transition represents a state change event.
type Transition struct {
	AgentID   string    `json:"agent_id"`
	From      State     `json:"from"`
	To        State     `json:"to"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

// DetectionConfig controls stall and velocity detection thresholds.
type DetectionConfig struct {
	StallTimeout     time.Duration `json:"stall_timeout"`      // Duration without activity before marking stalled (default 5m)
	VelocityWindow   time.Duration `json:"velocity_window"`    // Sliding window for velocity calculation (default 1m)
	MinTokensPerMin  float64       `json:"min_tokens_per_min"` // Below this = stalled (default 10)
	PollInterval     time.Duration `json:"poll_interval"`      // How often to check for stalls (default 5s)
}

// DefaultDetectionConfig returns a DetectionConfig with sensible defaults.
func DefaultDetectionConfig() DetectionConfig {
	return DetectionConfig{
		StallTimeout:    5 * time.Minute,
		VelocityWindow:  1 * time.Minute,
		MinTokensPerMin: 10,
		PollInterval:    5 * time.Second,
	}
}

// ActivitySample records a point-in-time observation of agent output.
type ActivitySample struct {
	Timestamp time.Time `json:"timestamp"`
	TokensOut int64     `json:"tokens_out"`
}

// Machine tracks the lifecycle state of a single agent.
type Machine struct {
	mu             sync.RWMutex
	agentID        string
	state          State
	lastActivity   time.Time
	lastTransition time.Time
	config         DetectionConfig
	history        []Transition
	samples        []ActivitySample
	onTransition   func(Transition)
}

// NewMachine creates a lifecycle machine for an agent, starting in Waiting state.
func NewMachine(agentID string, cfg DetectionConfig) *Machine {
	now := time.Now()
	return &Machine{
		agentID:        agentID,
		state:          StateWaiting,
		lastActivity:   now,
		lastTransition: now,
		config:         cfg,
		history:        make([]Transition, 0, 16),
		samples:        make([]ActivitySample, 0, 64),
	}
}

// OnTransition registers a callback invoked on every state transition.
// The callback is called with the lock held, so it must not call back into the machine.
func (m *Machine) OnTransition(fn func(Transition)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onTransition = fn
}

// State returns the current state.
func (m *Machine) State() State {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

// AgentID returns the agent identifier.
func (m *Machine) AgentID() string {
	return m.agentID
}

// LastActivity returns the time of the last recorded activity.
func (m *Machine) LastActivity() time.Time {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.lastActivity
}

// History returns a copy of the transition history.
func (m *Machine) History() []Transition {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Transition, len(m.history))
	copy(out, m.history)
	return out
}

// Transition attempts to move the machine to a new state.
// Returns an error if the transition is not allowed.
func (m *Machine) Transition(to State, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.state == to {
		return nil // No-op for same state
	}

	if !m.canTransition(to) {
		return fmt.Errorf("lifecycle: invalid transition %s -> %s for agent %s", m.state, to, m.agentID)
	}

	now := time.Now()
	t := Transition{
		AgentID:   m.agentID,
		From:      m.state,
		To:        to,
		Reason:    reason,
		Timestamp: now,
	}

	m.state = to
	m.lastTransition = now
	m.lastActivity = now
	m.history = append(m.history, t)

	if m.onTransition != nil {
		m.onTransition(t)
	}

	return nil
}

// RecordActivity updates the last activity timestamp and adds a sample.
func (m *Machine) RecordActivity(tokensOut int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := time.Now()
	m.lastActivity = now
	m.samples = append(m.samples, ActivitySample{
		Timestamp: now,
		TokensOut: tokensOut,
	})

	// Prune old samples outside the velocity window.
	cutoff := now.Add(-m.config.VelocityWindow * 2)
	pruneIdx := 0
	for pruneIdx < len(m.samples) && m.samples[pruneIdx].Timestamp.Before(cutoff) {
		pruneIdx++
	}
	if pruneIdx > 0 {
		m.samples = m.samples[pruneIdx:]
	}
}

// Velocity returns the current token output rate (tokens per minute)
// over the configured velocity window.
func (m *Machine) Velocity() float64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.velocityLocked()
}

func (m *Machine) velocityLocked() float64 {
	now := time.Now()
	cutoff := now.Add(-m.config.VelocityWindow)

	var totalTokens int64
	for _, s := range m.samples {
		if s.Timestamp.After(cutoff) {
			totalTokens += s.TokensOut
		}
	}

	minutes := m.config.VelocityWindow.Minutes()
	if minutes <= 0 {
		return 0
	}
	return float64(totalTokens) / minutes
}

// CheckStall evaluates whether the agent should be marked as stalled.
// Returns true if the agent was transitioned to stalled state.
func (m *Machine) CheckStall() bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Only active states can become stalled.
	if m.state != StateGenerating && m.state != StateThinking {
		return false
	}

	now := time.Now()
	sinceActivity := now.Sub(m.lastActivity)

	// Check timeout-based stall.
	if sinceActivity >= m.config.StallTimeout {
		m.transitionLocked(StateStalled, fmt.Sprintf("no activity for %s", sinceActivity.Round(time.Second)))
		return true
	}

	// Check velocity-based stall.
	velocity := m.velocityLocked()
	if velocity < m.config.MinTokensPerMin && sinceActivity > m.config.VelocityWindow {
		m.transitionLocked(StateStalled, fmt.Sprintf("velocity %.1f tokens/min below threshold %.1f", velocity, m.config.MinTokensPerMin))
		return true
	}

	return false
}

func (m *Machine) canTransition(to State) bool {
	allowed, ok := validTransitions[m.state]
	if !ok {
		return false
	}
	for _, s := range allowed {
		if s == to {
			return true
		}
	}
	return false
}

func (m *Machine) transitionLocked(to State, reason string) {
	now := time.Now()
	t := Transition{
		AgentID:   m.agentID,
		From:      m.state,
		To:        to,
		Reason:    reason,
		Timestamp: now,
	}
	m.state = to
	m.lastTransition = now
	m.history = append(m.history, t)

	if m.onTransition != nil {
		m.onTransition(t)
	}
}

// Snapshot returns a point-in-time summary of the agent's state.
type Snapshot struct {
	AgentID        string    `json:"agent_id"`
	State          State     `json:"state"`
	LastActivity   time.Time `json:"last_activity"`
	LastTransition time.Time `json:"last_transition"`
	Velocity       float64   `json:"velocity_tokens_per_min"`
	TransitionCount int      `json:"transition_count"`
}

// Snapshot returns a point-in-time summary.
func (m *Machine) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Snapshot{
		AgentID:         m.agentID,
		State:           m.state,
		LastActivity:    m.lastActivity,
		LastTransition:  m.lastTransition,
		Velocity:        m.velocityLocked(),
		TransitionCount: len(m.history),
	}
}

// Registry tracks lifecycle machines for multiple agents.
type Registry struct {
	mu       sync.RWMutex
	machines map[string]*Machine
	config   DetectionConfig
}

// NewRegistry creates an agent lifecycle registry.
func NewRegistry(cfg DetectionConfig) *Registry {
	return &Registry{
		machines: make(map[string]*Machine),
		config:   cfg,
	}
}

// Get returns the machine for an agent, creating one if needed.
func (r *Registry) Get(agentID string) *Machine {
	r.mu.RLock()
	m, ok := r.machines[agentID]
	r.mu.RUnlock()
	if ok {
		return m
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after write lock.
	if m, ok := r.machines[agentID]; ok {
		return m
	}
	m = NewMachine(agentID, r.config)
	r.machines[agentID] = m
	return m
}

// Remove removes an agent's machine from the registry.
func (r *Registry) Remove(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.machines, agentID)
}

// Agents returns a list of all tracked agent IDs.
func (r *Registry) Agents() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := make([]string, 0, len(r.machines))
	for id := range r.machines {
		ids = append(ids, id)
	}
	return ids
}

// Snapshots returns snapshots for all tracked agents.
func (r *Registry) Snapshots() []Snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	snaps := make([]Snapshot, 0, len(r.machines))
	for _, m := range r.machines {
		snaps = append(snaps, m.Snapshot())
	}
	return snaps
}

// CheckStalls evaluates all active agents for stall conditions.
// Returns the agent IDs that were transitioned to stalled.
func (r *Registry) CheckStalls() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var stalled []string
	for _, m := range r.machines {
		if m.CheckStall() {
			stalled = append(stalled, m.AgentID())
		}
	}
	return stalled
}
