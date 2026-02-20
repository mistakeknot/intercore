package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/mistakeknot/interverse/infra/intercore/internal/discovery"
)

func cmdDiscovery(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: discovery: missing subcommand (submit, status, list, score, promote, dismiss, feedback, profile, decay, rollback, search)\n")
		return 3
	}

	switch args[0] {
	case "submit":
		return cmdDiscoverySubmit(ctx, args[1:])
	case "status":
		return cmdDiscoveryStatus(ctx, args[1:])
	case "list":
		return cmdDiscoveryList(ctx, args[1:])
	case "score":
		return cmdDiscoveryScore(ctx, args[1:])
	case "promote":
		return cmdDiscoveryPromote(ctx, args[1:])
	case "dismiss":
		return cmdDiscoveryDismiss(ctx, args[1:])
	case "feedback":
		return cmdDiscoveryFeedback(ctx, args[1:])
	case "profile":
		return cmdDiscoveryProfile(ctx, args[1:])
	case "decay":
		return cmdDiscoveryDecay(ctx, args[1:])
	case "rollback":
		return cmdDiscoveryRollback(ctx, args[1:])
	case "search":
		return cmdDiscoverySearch(ctx, args[1:])
	default:
		fmt.Fprintf(os.Stderr, "ic: discovery: unknown subcommand: %s\n", args[0])
		return 3
	}
}

func cmdDiscoverySubmit(ctx context.Context, args []string) int {
	var source, sourceID, title, summary, url, metadataFile, embeddingFile string
	var scoreStr, dedupStr string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case strings.HasPrefix(args[i], "--source-id="):
			sourceID = strings.TrimPrefix(args[i], "--source-id=")
		case strings.HasPrefix(args[i], "--title="):
			title = strings.TrimPrefix(args[i], "--title=")
		case strings.HasPrefix(args[i], "--summary="):
			summary = strings.TrimPrefix(args[i], "--summary=")
		case strings.HasPrefix(args[i], "--url="):
			url = strings.TrimPrefix(args[i], "--url=")
		case strings.HasPrefix(args[i], "--metadata="):
			metadataFile = strings.TrimPrefix(args[i], "--metadata=")
		case strings.HasPrefix(args[i], "--embedding="):
			embeddingFile = strings.TrimPrefix(args[i], "--embedding=")
		case strings.HasPrefix(args[i], "--score="):
			scoreStr = strings.TrimPrefix(args[i], "--score=")
		case strings.HasPrefix(args[i], "--dedup-threshold="):
			dedupStr = strings.TrimPrefix(args[i], "--dedup-threshold=")
		}
	}

	if source == "" || sourceID == "" || title == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery submit: --source, --source-id, and --title are required\n")
		return 3
	}

	var score float64
	if scoreStr != "" {
		var err error
		score, err = strconv.ParseFloat(scoreStr, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery submit: invalid score: %s\n", scoreStr)
			return 3
		}
	}

	metadata := "{}"
	if metadataFile != "" {
		data, err := readFileArg(metadataFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery submit: metadata: %v\n", err)
			return 2
		}
		metadata = string(data)
	}

	var embedding []byte
	if embeddingFile != "" {
		data, err := readFileArg(embeddingFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery submit: embedding: %v\n", err)
			return 2
		}
		embedding = data
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery submit: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())

	var id string
	if dedupStr != "" {
		threshold, err := strconv.ParseFloat(dedupStr, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery submit: invalid dedup threshold: %s\n", dedupStr)
			return 3
		}
		id, err = store.SubmitWithDedup(ctx, source, sourceID, title, summary, url, metadata, embedding, score, threshold)
		if err != nil {
			return discoveryError("submit", err)
		}
	} else {
		id, err = store.Submit(ctx, source, sourceID, title, summary, url, metadata, embedding, score)
		if err != nil {
			return discoveryError("submit", err)
		}
	}

	fmt.Println(id)
	return 0
}

func cmdDiscoveryStatus(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: discovery status: usage: ic discovery status <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery status: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	disc, err := store.Get(ctx, args[0])
	if err != nil {
		return discoveryError("status", err)
	}

	if flagJSON {
		data, _ := json.Marshal(disc)
		fmt.Println(string(data))
	} else {
		fmt.Printf("%s\t%s\t%s\t%s\t%.2f\t%s\n", disc.ID, disc.Source, disc.Title, disc.Status, disc.RelevanceScore, disc.ConfidenceTier)
	}
	return 0
}

