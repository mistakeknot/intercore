package usage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	dbpkg "github.com/mistakeknot/intercore/internal/db"
)

func TestDecodeRecordStrict(t *testing.T) {
	valid := mustJSON(t, baseInput("uobs_valid"))
	tests := []struct {
		name string
		data []byte
	}{
		{"top_level_duplicate", []byte(`{"id":"a","id":"b"}`)},
		{"recursive_duplicate", bytes.Replace(valid, []byte(`"account":{"coverage"`), []byte(`"account":{"coverage":"unknown","coverage"`), 1)},
		{"unknown_nested_field", bytes.Replace(valid, []byte(`"safe_ref":null`), []byte(`"safe_ref":null,"raw_account_id":"forbidden"`), 1)},
		{"trailing_json", append(append([]byte{}, valid...), []byte(` {}`)...)},
		{"oversize", bytes.Repeat([]byte(" "), MaxRecordBytes+1)},
		{"nonfinite", bytes.Replace(valid, []byte(`"value":null`), []byte(`"value":NaN`), 1)},
		{"negative_counter", bytes.Replace(valid, []byte(`"value":null`), []byte(`"value":-1`), 1)},
		{"quoted_counter", bytes.Replace(valid, []byte(`"value":null`), []byte(`"value":"123"`), 1)},
		{"missing_nullable_top_level", bytes.Replace(valid, []byte(`,"supersedes":null`), nil, 1)},
		{"missing_nullable_nested", bytes.Replace(valid, []byte(`"safe_ref":null,`), nil, 1)},
		{"null_counters_array", bytes.Replace(valid,
			[]byte(`"counters":[{"name":"input_tokens","value":null,"unit":"tokens","semantics":"provider count","subset_of":null}]`),
			[]byte(`"counters":null`), 1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := DecodeRecord(bytes.NewReader(tt.data)); err == nil {
				t.Fatal("DecodeRecord accepted invalid input")
			}
		})
	}

	got, canonical, err := DecodeRecord(bytes.NewReader(valid))
	if err != nil {
		t.Fatalf("valid record: %v", err)
	}
	if got.ID != "uobs_valid" || len(canonical) == 0 {
		t.Fatalf("decoded = %#v, canonical bytes=%d", got, len(canonical))
	}
}

