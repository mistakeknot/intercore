package phase

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/runtrack"
	"github.com/mistakeknot/intercore/pkg/runtimeproof"
)

func runtimeDigest(b []byte) string {
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

type runtimeGateFixture struct {
	store       *Store
	rt          *runtrack.Store
	ctx         context.Context
	runID       string
	root        string
	buildPath   string
	installPath string
	expect      runtimeproof.Expectations
	env         runtimeproof.Environment
}

func setupRuntimeGateFixture(t *testing.T, perRunRules map[string][]SpecGateRule) *runtimeGateFixture {
	t.Helper()
	store, rt, _, ctx := setupMachineTest(t)
	root := t.TempDir()
	buildPath := filepath.Join(root, "build", "server")
	installPath := filepath.Join(root, "bin", "server")
	for _, path := range []string{buildPath, installPath} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("runtime-binary"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	expect := runtimeproof.Expectations{
		ExpectedBuildPath:     buildPath,
		ExpectedInstalledPath: installPath,
		RequiredSubsystems:    []string{"store"},
		NotApplicableFailureClasses: map[string]bool{
			"dependency_injection": true,
			"projection_catchup":   true,
		},
		RequiredAssertions: []string{"state-delta"},
		ExpectedSurfaces:   []string{"health", "event"},
		RequiredResources: []runtimeproof.ResourceExpectation{{
			Kind: "port", Ownership: "ephemeral",
		}},
	}
	metadataBytes, err := json.Marshal(map[string]any{
		"close_gate": map[string]any{
			"requirements":         []string{"runtime-evidence/v1"},
			"bead_id":              "sylveste-6h7x",
			"runtime_expectations": expect,
			"config_digest":        runtimeDigest([]byte("config")),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata := string(metadataBytes)
	runID, err := store.Create(ctx, &Run{
		ProjectDir:  root,
		Goal:        "runtime gate",
		Complexity:  3,
		AutoAdvance: true,
		Phases:      []string{PhaseReflect, PhaseDone},
		Metadata:    &metadata,
		GateRules:   perRunRules,
	})
	if err != nil {
		t.Fatal(err)
	}
	reflection := filepath.Join(root, "reflection.md")
	if err := os.WriteFile(reflection, []byte("reflection"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.AddArtifact(ctx, &runtrack.Artifact{
		RunID: runID, Phase: PhaseReflect, Path: reflection, Type: "reflection",
	}); err != nil {
		t.Fatal(err)
	}
	return &runtimeGateFixture{
		store: store, rt: rt, ctx: ctx, runID: runID, root: root,
		buildPath: buildPath, installPath: installPath, expect: expect,
		env: runtimeproof.Environment{
			Now:      func() time.Time { return time.Now().UTC().Add(time.Minute) },
			Hostname: func() (string, error) { return "test-host", nil },
			GitHead: func(context.Context, string) (string, error) {
				return strings.Repeat("a", 40), nil
			},
		},
	}
}

func (f *runtimeGateFixture) addReceipt(t *testing.T, mutate func(*runtimeproof.Receipt)) string {
	t.Helper()
	run, err := f.store.Get(f.ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(run.CreatedAt, 0).UTC().Add(time.Second)
	created := started.Add(time.Second)
	receipt := runtimeproof.Receipt{
		SchemaVersion: runtimeproof.SchemaVersion,
		Subject: runtimeproof.Subject{
			BeadID: "sylveste-6h7x", RunID: f.runID, ProjectRoot: f.root,
			GitHead: strings.Repeat("a", 40), Host: "test-host", CreatedAt: created.Format(time.RFC3339Nano),
		},
		Artifact: runtimeproof.Artifact{
			Kind: "file", BuildPath: f.buildPath, InstalledPath: f.installPath,
			BuildDigest: runtimeDigest([]byte("runtime-binary")), InstalledDigest: runtimeDigest([]byte("runtime-binary")),
			RuntimeDigest: runtimeDigest([]byte("runtime-binary")),
		},
		Boot: runtimeproof.Boot{
			StartedForProbe: true, ProcessID: 4242, StartedAt: started.Format(time.RFC3339Nano),
			InstanceNonce: "nonce", ObservedNonce: "nonce", State: runtimeproof.StateVerified,
		},
		Health: runtimeproof.Health{
			RequiredSubsystems: []string{"store"}, Observed: map[string]string{"store": "healthy"},
			FailureClasses: map[string]runtimeproof.State{
				"startup": runtimeproof.StateVerified, "dependency_injection": runtimeproof.StateNotApplicable,
				"connection": runtimeproof.StateVerified, "projection_catchup": runtimeproof.StateNotApplicable,
			},
		},
		Event: runtimeproof.Event{
			EventID: "event", ObservedEventID: "event", BeforeDigest: runtimeDigest([]byte("before")),
			AfterDigest: runtimeDigest([]byte("after")),
			Assertions:  []runtimeproof.Assertion{{Name: "state-delta", State: runtimeproof.StateVerified, Evidence: "changed"}},
		},
		SurfaceScan: runtimeproof.SurfaceScan{
			Expected: []string{"health", "event"}, Observed: []string{"event", "health"},
			Missing: []string{}, Unexpected: []string{},
		},
		Isolation: runtimeproof.Isolation{
			Resources:  []runtimeproof.Resource{{Kind: "port", Fingerprint: runtimeDigest([]byte("port")), Ownership: "ephemeral"}},
			Collisions: []string{},
		},
		Cleanup: runtimeproof.Cleanup{OwnedResourcesRemaining: []string{}},
	}
	if mutate != nil {
		mutate(&receipt)
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.root, fmt.Sprintf("receipt-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.rt.AddArtifact(f.ctx, &runtrack.Artifact{
		RunID: f.runID, Phase: PhaseReflect, Path: path, Type: RuntimeEvidenceArtifactType,
	}); err != nil {
		t.Fatal(err)
	}
	return path
}

func (f *runtimeGateFixture) gateConfig() GateConfig {
	return GateConfig{Priority: 0, RuntimeProofEnvironment: f.env}
}

func assertRuntimeBlocked(t *testing.T, f *runtimeGateFixture, cfg GateConfig) {
	t.Helper()
	result, err := Advance(f.ctx, f.store, f.runID, cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("Advance: %v", err)
	}
	if result.Advanced || result.EventType != EventBlock || result.GateTier != TierHard {
		t.Fatalf("result = %+v, want hard block", result)
	}
	if !strings.Contains(result.Reason, CheckRuntimeEvidence) {
		t.Fatalf("block reason lacks runtime condition: %s", result.Reason)
	}
	run, _ := f.store.Get(f.ctx, f.runID)
	if run.Phase != PhaseReflect || run.Status != StatusActive {
		t.Fatalf("run mutated on block: phase=%s status=%s", run.Phase, run.Status)
	}
}

func TestRuntimeGateMissingReceiptCannotBeBypassed(t *testing.T) {
	tests := []struct {
		name string
		cfg  func(*runtimeGateFixture) GateConfig
	}{
		{"normal", func(f *runtimeGateFixture) GateConfig { return f.gateConfig() }},
		{"priority disabled", func(f *runtimeGateFixture) GateConfig {
			cfg := f.gateConfig()
			cfg.Priority = 4
			return cfg
		}},
		{"disable all", func(f *runtimeGateFixture) GateConfig {
			cfg := f.gateConfig()
			cfg.DisableAll = true
			return cfg
		}},
		{"skip reason", func(f *runtimeGateFixture) GateConfig {
			cfg := f.gateConfig()
			cfg.Priority = 4
			cfg.SkipReason = "force it"
			return cfg
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := setupRuntimeGateFixture(t, nil)
			assertRuntimeBlocked(t, fixture, tt.cfg(fixture))
		})
	}
}

func TestRuntimeGateInjectionIgnoresEmptyPerRunRules(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, map[string][]SpecGateRule{PhaseReflect + "→" + PhaseDone: {}})
	assertRuntimeBlocked(t, fixture, fixture.gateConfig())
}

func TestRuntimeGateValidNewestReceiptAdvancesAtPriorityFour(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, nil)
	fixture.addReceipt(t, nil)
	cfg := fixture.gateConfig()
	cfg.Priority = 4
	result, err := Advance(fixture.ctx, fixture.store, fixture.runID, cfg, nil, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced || result.ToPhase != PhaseDone || result.GateTier != TierHard {
		t.Fatalf("result = %+v, want hard verified advance", result)
	}
	run, _ := fixture.store.Get(fixture.ctx, fixture.runID)
	if run.Status != StatusCompleted {
		t.Fatalf("status = %s, want completed", run.Status)
	}
}

func TestRuntimeGateDisableAllRejectedEvenWithValidReceipt(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, nil)
	fixture.addReceipt(t, nil)
	cfg := fixture.gateConfig()
	cfg.DisableAll = true
	assertRuntimeBlocked(t, fixture, cfg)
	events, err := fixture.store.Events(fixture.ctx, fixture.runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Reason == nil || !strings.Contains(*events[len(events)-1].Reason, "cannot be disabled") {
		t.Fatalf("disable bypass reason = %+v", events)
	}
}

func TestRuntimeGateDoesNotFallBackFromNewestInvalidReceipt(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, nil)
	fixture.addReceipt(t, nil)
	fixture.addReceipt(t, func(r *runtimeproof.Receipt) { r.Event.AfterDigest = r.Event.BeforeDigest })
	assertRuntimeBlocked(t, fixture, fixture.gateConfig())
}

func TestRuntimeGateMissingArtifactHashBlocks(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, nil)
	if _, err := fixture.rt.AddArtifact(fixture.ctx, &runtrack.Artifact{
		RunID: fixture.runID, Phase: PhaseReflect, Path: filepath.Join(fixture.root, "missing.json"), Type: RuntimeEvidenceArtifactType,
	}); err != nil {
		t.Fatal(err)
	}
	assertRuntimeBlocked(t, fixture, fixture.gateConfig())
}

func TestRuntimeGateDryRunReportsSameCondition(t *testing.T) {
	fixture := setupRuntimeGateFixture(t, nil)
	result, err := EvaluateGate(fixture.ctx, fixture.store, fixture.runID, fixture.gateConfig(), fixture.rt, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.Result != GateFail || result.Tier != TierHard || result.Evidence == nil {
		t.Fatalf("dry-run result = %+v", result)
	}
	found := false
	for _, condition := range result.Evidence.Conditions {
		found = found || condition.Check == CheckRuntimeEvidence
	}
	if !found {
		t.Fatalf("runtime condition missing: %+v", result.Evidence)
	}
}

func TestOrdinaryReflectRunRetainsExistingBehavior(t *testing.T) {
	store, rt, _, ctx := setupMachineTest(t)
	root := t.TempDir()
	runID, err := store.Create(ctx, &Run{
		ProjectDir: root, Goal: "ordinary", AutoAdvance: true, Phases: []string{PhaseReflect, PhaseDone},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "reflection.md")
	if err := os.WriteFile(path, []byte("reflection"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.AddArtifact(ctx, &runtrack.Artifact{RunID: runID, Phase: PhaseReflect, Path: path, Type: "reflection"}); err != nil {
		t.Fatal(err)
	}
	result, err := Advance(ctx, store, runID, GateConfig{Priority: 0}, rt, nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Advanced {
		t.Fatalf("ordinary run blocked: %+v", result)
	}
}