func cmdDiscoveryList(ctx context.Context, args []string) int {
	var source, status, tier string
	var limitStr string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case strings.HasPrefix(args[i], "--status="):
			status = strings.TrimPrefix(args[i], "--status=")
		case strings.HasPrefix(args[i], "--tier="):
			tier = strings.TrimPrefix(args[i], "--tier=")
		case strings.HasPrefix(args[i], "--limit="):
			limitStr = strings.TrimPrefix(args[i], "--limit=")
		}
	}

	limit := 100
	if limitStr != "" {
		var err error
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery list: invalid limit: %s\n", limitStr)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery list: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	results, err := store.List(ctx, discovery.ListFilter{
		Source: source, Status: status, Tier: tier, Limit: limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery list: %v\n", err)
		return 2
	}

	if flagJSON {
		data, _ := json.Marshal(results)
		fmt.Println(string(data))
	} else {
		for _, r := range results {
			fmt.Printf("%s\t%s\t%s\t%s\t%.2f\t%s\n", r.ID, r.Source, r.Title, r.Status, r.RelevanceScore, r.ConfidenceTier)
		}
	}
	return 0
}

func cmdDiscoveryScore(ctx context.Context, args []string) int {
	var id, scoreStr string

	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--score=") {
			scoreStr = strings.TrimPrefix(args[i], "--score=")
		} else if !strings.HasPrefix(args[i], "--") {
			id = args[i]
		}
	}

	if id == "" || scoreStr == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery score: usage: ic discovery score <id> --score=<0.0-1.0>\n")
		return 3
	}

	score, err := strconv.ParseFloat(scoreStr, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery score: invalid score: %s\n", scoreStr)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery score: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	if err := store.Score(ctx, id, score); err != nil {
		return discoveryError("score", err)
	}

	fmt.Printf("scored %s: %.2f (%s)\n", id, score, discovery.TierFromScore(score))
	return 0
}

func cmdDiscoveryPromote(ctx context.Context, args []string) int {
	var id, beadID string
	force := false

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--bead-id="):
			beadID = strings.TrimPrefix(args[i], "--bead-id=")
		case args[i] == "--force":
			force = true
		case !strings.HasPrefix(args[i], "--"):
			id = args[i]
		}
	}

	if id == "" || beadID == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery promote: usage: ic discovery promote <id> --bead-id=<bid> [--force]\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery promote: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	if err := store.Promote(ctx, id, beadID, force); err != nil {
		return discoveryError("promote", err)
	}

	fmt.Printf("promoted %s → %s\n", id, beadID)
	return 0
}

func cmdDiscoveryDismiss(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "ic: discovery dismiss: usage: ic discovery dismiss <id>\n")
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery dismiss: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	if err := store.Dismiss(ctx, args[0]); err != nil {
		return discoveryError("dismiss", err)
	}

	fmt.Printf("dismissed %s\n", args[0])
	return 0
}

func cmdDiscoveryFeedback(ctx context.Context, args []string) int {
	var id, signal, dataFile, actor string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--signal="):
			signal = strings.TrimPrefix(args[i], "--signal=")
		case strings.HasPrefix(args[i], "--data="):
			dataFile = strings.TrimPrefix(args[i], "--data=")
		case strings.HasPrefix(args[i], "--actor="):
			actor = strings.TrimPrefix(args[i], "--actor=")
		case !strings.HasPrefix(args[i], "--"):
			id = args[i]
		}
	}

	if id == "" || signal == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery feedback: usage: ic discovery feedback <id> --signal=<type> [--data=@file] [--actor=<name>]\n")
		return 3
	}

	if actor == "" {
		actor = "system"
	}

	data := "{}"
	if dataFile != "" {
		raw, err := readFileArg(dataFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery feedback: data: %v\n", err)
			return 2
		}
		data = string(raw)
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery feedback: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	if err := store.RecordFeedback(ctx, id, signal, data, actor); err != nil {
		return discoveryError("feedback", err)
	}

	fmt.Printf("feedback recorded: %s %s\n", id, signal)
	return 0
}

func cmdDiscoveryProfile(ctx context.Context, args []string) int {
	// Check for "update" subcommand
	if len(args) > 0 && args[0] == "update" {
		return cmdDiscoveryProfileUpdate(ctx, args[1:])
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	p, err := store.GetProfile(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile: %v\n", err)
		return 2
	}

	if flagJSON {
		data, _ := json.Marshal(p)
		fmt.Println(string(data))
	} else {
		fmt.Printf("keyword_weights: %s\nsource_weights: %s\nupdated_at: %d\n", p.KeywordWeights, p.SourceWeights, p.UpdatedAt)
	}
	return 0
}

func cmdDiscoveryProfileUpdate(ctx context.Context, args []string) int {
	var kwFile, swFile string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--keyword-weights="):
			kwFile = strings.TrimPrefix(args[i], "--keyword-weights=")
		case strings.HasPrefix(args[i], "--source-weights="):
			swFile = strings.TrimPrefix(args[i], "--source-weights=")
		}
	}

	if kwFile == "" || swFile == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery profile update: --keyword-weights and --source-weights are required\n")
		return 3
	}

	kw, err := readFileArg(kwFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile update: keyword-weights: %v\n", err)
		return 2
	}
	sw, err := readFileArg(swFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile update: source-weights: %v\n", err)
		return 2
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile update: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	if err := store.UpdateProfile(ctx, nil, string(kw), string(sw)); err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery profile update: %v\n", err)
		return 2
	}

	fmt.Println("profile updated")
	return 0
}

