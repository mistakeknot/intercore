// Package usage stores provider-neutral, append-only usage evidence and
// conservative validation snapshots. It deliberately does not make routing,
// acceptance, billing, or account-exclusivity decisions.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	MaxRecordBytes = 1 << 20

	KindRateLimits   = "rate_limits"
	KindAccountUsage = "account_usage"
	KindLocalUsage   = "local_usage"

	StatusAvailable   = "available"
	StatusPartial     = "partial"
	StatusUnavailable = "unavailable"
	StatusError       = "error"

	AccountCoverageUnknown    = "unknown"
	AccountCoverageIncomplete = "incomplete"
	AccountCoverageComplete   = "complete"

	ActivityScopeAllKernel  = "all-kernel-activity-unfiltered"
	OverlapObserved         = "observed"
	OverlapNoneObserved     = "none-observed"
	OverlapUnknown          = "unknown"
	ExternalActivityUnknown = "unknown"

	BindingCoverageComplete = "complete"
	BindingCoveragePartial  = "partial"
	BindingCoverageNone     = "none"

	ScanComplete    = "complete"
	ScanUnavailable = "unavailable"

	FreshnessFresh   = "fresh"
	FreshnessStale   = "stale"
	FreshnessUnknown = "unknown"
	StructureValid   = "valid"
	StructureInvalid = "invalid"
)

var (
	ErrIDConflict             = errors.New("usage observation ID already has different canonical input")
	ErrNotFound               = errors.New("usage observation not found")
	ErrIncompatibleSupersedes = errors.New("superseded observation has incompatible source or kind")
)

// Counter is a raw provider-reported quantity. Value is nullable so an
// unavailable measurement remains distinct from a measured zero.
type Counter struct {
	Name string `json:"name"`
	// Value retains its JSON number lexeme. Canonical identity is deliberately
	// lexical: equivalent spellings may conflict, but large values cannot alias.
	Value     *json.Number `json:"value" jsonschema:"type=number,nullable"`
	Unit      string       `json:"unit"`
	Semantics string       `json:"semantics"`
	SubsetOf  *string      `json:"subset_of" jsonschema:"nullable"`
}

type AccountIdentity struct {
	Coverage string   `json:"coverage" jsonschema:"enum=unknown,enum=incomplete,enum=complete"`
	SafeRef  *string  `json:"safe_ref" jsonschema:"nullable"`
	Reasons  []string `json:"reasons"`
}

type Identity struct {
	NativeThreadID  *string         `json:"native_thread_id" jsonschema:"nullable"`
	NativeSessionID *string         `json:"native_session_id" jsonschema:"nullable"`
	Account         AccountIdentity `json:"account"`
}

// ExecutionRefs contains actual, observed execution references. SessionRowID
// is sessions.id (INTEGER PRIMARY KEY), not the provider/native session string.
type ExecutionRefs struct {
	DispatchID        *string `json:"dispatch_id" jsonschema:"nullable"`
	SessionRowID      *int64  `json:"session_row_id" jsonschema:"nullable"`
	RunID             *string `json:"run_id" jsonschema:"nullable"`
	TaskID            *string `json:"task_id" jsonschema:"nullable"`
	DispatchRequestID *string `json:"dispatch_request_id" jsonschema:"nullable"`
	EnrollmentID      *string `json:"enrollment_id" jsonschema:"nullable"`
	ExecutionID       *string `json:"execution_id" jsonschema:"nullable"`
	AttemptID         *string `json:"attempt_id" jsonschema:"nullable"`
}

