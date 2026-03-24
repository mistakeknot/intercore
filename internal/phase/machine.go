package phase

import (
	"context"
	"fmt"
)

// SpecGateRule is a gate rule from an agency spec, injected into gate evaluation.
type SpecGateRule struct {
	Check string // CheckArtifactExists, CheckAgentsComplete, etc.
	Phase string // which phase's artifacts to check (empty = not applicable)
	Tier  string // "hard" or "soft" — per-rule tier override
}

// GateConfig controls gate evaluation for an advance attempt.
type GateConfig struct {
	Priority   int            // 0-1 = hard, 2-3 = soft, 4+ = none
	DisableAll bool           // skip all gate checks
	SkipReason string         // reason for manual skip/override
	SpecRules  []SpecGateRule // rules from agency specs (merged with hardcoded rules)
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
//  1. Begin transaction (all reads + writes are atomic)
//  2. Load run, check it's not terminal
//  3. Compute target phase (respecting complexity + force_full)
//  4. Check auto_advance (pause if disabled and no skip reason)
//  5. Evaluate gate using tx-scoped queriers (hard=block, soft=warn+advance, none=advance)
//  6. UpdatePhase with optimistic concurrency (inside same tx)
//  7. Record event in audit trail (inside same tx)
//  8. If target=done, set status=completed (inside same tx)
//  9. Commit transaction
//  10. Fire callback (if provided) for event bus notification (outside tx)
//
// Gate evaluation and phase update share a single transaction to prevent
// TOCTOU races where state changes between gate check and phase write.
//
// rt and vq may be nil when Priority >= 4 (TierNone skips gate evaluation).
// pq may be nil for non-portfolio runs.
// dq may be nil for non-child runs (runs without a parent_run_id).
// callback may be nil — Advance checks before calling.
func Advance(ctx context.Context, store *Store, runID string, cfg GateConfig, rt RuntrackQuerier, vq VerdictQuerier, pq PortfolioQuerier, dq DepQuerier, bq BudgetQuerier, callback PhaseEventCallback) (*AdvanceResult, error) {
	tx, err := store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("advance: begin: %w", err)
	}
	defer tx.Rollback()

	run, err := store.GetQ(ctx, tx, runID)
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
	skipped, err := store.SkippedPhasesQ(ctx, tx, runID)
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
		if err := store.AddEventQ(ctx, tx, &PhaseEvent{
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
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("advance: commit pause: %w", err)
		}
		if callback != nil {
			callback(runID, EventPause, fromPhase, toPhase, "auto_advance disabled")
		}
		return result, nil
	}

	// Build tx-scoped queriers for gate evaluation — all reads happen
	// inside the same transaction as the phase update, preventing TOCTOU.
	txRT := RuntrackQuerier(&txRuntrackQuerier{q: tx})
	txVQ := VerdictQuerier(&txVerdictQuerier{q: tx})
	txPQ := PortfolioQuerier(&txPortfolioQuerier{q: tx})
	txDQ := DepQuerier(&txDepQuerier{q: tx})

	// Use caller-provided queriers only when they're nil (Priority >= 4
	// bypasses gates entirely, so tx-scoped wrappers won't be called).
	// When gates ARE evaluated, always use tx-scoped queriers for atomicity.
	if rt == nil {
		txRT = nil
	}
	if vq == nil {
		txVQ = nil
	}
	if pq == nil {
		txPQ = nil
	}
	if dq == nil {
		txDQ = nil
	}

	// Build tx-scoped budget querier — all budget reads happen inside
	// the same transaction as the phase update, preventing TOCTOU.
	var txBQ BudgetQuerier
	if bq != nil && run.TokenBudget != nil && *run.TokenBudget > 0 {
		txBQ = &txBudgetQuerier{q: tx, tokenBudget: *run.TokenBudget}
	}

	// Evaluate gate — reads happen inside the transaction
	gateResult, gateTier, evidence, gateErr := evaluateGate(ctx, run, cfg, fromPhase, toPhase, txRT, txVQ, txPQ, txDQ, txBQ)
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
		if err := store.AddEventQ(ctx, tx, &PhaseEvent{
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
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("advance: commit block: %w", err)
		}
		if callback != nil {
			callback(runID, EventBlock, fromPhase, toPhase, blockReason)
		}
		return result, nil
	}

	// Perform the transition — inside the same transaction as gate evaluation
	if err := store.UpdatePhaseQ(ctx, tx, runID, fromPhase, toPhase); err != nil {
		return nil, fmt.Errorf("advance: %w", err)
	}

	// Record event — inside the same transaction
	if err := store.AddEventQ(ctx, tx, &PhaseEvent{
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

	// If we reached the terminal phase, mark the run as completed — inside tx
	if ChainIsTerminal(chain, toPhase) {
		if err := store.UpdateStatusQ(ctx, tx, runID, StatusCompleted); err != nil {
			return nil, fmt.Errorf("advance: complete run: %w", err)
		}
	}

	// Commit the entire atomic unit: gate check + phase update + event + status
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("advance: commit: %w", err)
	}

	// Fire event bus callback OUTSIDE transaction (fire-and-forget)
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
	tx, err := store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("rollback: begin: %w", err)
	}
	defer tx.Rollback()

	run, err := store.GetQ(ctx, tx, runID)
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

	// Perform the phase rewind (OCC + terminal-status rejection, inside tx)
	if err := store.RollbackPhaseQ(ctx, tx, runID, fromPhase, targetPhase); err != nil {
		return nil, fmt.Errorf("rollback: %w", err)
	}

	// Record rollback event (inside same tx — atomic with phase rewind)
	if err := store.AddEventQ(ctx, tx, &PhaseEvent{
		RunID:     runID,
		FromPhase: fromPhase,
		ToPhase:   targetPhase,
		EventType: EventRollback,
		Reason:    strPtrOrNil(reason),
	}); err != nil {
		return nil, fmt.Errorf("rollback: record event: %w", err)
	}

	// Commit the atomic unit: phase rewind + rollback event
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("rollback: commit: %w", err)
	}

	// Fire callback OUTSIDE transaction (fire-and-forget, matching Advance pattern)
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
