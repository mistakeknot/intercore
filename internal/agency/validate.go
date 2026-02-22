package agency

import (
	"fmt"
	"regexp"
	"strings"
)

// ValidationError describes a single validation issue in an agency spec.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

// KnownChecks is the set of valid gate check names.
var KnownChecks = map[string]bool{
	"artifact_exists":      true,
	"agents_complete":      true,
	"verdict_exists":       true,
	"children_at_phase":    true,
	"upstreams_at_phase":   true,
	"budget_not_exceeded":  true,
}

var capabilityPattern = regexp.MustCompile(`^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)*$`)

// Validate checks a spec for structural and semantic errors.
// knownPhases is the set of valid kernel phase names (from phase.DefaultPhaseChain).
func Validate(spec *Spec, knownPhases []string) []ValidationError {
	var errs []ValidationError

	phaseSet := make(map[string]bool, len(knownPhases))
	for _, p := range knownPhases {
		phaseSet[p] = true
	}

	// meta.stage
	if spec.Meta.Stage == "" {
		errs = append(errs, ValidationError{"meta.stage", "required"})
	} else if !KnownStages[spec.Meta.Stage] {
		errs = append(errs, ValidationError{"meta.stage", fmt.Sprintf("unknown stage %q; must be one of: discover, design, build, ship, reflect", spec.Meta.Stage)})
	}

	// meta.phases
	if len(spec.Meta.Phases) == 0 {
		errs = append(errs, ValidationError{"meta.phases", "required (at least one phase)"})
	}
	specPhases := make(map[string]bool, len(spec.Meta.Phases))
	for i, p := range spec.Meta.Phases {
		if !phaseSet[p] {
			errs = append(errs, ValidationError{fmt.Sprintf("meta.phases[%d]", i), fmt.Sprintf("unknown phase %q", p)})
		}
		specPhases[p] = true
	}

	// agents: valid phases, no duplicates
	type agentKey struct{ phase, command string }
	seen := make(map[agentKey]bool)
	for i, a := range spec.Agents {
		if a.Phase == "" {
			errs = append(errs, ValidationError{fmt.Sprintf("agents[%d].phase", i), "required"})
		} else if !specPhases[a.Phase] {
			errs = append(errs, ValidationError{fmt.Sprintf("agents[%d].phase", i), fmt.Sprintf("phase %q not in meta.phases", a.Phase)})
		}
		if a.Command == "" {
			errs = append(errs, ValidationError{fmt.Sprintf("agents[%d].command", i), "required"})
		}
		if a.Mode != "" && a.Mode != "interactive" && a.Mode != "autonomous" && a.Mode != "both" {
			errs = append(errs, ValidationError{fmt.Sprintf("agents[%d].mode", i), fmt.Sprintf("unknown mode %q; must be interactive, autonomous, or both", a.Mode)})
		}
		k := agentKey{a.Phase, a.Command}
		if seen[k] {
			errs = append(errs, ValidationError{fmt.Sprintf("agents[%d]", i), fmt.Sprintf("duplicate agent: phase=%q command=%q", a.Phase, a.Command)})
		}
		seen[k] = true
	}

	// models: valid phases
	for phase := range spec.Models {
		if !specPhases[phase] {
			errs = append(errs, ValidationError{fmt.Sprintf("models.%s", phase), fmt.Sprintf("phase %q not in meta.phases", phase)})
		}
	}

	// gates: valid checks and tiers
	// Gate rule phases can reference any known phase (cross-stage artifact checks),
	// not just this spec's phases.
	for i, g := range spec.Gates.Entry {
		errs = append(errs, validateGateRule(fmt.Sprintf("gates.entry[%d]", i), g, phaseSet)...)
	}
	for i, g := range spec.Gates.Exit {
		errs = append(errs, validateGateRule(fmt.Sprintf("gates.exit[%d]", i), g, phaseSet)...)
	}

	// budget
	if spec.Budget.Allocation < 0 || spec.Budget.Allocation > 1.0 {
		errs = append(errs, ValidationError{"budget.allocation", fmt.Sprintf("must be 0.0-1.0, got %f", spec.Budget.Allocation)})
	}
	if spec.Budget.WarnThreshold < 0 || spec.Budget.WarnThreshold > 1.0 {
		errs = append(errs, ValidationError{"budget.warn_threshold", fmt.Sprintf("must be 0.0-1.0, got %f", spec.Budget.WarnThreshold)})
	}

	// capabilities
	for agentType, caps := range spec.Capabilities {
		for _, names := range []struct {
			field string
			vals  []string
		}{
			{"kernel", caps.Kernel},
			{"filesystem", caps.Filesystem},
			{"dispatch", caps.Dispatch},
		} {
			for j, name := range names.vals {
				if !capabilityPattern.MatchString(name) {
					errs = append(errs, ValidationError{
						fmt.Sprintf("capabilities.%s.%s[%d]", agentType, names.field, j),
						fmt.Sprintf("capability %q must be dot-separated lowercase (e.g. events.tail)", name),
					})
				}
			}
		}
	}

	return errs
}

func validateGateRule(prefix string, g GateRule, knownPhases map[string]bool) []ValidationError {
	var errs []ValidationError
	if g.Check == "" {
		errs = append(errs, ValidationError{prefix + ".check", "required"})
	} else if !KnownChecks[g.Check] {
		known := make([]string, 0, len(KnownChecks))
		for k := range KnownChecks {
			known = append(known, k)
		}
		errs = append(errs, ValidationError{prefix + ".check", fmt.Sprintf("unknown check %q; known: %s", g.Check, strings.Join(known, ", "))})
	}
	if g.Phase != "" && !knownPhases[g.Phase] {
		errs = append(errs, ValidationError{prefix + ".phase", fmt.Sprintf("phase %q is not a known kernel phase", g.Phase)})
	}
	if g.Tier != "hard" && g.Tier != "soft" {
		errs = append(errs, ValidationError{prefix + ".tier", fmt.Sprintf("must be \"hard\" or \"soft\", got %q", g.Tier)})
	}
	return errs
}
