package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migration represents a numbered migration file.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Baseline bool
}

// Migrator applies versioned migration files sequentially.
type Migrator struct {
	db         *DB
	migrations []Migration
}

// NewMigrator creates a Migrator that reads embedded migration files.
func NewMigrator(d *DB) (*Migrator, error) {
	var migrations []Migration

	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations dir: %w", err)
	}

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), ".sql")
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		version, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}

		data, err := migrationsFS.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}

		migrations = append(migrations, Migration{
			Version:  version,
			Name:     name,
			SQL:      string(data),
			Baseline: strings.Contains(name, "baseline"),
		})
	}

	sort.Slice(migrations, func(i, j int) bool {
		return migrations[i].Version < migrations[j].Version
	})

	return &Migrator{db: d, migrations: migrations}, nil
}

// MaxVersion returns the highest migration version available.
func (m *Migrator) MaxVersion() int {
	if len(m.migrations) == 0 {
		return 0
	}
	return m.migrations[len(m.migrations)-1].Version
}

// Run applies pending migrations and returns the count applied.
func (m *Migrator) Run(ctx context.Context) (int, error) {
	currentVersion, err := m.db.SchemaVersion()
	if err != nil {
		return 0, fmt.Errorf("read version: %w", err)
	}

	if currentVersion == 0 {
		return m.applyBaseline(ctx)
	}

	return m.applyAdditive(ctx, currentVersion)
}

func (m *Migrator) applyBaseline(ctx context.Context) (int, error) {
	for _, mig := range m.migrations {
		if !mig.Baseline {
			continue
		}
		tx, err := m.db.db.BeginTx(ctx, nil)
		if err != nil {
			return 0, fmt.Errorf("begin baseline: %w", err)
		}

		if _, err := tx.ExecContext(ctx, mig.SQL); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("apply baseline %s: %w", mig.Name, err)
		}

		// Set version to max available — baseline includes the full schema,
		// so no additive migrations should run after it.
		targetVersion := m.MaxVersion()
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", targetVersion)); err != nil {
			tx.Rollback()
			return 0, fmt.Errorf("set version %d: %w", targetVersion, err)
		}
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("commit baseline: %w", err)
		}
		return 1, nil
	}
	return 0, fmt.Errorf("no baseline migration found")
}

func (m *Migrator) applyAdditive(ctx context.Context, currentVersion int) (int, error) {
	applied := 0
	for _, mig := range m.migrations {
		if mig.Baseline || mig.Version <= currentVersion {
			continue
		}
		tx, err := m.db.db.BeginTx(ctx, nil)
		if err != nil {
			return applied, fmt.Errorf("begin migration %s: %w", mig.Name, err)
		}

		if _, err := tx.ExecContext(ctx, mig.SQL); err != nil {
			// ALTER TABLE ADD COLUMN may fail with "duplicate column" if a prior
			// partial run added it. Tolerate this for idempotency.
			if !isDuplicateColumnError(err) {
				tx.Rollback()
				return applied, fmt.Errorf("apply migration %s: %w", mig.Name, err)
			}
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", mig.Version)); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("set version %d: %w", mig.Version, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("commit migration %s: %w", mig.Name, err)
		}
		applied++
	}
	return applied, nil
}
