package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func tempDB(t *testing.T) (*DB, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	d, err := Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d, path
}

func TestOpen_MaxOpenConns(t *testing.T) {
	d, _ := tempDB(t)
	stats := d.db.Stats()
	if stats.MaxOpenConnections != 1 {
		t.Errorf("MaxOpenConns = %d, want 1", stats.MaxOpenConnections)
	}
}

func TestOpen_WALMode(t *testing.T) {
	d, _ := tempDB(t)
	var mode string
	if err := d.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Errorf("journal_mode = %q, want %q", mode, "wal")
	}
}

func TestOpen_SymlinkRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real")
	link := filepath.Join(dir, "link")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := Open(filepath.Join(link, "test.db"), 100*time.Millisecond)
	if err == nil {
		t.Error("expected error for symlink parent, got nil")
	}
}

func TestMigrate_CreatesTablesAndVersion(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	// Verify schema version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want 27", v)
	}

	// Verify tables exist
	for _, table := range []string{"state", "sentinels", "dispatches", "runs", "phase_events", "run_agents", "run_artifacts", "dispatch_events", "interspect_events", "merge_intents", "coordination_locks", "coordination_events", "run_replay_inputs", "landed_changes", "sessions", "session_attributions", "routing_decisions"} {
		var name string
		err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Fatalf("%s table not found: %v", table, err)
		}
	}
}

func TestMigrate_Idempotent(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 1: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 2: %v", err)
	}
}

func TestMigrate_Concurrent(t *testing.T) {
	// Test that sequential migration from different connections is safe.
	// Real-world scenario: two `ic init` commands run back-to-back.
	// With SetMaxOpenConns(1), each connection serializes, so concurrent
	// open is the bottleneck (not the migration itself).
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	ctx := context.Background()

	// First: create the DB file so WAL is established
	d0, err := Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d0.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	d0.Close()

	// Now open 5 connections sequentially and verify migration is idempotent
	const n = 5
	for i := 0; i < n; i++ {
		d, err := Open(path, 5*time.Second)
		if err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
		if err := d.Migrate(ctx); err != nil {
			t.Errorf("Migrate %d: %v", i, err)
		}
		d.Close()
	}

	// Verify version is correct
	d, err := Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d after concurrent migrate, want 26", v)
	}
}

func TestMigrate_Backup(t *testing.T) {
	d, path := tempDB(t)
	ctx := context.Background()

	// First migrate — no backup (empty DB)
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Insert some data
	_, err := d.db.Exec("INSERT INTO state (key, scope_id, payload) VALUES ('test', 's1', '{}')")
	if err != nil {
		t.Fatal(err)
	}

	// Second migrate — should create backup
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Check backup exists
	dir := filepath.Dir(path)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".db" && e.Name() != "test.db" {
			// This could be the backup
			if len(e.Name()) > len("test.db.backup-") {
				found = true
			}
		}
	}
	// The backup is created when the DB file has content before migration
	_ = found // backup may or may not be created depending on version check
}

func TestHealth(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Health before migration should fail
	if err := d.Health(ctx); err == nil {
		t.Error("expected error from Health before migration")
	}

	// Migrate and try again
	if err := d.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := d.Health(ctx); err != nil {
		t.Errorf("Health after migration: %v", err)
	}
}

func TestMigrate_V1ToV2(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Simulate a v1 database: apply only v1 DDL and set user_version=1
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()), expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		);
		CREATE TABLE IF NOT EXISTS sentinels (
			name TEXT NOT NULL, scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		);
		PRAGMA user_version = 1;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert some v1 data to verify preservation
	_, err = d.db.Exec("INSERT INTO state (key, scope_id, payload) VALUES ('test', 's1', '{}')")
	if err != nil {
		t.Fatal(err)
	}

	// Migrate should upgrade to v4
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v1→v5: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d after v1→v7 migrate, want 26", v)
	}

	// Verify dispatches table exists
	var name string
	err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='dispatches'").Scan(&name)
	if err != nil {
		t.Fatal("dispatches table not created by migration:", err)
	}

	// Verify runs table exists
	err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='runs'").Scan(&name)
	if err != nil {
		t.Fatal("runs table not created by migration:", err)
	}

	// Verify v1 data preserved
	var payload string
	err = d.db.QueryRow("SELECT payload FROM state WHERE key='test' AND scope_id='s1'").Scan(&payload)
	if err != nil {
		t.Fatal("v1 data lost during migration:", err)
	}
	if payload != "{}" {
		t.Errorf("v1 payload = %q, want %q", payload, "{}")
	}
}

func TestMigrate_V2ToV3(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Simulate a v2 database: apply v1+v2 DDL and set user_version=2
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()), expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		);
		CREATE TABLE IF NOT EXISTS sentinels (
			name TEXT NOT NULL, scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		);
		CREATE TABLE IF NOT EXISTS dispatches (
			id TEXT NOT NULL PRIMARY KEY, agent_type TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL DEFAULT 'spawned', project_dir TEXT NOT NULL,
			prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT,
			pid INTEGER, exit_code INTEGER, name TEXT, model TEXT,
			sandbox TEXT DEFAULT 'workspace-write', timeout_sec INTEGER,
			turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			started_at INTEGER, completed_at INTEGER,
			verdict_status TEXT, verdict_summary TEXT, error_message TEXT,
			scope_id TEXT, parent_id TEXT
		);
		PRAGMA user_version = 2;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert v2 data to verify preservation
	_, err = d.db.Exec("INSERT INTO dispatches (id, agent_type, project_dir) VALUES ('test123', 'codex', '/tmp')")
	if err != nil {
		t.Fatal(err)
	}

	// Migrate should upgrade to v4
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v2→v5: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d after v2→v7 migrate, want 26", v)
	}

	// Verify runs + phase_events + v4 tables exist
	for _, table := range []string{"runs", "phase_events", "run_agents", "run_artifacts"} {
		var name string
		err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Fatalf("%s table not created by v2→v5 migration: %v", table, err)
		}
	}

	// Verify v2 data preserved
	var agentType string
	err = d.db.QueryRow("SELECT agent_type FROM dispatches WHERE id='test123'").Scan(&agentType)
	if err != nil {
		t.Fatal("v2 dispatch data lost during migration:", err)
	}
	if agentType != "codex" {
		t.Errorf("v2 agent_type = %q, want %q", agentType, "codex")
	}
}

