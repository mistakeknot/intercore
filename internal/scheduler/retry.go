package scheduler

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand"
	"time"
)

// Outcome classes. They are stored in scheduler_jobs.outcome_class and
// scheduler_job_attempts.outcome_class.
const (
	ClassOK                 = "ok"
	ClassPrestart           = "prestart"
	ClassCapacity           = "capacity"
	ClassAgentFailed        = "agent_failed"
	ClassFailedUnclassified = "failed_unclassified"
	ClassIndeterminate      = "indeterminate"
	ClassConfigError        = "config_error"
	ClassTimeout            = "timeout"
	ClassCancelled          = "cancelled"
	ClassUnknown            = "unknown"

	// Set by recovery only; never reported by a finished attempt.
	ClassAbandoned     = "abandoned"      // crash cap reached
	ClassPrestartAbort = "prestart_abort" // runner died before the link; the attempt is excused
	ClassReleased      = "released"       // an operator requeued a quarantined attempt
)

// Quarantine reasons. A quarantined job stores one in outcome_class.
const (
	ReasonRunnerInconsistent = "runner_inconsistent"
	ReasonSurvivorUnknown    = "survivor_unknown"
	ReasonLegacyUnknown      = "legacy_unknown"
	ReasonLegacyRunning      = "legacy_running"
)

// MaxCrashRequeues caps excused runner crashes per job.
const MaxCrashRequeues = 3

// maxPolicyAttempts bounds retry_policy.max_attempts.
const maxPolicyAttempts = 50

// capacityBackoffFloor is the minimum delay after a capacity or unclassified failure.
const capacityBackoffFloor = 15 * time.Minute

var (
	// ErrPolicyInvalid is returned for a malformed or out-of-range retry policy.
	ErrPolicyInvalid = errors.New("invalid retry policy")
	// ErrPolicyNeedsIdempotent is returned when retry_on lists a class other
	// than prestart and the job was not submitted with --idempotent.
	ErrPolicyNeedsIdempotent = errors.New("retry_on beyond prestart requires --idempotent")
)

// Backoff is the retry delay curve: min(MaxS, BaseS·Factor^(attempt-1))·(1±Jitter).
type Backoff struct {
	BaseS  float64 `json:"base_s"`
	Factor float64 `json:"factor"`
	MaxS   float64 `json:"max_s"`
	Jitter float64 `json:"jitter"`
}

// RetryPolicy decides whether a failed attempt is retried and after how long.
type RetryPolicy struct {
	MaxAttempts int      `json:"max_attempts"`
	Backoff     Backoff  `json:"backoff"`
	RetryOn     []string `json:"retry_on"`
}

// DefaultRetryPolicy is the policy of a job whose retry_policy is NULL.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 1,
		Backoff:     Backoff{BaseS: 30, Factor: 2, MaxS: 1800, Jitter: 0.2},
		RetryOn:     []string{ClassPrestart},
	}
}

// retryableClasses are the only classes retry_on may list.
var retryableClasses = map[string]bool{
	ClassPrestart:           true,
	ClassCapacity:           true,
	ClassAgentFailed:        true,
	ClassFailedUnclassified: true,
	ClassIndeterminate:      true,
}

// finishClasses are the classes a finished attempt may report to Finalize.
var finishClasses = map[string]bool{
	ClassOK:                 true,
	ClassPrestart:           true,
	ClassCapacity:           true,
	ClassAgentFailed:        true,
	ClassFailedUnclassified: true,
	ClassIndeterminate:      true,
	ClassConfigError:        true,
	ClassTimeout:            true,
	ClassCancelled:          true,
	ClassUnknown:            true,
}

