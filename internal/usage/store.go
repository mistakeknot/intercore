package usage

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

const defaultMaxAge = time.Hour

type Store struct {
	db  *sql.DB
	now func() time.Time
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db, now: time.Now}
}

// Observe validates and appends input in one BEGIN IMMEDIATE transaction.
// An identical stable-ID retry returns the stored row; any other reuse fails.
func (s *Store) Observe(ctx context.Context, input ObservationInput) (*Observation, bool, error) {
	if err := input.Validate(); err != nil {
		return nil, false, err
	}
	canonical, err := CanonicalJSON(input)
	if err != nil {
		return nil, false, fmt.Errorf("canonicalize observation: %w", err)
	}
	digest := canonicalDigest(canonical)

	conn, finish, err := s.beginImmediate(ctx)
	if err != nil {
		return nil, false, err
	}
	committed := false
	defer func() { finish(committed) }()

	var existingCanonical []byte
	var existingDigest string
	var existingInserted int64
	err = conn.QueryRowContext(ctx, `
		SELECT canonical_input, canonical_sha256, inserted_at
		FROM usage_observations WHERE id = ?`, input.ID,
	).Scan(&existingCanonical, &existingDigest, &existingInserted)
	if err == nil {
		if !bytes.Equal(existingCanonical, canonical) {
			return nil, false, fmt.Errorf("%w: %s", ErrIDConflict, input.ID)
		}
		if err := commitConn(ctx, conn); err != nil {
			return nil, false, err
		}
		committed = true
		stored, err := observationFromCanonical(existingCanonical, existingDigest, existingInserted)
		return stored, false, err
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, fmt.Errorf("read existing observation: %w", err)
	}

	if input.Supersedes != nil {
		var provider, source, kind string
		err := conn.QueryRowContext(ctx, `
			SELECT provider, source, kind FROM usage_observations WHERE id = ?`, *input.Supersedes,
		).Scan(&provider, &source, &kind)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("%w: %s", ErrNotFound, *input.Supersedes)
		}
		if err != nil {
			return nil, false, fmt.Errorf("read superseded observation: %w", err)
		}
		if provider != input.Provider || source != input.Source || kind != input.Kind {
			return nil, false, fmt.Errorf("%w: %s", ErrIncompatibleSupersedes, *input.Supersedes)
		}
	}

	countersJSON, _ := json.Marshal(input.Counters)
	identityJSON, _ := json.Marshal(input.Identity)
	executionRefsJSON, _ := json.Marshal(input.ExecutionRefs)
	insertedAt := s.now().UTC().Unix()
	_, err = conn.ExecContext(ctx, `INSERT INTO usage_observations (
		id, provider, source, kind, status, reason, captured_at, source_at,
		interval_start, interval_end, quota_bucket, quota_reset_at,
		counters_json, identity_json, execution_refs_json, payload_sha256,
		supersedes, canonical_input, canonical_sha256, inserted_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		input.ID, input.Provider, input.Source, input.Kind, input.Status,
		nullableString(input.Reason), input.CapturedAt, nullableString(input.SourceAt),
		nullableInt64(input.IntervalStart), nullableInt64(input.IntervalEnd),
		nullableString(input.QuotaBucket), nullableString(input.QuotaResetAt),
		string(countersJSON), string(identityJSON), string(executionRefsJSON), input.PayloadSHA256,
		nullableString(input.Supersedes), canonical, digest, insertedAt,
	)
	if err != nil {
		return nil, false, fmt.Errorf("insert usage observation %s: %w", input.ID, err)
	}
	if err := commitConn(ctx, conn); err != nil {
		return nil, false, err
	}
	committed = true
	return &Observation{ObservationInput: input, CanonicalSHA256: digest, InsertedAt: insertedAt}, true, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Observation, error) {
	var canonical []byte
	var digest string
	var insertedAt int64
	err := s.db.QueryRowContext(ctx, `
		SELECT canonical_input, canonical_sha256, inserted_at
		FROM usage_observations WHERE id = ?`, id,
	).Scan(&canonical, &digest, &insertedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("get usage observation: %w", err)
	}
	return observationFromCanonical(canonical, digest, insertedAt)
}

func (s *Store) List(ctx context.Context, limit int) ([]Observation, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		return nil, errors.New("usage list limit cannot exceed 1000")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT canonical_input, canonical_sha256, inserted_at
		FROM usage_observations ORDER BY inserted_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("list usage observations: %w", err)
	}
	defer rows.Close()
	result := make([]Observation, 0)
	for rows.Next() {
		var canonical []byte
		var digest string
		var insertedAt int64
		if err := rows.Scan(&canonical, &digest, &insertedAt); err != nil {
			return nil, fmt.Errorf("scan usage observation: %w", err)
		}
		observation, err := observationFromCanonical(canonical, digest, insertedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, *observation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage observations: %w", err)
	}
	return result, nil
}

// Validate appends a point-in-time snapshot after scanning every dispatch and
// session row in the authoritative database without filtering.
func (s *Store) Validate(ctx context.Context, observationID string, opts ValidateOptions) (*Validation, error) {
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = defaultMaxAge
	}
	maxAgeSeconds := int64(maxAge / time.Second)
	if maxAgeSeconds <= 0 {
		return nil, errors.New("max age must be at least one second")
	}

	conn, finish, err := s.beginImmediate(ctx)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() { finish(committed) }()

	var canonical []byte
	var digest string
	var insertedAt int64
	err = conn.QueryRowContext(ctx, `
		SELECT canonical_input, canonical_sha256, inserted_at
		FROM usage_observations WHERE id = ?`, observationID,
	).Scan(&canonical, &digest, &insertedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, observationID)
	}
	if err != nil {
		return nil, fmt.Errorf("read usage observation for validation: %w", err)
	}
	structureStatus := StructureValid
	structureReasons := []string{}
	storedInput, _, decodeErr := DecodeRecord(bytes.NewReader(canonical))
	if decodeErr != nil {
		structureStatus = StructureInvalid
		structureReasons = append(structureReasons, "canonical_input_invalid")
	}
	observation := &Observation{ObservationInput: storedInput, CanonicalSHA256: digest, InsertedAt: insertedAt}
	if canonicalDigest(canonical) != digest {
		structureStatus = StructureInvalid
		structureReasons = append(structureReasons, "canonical_digest_mismatch")
	}
	if storedInput.ID != observationID {
		structureStatus = StructureInvalid
		structureReasons = append(structureReasons, "canonical_identity_mismatch")
	}
	if structureStatus != StructureValid {
		// Untrusted structure cannot authorize interval exclusions or bindings.
		observation.ObservationInput = ObservationInput{}
	}

	now := s.now().UTC()
	result := Validation{
		ObservationID: observationID,
		EvaluatedAt:   now.Unix(),
		MaxAgeSeconds: maxAgeSeconds,
		KnownActivity: KnownActivity{
			Scope: ActivityScopeAllKernel, ScanStatus: ScanComplete,
			BoundActivity: []Activity{}, OtherActivity: []Activity{},
		},
		KnownOverlap:     OverlapUnknown,
		ExternalActivity: ExternalActivityUnknown,
		BindingReasons:   []string{},
		Reasons:          []string{},
		StructureStatus:  structureStatus,
		StructureReasons: structureReasons,
	}
	result.CaptureAgeSeconds, result.CaptureFreshness = freshness(now, observation.CapturedAt, maxAge)
	if result.CaptureFreshness == FreshnessStale {
		result.Reasons = append(result.Reasons, "capture_stale")
	} else if result.CaptureFreshness == FreshnessUnknown {
		result.Reasons = append(result.Reasons, "capture_time_unusable")
	}
	if observation.SourceAt == nil {
		result.SourceFreshness = FreshnessUnknown
		result.Reasons = append(result.Reasons, "source_time_unavailable")
	} else {
		result.SourceAgeSeconds, result.SourceFreshness = freshness(now, *observation.SourceAt, maxAge)
		if result.SourceFreshness == FreshnessStale {
			result.Reasons = append(result.Reasons, "source_stale")
		} else if result.SourceFreshness == FreshnessUnknown {
			result.Reasons = append(result.Reasons, "source_time_unusable")
		}
	}

	activities, scanComplete, invalidInterval := scanAllActivity(ctx, conn)
	if !scanComplete {
		result.KnownActivity.ScanStatus = ScanUnavailable
		result.Reasons = append(result.Reasons, "activity_scan_unavailable")
	}
	if invalidInterval {
		result.Reasons = append(result.Reasons, "invalid_kernel_activity_interval")
	}

	boundDispatch := ""
	boundSession := int64(0)
	wantedBindings := 0
	foundBindings := 0
	if observation.ExecutionRefs.DispatchID != nil {
		wantedBindings++
		for _, activity := range activities {
			if activity.DispatchID != nil && *activity.DispatchID == *observation.ExecutionRefs.DispatchID {
				boundDispatch = *activity.DispatchID
				foundBindings++
				break
			}
		}
		if boundDispatch == "" {
			result.BindingReasons = append(result.BindingReasons, "dispatch_primary_key_not_found")
		}
	}
	if observation.ExecutionRefs.SessionRowID != nil {
		wantedBindings++
		for _, activity := range activities {
			if activity.SessionRowID != nil && *activity.SessionRowID == *observation.ExecutionRefs.SessionRowID {
				boundSession = *activity.SessionRowID
				foundBindings++
				break
			}
		}
		if boundSession == 0 {
			result.BindingReasons = append(result.BindingReasons, "session_primary_key_not_found")
		}
	}
	switch {
	case wantedBindings == 0:
		result.BindingCoverage = BindingCoverageNone
		result.BindingReasons = append(result.BindingReasons, "no_primary_key_bindings")
		if observation.Identity.NativeThreadID != nil || observation.Identity.NativeSessionID != nil {
			result.BindingReasons = append(result.BindingReasons, "native_identity_not_a_primary_key_binding")
		}
	case foundBindings == wantedBindings:
		result.BindingCoverage = BindingCoverageComplete
	case foundBindings > 0:
		result.BindingCoverage = BindingCoveragePartial
	default:
		result.BindingCoverage = BindingCoverageNone
	}

	if observation.IntervalStart == nil || observation.IntervalEnd == nil {
		result.Reasons = append(result.Reasons, "observation_interval_unavailable")
	} else {
		for _, activity := range activities {
			if !intersectsClosed(*observation.IntervalStart, *observation.IntervalEnd, activity.StartedAt, activity.EndedAt) {
				continue
			}
			isBound := activity.DispatchID != nil && *activity.DispatchID == boundDispatch && boundDispatch != ""
			isBound = isBound || (activity.SessionRowID != nil && *activity.SessionRowID == boundSession && boundSession != 0)
			if isBound {
				result.KnownActivity.BoundActivity = append(result.KnownActivity.BoundActivity, activity)
			} else {
				result.KnownActivity.OtherActivity = append(result.KnownActivity.OtherActivity, activity)
			}
		}
	}

	if structureStatus == StructureValid && scanComplete && !invalidInterval && observation.IntervalStart != nil && observation.IntervalEnd != nil {
		if len(result.KnownActivity.OtherActivity) > 0 {
			result.KnownOverlap = OverlapObserved
		} else {
			result.KnownOverlap = OverlapNoneObserved
		}
	}
	result.Reasons = uniqueStrings(result.Reasons)
	result.BindingReasons = uniqueStrings(result.BindingReasons)

	if err := conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) + 1 FROM usage_validations`).Scan(&result.ID); err != nil {
		return nil, fmt.Errorf("allocate usage validation id: %w", err)
	}
	evidence, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal validation evidence: %w", err)
	}
	_, err = conn.ExecContext(ctx, `INSERT INTO usage_validations
		(id, observation_id, evaluated_at, max_age_seconds, evidence_json)
		VALUES (?, ?, ?, ?, ?)`, result.ID, observationID, result.EvaluatedAt, maxAgeSeconds, string(evidence))
	if err != nil {
		return nil, fmt.Errorf("insert usage validation: %w", err)
	}
	if err := commitConn(ctx, conn); err != nil {
		return nil, err
	}
	committed = true
	return &result, nil
}