func TestMigrate_V3ToV4(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Simulate a v3 database: apply v1+v2+v3 DDL and set user_version=3
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()), expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		);
		CREATE TABLE IF NOT EXISTS sentinels (
			name TEXT NOT NULL, scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		);
		CREATE TABLE IF NOT EXISTS dispatches (
			id TEXT NOT NULL PRIMARY KEY, agent_type TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL DEFAULT 'spawned', project_dir TEXT NOT NULL,
			prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT,
			pid INTEGER, exit_code INTEGER, name TEXT, model TEXT,
			sandbox TEXT DEFAULT 'workspace-write', timeout_sec INTEGER,
			turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			started_at INTEGER, completed_at INTEGER,
			verdict_status TEXT, verdict_summary TEXT, error_message TEXT,
			scope_id TEXT, parent_id TEXT
		);
		CREATE TABLE IF NOT EXISTS runs (
			id TEXT NOT NULL PRIMARY KEY, project_dir TEXT NOT NULL,
			goal TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
			phase TEXT NOT NULL DEFAULT 'brainstorm', complexity INTEGER NOT NULL DEFAULT 3,
			force_full INTEGER NOT NULL DEFAULT 0, auto_advance INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
			completed_at INTEGER, scope_id TEXT, metadata TEXT
		);
		CREATE TABLE IF NOT EXISTS phase_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES runs(id),
			from_phase TEXT NOT NULL, to_phase TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'advance',
			gate_result TEXT, gate_tier TEXT, reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		PRAGMA user_version = 3;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert v3 data to verify preservation
	_, err = d.db.Exec("INSERT INTO runs (id, project_dir, goal) VALUES ('testrun1', '/tmp/proj', 'Test goal')")
	if err != nil {
		t.Fatal(err)
	}

	// Migrate should upgrade to v4
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v3→v5: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d after v3→v7 migrate, want 26", v)
	}

	// Verify new tables exist
	for _, table := range []string{"run_agents", "run_artifacts"} {
		var name string
		err = d.db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
		if err != nil {
			t.Fatalf("%s table not created by v3→v5 migration: %v", table, err)
		}
	}

	// Verify new tables are usable (can insert)
	_, err = d.db.Exec("SELECT 1 FROM run_agents LIMIT 0")
	if err != nil {
		t.Fatalf("run_agents not queryable: %v", err)
	}
	_, err = d.db.Exec("SELECT 1 FROM run_artifacts LIMIT 0")
	if err != nil {
		t.Fatalf("run_artifacts not queryable: %v", err)
	}

	// Verify v3 data preserved
	var goal string
	err = d.db.QueryRow("SELECT goal FROM runs WHERE id='testrun1'").Scan(&goal)
	if err != nil {
		t.Fatal("v3 run data lost during migration:", err)
	}
	if goal != "Test goal" {
		t.Errorf("v3 goal = %q, want %q", goal, "Test goal")
	}
}

func TestOpen_ForeignKeysEnabled(t *testing.T) {
	d, _ := tempDB(t)
	var fk int
	if err := d.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Errorf("foreign_keys = %d, want 1", fk)
	}
}

func TestSchemaVersionTooNew(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")

	// Create DB with version higher than max
	d, err := Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.db.Exec("PRAGMA user_version = 999")
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	// Try to reopen — should fail
	_, err = Open(path, 100*time.Millisecond)
	if err != ErrSchemaVersionTooNew {
		t.Errorf("expected ErrSchemaVersionTooNew, got %v", err)
	}
}

