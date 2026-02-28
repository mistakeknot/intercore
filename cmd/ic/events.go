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

	"github.com/mistakeknot/intercore/internal/event"
	"github.com/mistakeknot/intercore/internal/state"
)

func cmdEvents(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("events: missing subcommand", "expected", "tail, cursor, emit, list-review")
		return 3
	}

	switch args[0] {
	case "tail":
		return cmdEventsTail(ctx, args[1:])
	case "cursor":
		return cmdEventsCursor(ctx, args[1:])
	case "emit":
		return cmdEventsEmit(ctx, args[1:])
	case "list-review":
		return cmdEventsListReview(ctx, args[1:])
	default:
		slog.Error("events: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdEventsTail(ctx context.Context, args []string) int {
	var runID, consumer string
	var follow bool
	var sincePhase, sinceDispatch, sinceInterspect, sinceDiscovery, sinceReview int64
	var allRuns bool
	pollInterval := 500 * time.Millisecond
	limit := 100

	var positional []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--follow" || args[i] == "-f":
			follow = true
		case args[i] == "--all":
			allRuns = true
		case strings.HasPrefix(args[i], "--since-phase="):
			val := strings.TrimPrefix(args[i], "--since-phase=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				slog.Error("events tail: invalid --since-phase", "value", val)
				return 3
			}
			sincePhase = n
		case strings.HasPrefix(args[i], "--since-dispatch="):
			val := strings.TrimPrefix(args[i], "--since-dispatch=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				slog.Error("events tail: invalid --since-dispatch", "value", val)
				return 3
			}
			sinceDispatch = n
		case strings.HasPrefix(args[i], "--since-discovery="):
			val := strings.TrimPrefix(args[i], "--since-discovery=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				slog.Error("events tail: invalid --since-discovery", "value", val)
				return 3
			}
			sinceDiscovery = n
		case strings.HasPrefix(args[i], "--since-review="):
			val := strings.TrimPrefix(args[i], "--since-review=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				slog.Error("events tail: invalid --since-review", "value", val)
				return 3
			}
			sinceReview = n
		case strings.HasPrefix(args[i], "--consumer="):
			consumer = strings.TrimPrefix(args[i], "--consumer=")
		case strings.HasPrefix(args[i], "--poll-interval="):
			val := strings.TrimPrefix(args[i], "--poll-interval=")
			d, err := time.ParseDuration(val)
			if err != nil {
				slog.Error("events tail: invalid --poll-interval", "value", val)
				return 3
			}
			pollInterval = d
		case strings.HasPrefix(args[i], "--limit="):
			val := strings.TrimPrefix(args[i], "--limit=")
			n, err := strconv.Atoi(val)
			if err != nil {
				slog.Error("events tail: invalid --limit", "value", val)
				return 3
			}
			limit = n
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) > 0 {
		runID = positional[0]
	}

	if runID == "" && !allRuns {
		slog.Error("events tail: provide <run_id> or --all")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("events tail failed", "error", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	stStore := state.New(d.SqlDB())

	// Restore cursor if consumer is named
	if consumer != "" && sincePhase == 0 && sinceDispatch == 0 && sinceDiscovery == 0 && sinceReview == 0 {
		sincePhase, sinceDispatch, sinceInterspect, sinceDiscovery, sinceReview = loadCursor(ctx, stStore, consumer, runID)
	}

	enc := json.NewEncoder(os.Stdout)

	for {
		var events []event.Event
		var err error

		if allRuns || runID == "" {
			events, err = evStore.ListAllEvents(ctx, sincePhase, sinceDispatch, sinceDiscovery, sinceReview, limit)
		} else {
			events, err = evStore.ListEvents(ctx, runID, sincePhase, sinceDispatch, sinceDiscovery, sinceReview, limit)
		}
		if err != nil {
			slog.Error("events tail failed", "error", err)
			return 2
		}

		encodeErr := false
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				slog.Error("events tail: write failed", "error", err)
				encodeErr = true
				break
			}
			// Track high water mark per source (only after successful write)
			if e.Source == event.SourcePhase && e.ID > sincePhase {
				sincePhase = e.ID
			}
			if e.Source == event.SourceDispatch && e.ID > sinceDispatch {
				sinceDispatch = e.ID
			}
			if e.Source == event.SourceDiscovery && e.ID > sinceDiscovery {
				sinceDiscovery = e.ID
			}
			if e.Source == event.SourceReview && e.ID > sinceReview {
				sinceReview = e.ID
			}
		}

		// Save cursor after each batch (skip on encode error to avoid advancing past undelivered events)
		if consumer != "" && len(events) > 0 && !encodeErr {
			saveCursor(ctx, stStore, consumer, runID, sincePhase, sinceDispatch, sinceInterspect, sinceDiscovery, sinceReview)
		}

		if encodeErr {
			return 2
		}

		if !follow {
			break
		}

		time.Sleep(pollInterval)
	}

	return 0
}

