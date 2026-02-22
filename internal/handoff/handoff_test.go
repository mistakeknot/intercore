package handoff

import (
	"encoding/json"
	"testing"
)

func TestNew(t *testing.T) {
	h := New("implemented auth", "add tests for auth")

	if h.Goal != "implemented auth" {
		t.Errorf("goal = %q, want 'implemented auth'", h.Goal)
	}
	if h.Now != "add tests for auth" {
		t.Errorf("now = %q, want 'add tests for auth'", h.Now)
	}
	if h.Version != Version {
		t.Errorf("version = %q, want %q", h.Version, Version)
	}
	if h.Status != StatusComplete {
		t.Errorf("status = %q, want %q", h.Status, StatusComplete)
	}
	if h.Date == "" {
		t.Error("date should be set")
	}
	if h.CreatedAt.IsZero() {
		t.Error("created_at should be set")
	}
}

func TestValidate_RequiredFields(t *testing.T) {
	// Missing goal.
	h := &Handoff{Now: "do something"}
	if err := h.Validate(); err == nil {
		t.Error("expected error for missing goal")
	}

	// Missing now.
	h = &Handoff{Goal: "did something"}
	if err := h.Validate(); err == nil {
		t.Error("expected error for missing now")
	}

	// Both present = valid.
	h = &Handoff{Goal: "did something", Now: "do next"}
	if err := h.Validate(); err != nil {
		t.Errorf("valid handoff should pass: %v", err)
	}
}

func TestValidate_InvalidStatus(t *testing.T) {
	h := New("g", "n")
	h.Status = "invalid"
	if err := h.Validate(); err == nil {
		t.Error("expected error for invalid status")
	}
}

func TestValidate_InvalidOutcome(t *testing.T) {
	h := New("g", "n")
	h.Outcome = "bogus"
	if err := h.Validate(); err == nil {
		t.Error("expected error for invalid outcome")
	}
}

func TestValidate_ValidStatuses(t *testing.T) {
	for _, status := range []string{StatusComplete, StatusPartial, StatusBlocked} {
		h := New("g", "n")
		h.Status = status
		if err := h.Validate(); err != nil {
			t.Errorf("status %q should be valid: %v", status, err)
		}
	}
}

func TestValidate_ValidOutcomes(t *testing.T) {
	for _, outcome := range []string{OutcomeSucceeded, OutcomePartialPlus, OutcomePartialMinus, OutcomeFailed} {
		h := New("g", "n")
		h.Outcome = outcome
		if err := h.Validate(); err != nil {
			t.Errorf("outcome %q should be valid: %v", outcome, err)
		}
	}
}

func TestValidate_TokensBounds(t *testing.T) {
	h := New("g", "n")

	// Negative used.
	h.Tokens.Used = -1
	if err := h.Validate(); err == nil {
		t.Error("expected error for negative tokens.used")
	}

	// Negative max.
	h.Tokens = TokenContext{Max: -1}
	if err := h.Validate(); err == nil {
		t.Error("expected error for negative tokens.max")
	}

	// Percentage out of range.
	h.Tokens = TokenContext{Percentage: 101}
	if err := h.Validate(); err == nil {
		t.Error("expected error for percentage > 100")
	}
}

func TestSetTokens(t *testing.T) {
	h := New("g", "n")
	h.SetTokens(80000, 200000)

	if h.Tokens.Used != 80000 {
		t.Errorf("used = %d, want 80000", h.Tokens.Used)
	}
	if h.Tokens.Max != 200000 {
		t.Errorf("max = %d, want 200000", h.Tokens.Max)
	}
	if h.Tokens.Percentage != 40 {
		t.Errorf("percentage = %.1f, want 40.0", h.Tokens.Percentage)
	}
}

func TestSetTokens_Clamp(t *testing.T) {
	h := New("g", "n")

	// Negative values clamped to 0.
	h.SetTokens(-10, -5)
	if h.Tokens.Used != 0 || h.Tokens.Max != 0 {
		t.Error("negative values should be clamped to 0")
	}

	// Used > max clamped.
	h.SetTokens(300, 200)
	if h.Tokens.Used != 200 {
		t.Errorf("used > max should be clamped: got %d", h.Tokens.Used)
	}
	if h.Tokens.Percentage != 100 {
		t.Errorf("clamped percentage should be 100, got %.1f", h.Tokens.Percentage)
	}
}

func TestSetAgent(t *testing.T) {
	h := New("g", "n").SetAgent("d-abc123", "claude")
	if h.AgentID != "d-abc123" {
		t.Errorf("agent_id = %q", h.AgentID)
	}
	if h.AgentType != "claude" {
		t.Errorf("agent_type = %q", h.AgentType)
	}
}