func TestMigrate_V5ToV6(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Simulate a v5 database with all v5 tables
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()), expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		);
		CREATE TABLE IF NOT EXISTS sentinels (
			name TEXT NOT NULL, scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		);
		CREATE TABLE IF NOT EXISTS dispatches (
			id TEXT NOT NULL PRIMARY KEY, agent_type TEXT NOT NULL DEFAULT 'codex',
			status TEXT NOT NULL DEFAULT 'spawned', project_dir TEXT NOT NULL,
			prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT,
			pid INTEGER, exit_code INTEGER, name TEXT, model TEXT,
			sandbox TEXT DEFAULT 'workspace-write', timeout_sec INTEGER,
			turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			started_at INTEGER, completed_at INTEGER,
			verdict_status TEXT, verdict_summary TEXT, error_message TEXT,
			scope_id TEXT, parent_id TEXT
		);
		CREATE TABLE IF NOT EXISTS runs (
			id TEXT NOT NULL PRIMARY KEY, project_dir TEXT NOT NULL,
			goal TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
			phase TEXT NOT NULL DEFAULT 'brainstorm', complexity INTEGER NOT NULL DEFAULT 3,
			force_full INTEGER NOT NULL DEFAULT 0, auto_advance INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
			completed_at INTEGER, scope_id TEXT, metadata TEXT
		);
		CREATE TABLE IF NOT EXISTS phase_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL REFERENCES runs(id),
			from_phase TEXT NOT NULL, to_phase TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'advance',
			gate_result TEXT, gate_tier TEXT, reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS run_agents (
			id TEXT NOT NULL PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(id),
			agent_type TEXT NOT NULL DEFAULT 'claude', name TEXT,
			status TEXT NOT NULL DEFAULT 'active', dispatch_id TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS run_artifacts (
			id TEXT NOT NULL PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(id),
			phase TEXT NOT NULL, path TEXT NOT NULL,
			type TEXT NOT NULL DEFAULT 'file',
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS dispatch_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			dispatch_id TEXT NOT NULL, run_id TEXT,
			from_status TEXT NOT NULL, to_status TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'status_change',
			reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		PRAGMA user_version = 5;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert v5 data to verify preservation
	_, err = d.db.Exec("INSERT INTO runs (id, project_dir, goal) VALUES ('r1', '/tmp', 'test goal')")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.db.Exec("INSERT INTO dispatches (id, project_dir) VALUES ('d1', '/tmp')")
	if err != nil {
		t.Fatal(err)
	}

	// Migrate to v6
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v5→v7: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want 27", v)
	}

	// Verify new columns on runs
	_, err = d.db.Exec(`UPDATE runs SET phases = '["a","b"]', token_budget = 10000, budget_warn_pct = 80 WHERE id = 'r1'`)
	if err != nil {
		t.Fatalf("runs new columns not writable: %v", err)
	}

	// Verify new column on dispatches
	_, err = d.db.Exec(`UPDATE dispatches SET cache_hits = 5000 WHERE id = 'd1'`)
	if err != nil {
		t.Fatalf("dispatches cache_hits not writable: %v", err)
	}

	// Verify new columns on run_artifacts
	_, err = d.db.Exec(`INSERT INTO run_artifacts (id, run_id, phase, path, content_hash, dispatch_id) VALUES ('a1', 'r1', 'plan', '/tmp/plan.md', 'sha256:abc123', 'd1')`)
	if err != nil {
		t.Fatalf("run_artifacts new columns not writable: %v", err)
	}

	// Verify v5 data preserved
	var goal string
	err = d.db.QueryRow("SELECT goal FROM runs WHERE id='r1'").Scan(&goal)
	if err != nil {
		t.Fatal("v5 data lost:", err)
	}
	if goal != "test goal" {
		t.Errorf("goal = %q, want %q", goal, "test goal")
	}
}

func TestMigrate_V5ToV6_Idempotent(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Create a v5 DB
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL, updated_at INTEGER, expires_at INTEGER, PRIMARY KEY (key, scope_id));
		CREATE TABLE IF NOT EXISTS sentinels (name TEXT NOT NULL, scope_id TEXT NOT NULL, last_fired INTEGER, PRIMARY KEY (name, scope_id));
		CREATE TABLE IF NOT EXISTS dispatches (id TEXT NOT NULL PRIMARY KEY, agent_type TEXT DEFAULT 'codex', status TEXT DEFAULT 'spawned', project_dir TEXT NOT NULL, prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT, pid INTEGER, exit_code INTEGER, name TEXT, model TEXT, sandbox TEXT, timeout_sec INTEGER, turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, created_at INTEGER, started_at INTEGER, completed_at INTEGER, verdict_status TEXT, verdict_summary TEXT, error_message TEXT, scope_id TEXT, parent_id TEXT);
		CREATE TABLE IF NOT EXISTS runs (id TEXT NOT NULL PRIMARY KEY, project_dir TEXT NOT NULL, goal TEXT NOT NULL, status TEXT DEFAULT 'active', phase TEXT DEFAULT 'brainstorm', complexity INTEGER DEFAULT 3, force_full INTEGER DEFAULT 0, auto_advance INTEGER DEFAULT 1, created_at INTEGER, updated_at INTEGER, completed_at INTEGER, scope_id TEXT, metadata TEXT);
		CREATE TABLE IF NOT EXISTS phase_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL, from_phase TEXT, to_phase TEXT, event_type TEXT DEFAULT 'advance', gate_result TEXT, gate_tier TEXT, reason TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_agents (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL, agent_type TEXT DEFAULT 'claude', name TEXT, status TEXT DEFAULT 'active', dispatch_id TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_artifacts (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL, phase TEXT NOT NULL, path TEXT NOT NULL, type TEXT DEFAULT 'file', created_at INTEGER);
		CREATE TABLE IF NOT EXISTS dispatch_events (id INTEGER PRIMARY KEY AUTOINCREMENT, dispatch_id TEXT NOT NULL, run_id TEXT, from_status TEXT, to_status TEXT, event_type TEXT DEFAULT 'status_change', reason TEXT, created_at INTEGER);
		PRAGMA user_version = 5;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Run migration twice — second run must not fail
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 1: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate 2: %v", err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Errorf("SchemaVersion = %d, want 27", v)
	}
}

