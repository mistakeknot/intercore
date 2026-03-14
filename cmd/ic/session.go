package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/intercore/internal/session"
)

func cmdSession(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: session: usage: ic session <start|attribute|end|current|list|tokens>\n")
		return 3
	}

	switch args[0] {
	case "start":
		return cmdSessionStart(ctx, args[1:])
	case "attribute":
		return cmdSessionAttribute(ctx, args[1:])
	case "end":
		return cmdSessionEnd(ctx, args[1:])
	case "current":
		return cmdSessionCurrent(ctx, args[1:])
	case "list":
		return cmdSessionList(ctx, args[1:])
	case "tokens":
		return cmdSessionTokens(ctx, args[1:])
	default:
		slog.Error("session: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdSessionStart(ctx context.Context, args []string) int {
	var opts session.StartOpts

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--agent-type="):
			opts.AgentType = strings.TrimPrefix(arg, "--agent-type=")
		case strings.HasPrefix(arg, "--model="):
			opts.Model = strings.TrimPrefix(arg, "--model=")
		case strings.HasPrefix(arg, "--metadata="):
			opts.Metadata = strings.TrimPrefix(arg, "--metadata=")
		default:
			slog.Error("session start: unknown flag", "value", arg)
			return 3
		}
	}

	if opts.SessionID == "" || opts.ProjectDir == "" {
		slog.Error("session start: --session and --project are required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session start failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	id, err := store.Start(ctx, opts)
	if err != nil {
		slog.Error("session start failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":         id,
			"session_id": opts.SessionID,
			"project":    opts.ProjectDir,
		})
	} else {
		fmt.Printf("Session started: %s (id=%d)\n", opts.SessionID, id)
	}
	return 0
}

func cmdSessionAttribute(ctx context.Context, args []string) int {
	var opts session.AttributeOpts

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--bead="):
			opts.BeadID = strings.TrimPrefix(arg, "--bead=")
		case strings.HasPrefix(arg, "--run="):
			opts.RunID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--phase="):
			opts.Phase = strings.TrimPrefix(arg, "--phase=")
		default:
			slog.Error("session attribute: unknown flag", "value", arg)
			return 3
		}
	}

	if opts.SessionID == "" {
		slog.Error("session attribute: --session is required")
		return 3
	}

	// Default project to CWD if not provided
	if opts.ProjectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("session attribute: cannot determine CWD", "error", err)
			return 2
		}
		opts.ProjectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session attribute failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	id, err := store.Attribute(ctx, opts)
	if err != nil {
		slog.Error("session attribute failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"id":         id,
			"session_id": opts.SessionID,
			"bead_id":    opts.BeadID,
			"phase":      opts.Phase,
		})
	} else {
		fmt.Printf("Attribution recorded (id=%d)\n", id)
	}
	return 0
}

func cmdSessionEnd(ctx context.Context, args []string) int {
	var sessionID string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--session="):
			sessionID = strings.TrimPrefix(arg, "--session=")
		default:
			slog.Error("session end: unknown flag", "value", arg)
			return 3
		}
	}

	if sessionID == "" {
		slog.Error("session end: --session is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session end failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	if err := store.End(ctx, sessionID); err != nil {
		slog.Error("session end failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"session_id": sessionID,
			"ended":      true,
		})
	} else {
		fmt.Printf("Session ended: %s\n", sessionID)
	}
	return 0
}

func cmdSessionCurrent(ctx context.Context, args []string) int {
	var sessionID, projectDir string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--session="):
			sessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--project="):
			projectDir = strings.TrimPrefix(arg, "--project=")
		default:
			slog.Error("session current: unknown flag", "value", arg)
			return 3
		}
	}

	if sessionID == "" {
		slog.Error("session current: --session is required")
		return 3
	}

	// Default project to CWD if not provided
	if projectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("session current: cannot determine CWD", "error", err)
			return 2
		}
		projectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session current failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	cur, err := store.Current(ctx, sessionID, projectDir)
	if err != nil {
		slog.Error("session current failed", "error", err)
		return 2
	}

	if cur == nil {
		if flagJSON {
			fmt.Println("null")
		} else {
			fmt.Println("No attribution found.")
		}
		return 1
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(cur)
	} else {
		fmt.Printf("Session:  %s\n", cur.SessionID)
		fmt.Printf("Project:  %s\n", cur.ProjectDir)
		if cur.BeadID != nil {
			fmt.Printf("Bead:     %s\n", *cur.BeadID)
		}
		if cur.RunID != nil {
			fmt.Printf("Run:      %s\n", *cur.RunID)
		}
		if cur.Phase != nil {
			fmt.Printf("Phase:    %s\n", *cur.Phase)
		}
		fmt.Printf("Updated:  %s\n", time.Unix(cur.UpdatedAt, 0).Format("2006-01-02 15:04:05"))
	}
	return 0
}