func TestSetDispatch(t *testing.T) {
	h := New("g", "n").SetDispatch("dispatch-1", "run-1")
	if h.DispatchID != "dispatch-1" {
		t.Errorf("dispatch_id = %q", h.DispatchID)
	}
	if h.RunID != "run-1" {
		t.Errorf("run_id = %q", h.RunID)
	}
}

func TestAddTask(t *testing.T) {
	h := New("g", "n").
		AddTask("implemented login", "auth.go", "auth_test.go").
		AddTask("fixed bug", "main.go")

	if len(h.Tasks) != 2 {
		t.Fatalf("expected 2 tasks, got %d", len(h.Tasks))
	}
	if h.Tasks[0].Task != "implemented login" {
		t.Errorf("task 0 = %q", h.Tasks[0].Task)
	}
	if len(h.Tasks[0].Files) != 2 {
		t.Errorf("task 0 files = %d", len(h.Tasks[0].Files))
	}
}

func TestAddFileChanges(t *testing.T) {
	h := New("g", "n")
	h.AddFileChanges([]string{"new.go"}, []string{"old.go"}, []string{"dead.go"})

	if !h.HasChanges() {
		t.Error("expected HasChanges to be true")
	}
	if h.TotalChanges() != 3 {
		t.Errorf("total changes = %d, want 3", h.TotalChanges())
	}
}

func TestHasChanges_Empty(t *testing.T) {
	h := New("g", "n")
	if h.HasChanges() {
		t.Error("new handoff should have no changes")
	}
	if h.TotalChanges() != 0 {
		t.Errorf("total changes = %d, want 0", h.TotalChanges())
	}
}

func TestIsComplete(t *testing.T) {
	h := New("g", "n")
	if !h.IsComplete() {
		t.Error("default should be complete")
	}
	h.Status = StatusBlocked
	if h.IsComplete() {
		t.Error("blocked should not be complete")
	}
}

func TestIsBlocked(t *testing.T) {
	h := New("g", "n")
	h.Status = StatusBlocked
	if !h.IsBlocked() {
		t.Error("should be blocked")
	}
	h.Status = StatusComplete
	if h.IsBlocked() {
		t.Error("complete should not be blocked")
	}
}

func TestJSON_RoundTrip(t *testing.T) {
	h := New("built auth", "add tests").
		SetAgent("d-123", "claude").
		SetDispatch("disp-1", "run-1").
		SetTokens(50000, 200000).
		AddTask("login endpoint", "login.go").
		AddFileChanges([]string{"login.go"}, nil, nil)

	h.Blockers = []string{"waiting on API key"}
	h.Decisions = map[string]string{"auth": "JWT"}
	h.ReservationTransfer = &ReservationTransfer{
		FromAgent:  "d-123",
		ProjectKey: "/project",
		TTLSeconds: 3600,
		Reservations: []ReservationSnapshot{
			{PathPattern: "src/**", Exclusive: true, Reason: "editing"},
		},
	}

	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var h2 Handoff
	if err := json.Unmarshal(data, &h2); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if h2.Goal != h.Goal {
		t.Errorf("goal mismatch: %q vs %q", h2.Goal, h.Goal)
	}
	if h2.Now != h.Now {
		t.Errorf("now mismatch: %q vs %q", h2.Now, h.Now)
	}
	if h2.Tokens.Used != h.Tokens.Used {
		t.Errorf("tokens.used mismatch: %d vs %d", h2.Tokens.Used, h.Tokens.Used)
	}
	if h2.AgentID != h.AgentID {
		t.Errorf("agent_id mismatch")
	}
	if h2.ReservationTransfer == nil {
		t.Fatal("reservation_transfer should be preserved")
	}
	if h2.ReservationTransfer.FromAgent != "d-123" {
		t.Errorf("from_agent mismatch")
	}
	if len(h2.ReservationTransfer.Reservations) != 1 {
		t.Errorf("reservations count mismatch")
	}
	if len(h2.Blockers) != 1 || h2.Blockers[0] != "waiting on API key" {
		t.Errorf("blockers mismatch")
	}
	if h2.Decisions["auth"] != "JWT" {
		t.Errorf("decisions mismatch")
	}
}

func TestJSON_CompactSize(t *testing.T) {
	// Verify a typical handoff is under ~2KB (well under 400 tokens).
	h := New("implemented authentication endpoint with JWT", "add integration tests for auth").
		SetAgent("d-abc", "claude").
		SetTokens(150000, 200000).
		AddTask("login endpoint", "auth.go", "auth_test.go").
		AddTask("middleware", "middleware.go")

	h.Decisions = map[string]string{"auth_method": "JWT", "token_expiry": "1h"}
	h.Worked = []string{"builder pattern for config", "table-driven tests"}

	data, err := json.Marshal(h)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if len(data) > 2048 {
		t.Errorf("handoff too large: %d bytes (want < 2048)", len(data))
	}
}
