package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/mistakeknot/intercore/internal/action"
	"github.com/mistakeknot/intercore/internal/agency"
	"github.com/mistakeknot/intercore/internal/phase"
	"github.com/mistakeknot/intercore/internal/state"
)

func cmdAgency(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("agency: missing subcommand", "expected", "load, validate, show, capabilities")
		return 3
	}
	switch args[0] {
	case "load":
		return cmdAgencyLoad(ctx, args[1:])
	case "validate":
		return cmdAgencyValidate(ctx, args[1:])
	case "show":
		return cmdAgencyShow(ctx, args[1:])
	case "capabilities":
		return cmdAgencyCapabilities(ctx, args[1:])
	default:
		slog.Error("agency: unknown subcommand", "subcommand", args[0])
		return 3
	}
}

// cmdAgencyLoad loads one or all agency specs into the kernel for a run.
// Usage: ic agency load <stage|all> --run=<id> [--spec-dir=<path>]
func cmdAgencyLoad(ctx context.Context, args []string) int {
	var runID, specDir, target string

	var positional []string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--run="):
			runID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--spec-dir="):
			specDir = strings.TrimPrefix(arg, "--spec-dir=")
		default:
			positional = append(positional, arg)
		}
	}
	if len(positional) > 0 {
		target = positional[0]
	}

	if runID == "" {
		slog.Error("agency load: --run=<id> is required")
		return 3
	}
	if target == "" {
		slog.Error("agency load: specify stage name or 'all'")
		return 3
	}
	if specDir == "" {
		slog.Error("agency load: --spec-dir=<path> is required")
		return 3
	}

	d, err := openDB()
	if err != nil {
		slog.Error("agency load failed", "error", err)
		return 2
	}
	defer d.Close()

	knownPhases := phase.DefaultPhaseChain

	var stages []string
	if target == "all" {
		stages = []string{"discover", "design", "build", "ship", "reflect"}
	} else {
		if !agency.KnownStages[target] {
			slog.Error("agency load: unknown stage", "stage", target)
			return 3
		}
		stages = []string{target}
	}

	actionStore := action.New(d.SqlDB())
	stateStore := state.New(d.SqlDB())

	for _, stageName := range stages {
		specPath := filepath.Join(specDir, stageName+".yaml")
		spec, perr := agency.ParseFile(specPath)
		if perr != nil {
			slog.Error("agency load failed", "error", perr)
			return 1
		}

		verrs := agency.Validate(spec, knownPhases)
		if len(verrs) > 0 {
			slog.Error("agency load: validation failed", "stage", stageName)
			for _, ve := range verrs {
				slog.Error("validation error", "detail", ve)
			}
			return 1
		}

		// Register agents as phase_actions (one Add per agent entry)
		for _, a := range spec.Agents {
			act := &action.Action{
				RunID:      runID,
				Phase:      a.Phase,
				ActionType: "command",
				Command:    a.Command,
				Mode:       a.Mode,
				Priority:   a.Priority,
			}
			if len(a.Args) > 0 {
				argsJSON, merr := json.Marshal(a.Args)
				if merr != nil {
					slog.Error("agency load: marshal args", "phase", a.Phase, "command", a.Command, "error", merr)
					return 1
				}
				s := string(argsJSON)
				act.Args = &s
			}
			if _, aerr := actionStore.Add(ctx, act); aerr != nil {
				// Ignore duplicate errors (idempotent reload)
				if !errors.Is(aerr, action.ErrDuplicate) {
					slog.Error("agency load: register agent", "phase", a.Phase, "command", a.Command, "stage", stageName, "error", aerr)
					return 1
				}
			}
		}

		// Store model overrides per phase
		for phaseName, mc := range spec.Models {
			mcJSON, merr := json.Marshal(mc)
			if merr != nil {
				slog.Error("agency load: marshal model config", "stage", stageName, "phase", phaseName, "error", merr)
				return 1
			}
			key := "agency.models." + phaseName
			if serr := stateStore.Set(ctx, key, runID, json.RawMessage(mcJSON), 0); serr != nil {
				slog.Error("agency load: store model config", "stage", stageName, "phase", phaseName, "error", serr)
				return 1
			}
		}

		// Store gate rules per phase
		if len(spec.Gates.Entry) > 0 || len(spec.Gates.Exit) > 0 {
			for _, phaseName := range spec.Meta.Phases {
				gateJSON, merr := json.Marshal(spec.Gates)
				if merr != nil {
					slog.Error("agency load: marshal gate config", "stage", stageName, "phase", phaseName, "error", merr)
					return 1
				}
				key := "agency.gates." + phaseName
				if serr := stateStore.Set(ctx, key, runID, json.RawMessage(gateJSON), 0); serr != nil {
					slog.Error("agency load: store gate config", "stage", stageName, "phase", phaseName, "error", serr)
					return 1
				}
			}
		}

		// Store capabilities (one key per stage)
		if len(spec.Capabilities) > 0 {
			capsJSON, merr := json.Marshal(spec.Capabilities)
			if merr != nil {
				slog.Error("agency load: marshal capabilities", "stage", stageName, "error", merr)
				return 1
			}
			key := "agency.capabilities." + stageName
			if serr := stateStore.Set(ctx, key, runID, json.RawMessage(capsJSON), 0); serr != nil {
				slog.Error("agency load: store capabilities", "stage", stageName, "error", serr)
				return 1
			}
		}

		if flagJSON {
			// NDJSON: one object per stage (intentional for streaming consumption)
			fmt.Printf("{\"stage\":%q,\"agents\":%d,\"models\":%d,\"gates\":%d}\n",
				stageName, len(spec.Agents), len(spec.Models), len(spec.Gates.Entry)+len(spec.Gates.Exit))
		} else {
			fmt.Printf("loaded %s: agents=%d models=%d gates=%d\n",
				stageName, len(spec.Agents), len(spec.Models), len(spec.Gates.Entry)+len(spec.Gates.Exit))
		}
	}

	return 0
}

