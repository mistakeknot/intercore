package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mistakeknot/interverse/infra/intercore/internal/event"
	"github.com/mistakeknot/interverse/infra/intercore/internal/state"
)

func cmdEvents(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: events: missing subcommand (tail, cursor)\n")
		return 3
	}

	switch args[0] {
	case "tail":
		return cmdEventsTail(ctx, args[1:])
	case "cursor":
		return cmdEventsCursor(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: events: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdEventsTail(ctx context.Context, args []string) int {
	var runID, consumer string
	var follow bool
	var sincePhase, sinceDispatch, sinceDiscovery int64
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
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --since-phase: %s\n", val)
				return 3
			}
			sincePhase = n
		case strings.HasPrefix(args[i], "--since-dispatch="):
			val := strings.TrimPrefix(args[i], "--since-dispatch=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --since-dispatch: %s\n", val)
				return 3
			}
			sinceDispatch = n
		case strings.HasPrefix(args[i], "--since-discovery="):
			val := strings.TrimPrefix(args[i], "--since-discovery=")
			n, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --since-discovery: %s\n", val)
				return 3
			}
			sinceDiscovery = n
		case strings.HasPrefix(args[i], "--consumer="):
			consumer = strings.TrimPrefix(args[i], "--consumer=")
		case strings.HasPrefix(args[i], "--poll-interval="):
			val := strings.TrimPrefix(args[i], "--poll-interval=")
			d, err := time.ParseDuration(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --poll-interval: %s\n", val)
				return 3
			}
			pollInterval = d
		case strings.HasPrefix(args[i], "--limit="):
			val := strings.TrimPrefix(args[i], "--limit=")
			n, err := strconv.Atoi(val)
			if err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: invalid --limit: %s\n", val)
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
		fmt.Fprintf(os.Stderr, "ic: events tail: provide <run_id> or --all\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events tail: %v\n", err)
		return 2
	}
	defer d.Close()

	evStore := event.NewStore(d.SqlDB())
	stStore := state.New(d.SqlDB())

	// Restore cursor if consumer is named
	if consumer != "" && sincePhase == 0 && sinceDispatch == 0 && sinceDiscovery == 0 {
		sincePhase, sinceDispatch, sinceDiscovery = loadCursor(ctx, stStore, consumer, runID)
	}

	enc := json.NewEncoder(os.Stdout)

	for {
		var events []event.Event
		var err error

		if allRuns || runID == "" {
			events, err = evStore.ListAllEvents(ctx, sincePhase, sinceDispatch, sinceDiscovery, limit)
		} else {
			events, err = evStore.ListEvents(ctx, runID, sincePhase, sinceDispatch, sinceDiscovery, limit)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: events tail: %v\n", err)
			return 2
		}

		encodeErr := false
		for _, e := range events {
			if err := enc.Encode(e); err != nil {
				fmt.Fprintf(os.Stderr, "ic: events tail: write: %v\n", err)
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
		}

		// Save cursor after each batch (skip on encode error to avoid advancing past undelivered events)
		if consumer != "" && len(events) > 0 && !encodeErr {
			saveCursor(ctx, stStore, consumer, runID, sincePhase, sinceDispatch, sinceDiscovery)
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
		fmt.Fprintf(os.Stderr, "ic: events cursor: missing subcommand (list, reset, register)\n")
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
		fmt.Fprintf(os.Stderr, "ic: events cursor: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdEventsCursorList(ctx context.Context) int {
	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor list: %v\n", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	ids, err := stStore.List(ctx, "cursor")
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor list: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: %v\n", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())
	deleted, err := stStore.Delete(ctx, "cursor", args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor reset: %v\n", err)
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
		fmt.Fprintf(os.Stderr, "ic: events cursor register: %v\n", err)
		return 2
	}
	defer d.Close()

	stStore := state.New(d.SqlDB())

	ttl := 24 * time.Hour
	if durable {
		ttl = 0
	}

	payload := `{"phase":0,"dispatch":0,"interspect":0,"discovery":0}`
	if err := stStore.Set(ctx, "cursor", consumer, json.RawMessage(payload), ttl); err != nil {
		fmt.Fprintf(os.Stderr, "ic: events cursor register: %v\n", err)
		return 2
	}

	if durable {
		fmt.Printf("registered %s (durable)\n", consumer)
	} else {
		fmt.Printf("registered %s (ttl: 24h)\n", consumer)
	}
	return 0
}

// --- cursor helpers ---

func loadCursor(ctx context.Context, store *state.Store, consumer, scope string) (int64, int64, int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload, err := store.Get(ctx, "cursor", key)
	if err != nil {
		return 0, 0, 0
	}

	var cursor struct {
		Phase      int64 `json:"phase"`
		Dispatch   int64 `json:"dispatch"`
		Interspect int64 `json:"interspect"`
		Discovery  int64 `json:"discovery"`
	}
	if err := json.Unmarshal(payload, &cursor); err != nil {
		return 0, 0, 0
	}
	return cursor.Phase, cursor.Dispatch, cursor.Discovery
}

func saveCursor(ctx context.Context, store *state.Store, consumer, scope string, phaseID, dispatchID, discoveryID int64) {
	key := consumer
	if scope != "" {
		key = consumer + ":" + scope
	}
	payload := fmt.Sprintf(`{"phase":%d,"dispatch":%d,"interspect":0,"discovery":%d}`, phaseID, dispatchID, discoveryID)
	// Use existing TTL if cursor was registered as durable; otherwise default 24h
	ttl := cursorTTL(ctx, store, key)
	if err := store.Set(ctx, "cursor", key, json.RawMessage(payload), ttl); err != nil {
		fmt.Fprintf(os.Stderr, "[event] saveCursor %s: %v\n", key, err)
	}
}

// cursorTTL returns 0 (durable) if the cursor exists with no expiry, else 24h default.
func cursorTTL(ctx context.Context, store *state.Store, key string) time.Duration {
	if store.IsDurable(ctx, "cursor", key) {
		return 0
	}
	return 24 * time.Hour
}
