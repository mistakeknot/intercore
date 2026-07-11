package phase

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mistakeknot/intercore/internal/db"
)

func TestCanonicalMetadataRequiresJSONObject(t *testing.T) {
	tests := []string{"", `[]`, `null`, `"text"`, `{`}
	for _, raw := range tests {
		if _, err := CanonicalMetadata(raw); !errors.Is(err, ErrInvalidMetadata) {
			t.Errorf("CanonicalMetadata(%q) error = %v, want ErrInvalidMetadata", raw, err)
		}
	}
	got, err := CanonicalMetadata(`{"z":1,"a":{"b":2}}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != `{"a":{"b":2},"z":1}` {
		t.Fatalf("canonical metadata = %s", got)
	}
}

func TestMetadataPreservesLargeIntegersAcrossMerge(t *testing.T) {
	canonical, err := CanonicalMetadata(`{"counter":9007199254740993}`)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != `{"counter":9007199254740993}` {
		t.Fatalf("canonical metadata rounded integer: %s", canonical)
	}
	merged, err := MergeMetadata(canonical, `{"other":true}`)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := parseMetadataObject(merged)
	if err != nil {
		t.Fatal(err)
	}
	if obj["counter"] != json.Number("9007199254740993") {
		t.Fatalf("merged metadata rounded integer: %#v", obj["counter"])
	}
}

func TestStoreCreateValidatesAndCanonicalizesMetadata(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()

	invalid := `{"close_gate":{"requirements":["runtime-evidence/v1"]}}`
	if _, err := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "bad", Complexity: 3, Metadata: &invalid}); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("Create invalid metadata error = %v, want ErrInvalidMetadata", err)
	}

	valid := `{"z":1,"close_gate":{"bead_id":"sylveste-6h7x","requirements":["runtime-evidence/v1"]}}`
	id, err := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "good", Complexity: 3, Metadata: &valid})
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"close_gate":{"bead_id":"sylveste-6h7x","requirements":["runtime-evidence/v1"]},"z":1}`
	if run.Metadata == nil || *run.Metadata != want {
		t.Fatalf("metadata = %v, want %s", run.Metadata, want)
	}
}

func TestStoreCreatePortfolioRejectsInvalidChildMetadataAtomically(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	invalid := `{"close_gate":{"requirements":["runtime-evidence/v1"]}}`
	_, _, err := store.CreatePortfolio(ctx,
		&Run{Goal: "portfolio", Complexity: 3},
		[]*Run{
			{ProjectDir: "/tmp/a", Goal: "a", Complexity: 3},
			{ProjectDir: "/tmp/b", Goal: "b", Complexity: 3, Metadata: &invalid},
		},
	)
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("CreatePortfolio error = %v, want ErrInvalidMetadata", err)
	}
	runs, listErr := store.List(ctx, nil)
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(runs) != 0 {
		t.Fatalf("partial portfolio created: %d runs", len(runs))
	}
}

func TestStoreCreateRejectsRuntimeGateWithoutDoneTerminal(t *testing.T) {
	store := setupTestStore(t)
	metadata := `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	_, err := store.Create(context.Background(), &Run{
		ProjectDir: "/tmp", Goal: "bad chain", Complexity: 3,
		Phases: []string{"reflect", "ship"}, Metadata: &metadata,
	})
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("Create error = %v, want ErrInvalidMetadata", err)
	}
}

func TestStoreCreateRejectsRuntimeGateWithoutRealTerminalTransition(t *testing.T) {
	store := setupTestStore(t)
	metadata := `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	for _, phases := range [][]string{{PhaseDone}, {PhaseReflect, PhaseReflect, PhaseDone}} {
		_, err := store.Create(context.Background(), &Run{
			ProjectDir: "/tmp", Goal: "bad chain", Complexity: 3, Phases: phases, Metadata: &metadata,
		})
		if !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("Create phases=%v error = %v, want ErrInvalidMetadata", phases, err)
		}
	}
}

func TestUpdateConfigurationRejectsBindingNonDoneChain(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "custom", Complexity: 3, Phases: []string{"reflect", "ship"},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = store.UpdateConfiguration(ctx, id, RunConfigUpdate{
		MetadataMerge: `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`,
	})
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("UpdateConfiguration error = %v, want ErrInvalidMetadata", err)
	}
	events, _ := store.Events(ctx, id)
	if len(events) != 0 {
		t.Fatalf("events written on rejected binding: %d", len(events))
	}
}

func sealedMetadata() string {
	return `{
		"close_gate": {
			"requirements": ["runtime-evidence/v1"],
			"bead_id": "sylveste-6h7x",
			"adoption": {"plan_digest": "sha256:plan", "source_heads": {"clavain": "abc"}},
			"runtime_expectations": {
				"expected_build_path": "/tmp/build/server",
				"expected_installed_path": "/tmp/bin/server",
				"required_subsystems": ["store"],
				"not_applicable_failure_classes": {"dependency_injection": true},
				"required_assertions": ["state-delta"],
				"expected_surfaces": ["health"],
				"required_resources": [{"kind":"port","ownership":"ephemeral"}]
			},
			"config_digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"other": {"original": true}
	}`
}

