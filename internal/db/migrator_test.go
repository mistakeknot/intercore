package db

import (
	"context"
	"testing"
)

func TestMigrator_EmptyDB(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}

	applied, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if applied == 0 {
		t.Error("expected at least 1 migration applied")
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", v, currentSchemaVersion)
	}

	// Verify key tables exist
	for _, table := range []string{"state", "sentinels", "dispatches", "runs", "coordination_locks", "scheduler_jobs", "run_replay_inputs"} {
		var name string
		err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Errorf("table %s not found: %v", table, err)
		}
	}
}

func TestMigrator_Idempotent(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("first Run: %v", err)
	}

	applied, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if applied != 0 {
		t.Errorf("second Run applied %d, want 0", applied)
	}
}

func TestMigrator_V16Upgrade(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Simulate v16 DB by applying the DDL (which uses IF NOT EXISTS)
	if _, err := d.db.ExecContext(ctx, schemaDDL); err != nil {
		t.Fatalf("apply DDL: %v", err)
	}
	if _, err := d.db.ExecContext(ctx, "PRAGMA user_version = 16"); err != nil {
		t.Fatalf("set version: %v", err)
	}

	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}

	applied, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if applied == 0 {
		t.Error("expected some migrations applied for v16 DB")
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", v, currentSchemaVersion)
	}
}

func TestMigrator_V20NoOp(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Use Migrate() which sets user_version = currentSchemaVersion
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}

	applied, err := m.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if applied != 0 {
		t.Errorf("applied = %d, want 0 for fully-migrated DB", applied)
	}
}

func TestMigrator_FinalSchemaShape(t *testing.T) {
	expectedTables := []string{
		"state", "sentinels", "dispatches", "merge_intents", "runs",
		"phase_events", "run_agents", "run_artifacts", "dispatch_events",
		"interspect_events", "discoveries", "discovery_events",
		"feedback_signals", "interest_profile", "project_deps",
		"lanes", "lane_events", "lane_members", "phase_actions",
		"audit_log", "cost_reconciliations", "coordination_locks",
		"coordination_events", "scheduler_jobs", "run_replay_inputs",
	}

	scenarios := []struct {
		name    string
		setupFn func(t *testing.T, d *DB)
	}{
		{"empty_db", func(t *testing.T, d *DB) {}},
		{"v16_db", func(t *testing.T, d *DB) {
			ctx := context.Background()
			if _, err := d.db.ExecContext(ctx, schemaDDL); err != nil {
				t.Fatalf("apply DDL: %v", err)
			}
			if _, err := d.db.ExecContext(ctx, "PRAGMA user_version = 16"); err != nil {
				t.Fatalf("set version: %v", err)
			}
		}},
		{"v20_db", func(t *testing.T, d *DB) {
			if err := d.Migrate(context.Background()); err != nil {
				t.Fatalf("Migrate: %v", err)
			}
		}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			d, _ := tempDB(t)
			ctx := context.Background()
			sc.setupFn(t, d)

			m, err := NewMigrator(d)
			if err != nil {
				t.Fatalf("NewMigrator: %v", err)
			}
			if _, err := m.Run(ctx); err != nil {
				t.Fatalf("Run: %v", err)
			}

			for _, table := range expectedTables {
				var name string
				err = d.db.QueryRow(
					"SELECT name FROM sqlite_master WHERE type='table' AND name=?",
					table,
				).Scan(&name)
				if err != nil {
					t.Errorf("[%s] table %s not found: %v", sc.name, table, err)
				}
			}

			v, err := d.SchemaVersion()
			if err != nil {
				t.Fatal(err)
			}
			if v != currentSchemaVersion {
				t.Errorf("[%s] SchemaVersion = %d, want %d", sc.name, v, currentSchemaVersion)
			}
		})
	}
}
