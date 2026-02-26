package event

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
	"github.com/mistakeknot/intercore/internal/phase"
)

func setupTestStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	return NewStore(d.SqlDB()), d
}

// insertTestRun creates a minimal run for FK satisfaction.
func insertTestRun(t *testing.T, d *db.DB, id string) {
	t.Helper()
	_, err := d.SqlDB().ExecContext(context.Background(), `
		INSERT INTO runs (id, project_dir, goal, status, phase, complexity, force_full, auto_advance, created_at, updated_at)
		VALUES (?, '/tmp', 'test', 'active', 'brainstorm', 3, 0, 1, unixepoch(), unixepoch())`, id)
	if err != nil {
		t.Fatalf("insert run %s: %v", id, err)
	}
}

func TestAddDispatchEvent(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	err := store.AddDispatchEvent(ctx, "disp001", "run001", "spawned", "running", "status_change", "", nil)
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	maxID, err := store.MaxDispatchEventID(ctx)
	if err != nil {
		t.Fatalf("MaxDispatchEventID: %v", err)
	}
	if maxID < 1 {
		t.Errorf("MaxDispatchEventID = %d, want >= 1", maxID)
	}
}

func TestAddDispatchEvent_DefaultEnvelope(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO dispatches (id, project_dir, sandbox_spec, sandbox_effective)
		VALUES ('disp-env', '/tmp', '{"mode":"workspace-write"}', '{"mode":"workspace-read"}')`)
	if err != nil {
		t.Fatalf("insert dispatch: %v", err)
	}

	err = store.AddDispatchEvent(ctx, "disp-env", "run-env", "spawned", "running", "status_change", "", nil)
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	events, err := store.ListEvents(ctx, "run-env", 0, 0, 0, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}

	got := events[0]
	if got.Envelope == nil {
		t.Fatal("expected envelope on dispatch event")
	}
	if got.Envelope.TraceID != "run-env" {
		t.Errorf("trace_id = %q, want run-env", got.Envelope.TraceID)
	}
	if got.Envelope.RequestedSandbox == "" {
		t.Error("requested_sandbox should be populated from dispatch row")
	}
	if got.Envelope.EffectiveSandbox == "" {
		t.Error("effective_sandbox should be populated from dispatch row")
	}
}

func TestListEvents_MergesPhaseAndDispatch(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "run001")

	// Insert a phase event
	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO phase_events (run_id, from_phase, to_phase, event_type, reason)
		VALUES ('run001', 'brainstorm', 'strategized', 'advance', 'test')`)
	if err != nil {
		t.Fatalf("insert phase event: %v", err)
	}

	// Insert a dispatch event
	err = store.AddDispatchEvent(ctx, "disp001", "run001", "spawned", "running", "status_change", "started", nil)
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	events, err := store.ListEvents(ctx, "run001", 0, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("ListEvents count = %d, want 2", len(events))
	}

	sources := map[string]bool{}
	for _, e := range events {
		sources[e.Source] = true
	}
	if !sources["phase"] {
		t.Error("missing phase event")
	}
	if !sources["dispatch"] {
		t.Error("missing dispatch event")
	}
}

func TestListEvents_CausalReconstructionByTraceID(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	pStore := phase.New(d.SqlDB())
	runID, err := pStore.Create(ctx, &phase.Run{
		ProjectDir: "/tmp",
		Goal:       "causal reconstruction",
		Complexity: 3,
	})
	if err != nil {
		t.Fatalf("phase create run: %v", err)
	}

	if err := pStore.AddEvent(ctx, &phase.PhaseEvent{
		RunID:     runID,
		FromPhase: phase.PhaseBrainstorm,
		ToPhase:   phase.PhaseBrainstormReviewed,
		EventType: phase.EventAdvance,
	}); err != nil {
		t.Fatalf("phase add event: %v", err)
	}

	_, err = d.SqlDB().ExecContext(ctx, `
		INSERT INTO dispatches (id, project_dir, sandbox_spec)
		VALUES ('disp-causal', '/tmp', '{"mode":"workspace-write"}')`)
	if err != nil {
		t.Fatalf("insert dispatch: %v", err)
	}
	err = store.AddDispatchEvent(ctx, "disp-causal", runID, "spawned", "running", "status_change", "", nil)
	if err != nil {
		t.Fatalf("AddDispatchEvent: %v", err)
	}

	events, err := store.ListEvents(ctx, runID, 0, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	seenPhase := false
	seenDispatch := false
	for _, e := range events {
		if e.Envelope == nil {
			t.Fatalf("event %s/%d missing envelope", e.Source, e.ID)
		}
		if e.Envelope.TraceID != runID {
			t.Fatalf("event %s/%d trace_id=%q want %q", e.Source, e.ID, e.Envelope.TraceID, runID)
		}
		if e.Source == SourcePhase {
			seenPhase = true
		}
		if e.Source == SourceDispatch {
			seenDispatch = true
		}
	}
	if !seenPhase || !seenDispatch {
		t.Fatalf("expected both phase and dispatch events, got phase=%v dispatch=%v", seenPhase, seenDispatch)
	}
}

func TestListEvents_SinceFiltering(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "run002")

	// Insert 3 phase events
	for i := 0; i < 3; i++ {
		_, err := d.SqlDB().ExecContext(ctx, `
			INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
			VALUES ('run002', 'brainstorm', 'strategized', 'advance')`)
		if err != nil {
			t.Fatalf("insert phase event %d: %v", i, err)
		}
	}

	// Get all first
	all, err := store.ListEvents(ctx, "run002", 0, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3 events, got %d", len(all))
	}

	// Get since first event — should return 2
	filtered, err := store.ListEvents(ctx, "run002", all[0].ID, 0, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents filtered: %v", err)
	}
	if len(filtered) != 2 {
		t.Errorf("expected 2 events after filtering, got %d", len(filtered))
	}
}

