package event

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestHookHandler_ExecutesPhaseHook(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	outputFile := filepath.Join(dir, "hook-output.json")
	hookScript := "#!/bin/sh\ncat > " + outputFile + "\n"
	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte(hookScript), 0755)

	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewHookHandler(dir, logger)

	e := Event{
		Source:    SourcePhase,
		Type:      "advance",
		RunID:     "run001",
		FromState: "brainstorm",
		ToState:   "strategized",
		Timestamp: time.Now(),
	}

	err := h(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}

	// Hook runs in a goroutine — wait for completion
	time.Sleep(200 * time.Millisecond)

	data, err := os.ReadFile(outputFile)
	if err != nil {
		t.Fatalf("hook output not written: %v", err)
	}
	if len(data) == 0 {
		t.Error("hook output is empty")
	}
}

func TestHookHandler_SkipsIfNoHook(t *testing.T) {
	dir := t.TempDir()
	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewHookHandler(dir, logger)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHookHandler_SkipsNonExecutable(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte("#!/bin/sh\necho test"), 0644)

	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewHookHandler(dir, logger)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHookHandler_FailingHookIsFireAndForget(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	hookPath := filepath.Join(hookDir, "on-phase-advance")
	os.WriteFile(hookPath, []byte("#!/bin/sh\nexit 1"), 0755)

	var buf syncBuffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	h := NewHookHandler(dir, logger)

	err := h(context.Background(), Event{Source: SourcePhase, Type: "advance"})
	if err != nil {
		t.Fatalf("hook failure should not return error, got: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	logOut := buf.String()
	if !strings.Contains(logOut, "hook failed") {
		t.Errorf("expected failure log containing 'hook failed', got: %s", logOut)
	}
}

func TestHookHandler_DispatchEvent(t *testing.T) {
	dir := t.TempDir()
	hookDir := filepath.Join(dir, ".clavain", "hooks")
	os.MkdirAll(hookDir, 0755)

	outputFile := filepath.Join(dir, "dispatch-output.json")
	hookScript := "#!/bin/sh\ncat > " + outputFile + "\n"
	hookPath := filepath.Join(hookDir, "on-dispatch-change")
	os.WriteFile(hookPath, []byte(hookScript), 0755)

	h := NewHookHandler(dir, nil)
	e := Event{Source: SourceDispatch, Type: "status_change", RunID: "run001"}
	h(context.Background(), e)

	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(outputFile); err != nil {
		t.Fatalf("dispatch hook not executed: %v", err)
	}
}
