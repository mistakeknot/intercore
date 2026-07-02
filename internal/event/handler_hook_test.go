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

// eventually polls cond every 10ms until it returns true or timeout elapses.
// It replaces fixed sleeps used as async-completion barriers: hooks run in a
// detached goroutine, so the completion condition (output file written, log
// line emitted) may hold well before or after any fixed delay. Fails the test
// with msg if the condition is not satisfied within timeout.
func eventually(t *testing.T, cond func() bool, timeout time.Duration, msg string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not met within %s: %s", timeout, msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// pollTimeout is a generous upper bound for hook-goroutine completion in tests.
// The production hook timeout is 5s; this matches it so a slow subprocess under
// concurrent/-race load still completes before the poll gives up.
const pollTimeout = 5 * time.Second

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

	// Hook runs in a goroutine — poll for the output file to be written.
	eventually(t, func() bool {
		info, statErr := os.Stat(outputFile)
		return statErr == nil && info.Size() > 0
	}, pollTimeout, "hook output file was not written")

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

	// Hook runs in a goroutine — poll for the failure log line to be emitted.
	eventually(t, func() bool {
		return strings.Contains(buf.String(), "hook failed")
	}, pollTimeout, "failure log containing 'hook failed' was not emitted")

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

	// Hook runs in a goroutine — poll for the output file to be created.
	eventually(t, func() bool {
		_, statErr := os.Stat(outputFile)
		return statErr == nil
	}, pollTimeout, "dispatch hook output file was not created")

	if _, err := os.Stat(outputFile); err != nil {
		t.Fatalf("dispatch hook not executed: %v", err)
	}
}
