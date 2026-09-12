package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	usagepkg "github.com/mistakeknot/intercore/internal/usage"
)

func cmdUsage(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "ic: usage: expected observe, list, validate, or --help")
		return 3
	}
	switch args[0] {
	case "help", "--help", "-h":
		printUsageHelp()
		return 0
	case "observe":
		return cmdUsageObserve(ctx, args[1:])
	case "list":
		return cmdUsageList(ctx, args[1:])
	case "validate":
		return cmdUsageValidate(ctx, args[1:])
	default:
		slog.Error("usage: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

func printUsageHelp() {
	fmt.Fprintln(os.Stdout, `Usage evidence is append-only and observational.

Commands:
  ic usage observe --record=<file>  Append one strict regular JSON file (maximum 1 MiB)
  ic usage list [--limit=<1-1000> | --observation=<id>]  List observations (default 100)
  ic usage validate --observation=<id> [--max-age=<seconds>]
                                     Append all-kernel activity evidence (default max age 3600s)

Validation never writes routing or acceptance state and always reports external activity as unknown.`)
}

func cmdUsageObserve(ctx context.Context, args []string) int {
	flags, err := parseUsageFlags(args, map[string]struct{}{"record": {}})
	if err != nil {
		slog.Error("usage observe: invalid arguments", "error", err)
		return 3
	}
	recordPath, ok := flags["record"]
	if !ok {
		slog.Error("usage observe: --record is required")
		return 3
	}

	input, err := readUsageRecord(recordPath)
	if err != nil {
		slog.Error("usage observe: invalid record", "error", err)
		return 3
	}
	d, err := openDB()
	if err != nil {
		slog.Error("usage observe: open database", "error", err)
		return 2
	}
	defer d.Close()
	observation, inserted, err := usagepkg.NewStore(d.SqlDB()).Observe(ctx, input)
	if err != nil {
		slog.Error("usage observe failed", "error", err)
		if errors.Is(err, usagepkg.ErrIDConflict) || errors.Is(err, usagepkg.ErrIncompatibleSupersedes) || errors.Is(err, usagepkg.ErrNotFound) {
			return 1
		}
		return 2
	}
	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(observation); err != nil {
			slog.Error("usage observe: encode", "error", err)
			return 2
		}
	} else {
		state := "existing"
		if inserted {
			state = "inserted"
		}
		fmt.Printf("%s\t%s\t%s\n", observation.ID, state, observation.CanonicalSHA256)
	}
	return 0
}

func cmdUsageList(ctx context.Context, args []string) int {
	flags, err := parseUsageFlags(args, map[string]struct{}{"limit": {}, "observation": {}})
	if err != nil {
		slog.Error("usage list: invalid arguments", "error", err)
		return 3
	}
	limit := 100
	if flags["observation"] != "" && flags["limit"] != "" {
		slog.Error("usage list: --observation and --limit are mutually exclusive")
		return 3
	}
	if raw, ok := flags["limit"]; ok {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit <= 0 || limit > 1000 {
			slog.Error("usage list: --limit must be an integer from 1 to 1000")
			return 3
		}
	}
	d, err := openDB()
	if err != nil {
		slog.Error("usage list: open database", "error", err)
		return 2
	}
	defer d.Close()
	store := usagepkg.NewStore(d.SqlDB())
	var observations []usagepkg.Observation
	if id := flags["observation"]; id != "" {
		var observation *usagepkg.Observation
		observation, err = store.Get(ctx, id)
		observations = []usagepkg.Observation{}
		if err == nil {
			observations = append(observations, *observation)
		}
	} else {
		observations, err = store.List(ctx, limit)
	}
	if err != nil {
		slog.Error("usage list failed", "error", err)
		if errors.Is(err, usagepkg.ErrNotFound) {
			return 1
		}
		return 2
	}
	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(observations); err != nil {
			slog.Error("usage list: encode", "error", err)
			return 2
		}
	} else {
		for _, observation := range observations {
			fmt.Printf("%s\t%s\t%s\t%s\t%s\n", observation.ID, observation.Provider, observation.Source, observation.Kind, observation.Status)
		}
	}
	return 0
}

func cmdUsageValidate(ctx context.Context, args []string) int {
	flags, err := parseUsageFlags(args, map[string]struct{}{"observation": {}, "max-age": {}})
	if err != nil {
		slog.Error("usage validate: invalid arguments", "error", err)
		return 3
	}
	observationID, ok := flags["observation"]
	if !ok {
		slog.Error("usage validate: --observation is required")
		return 3
	}
	maxAge := time.Hour
	if raw, ok := flags["max-age"]; ok {
		seconds, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil || seconds <= 0 {
			slog.Error("usage validate: --max-age must be a positive integer number of seconds")
			return 3
		}
		maxAge = time.Duration(seconds) * time.Second
		if maxAge/time.Second != time.Duration(seconds) {
			slog.Error("usage validate: --max-age is too large")
			return 3
		}
	}
	d, err := openDB()
	if err != nil {
		slog.Error("usage validate: open database", "error", err)
		return 2
	}
	defer d.Close()
	validation, err := usagepkg.NewStore(d.SqlDB()).Validate(ctx, observationID, usagepkg.ValidateOptions{MaxAge: maxAge})
	if err != nil {
		slog.Error("usage validate failed", "error", err)
		if errors.Is(err, usagepkg.ErrNotFound) {
			return 1
		}
		return 2
	}
	if flagJSON {
		if err := json.NewEncoder(os.Stdout).Encode(validation); err != nil {
			slog.Error("usage validate: encode", "error", err)
			return 2
		}
	} else {
		fmt.Printf("%d\t%s\t%s\t%s\t%s\n", validation.ID, validation.ObservationID, validation.KnownOverlap, validation.BindingCoverage, validation.ExternalActivity)
	}
	return 0
}

func readUsageRecord(path string) (usagepkg.ObservationInput, error) {
	var zero usagepkg.ObservationInput
	file, err := os.Open(path)
	if err != nil {
		return zero, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return zero, err
	}
	if !info.Mode().IsRegular() {
		return zero, errors.New("record must be a regular file")
	}
	if info.Size() > usagepkg.MaxRecordBytes {
		return zero, fmt.Errorf("record exceeds %d bytes", usagepkg.MaxRecordBytes)
	}
	input, _, err := usagepkg.DecodeRecord(file)
	return input, err
}

// parseUsageFlags is intentionally separate from cli.ParseFlags. Every usage
// flag takes exactly one value and duplicate/unknown grammar fails closed.
func parseUsageFlags(args []string, allowed map[string]struct{}) (map[string]string, error) {
	values := make(map[string]string, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") || len(arg) == 2 {
			return nil, fmt.Errorf("unexpected positional argument %q", arg)
		}
		nameValue := strings.TrimPrefix(arg, "--")
		name, value, hasEquals := strings.Cut(nameValue, "=")
		if _, ok := allowed[name]; !ok {
			return nil, fmt.Errorf("unknown flag --%s", name)
		}
		if _, duplicate := values[name]; duplicate {
			return nil, fmt.Errorf("duplicate flag --%s", name)
		}
		if !hasEquals {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return nil, fmt.Errorf("missing value for --%s", name)
			}
			i++
			value = args[i]
		}
		if value == "" {
			return nil, fmt.Errorf("missing value for --%s", name)
		}
		values[name] = value
	}
	return values, nil
}
