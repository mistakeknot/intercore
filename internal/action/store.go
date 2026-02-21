package action

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

var (
	ErrNotFound  = errors.New("action not found")
	ErrDuplicate = errors.New("duplicate action for run/phase/command")
)

var templateVarRE = regexp.MustCompile(`\$\{artifact:([^}]+)\}`)

// Store provides phase action CRUD operations.
type Store struct {
	db *sql.DB
}

// New creates an action store.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// Add inserts a new phase action. Returns the auto-increment ID.
func (s *Store) Add(ctx context.Context, a *Action) (int64, error) {
	if a.ActionType == "" {
		a.ActionType = "command"
	}
	if a.Mode == "" {
		a.Mode = "interactive"
	}
	now := time.Now().Unix()
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO phase_actions (run_id, phase, action_type, command, args, mode, priority, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.RunID, a.Phase, a.ActionType, a.Command, a.Args, a.Mode, a.Priority, now, now,
	)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint") {
			return 0, ErrDuplicate
		}
		if strings.Contains(err.Error(), "FOREIGN KEY constraint") {
			return 0, fmt.Errorf("run not found: %s", a.RunID)
		}
		return 0, fmt.Errorf("action add: %w", err)
	}
	return result.LastInsertId()
}

// AddBatch inserts multiple phase actions for a run in a single transaction.
// The map key is the phase name.
func (s *Store) AddBatch(ctx context.Context, runID string, actions map[string]*Action) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("action batch: begin: %w", err)
	}
	defer tx.Rollback()

	now := time.Now().Unix()
	for phase, a := range actions {
		actionType := a.ActionType
		if actionType == "" {
			actionType = "command"
		}
		mode := a.Mode
		if mode == "" {
			mode = "interactive"
		}
		_, err := tx.ExecContext(ctx, `
			INSERT INTO phase_actions (run_id, phase, action_type, command, args, mode, priority, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, phase, actionType, a.Command, a.Args, mode, a.Priority, now, now,
		)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE constraint") {
				return fmt.Errorf("duplicate action for phase %s: %s", phase, a.Command)
			}
			return fmt.Errorf("action batch insert %s: %w", phase, err)
		}
	}
	return tx.Commit()
}

// ListForPhase returns actions for a run+phase, ordered by priority ASC.
func (s *Store) ListForPhase(ctx context.Context, runID, phase string) ([]*Action, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, phase, action_type, command, args, mode, priority, created_at, updated_at
		FROM phase_actions WHERE run_id = ? AND phase = ? ORDER BY priority ASC, id ASC`,
		runID, phase,
	)
	if err != nil {
		return nil, fmt.Errorf("action list: %w", err)
	}
	defer rows.Close()
	return scanActions(rows)
}

// ListForPhaseResolved returns actions with template variables resolved.
func (s *Store) ListForPhaseResolved(ctx context.Context, runID, phase, projectDir string) ([]*Action, error) {
	actions, err := s.ListForPhase(ctx, runID, phase)
	if err != nil {
		return nil, err
	}
	for _, a := range actions {
		if a.Args != nil {
			resolved := s.resolveTemplateVars(ctx, runID, projectDir, *a.Args)
			a.Args = &resolved
		}
	}
	return actions, nil
}

// ListAll returns all actions for a run, ordered by phase then priority.
func (s *Store) ListAll(ctx context.Context, runID string) ([]*Action, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, run_id, phase, action_type, command, args, mode, priority, created_at, updated_at
		FROM phase_actions WHERE run_id = ? ORDER BY phase ASC, priority ASC, id ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("action list all: %w", err)
	}
	defer rows.Close()
	return scanActions(rows)
}

// Update modifies an existing action identified by (run_id, phase, command).
func (s *Store) Update(ctx context.Context, runID, phase, command string, upd *ActionUpdate) error {
	var sets []string
	var args []interface{}
	now := time.Now().Unix()

	if upd.Args != nil {
		sets = append(sets, "args = ?")
		args = append(args, *upd.Args)
	}
	if upd.Mode != nil {
		sets = append(sets, "mode = ?")
		args = append(args, *upd.Mode)
	}
	if upd.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *upd.Priority)
	}
	if len(sets) == 0 {
		return nil
	}

	sets = append(sets, "updated_at = ?")
	args = append(args, now, runID, phase, command)

	query := fmt.Sprintf("UPDATE phase_actions SET %s WHERE run_id = ? AND phase = ? AND command = ?",
		strings.Join(sets, ", "))
	result, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("action update: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Delete removes an action identified by (run_id, phase, command).
func (s *Store) Delete(ctx context.Context, runID, phase, command string) error {
	result, err := s.db.ExecContext(ctx,
		"DELETE FROM phase_actions WHERE run_id = ? AND phase = ? AND command = ?",
		runID, phase, command,
	)
	if err != nil {
		return fmt.Errorf("action delete: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// resolveTemplateVars replaces ${artifact:<type>}, ${run_id}, ${project_dir} in a string.
// Values are JSON-escaped before substitution to prevent injection into the JSON args string.
func (s *Store) resolveTemplateVars(ctx context.Context, runID, projectDir, input string) string {
	// Resolve ${artifact:<type>}
	result := templateVarRE.ReplaceAllStringFunc(input, func(match string) string {
		sub := templateVarRE.FindStringSubmatch(match)
		if len(sub) < 2 {
			return match
		}
		artType := sub[1]
		var path string
		err := s.db.QueryRowContext(ctx,
			`SELECT path FROM run_artifacts WHERE run_id = ? AND type = ? AND status = 'active' ORDER BY created_at DESC LIMIT 1`,
			runID, artType,
		).Scan(&path)
		if err != nil {
			return match // leave unresolved
		}
		return jsonEscapeValue(path)
	})

	// Resolve ${run_id}
	result = strings.ReplaceAll(result, "${run_id}", jsonEscapeValue(runID))

	// Resolve ${project_dir}
	result = strings.ReplaceAll(result, "${project_dir}", jsonEscapeValue(projectDir))

	return result
}

// jsonEscapeValue escapes a string for safe embedding in a JSON string context.
// It uses json.Marshal to get proper escaping, then strips the surrounding quotes.
func jsonEscapeValue(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return s
	}
	// Strip surrounding quotes from the marshaled JSON string
	return string(b[1 : len(b)-1])
}

func scanActions(rows *sql.Rows) ([]*Action, error) {
	var actions []*Action
	for rows.Next() {
		a := &Action{}
		var args sql.NullString
		if err := rows.Scan(
			&a.ID, &a.RunID, &a.Phase, &a.ActionType, &a.Command,
			&args, &a.Mode, &a.Priority, &a.CreatedAt, &a.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("action scan: %w", err)
		}
		if args.Valid {
			a.Args = &args.String
		}
		actions = append(actions, a)
	}
	return actions, rows.Err()
}