// ObservationInput is the exact strict ingestion contract. All JSON fields are
// required; nullable values must be encoded explicitly as null.
type ObservationInput struct {
	ID            string        `json:"id"`
	Provider      string        `json:"provider"`
	Source        string        `json:"source"`
	Kind          string        `json:"kind" jsonschema:"enum=rate_limits,enum=account_usage,enum=local_usage"`
	Status        string        `json:"status" jsonschema:"enum=available,enum=partial,enum=unavailable,enum=error"`
	Reason        *string       `json:"reason" jsonschema:"nullable"`
	CapturedAt    string        `json:"captured_at"`
	SourceAt      *string       `json:"source_at" jsonschema:"nullable"`
	IntervalStart *int64        `json:"interval_start" jsonschema:"nullable"`
	IntervalEnd   *int64        `json:"interval_end" jsonschema:"nullable"`
	QuotaBucket   *string       `json:"quota_bucket" jsonschema:"nullable"`
	QuotaResetAt  *string       `json:"quota_reset_at" jsonschema:"nullable"`
	Counters      []Counter     `json:"counters"`
	Identity      Identity      `json:"identity"`
	ExecutionRefs ExecutionRefs `json:"execution_refs"`
	PayloadSHA256 string        `json:"payload_sha256"`
	Supersedes    *string       `json:"supersedes" jsonschema:"nullable"`
}

// Observation is the immutable stored representation returned by the CLI.
type Observation struct {
	ObservationInput
	CanonicalSHA256 string `json:"canonical_sha256"`
	InsertedAt      int64  `json:"inserted_at"`
}

// Activity is a kernel row whose closed interval intersects the observation.
// Exactly one primary-key field is populated.
type Activity struct {
	Kind         string  `json:"kind" jsonschema:"enum=dispatch,enum=session"`
	DispatchID   *string `json:"dispatch_id" jsonschema:"nullable"`
	SessionRowID *int64  `json:"session_row_id" jsonschema:"nullable"`
	ProjectDir   string  `json:"project_dir"`
	StartedAt    int64   `json:"started_at"`
	EndedAt      *int64  `json:"ended_at" jsonschema:"nullable"`
}

type KnownActivity struct {
	Scope         string     `json:"scope" jsonschema:"enum=all-kernel-activity-unfiltered"`
	ScanStatus    string     `json:"scan_status" jsonschema:"enum=complete,enum=unavailable"`
	BoundActivity []Activity `json:"bound_activity"`
	OtherActivity []Activity `json:"other_activity"`
}

// Validation is append-only evidence about one observation at one point in
// time. It is observational only and cannot confer acceptance or exclusivity.
type Validation struct {
	ID                int64         `json:"id"`
	ObservationID     string        `json:"observation_id"`
	EvaluatedAt       int64         `json:"evaluated_at"`
	MaxAgeSeconds     int64         `json:"max_age_seconds"`
	KnownActivity     KnownActivity `json:"known_activity"`
	KnownOverlap      string        `json:"known_overlap" jsonschema:"enum=observed,enum=none-observed,enum=unknown"`
	ExternalActivity  string        `json:"external_activity" jsonschema:"enum=unknown"`
	BindingCoverage   string        `json:"binding_coverage" jsonschema:"enum=complete,enum=partial,enum=none"`
	BindingReasons    []string      `json:"binding_reasons"`
	CaptureAgeSeconds *int64        `json:"capture_age_seconds" jsonschema:"nullable"`
	CaptureFreshness  string        `json:"capture_freshness" jsonschema:"enum=fresh,enum=stale,enum=unknown"`
	SourceAgeSeconds  *int64        `json:"source_age_seconds" jsonschema:"nullable"`
	SourceFreshness   string        `json:"source_freshness" jsonschema:"enum=fresh,enum=stale,enum=unknown"`
	Reasons           []string      `json:"reasons"`
	StructureStatus   string        `json:"structure_status" jsonschema:"enum=valid,enum=invalid"`
	StructureReasons  []string      `json:"structure_reasons"`
}

type ValidateOptions struct {
	MaxAge time.Duration
}

