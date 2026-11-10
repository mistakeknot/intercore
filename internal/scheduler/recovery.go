package scheduler

// Recovery decision table (plan section 5.3d). DecideRecovery is a pure
// function: the caller gathers the facts, this picks the row, and Recover
// applies the outcome in one guarded transaction.

// DispatchState is what the recovering runner learned about an attempt's dispatch.
type DispatchState int

const (
	DispAbsent    DispatchState = iota // the attempt was never linked
	DispTerminal                       // linked, and the dispatch is terminal
	DispPruned                         // linked, but the dispatch row is gone
	DispLivePID                        // linked, non-terminal, a PID is recorded
	DispLiveNoPID                      // linked, non-terminal, no PID recorded
)

func (d DispatchState) String() string {
	switch d {
	case DispAbsent:
		return "absent"
	case DispTerminal:
		return "terminal"
	case DispPruned:
		return "pruned"
	case DispLivePID:
		return "live-pid"
	case DispLiveNoPID:
		return "live-no-pid"
	}
	return "invalid"
}

// RecoveryAction is what to do with an orphaned in-flight job.
type RecoveryAction int

const (
	ActNone          RecoveryAction = iota // never returned by DecideRecovery
	ActLegacy                              // row 1: apply the legacy rule
	ActCancelled                           // row 2: finish the cancel
	ActPrestartAbort                       // row 3: excused crash before the link
	ActFinalize                            // row 4: counted pre-start failure (Reason is the class)
	ActQuarantine                          // rows 5, 9: needs an operator (Reason is why)
	ActPipeline                            // row 6: run the evidence pipeline (Reason may force a class)
	ActCheckPID                            // row 7: verify the recorded PID
	ActSurvivor                            // row 8: no PID recorded; survivor handling
)

func (a RecoveryAction) String() string {
	switch a {
	case ActLegacy:
		return "legacy"
	case ActCancelled:
		return "cancelled"
	case ActPrestartAbort:
		return "prestart_abort"
	case ActFinalize:
		return "finalize"
	case ActQuarantine:
		return "quarantine"
	case ActPipeline:
		return "pipeline"
	case ActCheckPID:
		return "check_pid"
	case ActSurvivor:
		return "survivor"
	}
	return "none"
}

// RecoveryInput are the facts that select a row.
type RecoveryInput struct {
	Status       JobStatus // launching, cancelling or running
	Legacy       bool      // the open attempt is a synthetic legacy one
	Linked       bool      // linked_at is set
	LaunchResult string    // "" when NULL
	Dispatch     DispatchState
}

// RecoveryDecision is the matched row. Reason is the quarantine reason for
// ActQuarantine and the class to use for ActFinalize or a pruned ActPipeline.
type RecoveryDecision struct {
	Row    int
	Action RecoveryAction
	Reason string
}

// Launch results written by the runner before and after the spawn.
const (
	LaunchRejected    = "rejected"
	LaunchHookAborted = "hook_aborted"
	LaunchSpawnError  = "spawn_error"
	LaunchStarted     = "started"
)

// DecideRecovery matches the table top to bottom; the first match wins and
// row 9 catches everything else, so every input gets a decision. Tuples the
// plan calls impossible fall through to a conservative row: an unlinked
// `launching`/`started` is row 5, an unlinked `cancelling` is row 2, and
// unlinked `running` (or a linked attempt with no dispatch) is row 9.
func DecideRecovery(in RecoveryInput) RecoveryDecision {
	if in.Legacy {
		return RecoveryDecision{Row: 1, Action: ActLegacy}
	}
	unlinked := !in.Linked && in.Dispatch == DispAbsent
	switch {
	case unlinked && in.Status == StatusCancelling:
		return RecoveryDecision{Row: 2, Action: ActCancelled}
	case unlinked && in.Status == StatusLaunching && in.LaunchResult == "":
		return RecoveryDecision{Row: 3, Action: ActPrestartAbort}
	case unlinked && in.Status == StatusLaunching &&
		(in.LaunchResult == LaunchRejected || in.LaunchResult == LaunchHookAborted || in.LaunchResult == LaunchSpawnError):
		return RecoveryDecision{Row: 4, Action: ActFinalize, Reason: ClassPrestart}
	case unlinked && in.Status == StatusLaunching && in.LaunchResult == LaunchStarted:
		return RecoveryDecision{Row: 5, Action: ActQuarantine, Reason: ReasonRunnerInconsistent}
	case in.Linked && in.Dispatch == DispTerminal:
		return RecoveryDecision{Row: 6, Action: ActPipeline}
	case in.Linked && in.Dispatch == DispPruned:
		return RecoveryDecision{Row: 6, Action: ActPipeline, Reason: ClassIndeterminate}
	case in.Linked && in.Dispatch == DispLivePID:
		return RecoveryDecision{Row: 7, Action: ActCheckPID}
	case in.Linked && in.Dispatch == DispLiveNoPID:
		return RecoveryDecision{Row: 8, Action: ActSurvivor}
	}
	return RecoveryDecision{Row: 9, Action: ActQuarantine, Reason: ReasonRunnerInconsistent}
}

// InRecoveryScope reports whether a runner at epoch mine recovers a row:
// in-flight, and held by an older epoch. A NULL lease is epoch 0, which is
// where migrated legacy rows sit. A runner never touches its own epoch's rows.
func InRecoveryScope(status JobStatus, leaseEpoch *int64, mine int64) bool {
	switch status {
	case StatusLaunching, StatusRunning, StatusCancelling:
	default:
		return false
	}
	var lease int64
	if leaseEpoch != nil {
		lease = *leaseEpoch
	}
	return lease < mine
}
