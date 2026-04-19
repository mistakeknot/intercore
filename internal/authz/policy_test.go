package authz

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestLoadEffective(t *testing.T) {
	dir := t.TempDir()
	globalPath := filepath.Join(dir, "global.yaml")
	projectPath := filepath.Join(dir, "project.yaml")
	envPath := filepath.Join(dir, "env.yaml")

	if err := os.WriteFile(globalPath, []byte(`
version: 1
rules:
  - op: bead-close
    mode: auto
    requires:
      vetted_within_minutes: 60
      tests_passed: true
  - op: "*"
    mode: confirm
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectPath, []byte(`
version: 1
rules:
  - op: bead-close
    mode: auto
    requires:
      vetted_within_minutes: 30
      tests_passed: true
`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(envPath, []byte(`
version: 1
rules:
  - op: git-push-main
    mode: auto
`), 0644); err != nil {
		t.Fatal(err)
	}

	policy, hash1, err := LoadEffective(globalPath, projectPath, envPath)
	if err != nil {
		t.Fatalf("LoadEffective: %v", err)
	}
	if policy == nil {
		t.Fatal("LoadEffective returned nil policy")
	}
	if hash1 == "" {
		t.Fatal("LoadEffective returned empty hash")
	}
	policy2, hash2, err := LoadEffective(globalPath, projectPath, envPath)
	if err != nil {
		t.Fatalf("LoadEffective(2): %v", err)
	}
	if policy2 == nil {
		t.Fatal("LoadEffective(2) returned nil policy")
	}
	if hash1 != hash2 {
		t.Fatalf("hash mismatch for same policy: %s != %s", hash1, hash2)
	}
}

func TestRecord_InsertsRowWithPolicyHash(t *testing.T) {
	db := openAuthzTestDB(t)
	if err := createAuthorizationsTable(t, db); err != nil {
		t.Fatalf("createAuthorizationsTable: %v", err)
	}

	err := Record(db, RecordArgs{
		ID:          "authz-1",
		OpType:      "bead-close",
		Target:      "sylveste-qdqr",
		AgentID:     "codex",
		Mode:        "auto",
		PolicyMatch: "bead-close#0",
		PolicyHash:  "abc123",
		VettedSHA:   "deadbeef",
		Vetting: map[string]interface{}{
			"tests_passed": true,
		},
		CreatedAt: 123,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var gotHash string
	if err := db.QueryRow("SELECT policy_hash FROM authorizations WHERE id='authz-1'").Scan(&gotHash); err != nil {
		t.Fatalf("query policy_hash: %v", err)
	}
	if gotHash != "abc123" {
		t.Fatalf("policy_hash = %q, want %q", gotHash, "abc123")
	}
}

func TestRecord_CrossProjectIDPropagates(t *testing.T) {
	db := openAuthzTestDB(t)
	if err := createAuthorizationsTable(t, db); err != nil {
		t.Fatalf("createAuthorizationsTable: %v", err)
	}

	err := Record(db, RecordArgs{
		ID:             "authz-2",
		OpType:         "ic-publish-patch",
		Target:         "/tmp/plugin",
		AgentID:        "codex",
		Mode:           "confirmed",
		CrossProjectID: "xproj-01",
		CreatedAt:      456,
	})
	if err != nil {
		t.Fatalf("Record: %v", err)
	}

	var got string
	if err := db.QueryRow("SELECT cross_project_id FROM authorizations WHERE id='authz-2'").Scan(&got); err != nil {
		t.Fatalf("query cross_project_id: %v", err)
	}
	if got != "xproj-01" {
		t.Fatalf("cross_project_id = %q, want %q", got, "xproj-01")
	}
}

func openAuthzTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	db.SetMaxOpenConns(1)
	return db
}

func createAuthorizationsTable(t *testing.T, db *sql.DB) error {
	t.Helper()
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS authorizations (
		  id               TEXT PRIMARY KEY,
		  op_type          TEXT NOT NULL,
		  target           TEXT NOT NULL,
		  agent_id         TEXT NOT NULL CHECK(length(trim(agent_id)) > 0),
		  bead_id          TEXT,
		  mode             TEXT NOT NULL CHECK(mode IN ('auto','confirmed','blocked','force_auto')),
		  policy_match     TEXT,
		  policy_hash      TEXT,
		  vetted_sha       TEXT,
		  vetting          TEXT CHECK(vetting IS NULL OR json_valid(vetting)),
		  cross_project_id TEXT,
		  created_at       INTEGER NOT NULL
		)
	`)
	return err
}
