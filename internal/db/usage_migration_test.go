package db

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSchema040FreshAndV39UpgradeThroughBothAPIs(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name    string
		prepare func(*testing.T, *DB)
		migrate func(context.Context, *DB) (int, error)
	}{
		{
			name:    "db_migrate_fresh",
			prepare: func(*testing.T, *DB) {},
			migrate: func(ctx context.Context, d *DB) (int, error) {
				return 1, d.Migrate(ctx)
			},
		},
		{
			name:    "numbered_migrator_fresh",
			prepare: func(*testing.T, *DB) {},
			migrate: func(ctx context.Context, d *DB) (int, error) {
				m, err := NewMigrator(d)
				if err != nil {
					return 0, err
				}
				return m.Run(ctx)
			},
		},
		{
			name:    "db_migrate_v39",
			prepare: prepareSchemaV39,
			migrate: func(ctx context.Context, d *DB) (int, error) {
				return 1, d.Migrate(ctx)
			},
		},
		{
			name:    "numbered_migrator_v39",
			prepare: prepareSchemaV39,
			migrate: func(ctx context.Context, d *DB) (int, error) {
				m, err := NewMigrator(d)
				if err != nil {
					return 0, err
				}
				return m.Run(ctx)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d, _ := tempDB(t)
			tt.prepare(t, d)
			applied, err := tt.migrate(ctx, d)
			if err != nil {
				t.Fatalf("migrate: %v", err)
			}
			if applied != 1 {
				t.Fatalf("applied = %d, want 1", applied)
			}
			assertSchema040(t, d)

			m, err := NewMigrator(d)
			if err != nil {
				t.Fatal(err)
			}
			if again, err := m.Run(ctx); err != nil || again != 0 {
				t.Fatalf("idempotent Run = (%d, %v), want (0, nil)", again, err)
			}
		})
	}
}

func TestSchema040CurrentAndNumberedDDLAreEquivalent(t *testing.T) {
	ctx := context.Background()
	fresh, _ := tempDB(t)
	if err := fresh.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	upgraded, _ := tempDB(t)
	prepareSchemaV39(t, upgraded)
	m, err := NewMigrator(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Run(ctx); err != nil {
		t.Fatal(err)
	}

	objects := func(d *DB) []string {
		t.Helper()
		rows, err := d.db.Query(`
			SELECT type || ':' || name || ':' || replace(replace(sql, char(10), ' '), '  ', ' ')
			FROM sqlite_master
			WHERE (name LIKE 'usage_observations%' OR name LIKE 'usage_validations%' OR name LIKE 'idx_usage_%')
			  AND sql IS NOT NULL
			ORDER BY type, name`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var result []string
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			result = append(result, strings.Join(strings.Fields(value), " "))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got, want := objects(upgraded), objects(fresh); !reflect.DeepEqual(got, want) {
		t.Fatalf("v40 schema differs between migration paths\nnumbered: %#v\ncurrent:  %#v", got, want)
	}
}

func TestSchema040AppendOnlyTriggers(t *testing.T) {
	d, _ := tempDB(t)
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO usage_observations
		(id, provider, source, kind, status, captured_at, counters_json,
		 identity_json, execution_refs_json, payload_sha256, canonical_input,
		 canonical_sha256, inserted_at)
		VALUES ('u1','p','s','account_usage','available','2026-09-12T20:00:00Z','[]',
		 '{}','{}',printf('%064d',0),X'7B7D',printf('%064d',1),1)`); err != nil {
		t.Fatalf("seed observation: %v", err)
	}
	if _, err := d.db.Exec(`INSERT INTO usage_validations
		(observation_id, evaluated_at, max_age_seconds, evidence_json)
		VALUES ('u1',2,3600,'{}')`); err != nil {
		t.Fatalf("seed validation: %v", err)
	}

	for _, stmt := range []string{
		"UPDATE usage_observations SET status='error' WHERE id='u1'",
		"DELETE FROM usage_observations WHERE id='u1'",
		"UPDATE usage_validations SET max_age_seconds=1 WHERE observation_id='u1'",
		"DELETE FROM usage_validations WHERE observation_id='u1'",
	} {
		if _, err := d.db.Exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q error = %v, want append-only abort", stmt, err)
		}
	}
}

func TestSchema040DBMigrateRollbackDoesNotAdvanceVersion(t *testing.T) {
	d, _ := tempDB(t)
	prepareSchemaV39(t, d)

	original := usageMigrationDDL
	usageMigrationDDL = `CREATE TABLE usage_migration_partial (id INTEGER); SELECT no_such_function();`
	t.Cleanup(func() { usageMigrationDDL = original })

	if err := d.Migrate(context.Background()); err == nil {
		t.Fatal("Migrate succeeded with broken v40 DDL")
	}
	version, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 39 {
		t.Fatalf("version = %d, want 39", version)
	}
	var count int
	if err := d.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='usage_migration_partial'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("partial v40 DDL survived rollback")
	}
}

func TestSchema040NumberedRollbackDoesNotAdvanceVersion(t *testing.T) {
	d, _ := tempDB(t)
	prepareSchemaV39(t, d)
	m, err := NewMigrator(d)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.migrations {
		if m.migrations[i].Version == 40 {
			m.migrations[i].SQL = `CREATE TABLE usage_migration_partial (id INTEGER); SELECT no_such_function();`
		}
	}
	if _, err := m.Run(context.Background()); err == nil {
		t.Fatal("Run succeeded with broken numbered v40 DDL")
	}
	version, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 39 {
		t.Fatalf("version = %d, want 39", version)
	}
	var count int
	if err := d.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='usage_migration_partial'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("partial numbered v40 DDL survived rollback")
	}
}

func prepareSchemaV39(t *testing.T, d *DB) {
	t.Helper()
	// Use the exact pre-feature schema, rather than manufacturing v39 by
	// applying the current schema and dropping objects introduced by v40.
	schema, err := os.ReadFile("testdata/schema_v39.sql")
	if err != nil {
		t.Fatalf("load frozen v39 schema: %v", err)
	}
	if _, err := d.db.ExecContext(context.Background(), string(schema)); err != nil {
		t.Fatalf("apply frozen v39 schema: %v", err)
	}
	if _, err := d.db.Exec("PRAGMA user_version = 39"); err != nil {
		t.Fatalf("mark frozen schema v39: %v", err)
	}
}

func assertSchema040(t *testing.T, d *DB) {
	t.Helper()
	version, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != 40 {
		t.Fatalf("version = %d, want 40", version)
	}

	for _, object := range []struct{ typ, name string }{
		{"table", "usage_observations"},
		{"table", "usage_validations"},
		{"trigger", "usage_observations_no_update"},
		{"trigger", "usage_observations_no_delete"},
		{"trigger", "usage_validations_no_update"},
		{"trigger", "usage_validations_no_delete"},
	} {
		var found string
		if err := d.db.QueryRow(`SELECT name FROM sqlite_master WHERE type=? AND name=?`, object.typ, object.name).Scan(&found); err != nil {
			t.Errorf("%s %s missing: %v", object.typ, object.name, err)
		}
	}

	var tables int
	if err := d.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name LIKE 'usage_%'`).Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if tables != 2 {
		t.Errorf("usage tables = %d, want exactly 2", tables)
	}
}