func cmdSessionList(ctx context.Context, args []string) int {
	var opts session.ListOpts
	opts.Limit = 100

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--since="):
			t, err := time.Parse(time.RFC3339, strings.TrimPrefix(arg, "--since="))
			if err != nil {
				slog.Error("session list: invalid --since (use RFC3339)", "error", err)
				return 3
			}
			opts.Since = t.Unix()
		case arg == "--active-only":
			opts.ActiveOnly = true
		case strings.HasPrefix(arg, "--limit="):
			v, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil {
				slog.Error("session list: invalid --limit", "error", err)
				return 3
			}
			opts.Limit = v
		default:
			slog.Error("session list: unknown flag", "value", arg)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session list failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	sessions, err := store.List(ctx, opts)
	if err != nil {
		slog.Error("session list failed", "error", err)
		return 2
	}

	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(sessions)
	} else {
		if len(sessions) == 0 {
			fmt.Println("No sessions found.")
			return 0
		}
		for _, s := range sessions {
			started := time.Unix(s.StartedAt, 0).Format("2006-01-02 15:04")
			status := "active"
			if s.EndedAt != nil {
				status = "ended"
			}
			model := ""
			if s.Model != nil {
				model = " model=" + *s.Model
			}
			tokens := ""
			billing := s.InputTokens + s.OutputTokens
			if billing > 0 {
				tokens = fmt.Sprintf(" tokens=%d", billing)
			}
			fmt.Printf("[%s] %s  %s  %s%s%s\n", started, s.SessionID, s.ProjectDir, status, model, tokens)
		}
		fmt.Printf("\n%d session(s)\n", len(sessions))
	}
	return 0
}

func cmdSessionTokens(ctx context.Context, args []string) int {
	var opts session.TokensOpts

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--session="):
			opts.SessionID = strings.TrimPrefix(arg, "--session=")
		case strings.HasPrefix(arg, "--project="):
			opts.ProjectDir = strings.TrimPrefix(arg, "--project=")
		case strings.HasPrefix(arg, "--input="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--input="), 10, 64)
			if err != nil {
				slog.Error("session tokens: invalid --input", "error", err)
				return 3
			}
			opts.InputTokens = v
		case strings.HasPrefix(arg, "--output="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--output="), 10, 64)
			if err != nil {
				slog.Error("session tokens: invalid --output", "error", err)
				return 3
			}
			opts.OutputTokens = v
		case strings.HasPrefix(arg, "--cache-creation="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--cache-creation="), 10, 64)
			if err != nil {
				slog.Error("session tokens: invalid --cache-creation", "error", err)
				return 3
			}
			opts.CacheCreationTokens = v
		case strings.HasPrefix(arg, "--cache-read="):
			v, err := strconv.ParseInt(strings.TrimPrefix(arg, "--cache-read="), 10, 64)
			if err != nil {
				slog.Error("session tokens: invalid --cache-read", "error", err)
				return 3
			}
			opts.CacheReadTokens = v
		default:
			slog.Error("session tokens: unknown flag", "value", arg)
			return 3
		}
	}

	if opts.SessionID == "" {
		slog.Error("session tokens: --session is required")
		return 3
	}

	if opts.ProjectDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			slog.Error("session tokens: cannot determine CWD", "error", err)
			return 2
		}
		opts.ProjectDir = cwd
	}

	d, err := openDB()
	if err != nil {
		slog.Error("session tokens failed", "error", err)
		return 2
	}
	defer d.Close()

	store := session.NewStore(d.SqlDB())
	if err := store.UpdateTokens(ctx, opts); err != nil {
		slog.Error("session tokens failed", "error", err)
		return 2
	}

	total := opts.InputTokens + opts.OutputTokens
	if flagJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"session_id":             opts.SessionID,
			"added_input_tokens":     opts.InputTokens,
			"added_output_tokens":    opts.OutputTokens,
			"added_cache_creation":   opts.CacheCreationTokens,
			"added_cache_read":       opts.CacheReadTokens,
			"added_billing_tokens":   total,
		})
	} else {
		fmt.Printf("Tokens recorded: +%d in, +%d out (billing: +%d)\n",
			opts.InputTokens, opts.OutputTokens, total)
	}
	return 0
}
