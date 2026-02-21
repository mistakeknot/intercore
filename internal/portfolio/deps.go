package portfolio

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Dep represents a cross-project dependency within a portfolio.
type Dep struct {
	ID                int64
	PortfolioRunID    string
	UpstreamProject   string
	DownstreamProject string
	CreatedAt         int64
}

// DepStore provides project dependency operations against the intercore DB.
type DepStore struct {
	db *sql.DB
}

// NewDepStore creates a dependency store.
func NewDepStore(db *sql.DB) *DepStore {
	return &DepStore{db: db}
}

// Add inserts a dependency edge. Returns error if duplicate.
func (s *DepStore) Add(ctx context.Context, portfolioRunID, upstream, downstream string) error {
	if upstream == downstream {
		return fmt.Errorf("add dep: upstream and downstream cannot be the same project")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO project_deps (portfolio_run_id, upstream_project, downstream_project, created_at)
		VALUES (?, ?, ?, ?)`,
		portfolioRunID, upstream, downstream, time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("add dep: %w", err)
	}
	return nil
}

// List returns all dependency edges for a portfolio run.
func (s *DepStore) List(ctx context.Context, portfolioRunID string) ([]Dep, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, portfolio_run_id, upstream_project, downstream_project, created_at
		FROM project_deps WHERE portfolio_run_id = ? ORDER BY id ASC`, portfolioRunID)
	if err != nil {
		return nil, fmt.Errorf("list deps: %w", err)
	}
	defer rows.Close()

	var deps []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.ID, &d.PortfolioRunID, &d.UpstreamProject, &d.DownstreamProject, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("list deps scan: %w", err)
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

// Remove deletes a specific dependency edge.
func (s *DepStore) Remove(ctx context.Context, portfolioRunID, upstream, downstream string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM project_deps WHERE portfolio_run_id = ? AND upstream_project = ? AND downstream_project = ?`,
		portfolioRunID, upstream, downstream,
	)
	if err != nil {
		return fmt.Errorf("remove dep: %w", err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return fmt.Errorf("remove dep: not found")
	}
	return nil
}

// GetDownstream returns all downstream projects for a given upstream project in a portfolio.
func (s *DepStore) GetDownstream(ctx context.Context, portfolioRunID, upstream string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT downstream_project FROM project_deps
		WHERE portfolio_run_id = ? AND upstream_project = ?
		ORDER BY downstream_project ASC`, portfolioRunID, upstream)
	if err != nil {
		return nil, fmt.Errorf("get downstream: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("get downstream scan: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// GetUpstream returns all upstream projects for a given downstream project in a portfolio.
func (s *DepStore) GetUpstream(ctx context.Context, portfolioRunID, downstream string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT upstream_project FROM project_deps
		WHERE portfolio_run_id = ? AND downstream_project = ?
		ORDER BY upstream_project ASC`, portfolioRunID, downstream)
	if err != nil {
		return nil, fmt.Errorf("get upstream: %w", err)
	}
	defer rows.Close()

	var projects []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("get upstream scan: %w", err)
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}