func (in ObservationInput) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"id", in.ID}, {"provider", in.Provider}, {"source", in.Source},
		{"kind", in.Kind}, {"status", in.Status}, {"captured_at", in.CapturedAt},
		{"payload_sha256", in.PayloadSHA256},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%s is required", field.name)
		}
	}
	if len(in.ID) > 256 || len(in.Provider) > 128 || len(in.Source) > 256 {
		return errors.New("id/provider/source exceeds maximum length")
	}
	if !oneOf(in.Kind, KindRateLimits, KindAccountUsage, KindLocalUsage) {
		return fmt.Errorf("invalid kind %q", in.Kind)
	}
	if !oneOf(in.Status, StatusAvailable, StatusPartial, StatusUnavailable, StatusError) {
		return fmt.Errorf("invalid status %q", in.Status)
	}
	if in.Status != StatusAvailable && (in.Reason == nil || strings.TrimSpace(*in.Reason) == "") {
		return fmt.Errorf("status %s requires a reason", in.Status)
	}
	if in.Reason != nil && len(*in.Reason) > 1024 {
		return errors.New("reason exceeds maximum length")
	}
	if _, err := time.Parse(time.RFC3339, in.CapturedAt); err != nil {
		return fmt.Errorf("invalid captured_at: %w", err)
	}
	if err := validateOptionalTime("source_at", in.SourceAt); err != nil {
		return err
	}
	if err := validateOptionalTime("quota_reset_at", in.QuotaResetAt); err != nil {
		return err
	}
	if (in.IntervalStart == nil) != (in.IntervalEnd == nil) {
		return errors.New("interval_start and interval_end must be provided together")
	}
	if in.IntervalStart != nil && *in.IntervalStart > *in.IntervalEnd {
		return errors.New("interval_start must be <= interval_end")
	}
	if in.Supersedes != nil {
		if strings.TrimSpace(*in.Supersedes) == "" {
			return errors.New("supersedes cannot be empty")
		}
		if *in.Supersedes == in.ID {
			return errors.New("observation cannot supersede itself")
		}
	}
	if len(in.PayloadSHA256) != sha256.Size*2 {
		return errors.New("payload_sha256 must be 64 lowercase hexadecimal characters")
	}
	if _, err := hex.DecodeString(in.PayloadSHA256); err != nil || strings.ToLower(in.PayloadSHA256) != in.PayloadSHA256 {
		return errors.New("payload_sha256 must be 64 lowercase hexadecimal characters")
	}
	if in.Counters == nil {
		return errors.New("counters must be an array, not null")
	}
	if err := validateCounters(in.Counters, in.Status); err != nil {
		return err
	}
	if !oneOf(in.Identity.Account.Coverage, AccountCoverageUnknown, AccountCoverageIncomplete, AccountCoverageComplete) {
		return fmt.Errorf("invalid account identity coverage %q", in.Identity.Account.Coverage)
	}
	if in.Identity.Account.Coverage == AccountCoverageComplete && (in.Identity.Account.SafeRef == nil || strings.TrimSpace(*in.Identity.Account.SafeRef) == "") {
		return errors.New("complete account identity coverage requires safe_ref")
	}
	if in.Identity.Account.SafeRef != nil && strings.TrimSpace(*in.Identity.Account.SafeRef) == "" {
		return errors.New("account safe_ref cannot be empty")
	}
	if in.Identity.Account.Reasons == nil {
		return errors.New("account identity reasons must be an array, not null")
	}
	if in.Identity.Account.Coverage != AccountCoverageComplete && len(nonempty(in.Identity.Account.Reasons)) == 0 {
		return errors.New("unknown or incomplete account identity requires a reason")
	}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"native_thread_id", in.Identity.NativeThreadID}, {"native_session_id", in.Identity.NativeSessionID},
		{"dispatch_id", in.ExecutionRefs.DispatchID}, {"run_id", in.ExecutionRefs.RunID},
		{"task_id", in.ExecutionRefs.TaskID}, {"dispatch_request_id", in.ExecutionRefs.DispatchRequestID},
		{"enrollment_id", in.ExecutionRefs.EnrollmentID}, {"execution_id", in.ExecutionRefs.ExecutionID},
		{"attempt_id", in.ExecutionRefs.AttemptID}, {"quota_bucket", in.QuotaBucket},
	} {
		if field.value != nil && strings.TrimSpace(*field.value) == "" {
			return fmt.Errorf("%s cannot be empty", field.name)
		}
	}
	if in.ExecutionRefs.SessionRowID != nil && *in.ExecutionRefs.SessionRowID <= 0 {
		return errors.New("session_row_id must be a positive sessions.id primary key")
	}
	return nil
}