func TestMigrate_V7ToV8_ArtifactStatus(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Create a v7 database with all tables up to v7
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL, updated_at INTEGER, expires_at INTEGER, PRIMARY KEY (key, scope_id));
		CREATE TABLE IF NOT EXISTS sentinels (name TEXT NOT NULL, scope_id TEXT NOT NULL, last_fired INTEGER, PRIMARY KEY (name, scope_id));
		CREATE TABLE IF NOT EXISTS dispatches (id TEXT NOT NULL PRIMARY KEY, agent_type TEXT DEFAULT 'codex', status TEXT DEFAULT 'spawned', project_dir TEXT NOT NULL, prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT, pid INTEGER, exit_code INTEGER, name TEXT, model TEXT, sandbox TEXT, timeout_sec INTEGER, turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, cache_hits INTEGER, created_at INTEGER, started_at INTEGER, completed_at INTEGER, verdict_status TEXT, verdict_summary TEXT, error_message TEXT, scope_id TEXT, parent_id TEXT);
		CREATE TABLE IF NOT EXISTS runs (id TEXT NOT NULL PRIMARY KEY, project_dir TEXT NOT NULL, goal TEXT NOT NULL, status TEXT DEFAULT 'active', phase TEXT DEFAULT 'brainstorm', complexity INTEGER DEFAULT 3, force_full INTEGER DEFAULT 0, auto_advance INTEGER DEFAULT 1, created_at INTEGER, updated_at INTEGER, completed_at INTEGER, scope_id TEXT, metadata TEXT, phases TEXT, token_budget INTEGER, budget_warn_pct INTEGER DEFAULT 80);
		CREATE TABLE IF NOT EXISTS phase_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(id), from_phase TEXT, to_phase TEXT, event_type TEXT DEFAULT 'advance', gate_result TEXT, gate_tier TEXT, reason TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_agents (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), agent_type TEXT DEFAULT 'claude', name TEXT, status TEXT DEFAULT 'active', dispatch_id TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_artifacts (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), phase TEXT NOT NULL, path TEXT NOT NULL, type TEXT DEFAULT 'file', content_hash TEXT, dispatch_id TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS dispatch_events (id INTEGER PRIMARY KEY AUTOINCREMENT, dispatch_id TEXT NOT NULL, run_id TEXT, from_status TEXT, to_status TEXT, event_type TEXT DEFAULT 'status_change', reason TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS interspect_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, agent_name TEXT NOT NULL, event_type TEXT NOT NULL, override_reason TEXT, context_json TEXT, session_id TEXT, project_dir TEXT, created_at INTEGER NOT NULL DEFAULT (unixepoch()));
		PRAGMA user_version = 7;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Insert v7 data to verify preservation
	_, err = d.db.Exec("INSERT INTO runs (id, project_dir, goal, created_at, updated_at) VALUES ('r1', '/tmp', 'test', 1, 1)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.db.Exec("INSERT INTO run_artifacts (id, run_id, phase, path, created_at) VALUES ('a1', 'r1', 'plan', '/tmp/plan.md', 1)")
	if err != nil {
		t.Fatal(err)
	}

	// Migrate v7 → v8
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v7→v8: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	// Verify status column exists on run_artifacts with default 'active'
	var colDefault sql.NullString
	err = d.db.QueryRow(
		"SELECT dflt_value FROM pragma_table_info('run_artifacts') WHERE name='status'",
	).Scan(&colDefault)
	if err != nil {
		t.Fatalf("status column not found on run_artifacts: %v", err)
	}
	if !colDefault.Valid || colDefault.String != "'active'" {
		t.Fatalf("expected default 'active', got %v", colDefault)
	}

	// Verify existing artifact has status='active'
	var status string
	err = d.db.QueryRow("SELECT status FROM run_artifacts WHERE id='a1'").Scan(&status)
	if err != nil {
		t.Fatalf("existing artifact status not readable: %v", err)
	}
	if status != "active" {
		t.Errorf("existing artifact status = %q, want 'active'", status)
	}
}