func scanAllActivity(ctx context.Context, conn *sql.Conn) ([]Activity, bool, bool) {
	activities := make([]Activity, 0)
	complete := true
	invalid := false

	dispatchRows, err := conn.QueryContext(ctx, `
		SELECT id, project_dir, COALESCE(started_at, created_at), completed_at
		FROM dispatches ORDER BY id`)
	if err != nil {
		complete = false
	} else {
		for dispatchRows.Next() {
			var id, projectDir string
			var start, end sql.NullInt64
			if err := dispatchRows.Scan(&id, &projectDir, &start, &end); err != nil {
				complete = false
				break
			}
			if !start.Valid || (end.Valid && end.Int64 < start.Int64) {
				invalid = true
				continue
			}
			activity := Activity{Kind: "dispatch", DispatchID: &id, ProjectDir: projectDir, StartedAt: start.Int64}
			if end.Valid {
				value := end.Int64
				activity.EndedAt = &value
			}
			activities = append(activities, activity)
		}
		if err := dispatchRows.Err(); err != nil {
			complete = false
		}
		dispatchRows.Close()
	}

	sessionRows, err := conn.QueryContext(ctx, `
		SELECT id, project_dir, started_at, ended_at
		FROM sessions ORDER BY id`)
	if err != nil {
		complete = false
	} else {
		for sessionRows.Next() {
			var id int64
			var projectDir string
			var start, end sql.NullInt64
			if err := sessionRows.Scan(&id, &projectDir, &start, &end); err != nil {
				complete = false
				break
			}
			if !start.Valid || (end.Valid && end.Int64 < start.Int64) {
				invalid = true
				continue
			}
			activity := Activity{Kind: "session", SessionRowID: &id, ProjectDir: projectDir, StartedAt: start.Int64}
			if end.Valid {
				value := end.Int64
				activity.EndedAt = &value
			}
			activities = append(activities, activity)
		}
		if err := sessionRows.Err(); err != nil {
			complete = false
		}
		sessionRows.Close()
	}

	sort.Slice(activities, func(i, j int) bool {
		if activities[i].Kind != activities[j].Kind {
			return activities[i].Kind < activities[j].Kind
		}
		if activities[i].DispatchID != nil {
			return *activities[i].DispatchID < *activities[j].DispatchID
		}
		return *activities[i].SessionRowID < *activities[j].SessionRowID
	})
	return activities, complete, invalid
}

