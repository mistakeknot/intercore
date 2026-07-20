package receipt

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

// tempStore opens a migrated SQLite DB, returns a wrapped Store, and ensures
// cleanup. Mirrors the tempDB helper in internal/db/db_test.go.
func tempStore(t *testing.T) *Store {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "receipts.db"), 100*time.Millisecond)
	if err != nil {
		t.Fatalf("db open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(d.SqlDB())
}

// freshSigned builds a Receipt, applies mutate, registers the (possibly
// mutated) AgentID in a fresh keystore, and runs Sign. Tests get a
// post-Sign envelope ready for Insert. The keystore is per-call so cases
// stay independent.
func freshSigned(t *testing.T, mutate func(*Receipt)) (*Receipt, []byte) {
	t.Helper()
	r := Receipt{
		ReceiptID:     NewID(time.Now()),
		Timestamp:     FormatTimestamp(time.Now()),
		AgentID:       testAgentID,
		Model:         "claude-opus-4-7-mythos",
		ContentHash:   strings.Repeat("ab", 32),
		SchemaVersion: 1,
		ToolCalls: []ToolCall{
			{Name: "Bash", ArgsHash: "01", ResultHash: "02", DurationMs: 7},
		},
	}
	if mutate != nil {
		mutate(&r)
	}
	// Register the receipt's agent on the fly so mutate may freely change
	// AgentID without the caller having to pre-build a multi-agent keystore.
	ks := NewMemKeyStore()
	ks.Register(r.AgentID, testEpoch, testKey)
	canon, err := Sign(&r, ks, time.Now())
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return &r, canon
}

func TestStore_InsertGet_RoundTrip(t *testing.T) {
	s := tempStore(t)
	r, canon := freshSigned(t, nil)
	if err := s.Insert(context.Background(), r, canon); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, gotPayload, err := s.Get(context.Background(), r.ReceiptID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	if got.ReceiptID != r.ReceiptID || got.Signature != r.Signature ||
		got.KeyID != r.KeyID || got.Model != r.Model {
		t.Fatalf("round-trip field mismatch\nwant: %+v\ngot:  %+v", r, got)
	}
	if len(got.ToolCalls) != len(r.ToolCalls) || got.ToolCalls[0].Name != "Bash" {
		t.Fatalf("tool_calls mismatch: %+v", got.ToolCalls)
	}
	if got.ParentRunID != nil {
		t.Fatalf("expected nil ParentRunID, got %v", *got.ParentRunID)
	}
	if string(gotPayload) != string(canon) {
		t.Fatalf("payload_canonical mismatch\nwant len=%d\ngot  len=%d", len(canon), len(gotPayload))
	}

	// Round-trip the re-loaded receipt through Verify to confirm the bytes
	// in the DB still validate. This is the substrate-level guarantee
	// downstream beads (ewy3.5.3 verify CLI, ewy3.5.4 routing-calibration)
	// build on.
	if err := Verify(got, goldenStore()); err != nil {
		t.Fatalf("verify after round-trip: %v", err)
	}
}

func TestStore_Insert_RequiresSigned(t *testing.T) {
	s := tempStore(t)
	r := Receipt{
		ReceiptID:     NewID(time.Now()),
		Timestamp:     FormatTimestamp(time.Now()),
		AgentID:       testAgentID,
		Model:         "m",
		ContentHash:   strings.Repeat("0", 64),
		SchemaVersion: 1,
		// Envelope fields deliberately unset.
	}
	if err := s.Insert(context.Background(), &r, nil); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("want ErrUnsigned, got %v", err)
	}
}

func TestStore_Get_NotFound(t *testing.T) {
	s := tempStore(t)
	_, _, err := s.Get(context.Background(), "rcpt_nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestStore_Insert_DuplicateRejected(t *testing.T) {
	s := tempStore(t)
	r, canon := freshSigned(t, nil)
	if err := s.Insert(context.Background(), r, canon); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	if err := s.Insert(context.Background(), r, canon); err == nil {
		t.Fatal("expected duplicate insert to fail (PK violation), got nil")
	}
}

func TestStore_InsertOnly_NoDelete(t *testing.T) {
	s := tempStore(t)
	r, canon := freshSigned(t, nil)
	if err := s.Insert(context.Background(), r, canon); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := s.db.ExecContext(context.Background(),
		"DELETE FROM action_receipts WHERE receipt_id = ?", r.ReceiptID)
	if err == nil {
		t.Fatal("DELETE on action_receipts succeeded; trigger broken")
	}
	if !strings.Contains(err.Error(), "INSERT-only") {
		t.Fatalf("DELETE error missing canon-reference: %v", err)
	}
}

func TestStore_InsertOnly_NoUpdate(t *testing.T) {
	s := tempStore(t)
	r, canon := freshSigned(t, nil)
	if err := s.Insert(context.Background(), r, canon); err != nil {
		t.Fatalf("insert: %v", err)
	}
	_, err := s.db.ExecContext(context.Background(),
		"UPDATE action_receipts SET content_hash = 'tampered' WHERE receipt_id = ?", r.ReceiptID)
	if err == nil {
		t.Fatal("UPDATE on action_receipts succeeded; trigger broken")
	}
	if !strings.Contains(err.Error(), "INSERT-only") {
		t.Fatalf("UPDATE error missing canon-reference: %v", err)
	}
}

func TestStore_List_FiltersAndOrder(t *testing.T) {
	s := tempStore(t)
	parent := "task-parent-1"
	base := time.Date(2026, 5, 23, 10, 0, 0, 0, time.UTC)

	// Insert 3 receipts: 2 for agentA across 30 minutes, 1 for agentB.
	r1, c1 := freshSigned(t, func(r *Receipt) {
		r.ReceiptID = "rcpt_" + strings.Repeat("a", 26)
		r.AgentID = "sylveste://agent/A"
		r.Timestamp = FormatTimestamp(base)
		r.ParentRunID = &parent
	})
	r2, c2 := freshSigned(t, func(r *Receipt) {
		r.ReceiptID = "rcpt_" + strings.Repeat("b", 26)
		r.AgentID = "sylveste://agent/A"
		r.Timestamp = FormatTimestamp(base.Add(30 * time.Minute))
	})
	r3, c3 := freshSigned(t, func(r *Receipt) {
		r.ReceiptID = "rcpt_" + strings.Repeat("c", 26)
		r.AgentID = "sylveste://agent/B"
		r.Timestamp = FormatTimestamp(base.Add(15 * time.Minute))
	})

	for _, ins := range []struct {
		r *Receipt
		c []byte
	}{{r1, c1}, {r2, c2}, {r3, c3}} {
		if err := s.Insert(context.Background(), ins.r, ins.c); err != nil {
			t.Fatalf("insert %s: %v", ins.r.ReceiptID, err)
		}
	}

	t.Run("by-agent", func(t *testing.T) {
		got, err := s.List(context.Background(), ListOpts{AgentID: "sylveste://agent/A"})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 receipts for agent A, got %d", len(got))
		}
		if got[0].ReceiptID != r1.ReceiptID || got[1].ReceiptID != r2.ReceiptID {
			t.Fatalf("agent-A order wrong: %s, %s", got[0].ReceiptID, got[1].ReceiptID)
		}
	})

	t.Run("by-since", func(t *testing.T) {
		got, err := s.List(context.Background(), ListOpts{Since: base.Add(10 * time.Minute)})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("want 2 receipts after +10m, got %d", len(got))
		}
		// Ordered ASC by timestamp: r3 (+15m) before r2 (+30m).
		if got[0].ReceiptID != r3.ReceiptID || got[1].ReceiptID != r2.ReceiptID {
			t.Fatalf("since-order wrong: %s, %s", got[0].ReceiptID, got[1].ReceiptID)
		}
	})

	t.Run("by-agent-and-since-with-limit", func(t *testing.T) {
		got, err := s.List(context.Background(), ListOpts{
			AgentID: "sylveste://agent/A",
			Since:   base.Add(20 * time.Minute),
			Limit:   1,
		})
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 || got[0].ReceiptID != r2.ReceiptID {
			t.Fatalf("filtered list wrong: %+v", got)
		}
	})

	t.Run("parent-roundtrip", func(t *testing.T) {
		got, _, err := s.Get(context.Background(), r1.ReceiptID)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.ParentRunID == nil || *got.ParentRunID != parent {
			t.Fatalf("parent_run_id round-trip lost: %v", got.ParentRunID)
		}
	})
}

