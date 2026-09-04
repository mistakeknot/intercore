package routing

import (
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

// clientConfidentialConstraint is the DoD's first clause as a rule:
// client-confidential data may route only to local or approved-vendor zones.
func clientConfidentialConstraint() []Constraint {
	return []Constraint{{
		Match:            map[string]string{"data": "client-confidential"},
		RequireTrustZone: []TrustZone{ZoneLocal, ZoneApprovedVendor},
	}}
}

// TestDoD_ClientConfidentialRoutesLocalOnly is the executable statement of the
// goal's first DoD clause: a client-confidential task routes ONLY to a
// local/approved-vendor deployment and is BLOCKED otherwise.
func TestDoD_ClientConfidentialRoutesLocalOnly(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join("testdata", "registry-seed.yaml"))
	if err != nil {
		t.Fatalf("load seed registry: %v", err)
	}
	td := TaskDescriptor{
		Role:   "researcher",
		Class:  "client-confidential-synthesis",
		Fields: map[string]string{"data": "client-confidential"},
	}

	eligible := reg.EligibleDeployments(clientConfidentialConstraint(), td)
	sort.Strings(eligible)

	// Only the local zklw deployment is eligible; both vendor-cloud APIs blocked.
	want := []string{"nousresearch/hermes-4@zklw"}
	if len(eligible) != len(want) || (len(eligible) == 1 && eligible[0] != want[0]) {
		t.Fatalf("client-confidential eligible = %v, want %v (vendor-cloud must be blocked)", eligible, want)
	}

	// And the two vendor-cloud deployments each produce a typed hard block.
	for _, zone := range []TrustZone{ZoneVendorCloud} {
		err := CheckConstraints(clientConfidentialConstraint(), td, zone)
		var ce *ConstraintError
		if !errors.As(err, &ce) {
			t.Fatalf("zone %q: expected *ConstraintError, got %v", zone, err)
		}
		if ce.Value != "client-confidential" {
			t.Errorf("constraint error value = %q, want client-confidential", ce.Value)
		}
	}
}

func TestConstraints_NonConfidentialUnconstrained(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join("testdata", "registry-seed.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A task with no data-sensitivity field: no constraint applies, all eligible.
	td := TaskDescriptor{Role: "executor", Class: "terminal-grind", Fields: map[string]string{}}
	eligible := reg.EligibleDeployments(clientConfidentialConstraint(), td)
	if len(eligible) != len(reg.Deployments) {
		t.Errorf("unconstrained task eligible = %d, want all %d", len(eligible), len(reg.Deployments))
	}
}

func TestConstraints_AllMatchSemantics(t *testing.T) {
	// A two-field constraint applies only when BOTH match (Q-1.4 all-match).
	c := []Constraint{{
		Match:            map[string]string{"data": "confidential", "region": "eu"},
		RequireTrustZone: []TrustZone{ZoneLocal},
	}}
	// Only one field matches -> constraint does not apply -> vendor-cloud permitted.
	td := TaskDescriptor{Fields: map[string]string{"data": "confidential", "region": "us"}}
	if err := CheckConstraints(c, td, ZoneVendorCloud); err != nil {
		t.Errorf("partial match should not apply constraint, got block: %v", err)
	}
	// Both fields match -> constraint applies -> vendor-cloud blocked.
	td2 := TaskDescriptor{Fields: map[string]string{"data": "confidential", "region": "eu"}}
	if err := CheckConstraints(c, td2, ZoneVendorCloud); err == nil {
		t.Errorf("full match should block vendor-cloud, got permit")
	}
}

func TestRegistry_ValidateRejectsMissingFields(t *testing.T) {
	bad := &Registry{
		Version: 1,
		Deployments: map[string]Deployment{
			"x/y@z": {TrustZone: ZoneLocal, VersionStability: StabilityPinned}, // no cost.type
		},
	}
	if err := bad.Validate(); err == nil {
		t.Error("expected validation error for missing cost.type")
	}
}

func TestRegistry_StalenessDowngrade(t *testing.T) {
	reg := &Registry{Version: 1, StalenessHorizon: "90d"}
	fresh := CapabilityValue{Value: 0.9, Provenance: ProvBenchmark, VerifiedAt: "2026-07-01"}
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)

	// Pinned + within horizon: stays benchmark.
	pinned := Deployment{VersionStability: StabilityPinned}
	if got := reg.StalenessAdjusted(pinned, fresh, now); got != ProvBenchmark {
		t.Errorf("fresh pinned = %q, want benchmark", got)
	}
	// vendor-live halves the horizon (45d); 2026-05-01 is >45d but <90d, so a
	// pinned deployment keeps benchmark while vendor-live downgrades to judgment.
	older := CapabilityValue{Value: 0.9, Provenance: ProvBenchmark, VerifiedAt: "2026-05-20"}
	live := Deployment{VersionStability: StabilityVendorLive}
	if got := reg.StalenessAdjusted(pinned, older, now); got != ProvBenchmark {
		t.Errorf("older pinned (within 90d) = %q, want benchmark", got)
	}
	if got := reg.StalenessAdjusted(live, older, now); got != ProvJudgment {
		t.Errorf("older vendor-live (beyond 45d) = %q, want judgment (downgraded)", got)
	}
}