func TestMergeMetadataSealsRuntimeCloseGate(t *testing.T) {
	tests := []struct {
		name  string
		patch string
	}{
		{"remove requirement", `{"close_gate":{"requirements":[]}}`},
		{"change bead", `{"close_gate":{"bead_id":"other"}}`},
		{"remove close gate", `{"close_gate":null}`},
		{"rewrite adoption", `{"close_gate":{"adoption":{"plan_digest":"other"}}}`},
		{"rewrite expectations", `{"close_gate":{"runtime_expectations":{"required_subsystems":["other"]}}}`},
		{"rewrite config digest", `{"close_gate":{"config_digest":"sha256:other"}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := MergeMetadata(sealedMetadata(), tt.patch); !errors.Is(err, ErrSealedMetadata) {
				t.Fatalf("MergeMetadata error = %v, want ErrSealedMetadata", err)
			}
		})
	}
}

func TestMergeMetadataPreservesSealedFieldsAndAllowsAdditions(t *testing.T) {
	merged, err := MergeMetadata(sealedMetadata(), `{
		"close_gate":{"requirements":["runtime-evidence/v1","future-proof/v2"]},
		"other":{"new":42}
	}`)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := parseMetadataObject(merged)
	if err != nil {
		t.Fatal(err)
	}
	closeGate := obj["close_gate"].(map[string]any)
	if closeGate["bead_id"] != "sylveste-6h7x" {
		t.Fatalf("sealed bead changed: %#v", closeGate)
	}
	other := obj["other"].(map[string]any)
	if other["original"] != true || other["new"] != json.Number("42") {
		t.Fatalf("recursive merge lost data: %#v", other)
	}
}

func TestMergeMetadataRejectsNullAdoptionWithoutPoisoningFutureValue(t *testing.T) {
	base := `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`
	if _, err := MergeMetadata(base, `{"close_gate":{"adoption":null}}`); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("null adoption error = %v, want ErrInvalidMetadata", err)
	}
	merged, err := MergeMetadata(base, `{"close_gate":{"adoption":{"plan_digest":"sha256:plan"}}}`)
	if err != nil {
		t.Fatalf("valid adoption after rejected null: %v", err)
	}
	if !strings.Contains(merged, "plan_digest") {
		t.Fatalf("valid adoption missing: %s", merged)
	}
}

func TestUpdateConfigurationInvalidMergeIsAtomic(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	metadata := sealedMetadata()
	id, err := store.Create(ctx, &Run{ProjectDir: "/tmp", Goal: "test", Complexity: 3, AutoAdvance: true, Metadata: &metadata})
	if err != nil {
		t.Fatal(err)
	}
	complexity := 1
	err = store.UpdateConfiguration(ctx, id, RunConfigUpdate{
		Complexity:    &complexity,
		MetadataMerge: `{"close_gate":{"bead_id":"other"}}`,
	})
	if !errors.Is(err, ErrSealedMetadata) {
		t.Fatalf("UpdateConfiguration error = %v, want ErrSealedMetadata", err)
	}
	run, _ := store.Get(ctx, id)
	if run.Complexity != 3 {
		t.Fatalf("complexity changed on failed merge: %d", run.Complexity)
	}
	events, _ := store.Events(ctx, id)
	if len(events) != 0 {
		t.Fatalf("events written on failed merge: %d", len(events))
	}
}

func TestUpdateConfigurationConcurrentMergesDoNotLoseData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.db")
	openStore := func() (*db.DB, *Store) {
		d, err := db.Open(path, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		return d, New(d.SqlDB())
	}
	d1, store1 := openStore()
	defer d1.Close()
	if err := d1.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2, store2 := openStore()
	defer d2.Close()

	metadata := sealedMetadata()
	id, err := store1.Create(context.Background(), &Run{ProjectDir: "/tmp", Goal: "test", Complexity: 3, Metadata: &metadata})
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for idx, tc := range []struct {
		store *Store
		patch string
	}{{store1, `{"left":{"value":1}}`}, {store2, `{"right":{"value":2}}`}} {
		_ = idx
		wg.Add(1)
		go func(store *Store, patch string) {
			defer wg.Done()
			<-start
			errs <- store.UpdateConfiguration(context.Background(), id, RunConfigUpdate{MetadataMerge: patch})
		}(tc.store, tc.patch)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent merge: %v", err)
		}
	}

	run, err := store1.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := parseMetadataObject(*run.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if obj["left"] == nil || obj["right"] == nil {
		t.Fatalf("concurrent merge lost data: %s", *run.Metadata)
	}
	events, err := store1.Events(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("set events = %d, want 2", len(events))
	}
}

func TestUpdateConfigurationCannotRaceBindingPastTerminal(t *testing.T) {
	store := setupTestStore(t)
	ctx := context.Background()
	id, err := store.Create(ctx, &Run{
		ProjectDir: "/tmp", Goal: "race", Complexity: 3, AutoAdvance: true,
		Phases: []string{PhaseReflect, PhaseDone},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = store.UpdateConfiguration(ctx, id, RunConfigUpdate{
		MetadataMerge: `{"close_gate":{"requirements":["runtime-evidence/v1"],"bead_id":"sylveste-6h7x"}}`,
		beforeCAS: func() {
			if updateErr := store.UpdatePhase(ctx, id, PhaseReflect, PhaseDone); updateErr != nil {
				t.Fatalf("race phase update: %v", updateErr)
			}
			if statusErr := store.UpdateStatus(ctx, id, StatusCompleted); statusErr != nil {
				t.Fatalf("race status update: %v", statusErr)
			}
		},
	})
	if !errors.Is(err, ErrInvalidMetadata) {
		t.Fatalf("raced binding error = %v, want ErrInvalidMetadata", err)
	}
	run, getErr := store.Get(ctx, id)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if run.Metadata != nil {
		t.Fatalf("runtime metadata bound after terminal race: %s", *run.Metadata)
	}
	events, _ := store.Events(ctx, id)
	if len(events) != 0 {
		t.Fatalf("set event written after terminal race: %d", len(events))
	}
}
