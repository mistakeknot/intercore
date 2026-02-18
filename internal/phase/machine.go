package phase

import (
	"context"
	"fmt"
)

// GateConfig controls gate evaluation for an advance attempt.
type GateConfig struct {
	Priority   int    // 0-1 = hard, 2-3 = soft, 4+ = none
	DisableAll bool   // skip all gate checks
	SkipReason string // reason for manual skip/override
}

// AdvanceResult describes what happened during an advance attempt.
type AdvanceResult struct {
	FromPhase  string
	ToPhase    string
	EventType  string
	GateResult string
	GateTier   string
	Reason     string
	Advanced   bool
}

// Advance attempts to move a run to its next required phase.
//
// The lifecycle:
//  1. Load run, check it's not terminal
//  2. Compute target phase (respecting complexity + force_full)
//  3. Check auto_advance (pause if disabled and no skip reason)
//  4. Evaluate gate (hard=block, soft=warn+advance, none=advance)
//  5. UpdatePhase with optimistic concurrency
//  6. Record event in audit trail
//  7. If target=done, set status=completed
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig) (*AdvanceResult, error) {
	run, err := store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	// Check terminal status
	if IsTerminalStatus(run.Status) {
		return nil, ErrTerminalRun
	}

	// Check terminal phase
	if IsTerminalPhase(run.Phase) {
		return nil, ErrTerminalPhase
	}

	fromPhase := run.Phase
	toPhase := NextRequiredPhase(fromPhase, run.Complexity, run.ForceFull)

	// Determine if this is a skip (jumped over intermediate phases)
	directNext, _ := NextPhase(fromPhase)
	eventType := EventAdvance
	if toPhase != directNext {
		eventType = EventSkip
	}

	// Check auto_advance
	if !run.AutoAdvance && cfg.SkipReason == "" {
		result := &AdvanceResult{
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventPause,
			GateResult: GateNone,
			GateTier:   TierNone,
			Reason:     "auto_advance disabled",
			Advanced:   false,
		}
		if err := store.AddEvent(ctx, &PhaseEvent{
			RunID:      runID,
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventPause,
			GateResult: strPtr(GateNone),
			GateTier:   strPtr(TierNone),
			Reason:     strPtr("auto_advance disabled"),
		}); err != nil {
			return nil, fmt.Errorf("advance: record pause: %w", err)
		}
		return result, nil
	}

	// Evaluate gate
	gateResult, gateTier := evaluateGate(cfg, fromPhase, toPhase)

	if gateResult == GateFail && gateTier == TierHard {
		reason := "gate blocked advance"
		if cfg.SkipReason != "" {
			reason = cfg.SkipReason
		}
		result := &AdvanceResult{
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventBlock,
			GateResult: gateResult,
			GateTier:   gateTier,
			Reason:     reason,
			Advanced:   false,
		}
		if err := store.AddEvent(ctx, &PhaseEvent{
			RunID:      runID,
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventBlock,
			GateResult: strPtr(gateResult),
			GateTier:   strPtr(gateTier),
			Reason:     strPtr(reason),
		}); err != nil {
			return nil, fmt.Errorf("advance: record block: %w", err)
		}
		return result, nil
	}

	// Perform the transition
	if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
		return nil, fmt.Errorf("advance: %w", err)
	}

	reason := ""
	if cfg.SkipReason != "" {
		reason = cfg.SkipReason
	}

	// Record event
	if err := store.AddEvent(ctx, &PhaseEvent{
		RunID:      runID,
		FromPhase:  fromPhase,
		ToPhase:    toPhase,
		EventType:  eventType,
		GateResult: strPtr(gateResult),
		GateTier:   strPtr(gateTier),
		Reason:     strPtrOrNil(reason),
	}); err != nil {
		return nil, fmt.Errorf("advance: record event: %w", err)
	}

	// If we reached done, mark the run as completed
	if toPhase == PhaseDone {
		if err := store.UpdateStatus(ctx, runID, StatusCompleted); err != nil {
			return nil, fmt.Errorf("advance: complete run: %w", err)
		}
	}

	return &AdvanceResult{
		FromPhase:  fromPhase,
		ToPhase:    toPhase,
		EventType:  eventType,
		GateResult: gateResult,
		GateTier:   gateTier,
		Reason:     reason,
		Advanced:   true,
	}, nil
}

// evaluateGate checks whether a phase transition should be allowed.
// In this initial implementation, gates always pass.
// Future: plug in real gate checks (git log, artifact presence, etc.)
func evaluateGate(cfg GateConfig, from, to string) (result, tier string) {
	if cfg.DisableAll {
		return GateNone, TierNone
	}

	// Determine tier from priority
	switch {
	case cfg.Priority <= 1:
		tier = TierHard
	case cfg.Priority <= 3:
		tier = TierSoft
	default:
		return GateNone, TierNone
	}

	// Stub: all gates pass in v1.
	// Real implementation would check:
	// - Does a brainstorm artifact exist? (brainstorm → brainstorm-reviewed)
	// - Is there a strategy doc? (brainstorm-reviewed → strategized)
	// - Has a plan been written? (strategized → planned)
	// - Are all plan tasks done? (executing → review)
	// - Has code review happened? (review → polish)
	return GatePass, tier
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
