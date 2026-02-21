package lane

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// VelocityScore holds starvation and throughput data for a lane.
type VelocityScore struct {
	LaneID     string
	LaneName   string
	OpenBeads  int
	ClosedLast int     // beads closed in window
	Throughput float64 // closures per window
	Starvation float64 // higher = more starved
}

// VelocityCalculator computes lane velocity and starvation scores.
// It queries bead membership + closure events to derive throughput.
type VelocityCalculator struct {
	store *Store
}

// NewVelocityCalculator creates a velocity calculator.
func NewVelocityCalculator(store *Store) *VelocityCalculator {
	return &VelocityCalculator{store: store}
}

// BeadStatus holds a bead's priority and open/closed state.
// This is provided by the caller (e.g., from bd list).
type BeadStatus struct {
	BeadID   string
	Priority int  // 0-4
	IsClosed bool
	ClosedAt int64 // unix timestamp, 0 if open
}

// ComputeStarvation calculates starvation scores for all active lanes.
// beadStatuses maps bead_id to its current status (from bd).
// windowDays is the lookback period for throughput calculation.
func (v *VelocityCalculator) ComputeStarvation(ctx context.Context, beadStatuses map[string]*BeadStatus, windowDays int) (map[string]*VelocityScore, error) {
	lanes, err := v.store.List(ctx, "active")
	if err != nil {
		return nil, fmt.Errorf("velocity: list lanes: %w", err)
	}

	windowStart := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour).Unix()
	scores := make(map[string]*VelocityScore, len(lanes))

	for _, l := range lanes {
		members, err := v.store.GetMembers(ctx, l.ID)
		if err != nil {
			return nil, fmt.Errorf("velocity: get members for %s: %w", l.ID, err)
		}

		vs := &VelocityScore{
			LaneID:   l.ID,
			LaneName: l.Name,
		}

		var priorityWeightedOpen float64
		for _, bid := range members {
			bs, ok := beadStatuses[bid]
			if !ok {
				// Bead not in status map — count as open P2 (default)
				vs.OpenBeads++
				priorityWeightedOpen += 3 // 5 - P2(2) = 3
				continue
			}
			if bs.IsClosed {
				if bs.ClosedAt >= windowStart {
					vs.ClosedLast++
				}
			} else {
				vs.OpenBeads++
				priorityWeightedOpen += float64(5 - bs.Priority) // P0=5, P1=4, P2=3, P3=2, P4=1
			}
		}

		vs.Throughput = float64(vs.ClosedLast)
		vs.Starvation = priorityWeightedOpen / math.Max(vs.Throughput, 0.1)

		scores[l.Name] = vs
	}

	return scores, nil
}

// ComputeStarvationFromDB calculates starvation scores using only the kernel DB
// (no external bead data). Uses lane_members as the source of truth,
// treating all members as open P2 beads. Throughput comes from lane_events
// with type "bead_removed" or "snapshot" within the window.
func (v *VelocityCalculator) ComputeStarvationFromDB(ctx context.Context, windowDays int) (map[string]*VelocityScore, error) {
	lanes, err := v.store.List(ctx, "active")
	if err != nil {
		return nil, fmt.Errorf("velocity: list lanes: %w", err)
	}

	windowStart := time.Now().Add(-time.Duration(windowDays) * 24 * time.Hour).Unix()
	scores := make(map[string]*VelocityScore, len(lanes))

	for _, l := range lanes {
		members, err := v.store.GetMembers(ctx, l.ID)
		if err != nil {
			return nil, fmt.Errorf("velocity: get members for %s: %w", l.ID, err)
		}

		// Count "bead_removed" events in window as proxy for throughput
		closedCount, err := countRecentRemovals(ctx, v.store.db, l.ID, windowStart)
		if err != nil {
			return nil, fmt.Errorf("velocity: count removals for %s: %w", l.ID, err)
		}

		priorityWeightedOpen := float64(len(members)) * 3 // all treated as P2
		throughput := float64(closedCount)

		scores[l.Name] = &VelocityScore{
			LaneID:     l.ID,
			LaneName:   l.Name,
			OpenBeads:  len(members),
			ClosedLast: closedCount,
			Throughput: throughput,
			Starvation: priorityWeightedOpen / math.Max(throughput, 0.1),
		}
	}

	return scores, nil
}

func countRecentRemovals(ctx context.Context, db *sql.DB, laneID string, since int64) (int, error) {
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM lane_events
		WHERE lane_id = ? AND event_type = 'bead_removed' AND created_at >= ?`,
		laneID, since,
	).Scan(&count)
	return count, err
}

// SortedByStarvation returns velocity scores sorted by starvation (descending).
func SortedByStarvation(scores map[string]*VelocityScore) []*VelocityScore {
	sorted := make([]*VelocityScore, 0, len(scores))
	for _, vs := range scores {
		sorted = append(sorted, vs)
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Starvation > sorted[j].Starvation
	})
	return sorted
}