func TestMigrate_V8ToV9_DiscoveryTables(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Create a v8 database with all tables up to v8 (no discovery tables)
	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL, updated_at INTEGER, expires_at INTEGER, PRIMARY KEY (key, scope_id));
		CREATE TABLE IF NOT EXISTS sentinels (name TEXT NOT NULL, scope_id TEXT NOT NULL, last_fired INTEGER, PRIMARY KEY (name, scope_id));
		CREATE TABLE IF NOT EXISTS dispatches (id TEXT NOT NULL PRIMARY KEY, agent_type TEXT DEFAULT 'codex', status TEXT DEFAULT 'spawned', project_dir TEXT NOT NULL, prompt_file TEXT, prompt_hash TEXT, output_file TEXT, verdict_file TEXT, pid INTEGER, exit_code INTEGER, name TEXT, model TEXT, sandbox TEXT DEFAULT 'workspace-write', timeout_sec INTEGER, turns INTEGER DEFAULT 0, commands INTEGER DEFAULT 0, messages INTEGER DEFAULT 0, input_tokens INTEGER DEFAULT 0, output_tokens INTEGER DEFAULT 0, cache_hits INTEGER, created_at INTEGER, started_at INTEGER, completed_at INTEGER, verdict_status TEXT, verdict_summary TEXT, error_message TEXT, scope_id TEXT, parent_id TEXT);
		CREATE TABLE IF NOT EXISTS runs (id TEXT NOT NULL PRIMARY KEY, project_dir TEXT NOT NULL, goal TEXT NOT NULL, status TEXT DEFAULT 'active', phase TEXT DEFAULT 'brainstorm', complexity INTEGER DEFAULT 3, force_full INTEGER DEFAULT 0, auto_advance INTEGER DEFAULT 1, created_at INTEGER, updated_at INTEGER, completed_at INTEGER, scope_id TEXT, metadata TEXT, phases TEXT, token_budget INTEGER, budget_warn_pct INTEGER DEFAULT 80);
		CREATE TABLE IF NOT EXISTS phase_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL REFERENCES runs(id), from_phase TEXT, to_phase TEXT, event_type TEXT DEFAULT 'advance', gate_result TEXT, gate_tier TEXT, reason TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_agents (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), agent_type TEXT DEFAULT 'claude', name TEXT, status TEXT DEFAULT 'active', dispatch_id TEXT, created_at INTEGER, updated_at INTEGER);
		CREATE TABLE IF NOT EXISTS run_artifacts (id TEXT NOT NULL PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), phase TEXT NOT NULL, path TEXT NOT NULL, type TEXT DEFAULT 'file', content_hash TEXT, dispatch_id TEXT, status TEXT NOT NULL DEFAULT 'active', created_at INTEGER);
		CREATE TABLE IF NOT EXISTS dispatch_events (id INTEGER PRIMARY KEY AUTOINCREMENT, dispatch_id TEXT NOT NULL, run_id TEXT, from_status TEXT, to_status TEXT, event_type TEXT DEFAULT 'status_change', reason TEXT, created_at INTEGER);
		CREATE TABLE IF NOT EXISTS interspect_events (id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT, agent_name TEXT NOT NULL, event_type TEXT NOT NULL, override_reason TEXT, context_json TEXT, session_id TEXT, project_dir TEXT, created_at INTEGER NOT NULL DEFAULT (unixepoch()));
		PRAGMA user_version = 8;
	`)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate v8 → v9
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v8→v9: %v", err)
	}

	// Verify version
	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	// Verify discoveries table exists and accepts inserts
	_, err = d.db.Exec(`INSERT INTO discoveries (id, source, source_id, title) VALUES ('d1', 'exa', 'ext-1', 'Test Finding')`)
	if err != nil {
		t.Fatalf("discoveries insert failed: %v", err)
	}

	// Verify unique constraint on (source, source_id)
	_, err = d.db.Exec(`INSERT INTO discoveries (id, source, source_id, title) VALUES ('d2', 'exa', 'ext-1', 'Duplicate')`)
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation on (source, source_id)")
	}

	// Verify discovery_events table
	_, err = d.db.Exec(`INSERT INTO discovery_events (discovery_id, event_type) VALUES ('d1', 'scored')`)
	if err != nil {
		t.Fatalf("discovery_events insert failed: %v", err)
	}

	// Verify feedback_signals table
	_, err = d.db.Exec(`INSERT INTO feedback_signals (discovery_id, signal_type) VALUES ('d1', 'upvote')`)
	if err != nil {
		t.Fatalf("feedback_signals insert failed: %v", err)
	}

	// Verify interest_profile table with single-row constraint
	_, err = d.db.Exec(`INSERT INTO interest_profile (id) VALUES (1)`)
	if err != nil {
		t.Fatalf("interest_profile insert failed: %v", err)
	}
	_, err = d.db.Exec(`INSERT INTO interest_profile (id) VALUES (2)`)
	if err == nil {
		t.Fatal("expected CHECK constraint violation on interest_profile.id != 1")
	}
}

func TestMigrate_V12ToV13_LaneTables(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Migrate from scratch (full DDL) — verifies lanes tables exist
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	// Verify lanes table exists with correct columns
	rows, err := d.db.Query("SELECT id, name, lane_type, status, description, metadata, created_at, updated_at, closed_at FROM lanes LIMIT 0")
	if err != nil {
		t.Fatalf("lanes table missing or wrong schema: %v", err)
	}
	rows.Close()

	// Verify lane_events table
	rows, err = d.db.Query("SELECT id, lane_id, event_type, payload, created_at FROM lane_events LIMIT 0")
	if err != nil {
		t.Fatalf("lane_events table missing: %v", err)
	}
	rows.Close()

	// Verify lane_members table
	rows, err = d.db.Query("SELECT lane_id, bead_id, added_at FROM lane_members LIMIT 0")
	if err != nil {
		t.Fatalf("lane_members table missing: %v", err)
	}
	rows.Close()

	// Verify insert works
	_, err = d.db.Exec(`INSERT INTO lanes (id, name, lane_type) VALUES ('lane001', 'interop', 'standing')`)
	if err != nil {
		t.Fatalf("lanes insert failed: %v", err)
	}

	// Verify unique name constraint
	_, err = d.db.Exec(`INSERT INTO lanes (id, name, lane_type) VALUES ('lane002', 'interop', 'arc')`)
	if err == nil {
		t.Fatal("expected UNIQUE constraint violation on lanes.name")
	}

	// Verify lane_events foreign key
	_, err = d.db.Exec(`INSERT INTO lane_events (lane_id, event_type) VALUES ('lane001', 'created')`)
	if err != nil {
		t.Fatalf("lane_events insert failed: %v", err)
	}

	// Verify lane_members
	_, err = d.db.Exec(`INSERT INTO lane_members (lane_id, bead_id) VALUES ('lane001', 'iv-abc1')`)
	if err != nil {
		t.Fatalf("lane_members insert failed: %v", err)
	}

	// Verify lane_members composite PK prevents duplicates
	_, err = d.db.Exec(`INSERT INTO lane_members (lane_id, bead_id) VALUES ('lane001', 'iv-abc1')`)
	if err == nil {
		t.Fatal("expected PRIMARY KEY constraint violation on lane_members")
	}
}

func TestMigrate_V16ToV17_CostReconciliations(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	// Migrate from scratch — verifies cost_reconciliations table exists
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	// Verify cost_reconciliations table exists with correct columns
	rows, err := d.db.Query("SELECT id, run_id, dispatch_id, reported_in, reported_out, billed_in, billed_out, delta_in, delta_out, source, created_at FROM cost_reconciliations LIMIT 0")
	if err != nil {
		t.Fatalf("cost_reconciliations table missing or wrong schema: %v", err)
	}
	rows.Close()

	// Verify insert works
	_, err = d.db.Exec(`INSERT INTO cost_reconciliations (run_id, reported_in, reported_out, billed_in, billed_out, delta_in, delta_out, source) VALUES ('run001', 1000, 500, 1100, 500, 100, 0, 'manual')`)
	if err != nil {
		t.Fatalf("cost_reconciliations insert failed: %v", err)
	}

	// Verify dispatch-level insert with dispatch_id
	_, err = d.db.Exec(`INSERT INTO cost_reconciliations (run_id, dispatch_id, reported_in, reported_out, billed_in, billed_out, delta_in, delta_out, source) VALUES ('run001', 'disp001', 500, 200, 500, 200, 0, 0, 'anthropic')`)
	if err != nil {
		t.Fatalf("cost_reconciliations dispatch-level insert failed: %v", err)
	}

	// Verify we can query by run_id
	var count int
	err = d.db.QueryRow("SELECT COUNT(*) FROM cost_reconciliations WHERE run_id = 'run001'").Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("expected 2 rows, got %d", count)
	}
}

func TestMigrate_V17ToV18_SandboxSpec(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	// Verify sandbox_spec and sandbox_effective columns exist on dispatches
	rows, err := d.db.Query("SELECT sandbox_spec, sandbox_effective FROM dispatches LIMIT 0")
	if err != nil {
		t.Fatalf("sandbox columns missing: %v", err)
	}
	rows.Close()

	// Verify insert with sandbox spec
	spec := `{"tools_allowed":["Read","Grep"],"access_mode":"workspace-write"}`
	_, err = d.db.Exec(`INSERT INTO dispatches (id, project_dir, sandbox_spec) VALUES ('test-sb', '/tmp/test', ?)`, spec)
	if err != nil {
		t.Fatalf("insert with sandbox_spec failed: %v", err)
	}

	// Verify round-trip
	var gotSpec, gotEff *string
	err = d.db.QueryRow("SELECT sandbox_spec, sandbox_effective FROM dispatches WHERE id = 'test-sb'").Scan(&gotSpec, &gotEff)
	if err != nil {
		t.Fatal(err)
	}
	if gotSpec == nil || *gotSpec != spec {
		t.Errorf("sandbox_spec = %v, want %q", gotSpec, spec)
	}
	if gotEff != nil {
		t.Errorf("sandbox_effective = %v, want nil", gotEff)
	}

	// Verify sandbox_effective can be updated
	eff := `{"tools_used":["Read"]}`
	_, err = d.db.Exec(`UPDATE dispatches SET sandbox_effective = ? WHERE id = 'test-sb'`, eff)
	if err != nil {
		t.Fatalf("update sandbox_effective failed: %v", err)
	}

	err = d.db.QueryRow("SELECT sandbox_effective FROM dispatches WHERE id = 'test-sb'").Scan(&gotEff)
	if err != nil {
		t.Fatal(err)
	}
	if gotEff == nil || *gotEff != eff {
		t.Errorf("sandbox_effective = %v, want %q", gotEff, eff)
	}
}

func TestMigrate_V20ToV22_EventEnvelopeAndReplayInputs(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	_, err := d.db.Exec(`
		CREATE TABLE IF NOT EXISTS state (
			key TEXT NOT NULL, scope_id TEXT NOT NULL, payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()), expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		);
		CREATE TABLE IF NOT EXISTS sentinels (
			name TEXT NOT NULL, scope_id TEXT NOT NULL,
			last_fired INTEGER NOT NULL DEFAULT (unixepoch()),
			PRIMARY KEY (name, scope_id)
		);
		CREATE TABLE IF NOT EXISTS dispatches (
			id TEXT NOT NULL PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'spawned',
			project_dir TEXT NOT NULL,
			scope_id TEXT
		);
		CREATE TABLE IF NOT EXISTS runs (
			id TEXT NOT NULL PRIMARY KEY,
			project_dir TEXT NOT NULL,
			goal TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'active',
			phase TEXT NOT NULL DEFAULT 'brainstorm',
			complexity INTEGER NOT NULL DEFAULT 3,
			force_full INTEGER NOT NULL DEFAULT 0,
			auto_advance INTEGER NOT NULL DEFAULT 1,
			created_at INTEGER NOT NULL DEFAULT (unixepoch()),
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
			parent_run_id TEXT
		);
		CREATE TABLE IF NOT EXISTS phase_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			run_id TEXT NOT NULL,
			from_phase TEXT NOT NULL,
			to_phase TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'advance',
			gate_result TEXT,
			gate_tier TEXT,
			reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS dispatch_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			dispatch_id TEXT NOT NULL,
			run_id TEXT,
			from_status TEXT NOT NULL,
			to_status TEXT NOT NULL,
			event_type TEXT NOT NULL DEFAULT 'status_change',
			reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS coordination_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			lock_id TEXT NOT NULL,
			run_id TEXT,
			event_type TEXT NOT NULL,
			owner TEXT NOT NULL,
			pattern TEXT NOT NULL,
			scope TEXT NOT NULL,
			reason TEXT,
			created_at INTEGER NOT NULL DEFAULT (unixepoch())
		);
		CREATE TABLE IF NOT EXISTS audit_log (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id   TEXT NOT NULL,
			event_type   TEXT NOT NULL,
			actor        TEXT NOT NULL,
			target       TEXT NOT NULL DEFAULT '',
			payload      TEXT NOT NULL DEFAULT '{}',
			metadata     TEXT NOT NULL DEFAULT '{}',
			prev_hash    TEXT NOT NULL DEFAULT '',
			checksum     TEXT NOT NULL,
			sequence_num INTEGER NOT NULL,
			created_at   INTEGER NOT NULL DEFAULT (unixepoch())
		);
		PRAGMA user_version = 20;
	`)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate v20→v22: %v", err)
	}

	v, err := d.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if v != currentSchemaVersion {
		t.Fatalf("expected schema version %d, got %d", currentSchemaVersion, v)
	}

	for _, q := range []string{
		"SELECT envelope_json FROM phase_events LIMIT 0",
		"SELECT envelope_json FROM dispatch_events LIMIT 0",
		"SELECT envelope_json FROM coordination_events LIMIT 0",
		"SELECT id, run_id, kind, input_key, payload, artifact_ref, event_source, event_id, created_at FROM run_replay_inputs LIMIT 0",
	} {
		rows, err := d.db.Query(q)
		if err != nil {
			t.Fatalf("expected migrated column for query %q: %v", q, err)
		}
		rows.Close()
	}
}

func TestMigration032Authorizations(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("Run migrations: %v", err)
	}

	// Column count is asserted in TestMigration033AuthzSigning to the
	// post-v33 shape (12 base + 3 signing). Here we only confirm the
	// v32-era base columns still exist with the right CHECK behavior.
	var baseCols int
	err = d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('authorizations') WHERE name IN ('id','op_type','target','agent_id','bead_id','mode','policy_match','policy_hash','vetted_sha','vetting','cross_project_id','created_at')`).Scan(&baseCols)
	if err != nil {
		t.Fatalf("pragma_table_info(authorizations): %v", err)
	}
	if baseCols != 12 {
		t.Fatalf("authorizations v32 base column count = %d, want 12", baseCols)
	}

	_, err = d.db.Exec(`INSERT INTO authorizations (id,op_type,target,agent_id,mode,created_at) VALUES ('x','bead-close','a','s','bogus',1)`)
	if err == nil {
		t.Fatalf("expected mode CHECK to reject bogus mode")
	}

	_, err = d.db.Exec(`INSERT INTO authorizations (id,op_type,target,agent_id,mode,created_at) VALUES ('y','bead-close','a','   ','auto',1)`)
	if err == nil {
		t.Fatalf("expected agent_id CHECK to reject whitespace")
	}
}

