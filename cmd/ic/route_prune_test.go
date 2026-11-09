package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseRetention(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30d": 30 * 24 * time.Hour, "1d": 24 * time.Hour, "720h": 720 * time.Hour, "90m": 90 * time.Minute,
	} {
		got, err := parseRetention(in)
		if err != nil || got != want {
			t.Errorf("parseRetention(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "-3d", "d", "abc", "0", "-1h", "1.5d"} {
		if _, err := parseRetention(in); err == nil {
			t.Errorf("parseRetention(%q) accepted", in)
		}
	}
}

func seedRouteRows(t *testing.T) {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	now := time.Now().Unix()
	for i, age := range []int64{90 * 86400, 45 * 86400, 2 * 86400} {
		if _, err := d.SqlDB().Exec(`INSERT INTO routing_decisions (project_dir, agent, selected_model, rule_matched, decided_at, context_json)
			VALUES ('/p', ?, 'sonnet', 'r', ?, '{"k":1}')`, "a"+string(rune('0'+i)), now-age); err != nil {
			t.Fatal(err)
		}
	}
}

func routeRowCount(t *testing.T) int {
	t.Helper()
	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.SqlDB().QueryRow(`SELECT COUNT(*) FROM routing_decisions`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRoutePruneRejectsBadUsage(t *testing.T) {
	setupCommandMetadataDB(t)
	seedRouteRows(t)
	for _, args := range [][]string{
		{"--bogus"},
		{"extra-positional"},
		{"--older-than=0d"},
		{"--older-than=soon"},
		{"--no-archive", "--archive-dir=/x"},
	} {
		code, _ := captureRouteStderr(t, func() int { return cmdRoutePrune(context.Background(), args) })
		if code != 3 {
			t.Errorf("%v: code %d, want 3", args, code)
		}
	}
	if n := routeRowCount(t); n != 3 {
		t.Fatalf("rows = %d after rejected invocations, want 3", n)
	}
}

func TestRoutePruneDryRunThenPruneThenRestore(t *testing.T) {
	setupCommandMetadataDB(t)
	seedRouteRows(t)
	ctx := context.Background()

	code, out := captureRouteStdout(t, func() int { return cmdRoutePrune(ctx, []string{"--older-than=30d", "--dry-run"}) })
	if code != 0 || !strings.Contains(string(out), "would prune 2") {
		t.Fatalf("dry run: code %d out %q", code, out)
	}
	if routeRowCount(t) != 3 {
		t.Fatal("dry run deleted rows")
	}
	if _, err := os.Stat("routing-archive"); !os.IsNotExist(err) {
		t.Fatalf("dry run created the archive dir (err=%v)", err)
	}

	flagJSON = true
	code, out = captureRouteStdout(t, func() int { return cmdRoutePrune(ctx, []string{"--older-than=30d"}) })
	flagJSON = false
	if code != 0 {
		t.Fatalf("prune: code %d", code)
	}
	var res struct {
		Deleted, Archived int64
		Window            string
		Files             []string
	}
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("json %q: %v", out, err)
	}
	if res.Deleted != 2 || res.Archived != 2 || res.Window != "720h0m0s" || len(res.Files) == 0 {
		t.Fatalf("prune result %+v", res)
	}
	// The default archive dir sits next to the database file.
	if dir := filepath.Dir(res.Files[0]); filepath.Base(dir) != "routing-archive" {
		t.Fatalf("archive written to %s, want a routing-archive directory", dir)
	}
	if routeRowCount(t) != 1 {
		t.Fatalf("rows = %d, want 1", routeRowCount(t))
	}

	code, out = captureRouteStdout(t, func() int { return cmdRouteRestore(ctx, res.Files) })
	if code != 0 || routeRowCount(t) != 3 {
		t.Fatalf("restore: code %d out %q rows %d", code, out, routeRowCount(t))
	}
}

func TestRoutePruneNoArchiveWritesNothing(t *testing.T) {
	setupCommandMetadataDB(t)
	seedRouteRows(t)
	code, out := captureRouteStdout(t, func() int {
		return cmdRoutePrune(context.Background(), []string{"--older-than=30d", "--no-archive"})
	})
	if code != 0 || !strings.Contains(string(out), "NOT archived") {
		t.Fatalf("code %d out %q", code, out)
	}
	if routeRowCount(t) != 1 {
		t.Fatalf("rows = %d, want 1", routeRowCount(t))
	}
	if _, err := os.Stat("routing-archive"); !os.IsNotExist(err) {
		t.Fatalf("--no-archive created an archive dir (err=%v)", err)
	}
}

func TestRoutePruneVacuumRunsOnlyWhenRowsWereDeleted(t *testing.T) {
	setupCommandMetadataDB(t)
	seedRouteRows(t)
	ctx := context.Background()
	_, out := captureRouteStdout(t, func() int { return cmdRoutePrune(ctx, []string{"--older-than=30d", "--vacuum"}) })
	if !strings.Contains(string(out), "vacuumed") {
		t.Fatalf("out %q, want vacuumed", out)
	}
	_, out = captureRouteStdout(t, func() int { return cmdRoutePrune(ctx, []string{"--older-than=30d", "--vacuum"}) })
	if strings.Contains(string(out), "vacuumed") {
		t.Fatalf("out %q: vacuum ran with nothing deleted", out)
	}
}