func cmdDiscoveryDecay(ctx context.Context, args []string) int {
	var rateStr, minAgeStr string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--rate="):
			rateStr = strings.TrimPrefix(args[i], "--rate=")
		case strings.HasPrefix(args[i], "--min-age="):
			minAgeStr = strings.TrimPrefix(args[i], "--min-age=")
		}
	}

	if rateStr == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery decay: --rate is required\n")
		return 3
	}

	rate, err := strconv.ParseFloat(rateStr, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery decay: invalid rate: %s\n", rateStr)
		return 3
	}

	minAgeSec := int64(86400) // default 1 day
	if minAgeStr != "" {
		sec, err := strconv.ParseInt(minAgeStr, 10, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery decay: invalid min-age (seconds): %s\n", minAgeStr)
			return 3
		}
		minAgeSec = sec
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery decay: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	count, err := store.Decay(ctx, rate, minAgeSec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery decay: %v\n", err)
		return 2
	}

	fmt.Printf("%d decayed\n", count)
	return 0
}

func cmdDiscoveryRollback(ctx context.Context, args []string) int {
	var source, sinceStr string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case strings.HasPrefix(args[i], "--since="):
			sinceStr = strings.TrimPrefix(args[i], "--since=")
		}
	}

	if source == "" || sinceStr == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery rollback: --source and --since are required\n")
		return 3
	}

	since, err := strconv.ParseInt(sinceStr, 10, 64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery rollback: invalid since timestamp: %s\n", sinceStr)
		return 3
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery rollback: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	count, err := store.Rollback(ctx, source, since)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery rollback: %v\n", err)
		return 2
	}

	fmt.Printf("%d\n", count)
	return 0
}

func cmdDiscoverySearch(ctx context.Context, args []string) int {
	var embeddingFile, source, tier, status, minScoreStr, limitStr string

	for i := 0; i < len(args); i++ {
		switch {
		case strings.HasPrefix(args[i], "--embedding="):
			embeddingFile = strings.TrimPrefix(args[i], "--embedding=")
		case strings.HasPrefix(args[i], "--source="):
			source = strings.TrimPrefix(args[i], "--source=")
		case strings.HasPrefix(args[i], "--tier="):
			tier = strings.TrimPrefix(args[i], "--tier=")
		case strings.HasPrefix(args[i], "--status="):
			status = strings.TrimPrefix(args[i], "--status=")
		case strings.HasPrefix(args[i], "--min-score="):
			minScoreStr = strings.TrimPrefix(args[i], "--min-score=")
		case strings.HasPrefix(args[i], "--limit="):
			limitStr = strings.TrimPrefix(args[i], "--limit=")
		}
	}

	if embeddingFile == "" {
		fmt.Fprintf(os.Stderr, "ic: discovery search: --embedding is required\n")
		return 3
	}

	embedding, err := readFileArg(embeddingFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery search: embedding: %v\n", err)
		return 2
	}

	var minScore float64
	if minScoreStr != "" {
		minScore, err = strconv.ParseFloat(minScoreStr, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery search: invalid min-score: %s\n", minScoreStr)
			return 3
		}
	}

	limit := 10
	if limitStr != "" {
		limit, err = strconv.Atoi(limitStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ic: discovery search: invalid limit: %s\n", limitStr)
			return 3
		}
	}

	d, err := openDB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery search: %v\n", err)
		return 2
	}
	defer d.Close()

	store := discovery.NewStore(d.SqlDB())
	results, err := store.Search(ctx, embedding, discovery.SearchFilter{
		Source: source, Tier: tier, Status: status, MinScore: minScore, Limit: limit,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ic: discovery search: %v\n", err)
		return 2
	}

	if flagJSON {
		data, _ := json.Marshal(results)
		fmt.Println(string(data))
	} else {
		for _, r := range results {
			fmt.Printf("%s\t%s\t%s\t%.2f\t%.4f\t%s\n", r.ID, r.Source, r.Title, r.RelevanceScore, r.Similarity, r.ConfidenceTier)
		}
	}
	return 0
}

// readFileArg reads from a file if prefixed with @, otherwise returns the string as bytes.
func readFileArg(arg string) ([]byte, error) {
	if strings.HasPrefix(arg, "@") {
		return os.ReadFile(arg[1:])
	}
	return []byte(arg), nil
}

// discoveryError maps sentinel errors to exit codes.
func discoveryError(cmd string, err error) int {
	if errors.Is(err, discovery.ErrNotFound) {
		fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
		return 1
	}
	if errors.Is(err, discovery.ErrGateBlocked) {
		fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
		return 1
	}
	if errors.Is(err, discovery.ErrLifecycle) {
		fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
		return 1
	}
	if errors.Is(err, discovery.ErrDuplicate) {
		fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "ic: discovery %s: %v\n", cmd, err)
	return 2
}