func TestListEvents_DualCursorsIndependent(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "run003")

	// Insert 2 phase events (IDs 1,2) and 2 dispatch events (IDs 1,2)
	for i := 0; i < 2; i++ {
		_, err := d.SqlDB().ExecContext(ctx, `
			INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
			VALUES ('run003', 'brainstorm', 'strategized', 'advance')`)
		if err != nil {
			t.Fatalf("insert phase event: %v", err)
		}
	}
	for i := 0; i < 2; i++ {
		err := store.AddDispatchEvent(ctx, "disp"+string(rune('a'+i)), "run003", "spawned", "running", "status_change", "", nil)
		if err != nil {
			t.Fatalf("AddDispatchEvent: %v", err)
		}
	}

	// Advance dispatch cursor past both dispatch events, but leave phase cursor at 0
	// This should still return both phase events
	events, err := store.ListEvents(ctx, "run003", 0, 100, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	phaseCount := 0
	for _, e := range events {
		if e.Source == SourcePhase {
			phaseCount++
		}
	}
	if phaseCount != 2 {
		t.Errorf("expected 2 phase events with dispatch cursor at 100, got %d", phaseCount)
	}
}

func TestListAllEvents(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "runA")
	insertTestRun(t, d, "runB")

	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
		VALUES ('runA', 'brainstorm', 'strategized', 'advance')`)
	if err != nil {
		t.Fatal(err)
	}

	err = store.AddDispatchEvent(ctx, "disp001", "runB", "spawned", "running", "status_change", "", nil)
	if err != nil {
		t.Fatal(err)
	}

	events, err := store.ListAllEvents(ctx, 0, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 events across runs, got %d", len(events))
	}
}

func TestListEvents_ExcludesDiscovery(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "runExcl")

	// Insert a phase event for this run
	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
		VALUES ('runExcl', 'brainstorm', 'strategized', 'advance')`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a discovery event (system-level, no run association)
	_, err = d.SqlDB().ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload)
		VALUES ('disc-excl', 'scored', '', 'new', '{"score":0.5}')`)
	if err != nil {
		t.Fatal(err)
	}

	// Run-scoped ListEvents should NOT include discovery events
	events, err := store.ListEvents(ctx, "runExcl", 0, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if e.Source == SourceDiscovery {
			t.Error("run-scoped ListEvents should not include discovery events")
		}
	}
	if len(events) != 1 {
		t.Errorf("expected 1 event (phase only), got %d", len(events))
	}
}

func TestListAllEvents_IncludesDiscovery(t *testing.T) {
	store, d := setupTestStore(t)
	ctx := context.Background()

	insertTestRun(t, d, "runD")

	// Insert a phase event
	_, err := d.SqlDB().ExecContext(ctx, `
		INSERT INTO phase_events (run_id, from_phase, to_phase, event_type)
		VALUES ('runD', 'brainstorm', 'strategized', 'advance')`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert a discovery event directly
	_, err = d.SqlDB().ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status, payload)
		VALUES ('disc001', 'scored', '', 'new', '{"score":0.85}')`)
	if err != nil {
		t.Fatal(err)
	}

	events, err := store.ListAllEvents(ctx, 0, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	sources := map[string]bool{}
	for _, e := range events {
		sources[e.Source] = true
	}
	if !sources[SourcePhase] {
		t.Error("missing phase event")
	}
	if !sources[SourceDiscovery] {
		t.Error("missing discovery event")
	}
}

