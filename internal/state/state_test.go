package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.db")
	dsn := "file:" + path + "?_pragma=journal_mode%3DWAL&_pragma=busy_timeout%3D5000"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })

	_, err = db.Exec(`
		CREATE TABLE state (
			key TEXT NOT NULL,
			scope_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			updated_at INTEGER NOT NULL DEFAULT (unixepoch()),
			expires_at INTEGER,
			PRIMARY KEY (key, scope_id)
		)`)
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return db
}

func TestSetGetRoundtrip(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	payload := json.RawMessage(`{"phase":"brainstorm","bead":"iv-ieh7"}`)
	if err := store.Set(ctx, "dispatch", "sess1", payload, 0); err != nil {
		t.Fatal(err)
	}

	got, err := store.Get(ctx, "dispatch", "sess1")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("got %s, want %s", got, payload)
	}
}

func TestGet_NotFound(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	_, err := store.Get(ctx, "nonexistent", "s1")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestTTLEnforcement(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	payload := json.RawMessage(`{"temp":true}`)
	if err := store.Set(ctx, "ephemeral", "s1", payload, 1*time.Second); err != nil {
		t.Fatal(err)
	}

	// Should be visible immediately
	got, err := store.Get(ctx, "ephemeral", "s1")
	if err != nil {
		t.Fatal("expected to find entry:", err)
	}
	if string(got) != `{"temp":true}` {
		t.Errorf("unexpected payload: %s", got)
	}

	// Wait for TTL
	time.Sleep(2 * time.Second)

	// Should be invisible
	_, err = store.Get(ctx, "ephemeral", "s1")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound after TTL, got %v", err)
	}
}

func TestTTL_Truncation(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// Set with 1500ms TTL — should truncate to 1 second
	payload := json.RawMessage(`{"test":true}`)
	if err := store.Set(ctx, "trunc", "s1", payload, 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}

	// Check the expires_at value directly
	var expiresAt int64
	err := db.QueryRow("SELECT expires_at FROM state WHERE key='trunc' AND scope_id='s1'").Scan(&expiresAt)
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	expected := now + 1 // 1500ms truncated to 1 second
	if expiresAt != expected && expiresAt != expected+1 {
		t.Errorf("expires_at = %d, expected ~%d (now+1)", expiresAt, expected)
	}
}

func TestDelete(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	payload := json.RawMessage(`{"x":1}`)
	store.Set(ctx, "key", "s1", payload, 0)

	deleted, err := store.Delete(ctx, "key", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !deleted {
		t.Error("expected deleted=true")
	}

	// Second delete — not found
	deleted, err = store.Delete(ctx, "key", "s1")
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Error("expected deleted=false")
	}
}

func TestList(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	store.Set(ctx, "dispatch", "sess1", json.RawMessage(`{}`), 0)
	store.Set(ctx, "dispatch", "sess2", json.RawMessage(`{}`), 0)
	store.Set(ctx, "other", "sess3", json.RawMessage(`{}`), 0)

	ids, err := store.List(ctx, "dispatch")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("expected 2 ids, got %d", len(ids))
	}
}

func TestPrune(t *testing.T) {
	db := setupTestDB(t)
	store := New(db)
	ctx := context.Background()

	// Set with expired TTL
	store.Set(ctx, "expired", "s1", json.RawMessage(`{}`), 1*time.Second)
	store.Set(ctx, "fresh", "s1", json.RawMessage(`{}`), 0)

	time.Sleep(2 * time.Second)

	count, err := store.Prune(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected 1 pruned, got %d", count)
	}

	// Fresh entry should still exist
	_, err = store.Get(ctx, "fresh", "s1")
	if err != nil {
		t.Error("fresh entry should survive prune")
	}
}

