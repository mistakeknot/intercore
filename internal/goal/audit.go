package goal

import (
	"context"
	"fmt"
)

// Defect is one audit finding. A goal may have more than one finding; for
// example, an expired closing lease can also be dormant.
type Defect struct {
	GoalID string `json:"goal_id"`
	Title  string `json:"title"`
	Kind   string `json:"kind"` // closed_without_successor | dormant | stuck_closing
	Detail string `json:"detail"`
}

// TouchRunAdvance updates dormancy state after an attached run successfully
// advances. Dormancy is therefore a single timestamp comparison during Audit.
func (s *Store) TouchRunAdvance(ctx context.Context, id string) error {
	now := nowUnix()
	_, err := s.db.ExecContext(ctx,
		`UPDATE goals SET last_run_advanced_at = ?, updated_at = ? WHERE id = ?`,
		now, now, id)
	if err != nil {
		return fmt.Errorf("goal touch run advance: %w", err)
	}
	return nil
}

// Audit sweeps goals for successor, dormancy, and closing-lease defects in one
// query pass. dormantAfterSec bounds how long an open or closing goal may go
// without an attached-run advance. An empty projectDir audits all projects.
func (s *Store) Audit(ctx context.Context, projectDir string, dormantAfterSec int64) ([]Defect, error) {
	now := nowUnix()
	q := `SELECT id, title, status, lease_expires_at, successor_ref,
	             COALESCE(last_run_advanced_at, created_at) AS last_activity
	      FROM goals WHERE status IN ('open', 'closing', 'closed')`
	var args []any
	if projectDir != "" {
		q += ` AND project_dir = ?`
		args = append(args, projectDir)
	}
	q += ` ORDER BY id`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("goal audit query: %w", err)
	}
	defer rows.Close()

	var defects []Defect
	for rows.Next() {
		var id, title, status string
		var leaseExpiresAt *int64
		var successorRef *string
		var lastActivity int64
		if err := rows.Scan(
			&id,
			&title,
			&status,
			&leaseExpiresAt,
			&successorRef,
			&lastActivity,
		); err != nil {
			return nil, fmt.Errorf("goal audit scan: %w", err)
		}

		if status == "closed" && successorRef == nil {
			defects = append(defects, Defect{
				GoalID: id,
				Title:  title,
				Kind:   "closed_without_successor",
				Detail: "goal closed with no successor_ref recorded",
			})
		}
		if status == "closing" && leaseExpiresAt != nil && *leaseExpiresAt < now {
			defects = append(defects, Defect{
				GoalID: id,
				Title:  title,
				Kind:   "stuck_closing",
				Detail: fmt.Sprintf("close lease expired %ds ago", now-*leaseExpiresAt),
			})
		}
		if (status == "open" || status == "closing") && now-lastActivity > dormantAfterSec {
			defects = append(defects, Defect{
				GoalID: id,
				Title:  title,
				Kind:   "dormant",
				Detail: fmt.Sprintf("no attached-run advance for %ds", now-lastActivity),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("goal audit rows: %w", err)
	}
	return defects, nil
}

// SetSuccessor records the durable successor proposal target. The reference
// may be a bead ID, goal ID, or free-form external reference.
func (s *Store) SetSuccessor(ctx context.Context, id, ref string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE goals SET successor_ref = ?, updated_at = ? WHERE id = ?`,
		ref, nowUnix(), id)
	if err != nil {
		return fmt.Errorf("goal set successor: %w", err)
	}
	return nil
}