func TestMaxDiscoveryEventID(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	// Empty table
	maxID, err := store.MaxDiscoveryEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if maxID != 0 {
		t.Errorf("MaxDiscoveryEventID on empty = %d, want 0", maxID)
	}

	// Insert an event
	_, err = store.db.ExecContext(ctx, `
		INSERT INTO discovery_events (discovery_id, event_type, from_status, to_status)
		VALUES ('disc001', 'submitted', '', 'new')`)
	if err != nil {
		t.Fatal(err)
	}

	maxID, err = store.MaxDiscoveryEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if maxID != 1 {
		t.Errorf("MaxDiscoveryEventID = %d, want 1", maxID)
	}
}

func TestMaxEventIDs_EmptyTables(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	phaseMax, err := store.MaxPhaseEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if phaseMax != 0 {
		t.Errorf("MaxPhaseEventID on empty = %d, want 0", phaseMax)
	}

	dispMax, err := store.MaxDispatchEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dispMax != 0 {
		t.Errorf("MaxDispatchEventID on empty = %d, want 0", dispMax)
	}

	interspectMax, err := store.MaxInterspectEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if interspectMax != 0 {
		t.Errorf("MaxInterspectEventID on empty = %d, want 0", interspectMax)
	}

	discoveryMax, err := store.MaxDiscoveryEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if discoveryMax != 0 {
		t.Errorf("MaxDiscoveryEventID on empty = %d, want 0", discoveryMax)
	}
}

func TestAddInterspectEvent(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	id, err := store.AddInterspectEvent(ctx, "run001", "fd-safety", "correction", "agent_wrong", `{"detail":"wrong finding"}`, "sess-abc", "/tmp/project")
	if err != nil {
		t.Fatalf("AddInterspectEvent: %v", err)
	}
	if id < 1 {
		t.Errorf("expected id >= 1, got %d", id)
	}

	// Verify via query
	events, err := store.ListInterspectEvents(ctx, "fd-safety", 0, 100)
	if err != nil {
		t.Fatalf("ListInterspectEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	e := events[0]
	if e.AgentName != "fd-safety" {
		t.Errorf("AgentName = %q, want %q", e.AgentName, "fd-safety")
	}
	if e.EventType != "correction" {
		t.Errorf("EventType = %q, want %q", e.EventType, "correction")
	}
	if e.OverrideReason != "agent_wrong" {
		t.Errorf("OverrideReason = %q, want %q", e.OverrideReason, "agent_wrong")
	}
	if e.RunID != "run001" {
		t.Errorf("RunID = %q, want %q", e.RunID, "run001")
	}
	if e.SessionID != "sess-abc" {
		t.Errorf("SessionID = %q, want %q", e.SessionID, "sess-abc")
	}
}

func TestAddInterspectEvent_OptionalFields(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	// All optional fields empty
	id, err := store.AddInterspectEvent(ctx, "", "fd-quality", "agent_dispatch", "", "", "", "")
	if err != nil {
		t.Fatalf("AddInterspectEvent: %v", err)
	}
	if id < 1 {
		t.Errorf("expected id >= 1, got %d", id)
	}

	events, err := store.ListInterspectEvents(ctx, "fd-quality", 0, 100)
	if err != nil {
		t.Fatalf("ListInterspectEvents: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}

	e := events[0]
	if e.RunID != "" {
		t.Errorf("RunID should be empty, got %q", e.RunID)
	}
	if e.OverrideReason != "" {
		t.Errorf("OverrideReason should be empty, got %q", e.OverrideReason)
	}
}

func TestListInterspectEvents_FilterByAgent(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "agent_wrong", "", "", "")
	store.AddInterspectEvent(ctx, "", "fd-quality", "correction", "agent_wrong", "", "", "")
	store.AddInterspectEvent(ctx, "", "fd-safety", "agent_dispatch", "", "", "", "")

	// Filter by fd-safety
	events, err := store.ListInterspectEvents(ctx, "fd-safety", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Errorf("expected 2 fd-safety events, got %d", len(events))
	}

	// No filter — all events
	all, err := store.ListInterspectEvents(ctx, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Errorf("expected 3 total events, got %d", len(all))
	}
}

func TestListInterspectEvents_SinceCursor(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "", "", "", "")
	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "", "", "", "")
	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "", "", "", "")

	all, err := store.ListInterspectEvents(ctx, "", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3, got %d", len(all))
	}

	// Since first event — should get 2
	filtered, err := store.ListInterspectEvents(ctx, "", all[0].ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 2 {
		t.Errorf("expected 2 after cursor, got %d", len(filtered))
	}
}

func TestMaxInterspectEventID(t *testing.T) {
	store, _ := setupTestStore(t)
	ctx := context.Background()

	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "", "", "", "")
	store.AddInterspectEvent(ctx, "", "fd-safety", "correction", "", "", "", "")

	maxID, err := store.MaxInterspectEventID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if maxID != 2 {
		t.Errorf("MaxInterspectEventID = %d, want 2", maxID)
	}
}