// cmdAgencyValidate validates one or all agency spec files.
// Usage: ic agency validate <file> [--spec-dir=<path>] [--all]
func cmdAgencyValidate(ctx context.Context, args []string) int {
	var specDir string
	var all bool
	var files []string

	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "--spec-dir="):
			specDir = strings.TrimPrefix(arg, "--spec-dir=")
		case arg == "--all":
			all = true
		default:
			files = append(files, arg)
		}
	}

	if all {
		if specDir == "" {
			slog.Error("agency validate: --spec-dir required with --all")
			return 3
		}
		matches, _ := filepath.Glob(filepath.Join(specDir, "*.yaml"))
		files = append(files, matches...)
	}

	if len(files) == 0 {
		slog.Error("agency validate: specify a file or --all --spec-dir=<path>")
		return 3
	}

	knownPhases := phase.DefaultPhaseChain
	allValid := true

	for _, f := range files {
		spec, perr := agency.ParseFile(f)
		if perr != nil {
			slog.Error("agency validate failed", "file", filepath.Base(f), "error", perr)
			allValid = false
			continue
		}
		verrs := agency.Validate(spec, knownPhases)
		if len(verrs) > 0 {
			slog.Error("agency validate failed", "file", filepath.Base(f))
			for _, ve := range verrs {
				slog.Error("validation error", "detail", ve)
			}
			allValid = false
		} else {
			fmt.Printf("PASS %s\n", filepath.Base(f))
		}
	}

	if !allValid {
		return 1
	}
	return 0
}

// cmdAgencyShow displays a parsed agency spec as JSON.
// Usage: ic agency show <stage> --spec-dir=<path>
func cmdAgencyShow(ctx context.Context, args []string) int {
	var specDir, target string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--spec-dir=") {
			specDir = strings.TrimPrefix(arg, "--spec-dir=")
		} else {
			target = arg
		}
	}
	if target == "" || specDir == "" {
		fmt.Fprintf(os.Stderr, "ic: agency show: usage: ic agency show <stage> --spec-dir=<path>\n")
		return 3
	}
	specPath := filepath.Join(specDir, target+".yaml")
	spec, err := agency.ParseFile(specPath)
	if err != nil {
		slog.Error("agency show failed", "error", err)
		return 1
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(spec)
	return 0
}

// cmdAgencyCapabilities shows declared capabilities for a run.
// Usage: ic agency capabilities <run-id>
func cmdAgencyCapabilities(ctx context.Context, args []string) int {
	if len(args) == 0 {
		slog.Error("agency capabilities: specify run ID")
		return 3
	}
	runID := args[0]

	d, err := openDB()
	if err != nil {
		slog.Error("agency capabilities failed", "error", err)
		return 2
	}
	defer d.Close()

	stateStore := state.New(d.SqlDB())

	result := make(map[string]json.RawMessage)
	for _, stageName := range []string{"discover", "design", "build", "ship", "reflect"} {
		key := "agency.capabilities." + stageName
		payload, gerr := stateStore.Get(ctx, key, runID)
		if gerr != nil {
			continue // not found is fine
		}
		result[stageName] = payload
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.Encode(result)
	return 0
}