func TestExactNumberOrdering(t *testing.T) {
	for _, tc := range []struct {
		left, right string
		want        int
	}{
		{"9", "10", -1}, {"0.12", "0.2", -1}, {"1e3", "1000", 0},
		{"1.25e-2", "0.0125", 0}, {"9007199254740993", "9007199254740992", 1},
	} {
		left, err := exactNonnegativeNumber(json.Number(tc.left))
		if err != nil {
			t.Fatal(err)
		}
		right, err := exactNonnegativeNumber(json.Number(tc.right))
		if err != nil {
			t.Fatal(err)
		}
		if got := left.Cmp(right); got != tc.want {
			t.Fatalf("%s cmp %s: got %d want %d", tc.left, tc.right, got, tc.want)
		}
	}
	for _, value := range []string{"1e10001", "-1", "NaN", "0x10"} {
		if _, err := exactNonnegativeNumber(json.Number(value)); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
}

func TestInputValidationCountersIntervalsAndIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ObservationInput)
	}{
		{"interval_pair", func(in *ObservationInput) { in.IntervalEnd = nil }},
		{"interval_order", func(in *ObservationInput) { *in.IntervalStart, *in.IntervalEnd = 20, 10 }},
		{"invalid_kind", func(in *ObservationInput) { in.Kind = "combined" }},
		{"invalid_status", func(in *ObservationInput) { in.Status = "ok" }},
		{"invalid_timestamp", func(in *ObservationInput) { in.CapturedAt = "yesterday" }},
		{"null_counter_array", func(in *ObservationInput) { in.Counters = nil }},
		{"duplicate_counter", func(in *ObservationInput) { in.Counters = append(in.Counters, in.Counters[0]) }},
		{"nonfinite_counter", func(in *ObservationInput) { in.Counters[0].Value = floatptr(math.Inf(1)) }},
		{"missing_subset", func(in *ObservationInput) { in.Counters[0].SubsetOf = strptr("missing") }},
		{"subset_unit_mismatch", func(in *ObservationInput) {
			in.Counters = append(in.Counters, Counter{Name: "total", Value: floatptr(2), Unit: "credits", Semantics: "same"})
			in.Counters[0].SubsetOf = strptr("total")
		}},
		{"subset_semantics_mismatch", func(in *ObservationInput) {
			in.Counters = append(in.Counters, Counter{Name: "total", Value: floatptr(2), Unit: "tokens", Semantics: "different"})
			in.Counters[0].SubsetOf = strptr("total")
		}},
		{"subset_greater_than_parent", func(in *ObservationInput) {
			in.Counters[0].Value = floatptr(3)
			in.Counters = append(in.Counters, Counter{Name: "total", Value: floatptr(2), Unit: "tokens", Semantics: "provider count"})
			in.Counters[0].SubsetOf = strptr("total")
		}},
		{"subset_cycle", func(in *ObservationInput) {
			in.Counters = append(in.Counters, Counter{Name: "total", Value: nil, Unit: "tokens", Semantics: "provider count", SubsetOf: strptr("input_tokens")})
			in.Counters[0].SubsetOf = strptr("total")
		}},
		{"unavailable_without_reason", func(in *ObservationInput) { in.Status = StatusUnavailable; in.Reason = nil }},
		{"complete_account_without_safe_ref", func(in *ObservationInput) { in.Identity.Account.Coverage = AccountCoverageComplete }},
		{"complete_account_null_reasons", func(in *ObservationInput) {
			in.Identity.Account.Coverage = AccountCoverageComplete
			in.Identity.Account.SafeRef = strptr("safe:example")
			in.Identity.Account.Reasons = nil
		}},
		{"empty_execution_ref", func(in *ObservationInput) { in.ExecutionRefs.DispatchID = strptr("") }},
		{"supersedes_self", func(in *ObservationInput) { in.Supersedes = &in.ID }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := baseInput("uobs_" + tt.name)
			tt.mutate(&in)
			if err := in.Validate(); err == nil {
				t.Fatal("Validate accepted invalid input")
			}
		})
	}
}

func TestObserveIdempotencyConflictNullAndSupersedes(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	in := baseInput("uobs_one")
	in.Counters = []Counter{
		{Name: "unknown", Value: nil, Unit: "tokens", Semantics: "provider count"},
		{Name: "known_zero", Value: floatptr(0), Unit: "tokens", Semantics: "provider count"},
	}

	first, inserted, err := store.Observe(ctx, in)
	if err != nil || !inserted {
		t.Fatalf("first Observe = (%#v, %v, %v)", first, inserted, err)
	}
	again, inserted, err := store.Observe(ctx, in)
	if err != nil || inserted || again.CanonicalSHA256 != first.CanonicalSHA256 {
		t.Fatalf("idempotent Observe = (%#v, %v, %v)", again, inserted, err)
	}
	if again.Counters[0].Value != nil || again.Counters[1].Value == nil || again.Counters[1].Value.String() != "0" {
		t.Fatalf("null/zero lost: %#v", again.Counters)
	}

	conflict := in
	conflict.Source = "different-source"
	if _, _, err := store.Observe(ctx, conflict); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("conflicting ID error = %v, want ErrIDConflict", err)
	}
	resetConflict := in
	resetConflict.QuotaResetAt = strptr("2026-09-12T22:00:00Z")
	if _, _, err := store.Observe(ctx, resetConflict); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("reset-window ID error = %v, want ErrIDConflict", err)
	}

	successor := baseInput("uobs_two")
	successor.Supersedes = strptr(in.ID)
	if _, inserted, err := store.Observe(ctx, successor); err != nil || !inserted {
		t.Fatalf("compatible successor = (%v, %v)", inserted, err)
	}
	incompatible := baseInput("uobs_three")
	incompatible.Source = "other-source"
	incompatible.Supersedes = strptr(in.ID)
	if _, _, err := store.Observe(ctx, incompatible); !errors.Is(err, ErrIncompatibleSupersedes) {
		t.Fatalf("incompatible successor error = %v", err)
	}
	providerMismatch := baseInput("uobs_four")
	providerMismatch.Provider = "different-provider"
	providerMismatch.Supersedes = strptr(in.ID)
	if _, _, err := store.Observe(ctx, providerMismatch); !errors.Is(err, ErrIncompatibleSupersedes) {
		t.Fatalf("provider-incompatible successor error = %v", err)
	}
}

