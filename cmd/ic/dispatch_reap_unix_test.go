//go:build !windows

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type dispatchRow struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func activeDispatchIDs(t *testing.T, ctx context.Context) map[string]bool {
	t.Helper()
	flagJSON = true
	out := captureDispatchOutput(t, func() int { return cmdDispatch(ctx, []string{"list", "--active"}) })
	var rows []dispatchRow
	if err := json.Unmarshal(out, &rows); err != nil {
		t.Fatalf("list output %q: %v", out, err)
	}
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.ID] = true
	}
	return ids
}

func dispatchStatusOf(t *testing.T, ctx context.Context, id string) string {
	t.Helper()
	flagJSON = true
	out := captureDispatchOutput(t, func() int { return cmdDispatch(ctx, []string{"status", id}) })
	var row dispatchRow
	if err := json.Unmarshal(out, &row); err != nil {
		t.Fatalf("status output %q: %v", out, err)
	}
	return row.Status
}

// spawnDeadAgent spawns a fake agent through the CLI, then kills it, so the row
// stays `running` with a dead PID, as it does when an agent crashes.
func spawnDeadAgent(t *testing.T, ctx context.Context) string {
	t.Helper()
	dir := t.TempDir()
	prompt, wrapper := filepath.Join(dir, "prompt.md"), filepath.Join(dir, "dispatch.sh")
	for path, content := range map[string]string{prompt: "fixture", wrapper: "#!/bin/sh\nexec sleep 300\n"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	flagJSON = true
	out := captureDispatchOutput(t, func() int {
		return cmdDispatchSpawn(ctx, []string{"--prompt-file=" + prompt, "--dispatch-sh=" + wrapper, "--project=" + dir})
	})
	var spawned struct {
		ID  string `json:"id"`
		PID int    `json:"pid"`
	}
	if err := json.Unmarshal(out, &spawned); err != nil {
		t.Fatalf("spawn output %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-spawned.PID, syscall.SIGKILL); reapChild(spawned.PID) })
	if err := syscall.Kill(-spawned.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	waitGone(t, "fake agent", spawned.PID, true, 10*time.Second)
	return spawned.ID
}

// A crashed agent must not stay `running` until someone happens to poll it.
func TestDispatchStatusAndListReapDeadAgents(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()

	viaList, viaStatus := spawnDeadAgent(t, ctx), spawnDeadAgent(t, ctx)

	if got := dispatchStatusOf(t, ctx, viaStatus); got == "running" || got == "spawned" {
		t.Errorf("status of a dead agent = %q, want a terminal status", got)
	}
	if ids := activeDispatchIDs(t, ctx); ids[viaList] || ids[viaStatus] {
		t.Errorf("list --active still reports dead agents: %v", ids)
	}
	if got := dispatchStatusOf(t, ctx, viaList); got != "failed" {
		t.Errorf("dead agent without a verdict = %q, want failed", got)
	}
}

func TestDispatchReconcileSweepReapsDeadAgents(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	id := spawnDeadAgent(t, ctx)

	flagJSON = true
	out := captureDispatchOutput(t, func() int { return cmdDispatch(ctx, []string{"reconcile"}) })
	var got struct {
		Collected []string `json:"collected"`
		Released  []string `json:"released"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("reconcile output %q: %v", out, err)
	}
	if len(got.Collected) != 1 || got.Collected[0] != id {
		t.Errorf("reconcile collected %v, want [%s]", got.Collected, id)
	}
	if s := dispatchStatusOf(t, ctx, id); s != "failed" {
		t.Errorf("status after reconcile = %q, want failed", s)
	}
}

// `retry` reserves a slot that nothing can start. The reservation must not stay
// `spawned` in list --active, holding a cap slot, forever.
func TestDispatchRetryReservationIsReleasedAfterLease(t *testing.T) {
	setupCommandMetadataDB(t)
	ctx := context.Background()
	flagJSON = false
	runID := strings.TrimSpace(string(captureDispatchOutput(t, func() int {
		return cmdRunCreate(ctx, []string{"--goal=retry reservation", "--project=."})
	})))

	d, err := openDB()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	db := d.SqlDB()
	if _, err := db.Exec(`INSERT INTO dispatches(id, project_dir, scope_id, status) VALUES ('orig', '.', ?, 'failed')`, runID); err != nil {
		t.Fatal(err)
	}

	flagJSON = true
	out := captureDispatchOutput(t, func() int { return cmdDispatch(ctx, []string{"retry", "orig"}) })
	var retried struct {
		NewID string `json:"new_id"`
	}
	if err := json.Unmarshal(out, &retried); err != nil || retried.NewID == "" {
		t.Fatalf("retry output %q: %v", out, err)
	}

	// A fresh reservation is within its lease: the caller may still start it.
	if !activeDispatchIDs(t, ctx)[retried.NewID] {
		t.Fatalf("fresh retry reservation %s was reaped inside its lease", retried.NewID)
	}

	if _, err := db.Exec(`UPDATE dispatches SET created_at = created_at - 3600 WHERE id = ?`, retried.NewID); err != nil {
		t.Fatal(err)
	}
	if activeDispatchIDs(t, ctx)[retried.NewID] {
		t.Errorf("abandoned retry reservation %s is still active after its lease", retried.NewID)
	}
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM dispatches WHERE scope_id = ? AND status IN ('spawned','running')`, runID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Errorf("run still holds %d active dispatch slot(s)", active)
	}
	if s := dispatchStatusOf(t, ctx, retried.NewID); s != "cancelled" {
		t.Errorf("released reservation = %q, want cancelled", s)
	}
}