func (s *Store) beginImmediate(ctx context.Context) (*sql.Conn, func(bool), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("acquire usage connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		conn.Close()
		return nil, nil, fmt.Errorf("begin usage transaction: %w", err)
	}
	finish := func(committed bool) {
		if !committed {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
				_ = conn.Raw(func(any) error { return driver.ErrBadConn })
			}
		}
		conn.Close()
	}
	return conn, finish, nil
}

func commitConn(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("commit usage transaction: %w", err)
	}
	return nil
}

func observationFromCanonical(canonical []byte, digest string, insertedAt int64) (*Observation, error) {
	if actual := canonicalDigest(canonical); actual != digest {
		return nil, fmt.Errorf("stored usage observation digest mismatch: got %s want %s", actual, digest)
	}
	var input ObservationInput
	if err := json.Unmarshal(canonical, &input); err != nil {
		return nil, fmt.Errorf("decode stored usage observation: %w", err)
	}
	if err := input.Validate(); err != nil {
		return nil, fmt.Errorf("validate stored usage observation: %w", err)
	}
	return &Observation{ObservationInput: input, CanonicalSHA256: digest, InsertedAt: insertedAt}, nil
}

func nullableString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func freshness(now time.Time, timestamp string, maxAge time.Duration) (*int64, string) {
	parsed, err := time.Parse(time.RFC3339, timestamp)
	if err != nil {
		return nil, FreshnessUnknown
	}
	age := now.Unix() - parsed.Unix()
	if age < 0 {
		return nil, FreshnessUnknown
	}
	if time.Duration(age)*time.Second > maxAge {
		return &age, FreshnessStale
	}
	return &age, FreshnessFresh
}

func intersectsClosed(observationStart, observationEnd, activityStart int64, activityEnd *int64) bool {
	if activityStart > observationEnd {
		return false
	}
	return activityEnd == nil || *activityEnd >= observationStart
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}
