package routing

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strconv"
	"time"

	"gopkg.in/yaml.v3"
)

// This file implements the deployment-keyed capability registry from the
// routing-contract spec (docs/specs/routing-contract.md §2). It is the
// vector-valued successor to the ModelTier scalar in routing.go — models are
// keyed by vendor/model@deployment and carry a capability vector, typed cost,
// trust zone, and version-stability, rather than a single ordinal tier.
//
// v1 schema decisions locked in the spec interview (2026-07-14):
//   - Deployment-keyed unit: vendor/model@deployment (Q-2.5 seed = real only).
//   - Capability values carry provenance (benchmark|judgment|unknown) + verified_at.
//   - Cost is typed three ways (Q-2.2): per-token | subscription-quota | capacity.
//   - Per-deployment version_stability (fd-arch F3): pinned | vendor-live.

// Provenance tags how a capability value was established. A stale value is
// treated one rank more conservative than its tag (see StalenessAdjusted).
type Provenance string

const (
	ProvBenchmark Provenance = "benchmark" // measured against a named eval suite
	ProvJudgment  Provenance = "judgment"  // human-supplied estimate
	ProvUnknown   Provenance = "unknown"   // no value; router treats conservatively
)

// provenanceRank orders provenance from most to least authoritative, so a
// stale value can be downgraded one step (benchmark -> judgment -> unknown).
var provenanceRank = map[Provenance]int{
	ProvBenchmark: 2,
	ProvJudgment:  1,
	ProvUnknown:   0,
}

func rankToProvenance(r int) Provenance {
	switch {
	case r >= 2:
		return ProvBenchmark
	case r == 1:
		return ProvJudgment
	default:
		return ProvUnknown
	}
}

// TrustZone is where a deployment runs, which governs what data may reach it.
type TrustZone string

const (
	ZoneLocal          TrustZone = "local"           // self-hosted on our hardware
	ZoneApprovedVendor TrustZone = "approved-vendor" // contractually cleared vendor cloud
	ZoneVendorCloud    TrustZone = "vendor-cloud"    // general vendor API
)

// CostType selects how a deployment is priced (spec Q-2.2). The decision
// function must switch on this, never assume per-token.
type CostType string

const (
	CostPerToken     CostType = "per-token"          // in/out $ per MTok
	CostSubscription CostType = "subscription-quota" // flat seat, quota-limited
	CostCapacity     CostType = "capacity"           // self-hosted, marginal ~0, capacity-bound
)

// VersionStability captures whether the weights behind a deployment key can
// change without our action (fd-arch F3). vendor-live deployments age faster
// and are weighted more staleness-prone.
type VersionStability string

const (
	StabilityPinned     VersionStability = "pinned"      // changes only on our redeploy
	StabilityVendorLive VersionStability = "vendor-live" // vendor may reroll weights silently
)

// CapabilityValue is one axis of a deployment's capability vector, with the
// provenance metadata the staleness machinery needs.
type CapabilityValue struct {
	Value      float64    `yaml:"value"`
	Provenance Provenance `yaml:"provenance"`
	VerifiedAt string     `yaml:"verified_at"` // YYYY-MM-DD
	JudgedBy   string     `yaml:"judged_by,omitempty"`
	Rationale  string     `yaml:"rationale,omitempty"`
}

// Cost is a deployment's typed cost. Fields other than Type are interpreted
// per Type; a per-token deployment uses In/Out, capacity uses neither.
type Cost struct {
	Type CostType `yaml:"type"`
	In   float64  `yaml:"in,omitempty"`  // $ per MTok input (per-token)
	Out  float64  `yaml:"out,omitempty"` // $ per MTok output (per-token)
}

// Deployment is one vendor/model@deployment entry in the registry.
type Deployment struct {
	VersionStability VersionStability           `yaml:"version_stability"`
	TrustZone        TrustZone                  `yaml:"trust_zone"`
	Cost             Cost                       `yaml:"cost"`
	EffortMap        map[int]map[string]any     `yaml:"effort_map,omitempty"`
	Capabilities     map[string]CapabilityValue `yaml:"capabilities"`
}

// Registry is the whole model registry (spec §2).
type Registry struct {
	Version          int                   `yaml:"version"`
	StalenessHorizon string                `yaml:"staleness_horizon"` // e.g. "90d"
	Deployments      map[string]Deployment `yaml:"deployments"`
}

// LoadRegistry reads and validates a registry YAML file.
func LoadRegistry(path string) (*Registry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read registry: %w", err)
	}
	var reg Registry
	if err := yaml.Unmarshal(data, &reg); err != nil {
		return nil, fmt.Errorf("parse registry: %w", err)
	}
	if reg.Deployments == nil {
		reg.Deployments = map[string]Deployment{}
	}
	if err := reg.Validate(); err != nil {
		return nil, err
	}
	return &reg, nil
}