func TestStore_FindActionUsesExactAgentParentAndContentHash(t *testing.T) {
	s := tempStore(t)
	parent := "run-remontoire-1"
	wanted, canon := freshSigned(t, func(r *Receipt) {
		r.AgentID = "remontoire"
		r.ParentRunID = &parent
		r.ContentHash = strings.Repeat("a", 64)
	})
	if err := s.Insert(context.Background(), wanted, canon); err != nil {
		t.Fatal(err)
	}
	other, otherCanon := freshSigned(t, func(r *Receipt) {
		r.ReceiptID = "rcpt_" + strings.Repeat("z", 26)
		r.AgentID = "remontoire"
		r.ParentRunID = &parent
		r.ContentHash = strings.Repeat("b", 64)
	})
	if err := s.Insert(context.Background(), other, otherCanon); err != nil {
		t.Fatal(err)
	}

	got, err := s.FindAction(context.Background(), "remontoire", parent, strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	if got.ReceiptID != wanted.ReceiptID {
		t.Fatalf("receipt = %s, want %s", got.ReceiptID, wanted.ReceiptID)
	}
	if _, err := s.FindAction(context.Background(), "remontoire", parent, strings.Repeat("c", 64)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing action error = %v", err)
	}
}

// TestBulkVerifyPerf covers acceptance criterion #5 of sylveste-ewy3.5.3:
// bulk verification of 1K receipts completes well under 100ms. It exercises
// the exact path `ic receipt verify --since` uses — Store.List followed by a
// Verify per row — so a regression in either shows up here.
func TestBulkVerifyPerf(t *testing.T) {
	if testing.Short() {
		t.Skip("perf test skipped in -short mode")
	}
	if raceEnabled {
		t.Skip("perf assertion invalid under -race (instrumentation slows execution ~10x)")
	}
	s := tempStore(t)
	ks := goldenStore()
	ctx := context.Background()

	const n = 1000
	base := time.Date(2026, 5, 23, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		r := Receipt{
			ReceiptID:     fmt.Sprintf("rcpt_%026d", i),
			Timestamp:     FormatTimestamp(base.Add(time.Duration(i) * time.Second)),
			AgentID:       testAgentID,
			Model:         "claude-opus-4-7-mythos",
			ContentHash:   strings.Repeat("ab", 32),
			SchemaVersion: 1,
			ToolCalls: []ToolCall{
				{Name: "Bash", ArgsHash: "01", ResultHash: "02", DurationMs: int64(i)},
			},
		}
		canon, err := Sign(&r, ks, time.Now())
		if err != nil {
			t.Fatalf("sign %d: %v", i, err)
		}
		if err := s.Insert(ctx, &r, canon); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	start := time.Now()
	receipts, err := s.List(ctx, ListOpts{Since: base.Add(-time.Hour)})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	verified := 0
	for _, r := range receipts {
		if err := Verify(r, ks); err != nil {
			t.Fatalf("verify %s: %v", r.ReceiptID, err)
		}
		verified++
	}
	elapsed := time.Since(start)

	if verified != n {
		t.Fatalf("verified %d receipts, want %d", verified, n)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("bulk verify of %d receipts took %s, want <100ms", n, elapsed)
	}
	t.Logf("bulk-verified %d receipts in %s (%.1fµs/receipt)", n, elapsed, float64(elapsed.Microseconds())/float64(n))
}

func TestStore_MigrationIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "receipts.db")
	d, err := db.Open(path, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	ctx := context.Background()
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}
	if err := d.Migrate(ctx); err != nil {
		t.Fatalf("migrate 2 (should be no-op): %v", err)
	}
	if v, _ := d.SchemaVersion(); v != 39 {
		t.Fatalf("schema version after double-migrate: %d, want 39", v)
	}
}
