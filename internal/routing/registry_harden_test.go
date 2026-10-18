package routing

import (
	"strings"
	"testing"
	"time"
)

func regWith(key, horizon string, prov Provenance) *Registry {
	return &Registry{
		Version:          1,
		StalenessHorizon: horizon,
		Deployments: map[string]Deployment{key: {
			VersionStability: StabilityPinned,
			TrustZone:        ZoneLocal,
			Cost:             Cost{Type: CostCapacity},
			Capabilities:     map[string]CapabilityValue{"discernment": {Value: 0.5, Provenance: prov}},
		}},
	}
}

func TestRegistry_ValidateRejectsBadDeploymentKeys(t *testing.T) {
	for _, key := range []string{"", "a/b@", "a/@c", "/b@c", "@c", "a/b", "a@b", "a/b@c d", "a/b/c@d"} {
		err := regWith(key, "90d", ProvJudgment).Validate()
		if err == nil || !strings.Contains(err.Error(), "deployment key") {
			t.Errorf("key %q: err=%v, want a deployment-key error", key, err)
		}
	}
	if err := regWith("anthropic/claude-fable-5@api", "90d", ProvJudgment).Validate(); err != nil {
		t.Errorf("valid key rejected: %v", err)
	}
}

func TestRegistry_ValidateRejectsUnknownProvenance(t *testing.T) {
	err := regWith("a/b@c", "90d", Provenance("vibes")).Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown provenance") {
		t.Fatalf("err=%v, want unknown provenance", err)
	}
}

func TestStalenessAdjusted_UnrecognisedProvenanceIsUnknown(t *testing.T) {
	r := regWith("a/b@c", "90d", ProvJudgment)
	v := CapabilityValue{Value: 1, Provenance: Provenance("vibes"), VerifiedAt: "2026-07-13"}
	if got := r.StalenessAdjusted(Deployment{}, v, time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)); got != ProvUnknown {
		t.Errorf("got %q, want unknown", got)
	}
}

func TestParseHorizonStrict(t *testing.T) {
	good := map[string]time.Duration{"90d": 90 * 24 * time.Hour, "1d": 24 * time.Hour, "12h": 12 * time.Hour}
	for s, want := range good {
		if got, err := parseHorizon(s); err != nil || got != want {
			t.Errorf("%q: got %v err=%v, want %v", s, got, err, want)
		}
	}
	for _, s := range []string{
		"", "90junkd", "0d", "-5d", "d", "9d9d", "1.5d", "+3d", " 3d",
		"99999999999999999999d", "9999999999d", "0s", "-1h",
	} {
		if got, err := parseHorizon(s); err == nil {
			t.Errorf("%q: accepted as %v, want an error", s, got)
		}
	}
}

func TestRegistry_ValidateRejectsBadHorizon(t *testing.T) {
	for _, h := range []string{"90junkd", "0d", "-1h"} {
		if err := regWith("a/b@c", h, ProvJudgment).Validate(); err == nil {
			t.Errorf("horizon %q accepted", h)
		}
	}
}
