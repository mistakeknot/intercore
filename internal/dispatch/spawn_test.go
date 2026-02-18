package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestSpawn_MockProcess(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	// Create a prompt file
	promptFile := filepath.Join(t.TempDir(), "prompt.md")
	os.WriteFile(promptFile, []byte("test prompt"), 0644)

	// Use /bin/echo as the "dispatch.sh" — it will exit immediately
	result, err := Spawn(ctx, store, SpawnOptions{
		AgentType:  "codex",
		ProjectDir: t.TempDir(),
		PromptFile: promptFile,
		OutputFile: filepath.Join(t.TempDir(), "output.md"),
		Name:       "test",
		DispatchSH: "/bin/echo", // mock: exits immediately
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if result.ID == "" {
		t.Error("expected non-empty ID")
	}
	if result.PID == 0 {
		t.Error("expected non-zero PID")
	}

	// Wait for the mock process to finish
	result.Cmd.Wait()

	// Verify DB state
	d, err := store.Get(ctx, result.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if d.Status != StatusRunning {
		t.Errorf("Status = %q, want %q", d.Status, StatusRunning)
	}
	if d.PID == nil || *d.PID != result.PID {
		t.Errorf("PID = %v, want %d", d.PID, result.PID)
	}
	if d.PromptHash == nil || *d.PromptHash == "" {
		t.Error("expected prompt hash to be set")
	}
}

func TestSpawn_MissingPromptFile(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	_, err := Spawn(ctx, store, SpawnOptions{
		AgentType:  "codex",
		ProjectDir: t.TempDir(),
		PromptFile: "/nonexistent/prompt.md",
		DispatchSH: "/bin/echo",
	})
	if err == nil {
		t.Error("expected error for missing prompt file")
	}
}

func TestSpawn_MissingProjectDir(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()

	_, err := Spawn(ctx, store, SpawnOptions{
		PromptFile: "/tmp/whatever.md",
	})
	if err == nil {
		t.Error("expected error for missing project_dir")
	}
}

func TestHashFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.md")
	os.WriteFile(path, []byte("hello world"), 0644)

	h1, err := hashFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(h1) != 16 { // 8 bytes = 16 hex chars
		t.Errorf("hash length = %d, want 16", len(h1))
	}

	// Same content = same hash
	path2 := filepath.Join(t.TempDir(), "test2.md")
	os.WriteFile(path2, []byte("hello world"), 0644)
	h2, _ := hashFile(path2)
	if h1 != h2 {
		t.Errorf("same content should produce same hash: %q != %q", h1, h2)
	}

	// Different content = different hash
	path3 := filepath.Join(t.TempDir(), "test3.md")
	os.WriteFile(path3, []byte("goodbye world"), 0644)
	h3, _ := hashFile(path3)
	if h1 == h3 {
		t.Error("different content should produce different hash")
	}
}

func TestResolveDispatchSH(t *testing.T) {
	// Explicit path that exists → returns it (takes priority over walk-up)
	f := filepath.Join(t.TempDir(), "dispatch.sh")
	os.WriteFile(f, []byte("#!/bin/bash"), 0755)
	got := resolveDispatchSH(f)
	if got != f {
		t.Errorf("expected %q, got %q", f, got)
	}

	// Explicit path that doesn't exist → falls through to env/walk-up
	// (may find monorepo dispatch.sh depending on CWD, so just verify no panic)
	_ = resolveDispatchSH("/nonexistent/dispatch.sh")
}