// ParseRetryPolicy parses and validates a stored or submitted policy. An empty
// string is the default; omitted fields take the default's values. Naming a
// class other than prestart in retry_on needs idempotent, because a retry may
// run the work a second time.
func ParseRetryPolicy(raw string, idempotent bool) (RetryPolicy, error) {
	p := DefaultRetryPolicy()
	if raw == "" {
		return p, nil
	}
	var in struct {
		MaxAttempts *int `json:"max_attempts"`
		Backoff     *struct {
			BaseS  *float64 `json:"base_s"`
			Factor *float64 `json:"factor"`
			MaxS   *float64 `json:"max_s"`
			Jitter *float64 `json:"jitter"`
		} `json:"backoff"`
		RetryOn *[]string `json:"retry_on"`
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return RetryPolicy{}, fmt.Errorf("%w: %v", ErrPolicyInvalid, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return RetryPolicy{}, fmt.Errorf("%w: trailing data after the policy object", ErrPolicyInvalid)
	}
	if in.MaxAttempts != nil {
		p.MaxAttempts = *in.MaxAttempts
	}
	if in.Backoff != nil {
		if in.Backoff.BaseS != nil {
			p.Backoff.BaseS = *in.Backoff.BaseS
		}
		if in.Backoff.Factor != nil {
			p.Backoff.Factor = *in.Backoff.Factor
		}
		if in.Backoff.MaxS != nil {
			p.Backoff.MaxS = *in.Backoff.MaxS
		}
		if in.Backoff.Jitter != nil {
			p.Backoff.Jitter = *in.Backoff.Jitter
		}
	}
	if in.RetryOn != nil {
		p.RetryOn = append([]string{}, (*in.RetryOn)...)
	}

	if p.MaxAttempts < 1 || p.MaxAttempts > maxPolicyAttempts {
		return RetryPolicy{}, fmt.Errorf("%w: max_attempts %d is outside 1..%d", ErrPolicyInvalid, p.MaxAttempts, maxPolicyAttempts)
	}
	b := p.Backoff
	switch {
	case !(b.BaseS > 0) || math.IsInf(b.BaseS, 0):
		return RetryPolicy{}, fmt.Errorf("%w: backoff.base_s must be positive", ErrPolicyInvalid)
	case !(b.Factor >= 1) || math.IsInf(b.Factor, 0):
		return RetryPolicy{}, fmt.Errorf("%w: backoff.factor must be at least 1", ErrPolicyInvalid)
	case !(b.MaxS >= b.BaseS) || math.IsInf(b.MaxS, 0):
		return RetryPolicy{}, fmt.Errorf("%w: backoff.max_s must be at least base_s", ErrPolicyInvalid)
	case !(b.Jitter >= 0 && b.Jitter < 1):
		return RetryPolicy{}, fmt.Errorf("%w: backoff.jitter must be in [0, 1)", ErrPolicyInvalid)
	}
	needsIdempotent := false
	for _, c := range p.RetryOn {
		if !retryableClasses[c] {
			return RetryPolicy{}, fmt.Errorf("%w: class %q cannot be retried", ErrPolicyInvalid, c)
		}
		if c != ClassPrestart {
			needsIdempotent = true
		}
	}
	if needsIdempotent && !idempotent {
		return RetryPolicy{}, ErrPolicyNeedsIdempotent
	}
	return p, nil
}

// ShouldRetry reports whether a failed attempt returns the job to pending.
// There are no refunds: attemptSeq counts every claim, and only excused
// crashes (crashRequeues) are subtracted. config_error, cancelled, unknown and
// ok are never retried, and indeterminate only when the policy names it.
func ShouldRetry(p RetryPolicy, class string, attemptSeq, crashRequeues int, retryBlocked bool) bool {
	if retryBlocked || !retryableClasses[class] {
		return false
	}
	listed := false
	for _, c := range p.RetryOn {
		if c == class {
			listed = true
			break
		}
	}
	if !listed {
		return false
	}
	return attemptSeq-crashRequeues < p.MaxAttempts
}

// BackoffDelay is the wait before the attempt after attemptSeq. rnd returns a
// value in [0,1); nil uses math/rand. The result is never negative, and
// capacity and unclassified failures wait at least 15 minutes.
func BackoffDelay(p RetryPolicy, class string, attemptSeq int, rnd func() float64) time.Duration {
	if rnd == nil {
		rnd = rand.Float64
	}
	exp := attemptSeq - 1
	if exp < 0 {
		exp = 0
	}
	secs := p.Backoff.BaseS * math.Pow(p.Backoff.Factor, float64(exp))
	if math.IsNaN(secs) || secs > p.Backoff.MaxS {
		secs = p.Backoff.MaxS
	}
	secs *= 1 + p.Backoff.Jitter*(2*rnd()-1)
	d := time.Duration(secs * float64(time.Second))
	if class == ClassCapacity || class == ClassFailedUnclassified {
		if d < capacityBackoffFloor {
			d = capacityBackoffFloor
		}
	}
	if d < 0 {
		d = 0
	}
	return d
}

// terminalStatus maps a final outcome class to the job's terminal status.
func terminalStatus(class string) JobStatus {
	switch class {
	case ClassOK:
		return StatusCompleted
	case ClassCancelled:
		return StatusCancelled
	default:
		return StatusFailed
	}
}
