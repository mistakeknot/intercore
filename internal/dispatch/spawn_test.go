package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestSpawn_ForwardsRequestedBackend(t *testing.T) {
	for _, backend := range []string{"", "codex", "claude", "claude-code", "kimi", "flere"} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			prompt := filepath.Join(dir, "prompt.md")
			argvFile := filepath.Join(dir, "argv")
			wrapper := filepath.Join(dir, "dispatch.sh")
			for path, content := range map[string]string{
				prompt:  "test prompt",
				wrapper: "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$IC_TEST_ARGV\"\n",
			} {
				if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("IC_TEST_ARGV", argvFile)
			store := testStore(t)
			runID := ""
			if backend == "flere" {
				admissionRun(t, store, false, nil, 0)
				runID = "run"
			}
			result, err := Spawn(context.Background(), store, SpawnOptions{
				AgentType: backend, ProjectDir: dir, PromptFile: prompt, DispatchSH: wrapper, RunID: runID,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := result.Cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			argv, err := os.ReadFile(argvFile)
			if err != nil {
				t.Fatal(err)
			}
			wantBackend := backend
			if wantBackend == "" {
				wantBackend = "codex"
			}
			args := strings.Split(strings.TrimSpace(string(argv)), "\n")
			found := false
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "--to" && args[i+1] == wantBackend {
					found = true
				}
			}
			if !found {
				t.Errorf("wrapper argv = %q, want --to %s", args, wantBackend)
			}
			d, err := store.Get(context.Background(), result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if d.AgentType != wantBackend {
				t.Errorf("recorded backend = %q, want %q", d.AgentType, wantBackend)
			}
		})
	}
}

func TestSpawn_WithoutWrapperRejectsOtherBackends(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CLAVAIN_DISPATCH_SH", "")
	t.Setenv("PATH", dir)
	marker := filepath.Join(dir, "codex-ran")
	t.Setenv("IC_TEST_ARGV", marker)
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$IC_TEST_ARGV\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("test prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, backend := range []string{"flere", "claude", "kimi", "auto"} {
		t.Run(backend, func(t *testing.T) {
			store := testStore(t)
			result, err := Spawn(context.Background(), store, SpawnOptions{
				AgentType: backend, ProjectDir: dir, PromptFile: prompt,
			})
			if result != nil {
				_ = result.Cmd.Wait()
			}
			if err == nil || !strings.Contains(err.Error(), backend) {
				t.Errorf("Spawn error = %v, want explicit unsupported backend %q", err, backend)
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Errorf("Codex ran for requested backend %q", backend)
			}
		})
	}
}

func TestSpawn_WithoutWrapperRunsCodex(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("CLAVAIN_DISPATCH_SH", "")
	t.Setenv("PATH", dir)
	argvFile := filepath.Join(dir, "argv")
	t.Setenv("IC_TEST_ARGV", argvFile)
	if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$IC_TEST_ARGV\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	prompt := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(prompt, []byte("test prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := Spawn(context.Background(), testStore(t), SpawnOptions{
		ProjectDir: dir, PromptFile: prompt, Model: "test-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSpace(string(argv)), "\n")
	want := []string{"exec", "--prompt-file", prompt, "-m", "test-model"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex argv = %q, want %q", got, want)
	}
}

func TestResolveDispatchSH_CurrentLayout(t *testing.T) {
	root := t.TempDir()
	wrapper := filepath.Join(root, "os", "Clavain", "scripts", "dispatch.sh")
	legacyWrapper := filepath.Join(root, "hub", "clavain", "scripts", "dispatch.sh")
	for _, path := range []string{wrapper, legacyWrapper} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(root, "core", "intercore")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(nested)
	t.Setenv("CLAVAIN_DISPATCH_SH", "")
	if got := resolveDispatchSH(""); got != wrapper {
		t.Fatalf("discovered wrapper = %q, want %q", got, wrapper)
	}
	if err := os.Remove(wrapper); err != nil {
		t.Fatal(err)
	}
	if got := resolveDispatchSH(""); got != legacyWrapper {
		t.Fatalf("legacy wrapper = %q, want %q", got, legacyWrapper)
	}
}
