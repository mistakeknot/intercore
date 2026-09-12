package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dbpkg "github.com/mistakeknot/intercore/internal/db"
	usagepkg "github.com/mistakeknot/intercore/internal/usage"
)

func TestParseUsageFlagsStrict(t *testing.T) {
	allowed := map[string]struct{}{"record": {}, "max-age": {}}
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown", []string{"--other=x"}},
		{"positional", []string{"surprise"}},
		{"missing_value", []string{"--record"}},
		{"missing_before_flag", []string{"--record", "--max-age=3"}},
		{"duplicate_equals", []string{"--record=a", "--record=b"}},
		{"duplicate_forms", []string{"--record=a", "--record", "b"}},
		{"empty_value", []string{"--record="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseUsageFlags(tc.args, allowed); err == nil {
				t.Fatal("parseUsageFlags accepted invalid grammar")
			}
		})
	}
	got, err := parseUsageFlags([]string{"--record", "a.json", "--max-age=60"}, allowed)
	if err != nil || got["record"] != "a.json" || got["max-age"] != "60" {
		t.Fatalf("valid parse = (%v, %v)", got, err)
	}
}

func TestCmdUsageHelp(t *testing.T) {
	out := captureDispatchOutput(t, func() int {
		return cmdUsage(context.Background(), []string{"--help"})
	})
	for _, want := range []string{"usage observe --record", "usage list", "usage validate --observation", "1 MiB"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}

func TestCmdUsageObserveRejectsInvalidFilesAndJSON(t *testing.T) {
	root := setupUsageCLI(t)
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"duplicate_recursive", []byte(`{"id":"x","identity":{"account":{"coverage":"unknown","coverage":"complete"}}}`)},
		{"unknown_field", []byte(`{"unexpected":true}`)},
		{"trailing", []byte(`{} {}`)},
		{"oversize", []byte(strings.Repeat(" ", usagepkg.MaxRecordBytes+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(root, tc.name+".json")
			if err := os.WriteFile(path, tc.data, 0600); err != nil {
				t.Fatal(err)
			}
			if code := cmdUsage(context.Background(), []string{"observe", "--record", path}); code != 3 {
				t.Fatalf("exit = %d, want 3", code)
			}
		})
	}

	dir := filepath.Join(root, "directory")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if code := cmdUsage(context.Background(), []string{"observe", "--record", dir}); code != 3 {
		t.Fatalf("directory exit = %d, want 3", code)
	}
}

func TestCmdUsageObserveListValidate(t *testing.T) {
	root := setupUsageCLI(t)
	record := usagepkg.ObservationInput{
		ID: "uobs_cli", Provider: "openai", Source: "fixture", Kind: usagepkg.KindLocalUsage,
		Status: usagepkg.StatusUnavailable, Reason: stringPointer("fixture has no measurement"),
		CapturedAt: "2026-09-12T20:00:00Z", SourceAt: nil,
		IntervalStart: nil, IntervalEnd: nil, QuotaBucket: nil, QuotaResetAt: nil,
		Counters:      []usagepkg.Counter{{Name: "tokens", Value: nil, Unit: "tokens", Semantics: "fixture count"}},
		Identity:      usagepkg.Identity{Account: usagepkg.AccountIdentity{Coverage: usagepkg.AccountCoverageUnknown, SafeRef: nil, Reasons: []string{"fixture has no account identity"}}},
		ExecutionRefs: usagepkg.ExecutionRefs{}, PayloadSHA256: strings.Repeat("b", 64), Supersedes: nil,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "record.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	oldJSON := flagJSON
	flagJSON = true
	t.Cleanup(func() { flagJSON = oldJSON })
	observeOut := captureDispatchOutput(t, func() int {
		return cmdUsage(context.Background(), []string{"observe", "--record", path})
	})
	var observed usagepkg.Observation
	if err := json.Unmarshal(observeOut, &observed); err != nil || observed.ID != record.ID {
		t.Fatalf("observe output = %s, error=%v", observeOut, err)
	}

	listOut := captureDispatchOutput(t, func() int {
		return cmdUsage(context.Background(), []string{"list", "--limit=1"})
	})
	var listed []usagepkg.Observation
	if err := json.Unmarshal(listOut, &listed); err != nil || len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("list output = %s, error=%v", listOut, err)
	}
	d, err := dbpkg.Open(filepath.Join(root, "usage.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 105; index++ {
		newer := record
		newer.ID = fmt.Sprintf("zz_newer_%03d", index)
		if _, _, err := usagepkg.NewStore(d.SqlDB()).Observe(context.Background(), newer); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	exactOut := captureDispatchOutput(t, func() int {
		return cmdUsage(context.Background(), []string{"list", "--observation", record.ID})
	})
	if err := json.Unmarshal(exactOut, &listed); err != nil || len(listed) != 1 || listed[0].ID != record.ID {
		t.Fatalf("exact list must find older evidence beyond default page: %s error=%v", exactOut, err)
	}
	if rc := cmdUsage(context.Background(), []string{"list", "--observation", record.ID, "--limit=1"}); rc != 3 {
		t.Fatalf("ambiguous list flags returned %d", rc)
	}
	if rc := cmdUsage(context.Background(), []string{"list", "--observation", "missing"}); rc != 1 {
		t.Fatalf("missing exact observation returned %d", rc)
	}

	validateOut := captureDispatchOutput(t, func() int {
		return cmdUsage(context.Background(), []string{"validate", "--observation", record.ID, "--max-age", "3600"})
	})
	var validation usagepkg.Validation
	if err := json.Unmarshal(validateOut, &validation); err != nil || validation.ObservationID != record.ID || validation.ExternalActivity != usagepkg.ExternalActivityUnknown {
		t.Fatalf("validate output = %s, error=%v", validateOut, err)
	}
}

func setupUsageCLI(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	oldDB, oldTimeout := flagDB, flagTimeout
	flagDB, flagTimeout = "usage.db", time.Second
	t.Cleanup(func() {
		flagDB, flagTimeout = oldDB, oldTimeout
		_ = os.Chdir(oldWD)
	})

	d, err := dbpkg.Open(filepath.Join(root, "usage.db"), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(context.Background()); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	return root
}

func stringPointer(value string) *string { return &value }