// deploymentKeyRE is the identity of a deployment: vendor/model@deployment,
// each part non-empty.
var deploymentKeyRE = regexp.MustCompile(`^[^/@\s]+/[^/@\s]+@[^/@\s]+$`)

// Validate checks structural invariants the router relies on.
func (r *Registry) Validate() error {
	if r.Version < 1 {
		return fmt.Errorf("registry version must be >= 1, got %d", r.Version)
	}
	if r.StalenessHorizon != "" {
		if _, err := parseHorizon(r.StalenessHorizon); err != nil {
			return fmt.Errorf("staleness_horizon: %w", err)
		}
	}
	for key, dep := range r.Deployments {
		if !deploymentKeyRE.MatchString(key) {
			return fmt.Errorf("deployment key %q: want vendor/model@deployment with no empty part", key)
		}
		for cname, cv := range dep.Capabilities {
			if cv.Provenance == "" {
				continue
			}
			if _, ok := provenanceRank[cv.Provenance]; !ok {
				return fmt.Errorf("deployment %q: capability %q: unknown provenance %q", key, cname, cv.Provenance)
			}
		}
		if dep.TrustZone == "" {
			return fmt.Errorf("deployment %q: trust_zone is required", key)
		}
		switch dep.TrustZone {
		case ZoneLocal, ZoneApprovedVendor, ZoneVendorCloud:
		default:
			return fmt.Errorf("deployment %q: unknown trust_zone %q", key, dep.TrustZone)
		}
		if dep.Cost.Type == "" {
			return fmt.Errorf("deployment %q: cost.type is required", key)
		}
		switch dep.Cost.Type {
		case CostPerToken, CostSubscription, CostCapacity:
		default:
			return fmt.Errorf("deployment %q: unknown cost.type %q", key, dep.Cost.Type)
		}
		if dep.VersionStability == "" {
			return fmt.Errorf("deployment %q: version_stability is required", key)
		}
		switch dep.VersionStability {
		case StabilityPinned, StabilityVendorLive:
		default:
			return fmt.Errorf("deployment %q: unknown version_stability %q", key, dep.VersionStability)
		}
	}
	return nil
}

// StalenessAdjusted returns the effective provenance of a capability value as
// of `now`, downgrading one rank if the value is older than the horizon.
// vendor-live deployments use half the horizon (they age twice as fast).
// A value with no VerifiedAt, or an unparseable one, is treated as unknown.
func (r *Registry) StalenessAdjusted(dep Deployment, v CapabilityValue, now time.Time) Provenance {
	if _, ok := provenanceRank[v.Provenance]; !ok || v.Provenance == ProvUnknown {
		return ProvUnknown // unrecognised or missing provenance is never trusted
	}
	horizon, err := parseHorizon(r.StalenessHorizon)
	if err != nil || v.VerifiedAt == "" {
		return ProvUnknown
	}
	verified, perr := time.Parse("2006-01-02", v.VerifiedAt)
	if perr != nil {
		return ProvUnknown
	}
	if dep.VersionStability == StabilityVendorLive {
		horizon /= 2
	}
	if now.Sub(verified) > horizon {
		return rankToProvenance(provenanceRank[v.Provenance] - 1)
	}
	return v.Provenance
}

// parseHorizon parses a "90d" / "12h" style duration (days supported beyond
// the stdlib, which stops at hours).
func parseHorizon(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("empty horizon")
	}
	if n := len(s); n > 1 && s[n-1] == 'd' {
		digits := s[:n-1]
		for _, r := range digits {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("bad day horizon %q: want digits then d", s)
			}
		}
		days, err := strconv.Atoi(digits)
		if err != nil {
			return 0, fmt.Errorf("bad day horizon %q: %w", s, err)
		}
		if days <= 0 || days > maxHorizonDays {
			return 0, fmt.Errorf("bad day horizon %q: must be 1..%d days", s, maxHorizonDays)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, err
	}
	if d <= 0 {
		return 0, fmt.Errorf("bad horizon %q: must be positive", s)
	}
	return d, nil
}

// maxHorizonDays keeps days*24h inside time.Duration.
const maxHorizonDays = int(math.MaxInt64 / int64(24*time.Hour))

// KnownDataLabels are the data-sensitivity labels a task may carry. A label
// outside this set could never match a constraint, so a misspelling of
// client-confidential would silently drop the trust-zone rule; callers reject
// anything not listed here.
var KnownDataLabels = map[string]bool{
	"public":              true,
	"internal":            true,
	"client-confidential": true,
}
