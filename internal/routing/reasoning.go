package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// ReasoningPolicy belongs to the selected Clavain installation. The kernel
// enforces declared judgments; it does not infer uncertainty from domain names.
type ReasoningPolicy struct {
	FrontierModels    []string                 `yaml:"frontier_models"`
	FrontierReasons   []string                 `yaml:"frontier_reasons"`
	DualReviewReasons []string                 `yaml:"dual_review_reasons"`
	Strikes           int                      `yaml:"strikes"`
	Profiles          map[string]PolicyProfile `yaml:"profiles"`
}
type PolicyProfile struct {
	Scope string            `yaml:"scope"`
	Roles map[string]string `yaml:"roles"`
}
type ReasoningHandoff struct {
	Decisions    string `json:"decisions"`
	Constraints  string `json:"constraints"`
	Verification string `json:"verification"`
	Escalation   string `json:"escalation"`
}

func (h *ReasoningHandoff) Ready() bool {
	return h != nil && strings.TrimSpace(h.Decisions) != "" && strings.TrimSpace(h.Constraints) != "" && strings.TrimSpace(h.Verification) != "" && strings.TrimSpace(h.Escalation) != ""
}

type DecisionContext struct {
	Reasons             []string          `json:"reasons"`
	Rationale           string            `json:"rationale"`
	Domain              string            `json:"domain,omitempty"`
	Scope               string            `json:"scope,omitempty"`
	InvestigationActive bool              `json:"investigation_active,omitempty"`
	Handoff             *ReasoningHandoff `json:"handoff,omitempty"`
	// nil means access has not been probed; an empty array means no access.
	AvailableModels []string `json:"available_models,omitempty"`
}
type ReasoningDecision struct {
	ResolvedDispatch
	PolicySource          string          `json:"policy_source"`
	PolicyHash            string          `json:"policy_hash"`
	PolicyProfile         string          `json:"policy_profile"`
	ClassificationReasons []string        `json:"classification_reasons"`
	ReviewRequirement     string          `json:"review_requirement"`
	FrontierRequired      bool            `json:"frontier_required"`
	Context               DecisionContext `json:"decision_context"`
}