func TestValidatePayload_InvalidJSON(t *testing.T) {
	err := ValidatePayload([]byte("not json"))
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestValidatePayload_TooLarge(t *testing.T) {
	large := make([]byte, maxPayloadSize+1)
	for i := range large {
		large[i] = 'a'
	}
	err := ValidatePayload(large)
	if err == nil {
		t.Error("expected error for oversized payload")
	}
}

func TestValidatePayload_TooDeep(t *testing.T) {
	// Create deeply nested JSON
	var b strings.Builder
	for i := 0; i <= maxNestingDepth+1; i++ {
		b.WriteString(`{"a":`)
	}
	b.WriteString(`1`)
	for i := 0; i <= maxNestingDepth+1; i++ {
		b.WriteString(`}`)
	}

	err := ValidatePayload([]byte(b.String()))
	if err == nil {
		t.Error("expected error for deeply nested JSON")
	}
}

func TestValidatePayload_LongArray(t *testing.T) {
	// Create an array with too many elements
	var b strings.Builder
	b.WriteString(`[`)
	for i := 0; i < maxArrayLength+1; i++ {
		if i > 0 {
			b.WriteString(`,`)
		}
		b.WriteString(`1`)
	}
	b.WriteString(`]`)

	err := ValidatePayload([]byte(b.String()))
	if err == nil {
		t.Error("expected error for long array")
	}
}

func TestValidateKey_Valid(t *testing.T) {
	valid := []string{
		"dispatch",
		"checkpoint",
		"sprint.checkpoint",
		"events.cursor",
		"discovery_brief",
		"interlock.reservation",
		"os.clavain.session",
		"my-plugin.config",
		"a",
	}
	for _, k := range valid {
		if err := ValidateKey(k); err != nil {
			t.Errorf("ValidateKey(%q) = %v, want nil", k, err)
		}
	}
}

func TestValidateKey_Invalid(t *testing.T) {
	cases := []struct {
		key  string
		desc string
	}{
		{"", "empty key"},
		{"Dispatch", "uppercase"},
		{"DISPATCH", "all uppercase"},
		{"dispatch key", "contains space"},
		{".leading-dot", "leading dot"},
		{"123-starts-with-number", "leading digit"},
		{"dispatch/traversal", "contains slash"},
		{"dispatch;injection", "contains semicolon"},
	}
	for _, tc := range cases {
		t.Run(tc.desc, func(t *testing.T) {
			if err := ValidateKey(tc.key); err == nil {
				t.Errorf("ValidateKey(%q) = nil, want error (%s)", tc.key, tc.desc)
			}
		})
	}
}

func TestValidatePayload_RejectsSecrets(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{"OpenAI key", `{"key":"sk-abc1234567890abcdefghijklmnop"}`},
		{"GitHub token", `{"token":"ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZabcdef1234"}`},
		{"Slack token", `{"token":"xoxb-123456789-abcdefghijklmnop"}`},
		{"JWT token", `{"auth":"eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkpvaG4gRG9lIiwiaWF0IjoxNTE2MjM5MDIyfQ.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"}`},
		{"AWS key", `{"aws":"AKIAIOSFODNN7EXAMPLE"}`},
		{"PEM key", `{"key":"-----BEGIN RSA PRIVATE KEY-----\nblah"}`},
		{"password assignment", `{"config":"password: hunter2superlong"}`},
		{"nested secret", `{"outer":{"inner":"sk-abc1234567890abcdefghijklmnop"}}`},
		{"array secret", `["sk-abc1234567890abcdefghijklmnop"]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePayload([]byte(tc.payload))
			if err == nil {
				t.Errorf("expected secret rejection for %s", tc.name)
			}
			if err != nil && !strings.Contains(err.Error(), "likely secret") {
				t.Errorf("expected 'likely secret' error, got: %v", err)
			}
		})
	}
}

func TestValidatePayload_AllowsNonSecrets(t *testing.T) {
	safe := []string{
		`{"phase":"brainstorm"}`,
		`{"api_key_name":"OPENAI_API_KEY"}`,
		`{"token_count":42}`,
		`{"password_reset":true}`,
		`{"description":"Use sk command to skip"}`,
		`{"short":"sk-ab"}`,
	}
	for _, v := range safe {
		if err := ValidatePayload([]byte(v)); err != nil {
			t.Errorf("ValidatePayload(%s) = %v, want nil", v, err)
		}
	}
}

func TestValidatePayload_Valid(t *testing.T) {
	valid := []string{
		`{}`,
		`{"key":"value"}`,
		`[1,2,3]`,
		`{"nested":{"a":{"b":1}}}`,
		`"string"`,
		`42`,
	}
	for _, v := range valid {
		if err := ValidatePayload([]byte(v)); err != nil {
			t.Errorf("ValidatePayload(%s) = %v, want nil", v, err)
		}
	}
}
