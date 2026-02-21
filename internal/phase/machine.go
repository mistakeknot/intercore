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

// PhaseEventCallback is called after every advance attempt (advance, skip, block, pause).
// Errors are logged but do not fail the advance.
type PhaseEventCallback func(runID, eventType, fromPhase, toPhase, reason string)

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
//  8. Fire callback (if provided) for event bus notification
//
// rt and vq may be nil when Priority >= 4 (TierNone skips gate evaluation).
// pq may be nil for non-portfolio runs.
// callback may be nil — Advance checks before calling.
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier, pq PortfolioQuerier, callback PhaseEventCallback) (*AdvanceResult, error) {
	run, err := store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	// Check terminal status
	if IsTerminalStatus(run.Status) {
		return nil, ErrTerminalRun
	}

	// Resolve the phase chain (custom or default)
	chain := ResolveChain(run)

	// Check terminal phase using chain
	if ChainIsTerminal(chain, run.Phase) {
		return nil, ErrTerminalPhase
	}

	fromPhase := run.Phase
	toPhase, err := ChainNextPhase(chain, fromPhase)
	if err != nil {
		return nil, fmt.Errorf("advance: %w", err)
	}

	// Walk past pre-skipped phases
	skipped, err := store.SkippedPhases(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("advance: %w", err)
	}
	for skipped[toPhase] && !ChainIsTerminal(chain, toPhase) {
		next, err := ChainNextPhase(chain, toPhase)
		if err != nil {
			return nil, fmt.Errorf("advance: skip walk: %w", err)
		}
		toPhase = next
	}

	// Determine event type — advance is the only automatic transition now
	// (explicit skips are handled by the separate Skip command)
	eventType := EventAdvance

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
		if callback != nil {
			callback(runID, EventPause, fromPhase, toPhase, "auto_advance disabled")
		}
		return result, nil
	}

	// Evaluate gate
	gateResult, gateTier, evidence, gateErr := evaluateGate(ctx, run, cfg, fromPhase, toPhase, rt, vq, pq)
	if gateErr != nil {
		return nil, fmt.Errorf("advance: %w", gateErr)
	}

	// Build reason string
	reason := ""
	if evidence != nil {
		reason = evidence.String()
	}
	if cfg.SkipReason != "" {
		if reason != "" {
			reason = cfg.SkipReason + " | " + reason
		} else {
			reason = cfg.SkipReason
		}
	}

	if gateResult == GateFail && gateTier == TierHard {
		blockReason := reason
		if blockReason == "" {
			blockReason = "gate blocked advance"
		}
		result := &AdvanceResult{
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventBlock,
			GateResult: gateResult,
			GateTier:   gateTier,
			Reason:     blockReason,
			Advanced:   false,
		}
		if err := store.AddEvent(ctx, &PhaseEvent{
			RunID:      runID,
			FromPhase:  fromPhase,
			ToPhase:    toPhase,
			EventType:  EventBlock,
			GateResult: strPtr(gateResult),
			GateTier:   strPtr(gateTier),
			Reason:     strPtr(blockReason),
		}); err != nil {
			return nil, fmt.Errorf("advance: record block: %w", err)
		}
		if callback != nil {
			callback(runID, EventBlock, fromPhase, toPhase, blockReason)
		}
		return result, nil
	}

	// Perform the transition
	if err := store.UpdatePhase(ctx, runID, fromPhase, toPhase); err != nil {
		return nil, fmt.Errorf("advance: %w", err)
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

	// If we reached the terminal phase, mark the run as completed
	if ChainIsTerminal(chain, toPhase) {
		if err := store.UpdateStatus(ctx, runID, StatusCompleted); err != nil {
			return nil, fmt.Errorf("advance: complete run: %w", err)
		}
	}

	// Fire event bus callback (fire-and-forget)
	if callback != nil {
		callback(runID, eventType, fromPhase, toPhase, reason)
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

// RollbackResult describes what happened during a rollback.
type RollbackResult struct {
	FromPhase        string   // phase before rollback
	ToPhase          string   // target phase (now current)
	RolledBackPhases []string // phases between target and from (exclusive of target, inclusive of from)
	Reason           string
}

// Rollback rewinds a run to a prior phase in its chain.
//
// Unlike Advance, rollback:
//   - Goes backward (target must be behind current)
//   - Uses optimistic concurrency on phase (AND phase = ?) to prevent TOCTOU races
//   - Reverts completed runs back to active
//   - Records a rollback event in the audit trail
//   - Returns the list of phases that were rolled back
//
// Terminal status rejection (cancelled/failed) is enforced by the store layer.
// Rollback does NOT delete events or artifacts — those are marked separately
// by the caller (see runtrack.MarkArtifactsRolledBack).
func Rollback(ctx context.Context, store *Store, runID, targetPhase, reason string, callback PhaseEventCallback) (*RollbackResult, error) {
	run, err := store.Get(ctx, runID)
	if err != nil {
		return nil, err
	}

	chain := ResolveChain(run)
	fromPhase := run.Phase

	// Compute phases that will be rolled back
	rolledBack := ChainPhasesBetween(chain, targetPhase, fromPhase)
	if rolledBack == nil {
		return nil, ErrInvalidRollback
	}

	// Perform the phase rewind (store enforces terminal-status rejection + OCC)
	if err := store.RollbackPhase(ctx, runID, fromPhase, targetPhase); err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}

	// Record rollback event
	if err := store.AddEvent(ctx, &PhaseEvent{
		RunID:     runID,
		FromPhase: fromPhase,
		ToPhase:   targetPhase,
		EventType: EventRollback,
		Reason:    strPtrOrNil(reason),
	}); err != nil {
		return nil, fmt.Errorf("rollback: record event: %w", err)
	}

	// Fire callback
	if callback != nil {
		callback(runID, EventRollback, fromPhase, targetPhase, reason)
	}

	return &RollbackResult{
		FromPhase:        fromPhase,
		ToPhase:          targetPhase,
		RolledBackPhases: rolledBack,
		Reason:           reason,
	}, nil
}

func strPtrOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