// ResolvePolicyPath uses an explicitly selected installation before legacy CWD
// discovery. Invalid explicit selections fail closed, including dangling links.
func ResolvePolicyPath(explicit string) (string, error) {
	selected := explicit
	if selected == "" {
		selected = os.Getenv("CLAVAIN_ROUTING_POLICY")
	}
	if selected == "" && os.Getenv("CLAVAIN_ROOT") != "" {
		selected = filepath.Join(os.Getenv("CLAVAIN_ROOT"), "config", "routing.yaml")
	}
	if selected == "" && os.Getenv("CLAUDE_PLUGIN_ROOT") != "" {
		p := filepath.Join(os.Getenv("CLAUDE_PLUGIN_ROOT"), "config", "routing.yaml")
		if _, err := os.Stat(p); err == nil {
			selected = p
		}
	}
	check := func(p string) (string, error) {
		// Follow installation links before cleaning '..'. Shared instructions may
		// select ~/.agents/skills/clavain/../config/routing.yaml.
		p, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		p, err = filepath.Abs(p)
		if err != nil {
			return "", err
		}
		st, err := os.Stat(p)
		if err != nil {
			return "", err
		}
		if !st.Mode().IsRegular() {
			return "", fmt.Errorf("policy is not a regular file: %s", p)
		}
		return p, nil
	}
	if selected != "" {
		return check(selected)
	}
	// The managed skill link identifies one installation, never a newest-cache guess.
	if home, err := os.UserHomeDir(); err == nil {
		skill := filepath.Join(home, ".agents", "skills", "clavain", "using-clavain", "SKILL.md")
		if actual, err := filepath.EvalSymlinks(skill); err == nil {
			p := filepath.Join(filepath.Dir(actual), "..", "..", "config", "routing.yaml")
			if _, err := os.Stat(p); err == nil {
				return check(p)
			}
		}
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		for _, rel := range []string{"config/routing.yaml", "os/Clavain/config/routing.yaml", "os/clavain/config/routing.yaml"} {
			if p, err := check(filepath.Join(dir, rel)); err == nil {
				return p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("routing policy unavailable: select --policy, CLAVAIN_ROUTING_POLICY or CLAVAIN_ROOT")
}

func (r *Resolver) ResolveDecision(role, producer, policyProfile string, c DecisionContext) (ReasoningDecision, error) {
	d := ReasoningDecision{PolicySource: r.cfg.PolicySource, PolicyHash: r.cfg.PolicyHash, PolicyProfile: policyProfile, ClassificationReasons: c.Reasons, Context: c, ReviewRequirement: "existing-gates"}
	if d.PolicyProfile == "" {
		d.PolicyProfile = "default"
	}
	if d.ClassificationReasons == nil {
		d.ClassificationReasons = []string{}
	}
	policy := r.cfg.Reasoning
	if len(c.Reasons) > 0 && strings.TrimSpace(c.Rationale) == "" {
		return d, fmt.Errorf("classification requires rationale")
	}
	for _, reason := range c.Reasons {
		if !slices.Contains(policy.FrontierReasons, reason) {
			return d, fmt.Errorf("unknown classification reason %q", reason)
		}
		if slices.Contains(policy.DualReviewReasons, reason) {
			d.ReviewRequirement = "other-frontier"
		}
	}
	cfg := *r.cfg
	cfg.Dispatch = r.cfg.Dispatch
	cfg.Dispatch.Roles = make(map[string]string)
	for k, v := range r.cfg.Dispatch.Roles {
		cfg.Dispatch.Roles[k] = v
	}
	if d.PolicyProfile != "default" {
		p, ok := policy.Profiles[d.PolicyProfile]
		if !ok {
			return d, fmt.Errorf("unknown policy profile %q", d.PolicyProfile)
		}
		if p.Scope != "" && p.Scope != c.Scope {
			return d, fmt.Errorf("policy profile %q requires scope %q", d.PolicyProfile, p.Scope)
		}
		for k, v := range p.Roles {
			cfg.Dispatch.Roles[k] = v
		}
	}
	selectedRole := role
	elevated := len(c.Reasons) > 0 || c.InvestigationActive
	switch role {
	case "planning":
		if elevated {
			selectedRole = "frontier-planning"
			d.FrontierRequired = true
		}
	case "frontier-planning", "escalation":
		d.FrontierRequired = true
	case "plan-review":
		d.FrontierRequired = d.ReviewRequirement == "other-frontier"
	case "routine-execution", "deep-execution":
		if elevated && (!c.Handoff.Ready() || c.InvestigationActive) {
			selectedRole = "deep-execution"
			d.FrontierRequired = true
		}
	}
	if c.InvestigationActive && len(c.Reasons) == 0 {
		return d, fmt.Errorf("active investigation requires classification reasons")
	}
	rr := NewResolver(&cfg)
	resolved, err := rr.ResolveDispatchRoleForProducer(selectedRole, producer)
	d.ResolvedDispatch = resolved
	d.RequestedRole = role
	if err != nil {
		return d, err
	}
	candidates := append([]DispatchCandidate{{ProfileRef: resolved.ProfileRef, Profile: resolved.Profile}}, resolved.FallbackChain...)
	available := map[string]bool{}
	for _, m := range c.AvailableModels {
		id, e := rr.CanonicalModelIdentity(m)
		if e != nil {
			return d, e
		}
		available[id] = true
	}
	frontier := map[string]bool{}
	for _, m := range policy.FrontierModels {
		id, e := rr.CanonicalModelIdentity(m)
		if e != nil {
			return d, e
		}
		frontier[id] = true
	}
	eligible := []DispatchCandidate{}
	for _, candidate := range candidates {
		reason := ""
		if d.FrontierRequired && !frontier[candidate.Profile.ModelIdentity] {
			reason = "frontier_required"
		}
		if reason == "" && c.AvailableModels != nil && !available[candidate.Profile.ModelIdentity] {
			reason = "model_unavailable"
		}
		if reason != "" {
			d.Excluded = append(d.Excluded, ExcludedDispatchCandidate{candidate, reason})
			continue
		}
		if candidate.Profile.Backend == "" || candidate.Profile.Model == "" || candidate.Profile.ReasoningEffort == "" {
			return d, fmt.Errorf("incomplete profile %q", candidate.ProfileRef)
		}
		eligible = append(eligible, candidate)
	}
	if len(eligible) == 0 {
		return d, fmt.Errorf("role %q: no eligible model satisfies reasoning contract", role)
	}
	d.ProfileRef, d.Profile = eligible[0].ProfileRef, eligible[0].Profile
	d.FallbackChain = eligible[1:]
	if d.ProfileRef != resolved.ProfileRef && d.FallbackReason == "" {
		d.FallbackReason = "reasoning_contract"
	}
	return d, nil
}