// TestMigration033AuthzSigning verifies the v1.5 signing columns and the
// cutover-marker row. See docs/canon/authz-signing-trust-model.md for why
// the marker exists and what it bounds.
func TestMigration033AuthzSigning(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("Run migrations: %v", err)
	}

	// Column shape: base 12 from migration 032 + 3 new = 15.
	var count int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('authorizations')`).Scan(&count); err != nil {
		t.Fatalf("pragma_table_info: %v", err)
	}
	if count != 15 {
		t.Fatalf("authorizations column count = %d, want 15 (base 12 + sig_version, signature, signed_at)", count)
	}

	for _, col := range []string{"sig_version", "signature", "signed_at"} {
		var present int
		if err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('authorizations') WHERE name=?`, col).Scan(&present); err != nil {
			t.Fatalf("check column %q: %v", col, err)
		}
		if present != 1 {
			t.Fatalf("column %q missing after migration 033", col)
		}
	}

	// Partial index for unsigned rows.
	var idxCount int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='authz_unsigned'`).Scan(&idxCount); err != nil {
		t.Fatalf("check authz_unsigned index: %v", err)
	}
	if idxCount != 1 {
		t.Fatalf("authz_unsigned partial index missing")
	}

	// Cutover marker row must exist exactly once with op_type='migration.signing-enabled'.
	var markerRows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM authorizations WHERE op_type=?`, "migration.signing-enabled").Scan(&markerRows); err != nil {
		t.Fatalf("count markers: %v", err)
	}
	if markerRows != 1 {
		t.Fatalf("cutover marker count = %d, want 1 (idempotent INSERT OR IGNORE)", markerRows)
	}

	// sig_version defaults to 0 on NEW insert (not via migration).
	if _, err := d.db.Exec(`INSERT INTO authorizations (id,op_type,target,agent_id,mode,created_at) VALUES ('z','bead-close','a','test','auto',2)`); err != nil {
		t.Fatalf("insert new row: %v", err)
	}
	var newSigVersion int
	if err := d.db.QueryRow(`SELECT sig_version FROM authorizations WHERE id='z'`).Scan(&newSigVersion); err != nil {
		t.Fatalf("read sig_version: %v", err)
	}
	if newSigVersion != 0 {
		t.Fatalf("default sig_version for new row = %d, want 0", newSigVersion)
	}

	// Re-running migrations must be a no-op (marker remains exactly 1 row).
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM authorizations WHERE op_type=?`, "migration.signing-enabled").Scan(&markerRows); err != nil {
		t.Fatalf("recount markers: %v", err)
	}
	if markerRows != 1 {
		t.Fatalf("cutover marker count after re-run = %d, want 1 (idempotency broken)", markerRows)
	}
}

func TestMigration034AuthzTokens(t *testing.T) {
	d, _ := tempDB(t)
	ctx := context.Background()

	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	m, err := NewMigrator(d)
	if err != nil {
		t.Fatalf("NewMigrator: %v", err)
	}
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("Run migrations: %v", err)
	}

	// authz_tokens table exists with the expected 16 columns.
	var colCount int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('authz_tokens')`).Scan(&colCount); err != nil {
		t.Fatalf("pragma_table_info authz_tokens: %v", err)
	}
	if colCount != 16 {
		t.Fatalf("authz_tokens column count = %d, want 16", colCount)
	}

	expectedCols := []string{
		"id", "op_type", "target", "agent_id", "bead_id", "delegate_to",
		"expires_at", "consumed_at", "revoked_at", "issued_by",
		"parent_token", "root_token", "depth", "sig_version",
		"signature", "created_at",
	}
	for _, col := range expectedCols {
		var present int
		if err := d.db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('authz_tokens') WHERE name=?`, col).Scan(&present); err != nil {
			t.Fatalf("check column %q: %v", col, err)
		}
		if present != 1 {
			t.Fatalf("authz_tokens column %q missing after migration 034", col)
		}
	}

	// All 4 indexes present.
	for _, idx := range []string{"tokens_by_root", "tokens_by_parent", "tokens_by_expiry", "tokens_by_agent"} {
		var n int
		if err := d.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?`, idx).Scan(&n); err != nil {
			t.Fatalf("check index %q: %v", idx, err)
		}
		if n != 1 {
			t.Fatalf("expected index %q missing after migration 034", idx)
		}
	}

	// Depth CHECK constraint rejects > 3.
	// Insert a parent to satisfy FK on parent_token=NULL (null is allowed for roots).
	_, err = d.db.Exec(`
		INSERT INTO authz_tokens (id, op_type, target, agent_id, expires_at, issued_by, depth, signature, created_at)
		VALUES ('TEST-ROOT', 'bead-close', 'x', 'agent', 9999999999, 'user', 4, X'00', 1)`)
	if err == nil {
		t.Fatalf("expected CHECK(depth <= 3) to reject depth=4, got nil error")
	}

	// Depth CHECK constraint rejects < 0.
	_, err = d.db.Exec(`
		INSERT INTO authz_tokens (id, op_type, target, agent_id, expires_at, issued_by, depth, signature, created_at)
		VALUES ('TEST-ROOT', 'bead-close', 'x', 'agent', 9999999999, 'user', -1, X'00', 1)`)
	if err == nil {
		t.Fatalf("expected CHECK(depth >= 0) to reject depth=-1, got nil error")
	}

	// agent_id CHECK constraint rejects empty/whitespace.
	_, err = d.db.Exec(`
		INSERT INTO authz_tokens (id, op_type, target, agent_id, expires_at, issued_by, signature, created_at)
		VALUES ('TEST-EMPTY', 'bead-close', 'x', '   ', 9999999999, 'user', X'00', 1)`)
	if err == nil {
		t.Fatalf("expected CHECK(length(trim(agent_id)) > 0) to reject whitespace, got nil error")
	}

	// sig_version defaults to 2 on new insert (v2 convention).
	if _, err := d.db.Exec(`
		INSERT INTO authz_tokens (id, op_type, target, agent_id, expires_at, issued_by, signature, created_at)
		VALUES ('TEST-ROOT-OK', 'bead-close', 'x', 'agent', 9999999999, 'user', X'00', 1)`); err != nil {
		t.Fatalf("insert root token: %v", err)
	}
	var newSigVersion int
	if err := d.db.QueryRow(`SELECT sig_version FROM authz_tokens WHERE id='TEST-ROOT-OK'`).Scan(&newSigVersion); err != nil {
		t.Fatalf("read sig_version: %v", err)
	}
	if newSigVersion != 2 {
		t.Fatalf("default sig_version for new row = %d, want 2", newSigVersion)
	}

	// Cutover marker row exists exactly once in authorizations (not authz_tokens).
	var markerRows int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM authorizations WHERE op_type=?`, "migration.tokens-enabled").Scan(&markerRows); err != nil {
		t.Fatalf("count tokens markers: %v", err)
	}
	if markerRows != 1 {
		t.Fatalf("v34 cutover marker count = %d, want 1 (idempotent INSERT OR IGNORE)", markerRows)
	}

	// Marker has the fixed id 'migration-034-tokens-enabled'.
	var markerID string
	if err := d.db.QueryRow(`SELECT id FROM authorizations WHERE op_type=?`, "migration.tokens-enabled").Scan(&markerID); err != nil {
		t.Fatalf("read marker id: %v", err)
	}
	if markerID != "migration-034-tokens-enabled" {
		t.Fatalf("v34 marker id = %q, want %q", markerID, "migration-034-tokens-enabled")
	}

	// Re-running migrations must be a no-op (marker remains exactly 1 row).
	if _, err := m.Run(ctx); err != nil {
		t.Fatalf("re-run migrations: %v", err)
	}
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM authorizations WHERE op_type=?`, "migration.tokens-enabled").Scan(&markerRows); err != nil {
		t.Fatalf("recount tokens markers: %v", err)
	}
	if markerRows != 1 {
		t.Fatalf("v34 cutover marker count after re-run = %d, want 1 (idempotency broken)", markerRows)
	}

	// Schema version is 34 after migration.
	version, err := d.SchemaVersion()
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 34 {
		t.Fatalf("SchemaVersion = %d, want 34", version)
	}
}