func TestObserveLargeCountersRetainDistinctLexemes(t *testing.T) {
	store := testStore(t)
	a := baseInput("uobs_large")
	a.Counters[0].Value = numberptr("9007199254740992")
	if _, inserted, err := store.Observe(context.Background(), a); err != nil || !inserted {
		t.Fatalf("first large counter = inserted %v, err %v", inserted, err)
	}
	b := a
	b.Counters = append([]Counter(nil), a.Counters...)
	b.Counters[0].Value = numberptr("9007199254740993")
	if _, _, err := store.Observe(context.Background(), b); !errors.Is(err, ErrIDConflict) {
		t.Fatalf("distinct >2^53 retry error = %v, want ErrIDConflict", err)
	}
}

func TestObserveConcurrentStableID(t *testing.T) {
	stores := testStores(t)
	ctx := context.Background()

	t.Run("identical", func(t *testing.T) {
		in := baseInput("uobs_concurrent_same")
		var wg sync.WaitGroup
		results := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(store *Store) {
				defer wg.Done()
				_, _, err := store.Observe(ctx, in)
				results <- err
			}(stores[i%len(stores)])
		}
		wg.Wait()
		close(results)
		for err := range results {
			if err != nil {
				t.Errorf("identical retry: %v", err)
			}
		}
	})

	t.Run("conflicting", func(t *testing.T) {
		a := baseInput("uobs_concurrent_conflict")
		b := a
		b.Source = "different-source"
		start := make(chan struct{})
		results := make(chan error, 2)
		for i, in := range []ObservationInput{a, b} {
			go func(store *Store, input ObservationInput) {
				<-start
				_, _, err := store.Observe(ctx, input)
				results <- err
			}(stores[i], in)
		}
		close(start)
		err1, err2 := <-results, <-results
		if (err1 == nil) == (err2 == nil) || !(errors.Is(err1, ErrIDConflict) || errors.Is(err2, ErrIDConflict)) {
			t.Fatalf("conflicting results = (%v, %v), want one success and one ErrIDConflict", err1, err2)
		}
	})
}

