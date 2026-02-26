package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	hookPhaseAdvance   = "on-phase-advance"
	hookDispatchChange = "on-dispatch-change"
	hookTimeout        = 5 * time.Second
)

// NewHookHandler returns a handler that executes convention-based shell hooks.
// projectDir is the base directory where .clavain/hooks/ is searched.
// Hooks run in a detached goroutine to avoid blocking the single DB connection.
func NewHookHandler(projectDir string, logger *slog.Logger) Handler {
	return func(ctx context.Context, e Event) error {
		var hookName string
		switch e.Source {
		case SourcePhase:
			hookName = hookPhaseAdvance
		case SourceDispatch:
			hookName = hookDispatchChange
		default:
			return nil
		}

		hookPath := filepath.Join(projectDir, ".clavain", "hooks", hookName)
		info, err := os.Stat(hookPath)
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Mode()&0111 == 0 {
			return nil
		}

		eventJSON, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("hook: marshal event: %w", err)
		}

		go func() {
			hookCtx, cancel := context.WithTimeout(context.Background(), hookTimeout)
			defer cancel()

			cmd := exec.CommandContext(hookCtx, hookPath)
			cmd.Stdin = bytes.NewReader(eventJSON)
			cmd.Dir = projectDir

			var stderr bytes.Buffer
			cmd.Stderr = &stderr

			if err := cmd.Run(); err != nil {
				if logger != nil {
					logger.WarnContext(hookCtx, "hook failed",
						"hook", hookName,
						"error", err.Error(),
						"stderr", stderr.String(),
					)
				}
			}
		}()

		return nil
	}
}
