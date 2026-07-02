package dispatch

import (
	"context"
	"fmt"
	"math"
	"time"
)

// RetryPolicy configures exponential backoff for dispatch retries.
type RetryPolicy struct {
	MaxRetries     int           // maximum number of retry attempts (0 = no retries)
	BaseBackoff    time.Duration // initial backoff duration (default: 5s)
	MaxBackoff     time.Duration // maximum backoff cap (default: 5m)
	BackoffFactor  float64       // multiplier per attempt (default: 2.0)
	RetryOnTimeout bool          // whether to retry on timeout status (default: true)
}

// DefaultRetryPolicy returns a sensible default retry policy.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxRetries:     3,
		BaseBackoff:    5 * time.Second,
		MaxBackoff:     5 * time.Minute,
		BackoffFactor:  2.0,
		RetryOnTimeout: true,
	}
}

// Backoff computes the backoff duration for a given attempt number (0-indexed).
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	if attempt <= 0 {
		return p.baseBackoff()
	}
	factor := p.backoffFactor()
	d := time.Duration(float64(p.baseBackoff()) * math.Pow(factor, float64(attempt)))
	if max := p.maxBackoff(); d > max {
		d = max
	}
	return d
}

func (p RetryPolicy) baseBackoff() time.Duration {
	if p.BaseBackoff <= 0 {
		return 5 * time.Second
	}
	return p.BaseBackoff
}

func (p RetryPolicy) maxBackoff() time.Duration {
	if p.MaxBackoff <= 0 {
		return 5 * time.Minute
	}
	return p.MaxBackoff
}

func (p RetryPolicy) backoffFactor() float64 {
	if p.BackoffFactor <= 0 {
		return 2.0
	}
	return p.BackoffFactor
}

// ShouldRetry returns true if the dispatch is eligible for retry under the given policy.
func ShouldRetry(d *Dispatch, policy RetryPolicy) bool {
	if d == nil {
		return false
	}
	if policy.MaxRetries <= 0 {
		return false
	}
	if d.RetryCount >= policy.MaxRetries {
		return false
	}
	switch d.Status {
	case StatusFailed:
		return true
	case StatusTimeout:
		return policy.RetryOnTimeout
	default:
		return false
	}
}

// RetryResult holds the result of a retry operation.
type RetryResult struct {
	OriginalID string
	NewID      string
	Attempt    int
	BackoffMs  int64
}

// Retry creates a new dispatch that re-runs a failed/timeout dispatch.
// It copies the original dispatch's configuration, increments RetryCount,
// and records the retry relationship via ParentDispatchID.
// The caller is responsible for waiting the backoff duration before calling Retry.
func Retry(ctx context.Context, store *Store, originalID string, policy RetryPolicy) (*RetryResult, error) {
	orig, err := store.Get(ctx, originalID)
	if err != nil {
		return nil, fmt.Errorf("retry: get original: %w", err)
	}

	if !ShouldRetry(orig, policy) {
		if orig.RetryCount >= policy.MaxRetries {
			return nil, fmt.Errorf("retry: max retries (%d) exceeded for dispatch %s", policy.MaxRetries, originalID)
		}
		return nil, fmt.Errorf("retry: dispatch %s is not retryable (status=%s)", originalID, orig.Status)
	}

	attempt := orig.RetryCount + 1

	// Build the retry dispatch — copy all configuration fields
	d := &Dispatch{
		AgentType:        orig.AgentType,
		ProjectDir:       orig.ProjectDir,
		PromptFile:       orig.PromptFile,
		PromptHash:       orig.PromptHash,
		OutputFile:       nil, // new output file will be generated
		VerdictFile:      nil,
		Name:             orig.Name,
		Model:            orig.Model,
		Sandbox:          orig.Sandbox,
		SandboxSpec:      orig.SandboxSpec,
		TimeoutSec:       orig.TimeoutSec,
		ScopeID:          orig.ScopeID,
		ParentID:         orig.ParentID,
		RetryCount:       attempt,
		ParentDispatchID: originalID,
		SpawnDepth:       orig.SpawnDepth,
	}

	newID, err := store.Create(ctx, d)
	if err != nil {
		return nil, fmt.Errorf("retry: create: %w", err)
	}

	backoff := policy.Backoff(orig.RetryCount)

	return &RetryResult{
		OriginalID: originalID,
		NewID:      newID,
		Attempt:    attempt,
		BackoffMs:  backoff.Milliseconds(),
	}, nil
}

// RetryWithBackoff creates a retry dispatch and waits the computed backoff
// duration before returning. The new dispatch is created immediately (in
// "spawned" status) but the caller should start the actual process after
// this function returns.
func RetryWithBackoff(ctx context.Context, store *Store, originalID string, policy RetryPolicy) (*RetryResult, error) {
	result, err := Retry(ctx, store, originalID, policy)
	if err != nil {
		return nil, err
	}

	backoff := policy.Backoff(result.Attempt - 1)
	if backoff > 0 {
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}

	return result, nil
}

// ListRetryChain returns all dispatches in a retry chain, starting from the
// original and following ParentDispatchID links. Useful for debugging.
func ListRetryChain(ctx context.Context, store *Store, dispatchID string) ([]*Dispatch, error) {
	var chain []*Dispatch
	seen := make(map[string]bool)

	// Walk backwards to find the root
	current := dispatchID
	for current != "" && !seen[current] {
		seen[current] = true
		d, err := store.Get(ctx, current)
		if err != nil {
			break
		}
		chain = append([]*Dispatch{d}, chain...)
		current = d.ParentDispatchID
	}

	// Walk forwards from root to find retries
	if len(chain) > 0 {
		root := chain[0]
		chain = []*Dispatch{root}
		// Find dispatches that reference this chain via parent_dispatch_id
		all, err := store.List(ctx, root.ScopeID)
		if err != nil {
			return chain, nil // best-effort
		}
		// Build forward chain by following parent links
		childMap := make(map[string]*Dispatch)
		for _, d := range all {
			if d.ParentDispatchID != "" {
				childMap[d.ParentDispatchID] = d
			}
		}
		cur := root.ID
		for {
			child, ok := childMap[cur]
			if !ok {
				break
			}
			chain = append(chain, child)
			cur = child.ID
		}
	}

	return chain, nil
}