func validateCounters(counters []Counter, status string) error {
	byName := make(map[string]Counter, len(counters))
	for _, counter := range counters {
		if strings.TrimSpace(counter.Name) == "" || strings.TrimSpace(counter.Unit) == "" || strings.TrimSpace(counter.Semantics) == "" {
			return errors.New("counter name, unit, and semantics are required")
		}
		if _, exists := byName[counter.Name]; exists {
			return fmt.Errorf("duplicate counter name %q", counter.Name)
		}
		if counter.Value != nil {
			value, err := exactNonnegativeNumber(*counter.Value)
			if err != nil || value.Sign() < 0 {
				return fmt.Errorf("counter %q value must be an exact finite nonnegative JSON number", counter.Name)
			}
		}
		if status == StatusUnavailable && counter.Value != nil {
			return fmt.Errorf("unavailable counter %q must have a null value", counter.Name)
		}
		byName[counter.Name] = counter
	}
	for _, counter := range counters {
		if counter.SubsetOf == nil {
			continue
		}
		parent, exists := byName[*counter.SubsetOf]
		if !exists {
			return fmt.Errorf("counter %q subset_of references unknown counter %q", counter.Name, *counter.SubsetOf)
		}
		if parent.Name == counter.Name {
			return fmt.Errorf("counter %q cannot be a subset of itself", counter.Name)
		}
		if parent.Unit != counter.Unit || parent.Semantics != counter.Semantics {
			return fmt.Errorf("counter %q subset has incompatible unit or semantics", counter.Name)
		}
		if counter.Value != nil && parent.Value != nil {
			childValue, _ := exactNonnegativeNumber(*counter.Value)
			parentValue, _ := exactNonnegativeNumber(*parent.Value)
			if childValue.Cmp(parentValue) > 0 {
				return fmt.Errorf("counter %q exceeds parent %q", counter.Name, parent.Name)
			}
		}
	}
	visiting := make(map[string]bool)
	visited := make(map[string]bool)
	var visit func(string) error
	visit = func(name string) error {
		if visiting[name] {
			return fmt.Errorf("counter subset cycle at %q", name)
		}
		if visited[name] {
			return nil
		}
		visiting[name] = true
		if parent := byName[name].SubsetOf; parent != nil {
			if err := visit(*parent); err != nil {
				return err
			}
		}
		visiting[name] = false
		visited[name] = true
		return nil
	}
	for name := range byName {
		if err := visit(name); err != nil {
			return err
		}
	}
	return nil
}

var jsonNumberPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.([0-9]+))?(?:[eE]([+-]?[0-9]+))?$`)

// exactNonnegativeNumber avoids binary floating point. Bounds prevent hostile
// numeric input from forcing unbounded big.Int allocation.
func exactNonnegativeNumber(number json.Number) (*big.Rat, error) {
	text := number.String()
	if len(text) == 0 || len(text) > 4096 || strings.HasPrefix(text, "-") {
		return nil, errors.New("number is negative, empty, or too long")
	}
	match := jsonNumberPattern.FindStringSubmatch(text)
	if match == nil {
		return nil, errors.New("invalid JSON number")
	}
	if match[3] != "" {
		parsed, err := strconv.ParseInt(match[3], 10, 32)
		if err != nil || parsed < -10000 || parsed > 10000 {
			return nil, errors.New("number exponent is out of bounds")
		}
	}
	value, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, errors.New("invalid exact number")
	}
	return value, nil
}

func validateOptionalTime(name string, value *string) error {
	if value == nil {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, *value); err != nil {
		return fmt.Errorf("invalid %s: %w", name, err)
	}
	return nil
}

func CanonicalJSON(value any) ([]byte, error) {
	return json.Marshal(value)
}

func canonicalDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

func nonempty(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			result = append(result, value)
		}
	}
	return result
}
