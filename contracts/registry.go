package contracts

import (
	"github.com/mistakeknot/intercore/internal/coordination"
	"github.com/mistakeknot/intercore/internal/discovery"
	"github.com/mistakeknot/intercore/internal/dispatch"
	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/lane"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/internal/scheduler"
)

// ContractType maps a CLI output name to a Go struct instance.
// The Instance field must be a zero-value of the struct (not a pointer)
// so the jsonschema reflector can inspect its type.
type ContractType struct {
	Name     string // kebab-case identifier, e.g. "dispatch"
	Instance any    // zero-value struct instance
}

// CLIContracts lists every type that appears in ic CLI JSON output.
var CLIContracts = []ContractType{
	// coordination
	{Name: "coordination-lock", Instance: coordination.Lock{}},
	{Name: "coordination-conflict", Instance: coordination.ConflictInfo{}},
	{Name: "coordination-reserve-result", Instance: coordination.ReserveResult{}},
	{Name: "coordination-sweep-result", Instance: coordination.SweepResult{}},

	// dispatch
	// Note: SpawnResult is excluded because it embeds *exec.Cmd which contains
	// uintptr fields that the jsonschema reflector cannot handle. SpawnResult is
	// an in-process type; the CLI serializes only ID + PID.
	{Name: "dispatch", Instance: dispatch.Dispatch{}},
	{Name: "dispatch-token-aggregation", Instance: dispatch.TokenAggregation{}},

	// phase
	{Name: "run", Instance: phase.Run{}},
	{Name: "phase-event", Instance: phase.PhaseEvent{}},
	{Name: "gate-condition", Instance: phase.GateCondition{}},
	{Name: "gate-evidence", Instance: phase.GateEvidence{}},
	{Name: "gate-check-result", Instance: phase.GateCheckResult{}},
	{Name: "advance-result", Instance: phase.AdvanceResult{}},
	{Name: "rollback-result", Instance: phase.RollbackResult{}},

	// runtrack
	{Name: "agent", Instance: runtrack.Agent{}},
	{Name: "artifact", Instance: runtrack.Artifact{}},
	{Name: "code-rollback-entry", Instance: runtrack.CodeRollbackEntry{}},

	// scheduler
	{Name: "spawn-job", Instance: scheduler.SpawnJob{}},
	{Name: "scheduler-stats", Instance: scheduler.Stats{}},

	// lane
	{Name: "lane", Instance: lane.Lane{}},
	{Name: "lane-event", Instance: lane.LaneEvent{}},

	// discovery
	{Name: "discovery", Instance: discovery.Discovery{}},
	{Name: "feedback-signal", Instance: discovery.FeedbackSignal{}},
	{Name: "interest-profile", Instance: discovery.InterestProfile{}},
	{Name: "discovery-event", Instance: discovery.DiscoveryEvent{}},
}

// EventContracts lists types used for the event bus.
var EventContracts = []ContractType{
	{Name: "event", Instance: event.Event{}},
	{Name: "interspect-event", Instance: event.InterspectEvent{}},
	{Name: "event-envelope", Instance: event.EventEnvelope{}},
}