func cmdEventsCursor(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("events cursor: missing subcommand", "expected", "list, reset, register")
		return 3
	}

	switch args[0] {
	case "list":
		return cmdEventsCursorList(ctx)
	case "reset":
		return cmdEventsCursorReset(ctx, args[1:])
	case "register":
		return cmdEventsCursorRegister(ctx, args[1:])
	default:
		slog.Error("events cursor: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func cmdEventsCursorList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		slog.Error("events cursor list failed", "error", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	ids, err := stStore.List(ctx, "cursor")
	if err != nil {
		slog.Error("events cursor list failed", "error", err)
		return 2
	}

	for _, id := range ids {
		payload, err := stStore.Get(ctx, "cursor", id)
		if err != nil {
			continue
		}
		fmt.Printf("%s\t%s\n", id, string(payload))
	}
	return 0
}

func cmdEventsCursorReset(ctx context.Context, args []string) int {
	if len(args) < 1 {
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: usage: ic events cursor reset <consumer-name>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("events cursor reset failed", "error", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	deleted, err := stStore.Delete(ctx, "cursor", args[0])
	if err != nil {
		slog.Error("events cursor reset failed", "error", err)
		return 2
	}

	if deleted {
		fmt.Println("reset")
	} else {
		fmt.Println("not found")
	}
	return 0
}

func cmdEventsCursorRegister(ctx context.Context, args []string) int {
	var durable bool
	var positional []string

	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--durable":
			durable = true
		default:
			positional = append(positional, args[i])
		}
	}

	if len(positional) < 1 {
		fmt.Fprintf(os.Stderr, "ic: events cursor register: usage: ic events cursor register <consumer> [--durable]\n")
		return 3
	}

	consumer := positional[0]

	d, err := openDB()
	if err != nil {
		slog.Error("events cursor register failed", "error", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())

	ttl := 24 * time.Hour
	if durable {
		ttl = 0
	}

	payload := `{"phase":0,"dispatch":0,"interspect":0,"discovery":0,"review":0}`
	if err := stStore.Set(ctx, "cursor", consumer, json.RawMessage(payload), ttl); err != nil {
		slog.Error("events cursor register failed", "error", err)
		return 2
	}

	if durable {
		fmt.Printf("registered %s (durable)\n", consumer)
	} else {
		fmt.Printf("registered %s (ttl: 24h)\n", consumer)
	}
	return 0
}

func cmdEventsEmit(ctx context.Context, args []string) int {
	var source, eventType, contextJSON, runID, sessionID, projectDir string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case strings.HasPrefix(args[i], "--type="):
			eventType = strings.TrimPrefix(args[i], "--type=")
		case strings.HasPrefix(args[i], "--context="):
			contextJSON = strings.TrimPrefix(args[i], "--context=")
		case strings.HasPrefix(args[i], "--run="):
			runID = strings.TrimPrefix(args[i], "--run=")
		case strings.HasPrefix(args[i], "--session="):
			sessionID = strings.TrimPrefix(args[i], "--session=")
		case strings.HasPrefix(args[i], "--project="):
			projectDir = strings.TrimPrefix(args[i], "--project=")
		default:
			slog.Error("events emit: unknown flag", "value", args[i])
			return 3
		}
	}

	if source == "" {
		slog.Error("events emit: --source is required")
		return 3
	}
	if eventType == "" {
		slog.Error("events emit: --type is required")
		return 3
	}

	// v1: only review source supported via emit
	if source != event.SourceReview {
		slog.Error("events emit: only --source=review is supported", "source", source)
		return 3
	}

	if contextJSON == "" {
		slog.Error("events emit: --context is required for review events")
		return 3
	}
	if !json.Valid([]byte(contextJSON)) {
		slog.Error("events emit: --context must be valid JSON")
		return 3
	}

	// Default session/project from env
	if sessionID == "" {
		sessionID = os.Getenv("CLAUDE_SESSION_ID")
	}
	if projectDir == "" {
		projectDir, _ = os.Getwd()
	}

	d, err := openDB()
	if err != nil {
		slog.Error("events emit failed", "error", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())

	var payload struct {
		FindingID       string            `json:"finding_id"`
		Agents          map[string]string `json:"agents"`
		Resolution      string            `json:"resolution"`
		DismissalReason string            `json:"dismissal_reason"`
		ChosenSeverity  string            `json:"chosen_severity"`
		Impact          string            `json:"impact"`
	}
	if err := json.Unmarshal([]byte(contextJSON), &payload); err != nil {
		slog.Error("events emit: failed to parse review context", "error", err)
		return 3
	}
	if payload.FindingID == "" || payload.Resolution == "" || payload.ChosenSeverity == "" || payload.Impact == "" {
		slog.Error("events emit: review context requires finding_id, resolution, chosen_severity, impact")
		return 3
	}
	if len(payload.Agents) == 0 {
		slog.Error("events emit: review context requires non-empty agents map")
		return 3
	}
	agentsJSON, _ := json.Marshal(payload.Agents)

	_ = eventType // eventType validated but not used in AddReviewEvent (hardcoded as disagreement_resolved)

	id, err := evStore.AddReviewEvent(ctx, runID, payload.FindingID, string(agentsJSON), payload.Resolution, payload.DismissalReason, payload.ChosenSeverity, payload.Impact, sessionID, projectDir)
	if err != nil {
		slog.Error("events emit failed", "error", err)
		return 2
	}
	fmt.Printf("%d\n", id)

	return 0
}

func cmdEventsListReview(ctx context.Context, args []string) int {
	var since int64
	limit := 1000

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--since="):
			val := strings.TrimPrefix(args[i], "--since=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				slog.Error("events list-review: invalid --since", "value", val)
				return 3
			}
			since = n
		case strings.HasPrefix(args[i], "--limit="):
			val := strings.TrimPrefix(args[i], "--limit=")
			n, err := strconv.Atoi(val)
			if err != nil {
				slog.Error("events list-review: invalid --limit", "value", val)
				return 3
			}
			limit = n
		}
	}

	d, err := openDB()
	if err != nil {
		slog.Error("events list-review failed", "error", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	events, err := evStore.ListReviewEvents(ctx, since, limit)
	if err != nil {
		slog.Error("events list-review failed", "error", err)
		return 2
	}

	enc := json.NewEncoder(os.Stdout)
	for _, e := range events {
		enc.Encode(e)
	}
	return 0
}

// --- cursor helpers ---

func loadCursor(ctx context.Context, store *state.Store, consumer, scope string) (phase, dispatch, interspect, discovery, review int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload, err := store.Get(ctx, "cursor", key)
	if err != nil {
		return 0, 0, 0, 0, 0
	}

	var cursor struct {
		Phase      int64 `json:"phase"`
		Dispatch   int64 `json:"dispatch"`
		Interspect int64 `json:"interspect"`
		Discovery  int64 `json:"discovery"`
		Review     int64 `json:"review"`
	}
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return 0, 0, 0, 0, 0
	}
	return cursor.Phase, cursor.Dispatch, cursor.Interspect, cursor.Discovery, cursor.Review
}

func saveCursor(ctx context.Context, store *state.Store, consumer, scope string, phaseID, dispatchID, interspectID, discoveryID, reviewID int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":%d,"discovery":%d,"review":%d}`, phaseID, dispatchID, interspectID, discoveryID, reviewID)
	// Use existing TTL if cursor was registered as durable; otherwise default 24h
	ttl := cursorTTL(ctx, store, key)
	if err := store.Set(ctx, "cursor", key, json.RawMessage(payload), ttl); err != nil {
		slog.Debug("event: saveCursor", "cursor", key, "error", err)
	}
}

// cursorTTL returns 0 (durable) if the cursor exists with no expiry, else 24h default.
func cursorTTL(ctx context.Context, store *state.Store, key string) time.Duration {
	if store.IsDurable(ctx, "cursor", key) {
		return 0
	}
	return 24 * time.Hour
}