func TestValidateAllKernelActivityAndExactBindings(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	start, end := int64(100), int64(200)
	in := baseInput("uobs_activity")
	in.IntervalStart, in.IntervalEnd = &start, &end
	in.ExecutionRefs.DispatchID = strptr("bound-dispatch")
	sessionID := int64(1)
	in.ExecutionRefs.SessionRowID = &sessionID
	if _, _, err := store.Observe(ctx, in); err != nil {
		t.Fatal(err)
	}

	mustExec(t, store.db, `INSERT INTO dispatches(id,status,project_dir,created_at,started_at,completed_at) VALUES
		('bound-dispatch','completed','/bound',50,90,100),
		('endpoint-other','completed','/other-project',200,200,250),
		('unstarted-crashed','failed','/third',150,NULL,NULL),
		('outside','completed','/outside',1,1,99)`)
	mustExec(t, store.db, `INSERT INTO sessions(id,session_id,project_dir,agent_type,started_at,ended_at) VALUES
		(1,'native-bound','/bound','codex',100,110),
		(2,'open-other','/fourth','claude',199,NULL)`)

	result, err := store.Validate(ctx, in.ID, ValidateOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.KnownActivity.Scope != ActivityScopeAllKernel || result.KnownOverlap != OverlapObserved || result.ExternalActivity != ExternalActivityUnknown {
		t.Fatalf("validation classification = %#v", result)
	}
	if result.BindingCoverage != BindingCoverageComplete || len(result.KnownActivity.BoundActivity) != 2 {
		t.Fatalf("binding evidence = %#v", result)
	}
	if got := activityKeys(result.KnownActivity.OtherActivity); strings.Join(got, ",") != "dispatch:endpoint-other,dispatch:unstarted-crashed,session:2" {
		t.Fatalf("other activity = %v", got)
	}
	if strings.Contains(fmt.Sprint(result), "outside") {
		t.Fatalf("non-overlapping activity leaked into overlap evidence: %#v", result)
	}
}

func TestValidateNativeIdentityNeverExcludesAndNoEvidenceIsUnknown(t *testing.T) {
	store := testStore(t)
	ctx := context.Background()
	start, end := int64(100), int64(200)
	in := baseInput("uobs_native_only")
	in.IntervalStart, in.IntervalEnd = &start, &end
	in.Identity.NativeSessionID = strptr("same-native-string")
	if _, _, err := store.Observe(ctx, in); err != nil {
		t.Fatal(err)
	}

	first, err := store.Validate(ctx, in.ID, ValidateOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if first.KnownOverlap != OverlapNoneObserved {
		t.Fatalf("empty complete scan = %#v, want none-observed", first)
	}

	mustExec(t, store.db, `INSERT INTO sessions(session_id,project_dir,agent_type,started_at,ended_at) VALUES
		('same-native-string','/project','codex',100,200)`)
	second, err := store.Validate(ctx, in.ID, ValidateOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if second.KnownOverlap != OverlapObserved || len(second.KnownActivity.BoundActivity) != 0 || len(second.KnownActivity.OtherActivity) != 1 {
		t.Fatalf("native-only identity excluded activity: %#v", second)
	}
}

func TestValidateReportsCanonicalStructure(t *testing.T) {
	store := testStore(t)
	in := baseInput("uobs_structure")
	if _, _, err := store.Observe(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	valid, err := store.Validate(context.Background(), in.ID, ValidateOptions{})
	if err != nil || valid.StructureStatus != StructureValid || len(valid.StructureReasons) != 0 {
		t.Fatalf("valid structure = %#v, err=%v", valid, err)
	}
	mustExec(t, store.db, "DROP TRIGGER usage_observations_no_update")
	mustExec(t, store.db, "UPDATE usage_observations SET canonical_sha256='"+strings.Repeat("0", 64)+"' WHERE id='uobs_structure'")
	invalid, err := store.Validate(context.Background(), in.ID, ValidateOptions{})
	if err != nil || invalid.StructureStatus != StructureInvalid || !contains(invalid.StructureReasons, "canonical_digest_mismatch") {
		t.Fatalf("invalid structure = %#v, err=%v", invalid, err)
	}
}

func TestValidateMalformedIntervalsAndUnavailableScanForceUnknown(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		setup  func(*testing.T, *Store)
		reason string
	}{
		{"malformed", func(t *testing.T, s *Store) {
			mustExec(t, s.db, `INSERT INTO dispatches(id,status,project_dir,created_at,started_at,completed_at)
				VALUES ('bad','failed','/p',300,300,200)`)
		}, "invalid_kernel_activity_interval"},
		{"unavailable", func(t *testing.T, s *Store) {
			mustExec(t, s.db, `DROP TABLE sessions`)
		}, "activity_scan_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := testStore(t)
			start, end := int64(100), int64(200)
			in := baseInput("uobs_" + tc.name)
			in.IntervalStart, in.IntervalEnd = &start, &end
			if _, _, err := store.Observe(ctx, in); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, store)
			result, err := store.Validate(ctx, in.ID, ValidateOptions{MaxAge: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			if result.KnownOverlap != OverlapUnknown || !contains(result.Reasons, tc.reason) {
				t.Fatalf("result = %#v, want unknown/%s", result, tc.reason)
			}
		})
	}
}

func TestValidateFreshnessAndNoRoutingWrites(t *testing.T) {
	store := testStore(t)
	store.now = func() time.Time { return time.Date(2026, 9, 12, 20, 0, 0, 0, time.UTC) }
	in := baseInput("uobs_freshness")
	in.CapturedAt = "2026-09-12T19:59:30Z"
	in.SourceAt = strptr("2026-09-12T18:00:00Z")
	if _, _, err := store.Observe(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	mustExec(t, store.db, `INSERT INTO dispatches(id,status,project_dir,created_at,completed_at) VALUES ('old','completed','/p',1,2)`)

	var routingBefore, reviewBefore int
	if err := store.db.QueryRow(`SELECT count(*) FROM routing_decisions`).Scan(&routingBefore); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM review_events`).Scan(&reviewBefore); err != nil {
		t.Fatal(err)
	}
	result, err := store.Validate(context.Background(), in.ID, ValidateOptions{MaxAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if result.CaptureFreshness != FreshnessFresh || result.SourceFreshness != FreshnessStale || result.CaptureAgeSeconds == nil || *result.CaptureAgeSeconds != 30 {
		t.Fatalf("freshness = %#v", result)
	}
	var evidenceJSON string
	if err := store.db.QueryRow(`SELECT evidence_json FROM usage_validations WHERE id=?`, result.ID).Scan(&evidenceJSON); err != nil {
		t.Fatal(err)
	}
	var stored Validation
	if err := json.Unmarshal([]byte(evidenceJSON), &stored); err != nil || stored.ID != result.ID || stored.ObservationID != in.ID {
		t.Fatalf("stored validation evidence = %s, error=%v", evidenceJSON, err)
	}
	if result.KnownOverlap != OverlapNoneObserved {
		t.Fatalf("non-overlapping complete scan = %q, want none-observed", result.KnownOverlap)
	}
	var routingAfter, reviewAfter int
	if err := store.db.QueryRow(`SELECT count(*) FROM routing_decisions`).Scan(&routingAfter); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM review_events`).Scan(&reviewAfter); err != nil {
		t.Fatal(err)
	}
	if routingAfter != routingBefore || reviewAfter != reviewBefore {
		t.Fatalf("validation wrote policy/acceptance evidence: routing %d->%d, review %d->%d", routingBefore, routingAfter, reviewBefore, reviewAfter)
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	path := t.TempDir() + "/usage.db"
	d, err := dbpkg.Open(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if err := d.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return NewStore(d.SqlDB())
}

func testStores(t *testing.T) []*Store {
	t.Helper()
	path := t.TempDir() + "/usage.db"
	d1, err := dbpkg.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d1.Close() })
	if err := d1.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	d2, err := dbpkg.Open(path, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d2.Close() })
	return []*Store{NewStore(d1.SqlDB()), NewStore(d2.SqlDB())}
}

func baseInput(id string) ObservationInput {
	start, end := int64(10), int64(20)
	return ObservationInput{
		ID: id, Provider: "openai", Source: "codex-app-server.account-usage-read",
		Kind: KindAccountUsage, Status: StatusAvailable, Reason: nil,
		CapturedAt: "2026-09-12T20:00:00Z", SourceAt: strptr("2026-09-12T19:59:58Z"),
		IntervalStart: &start, IntervalEnd: &end, QuotaBucket: strptr("primary"),
		QuotaResetAt:  strptr("2026-09-12T21:00:00Z"),
		Counters:      []Counter{{Name: "input_tokens", Value: nil, Unit: "tokens", Semantics: "provider count"}},
		Identity:      Identity{Account: AccountIdentity{Coverage: AccountCoverageIncomplete, Reasons: []string{"no safe account binding"}}},
		ExecutionRefs: ExecutionRefs{},
		PayloadSHA256: strings.Repeat("a", 64),
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := CanonicalJSON(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func mustExec(t *testing.T, db *sql.DB, query string) {
	t.Helper()
	if _, err := db.Exec(query); err != nil {
		t.Fatalf("exec SQL: %v", err)
	}
}

func activityKeys(items []Activity) []string {
	keys := make([]string, 0, len(items))
	for _, item := range items {
		if item.DispatchID != nil {
			keys = append(keys, "dispatch:"+*item.DispatchID)
		} else if item.SessionRowID != nil {
			keys = append(keys, fmt.Sprintf("session:%d", *item.SessionRowID))
		}
	}
	return keys
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func strptr(value string) *string         { return &value }
func numberptr(value string) *json.Number { n := json.Number(value); return &n }
func floatptr(value float64) *json.Number {
	n := json.Number(strconv.FormatFloat(value, 'g', -1, 64))
	return &n
}
