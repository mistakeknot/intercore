// Package scheduler provides a fair spawn scheduler with paced dispatch creation.
// It serializes and paces all agent dispatches to prevent resource exhaustion
// and rate limit errors.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// SpawnExecutor is a function that executes a spawn job.
type SpawnExecutor func(ctx context.Context, job *SpawnJob) error

// Hooks contains callbacks for job lifecycle events.
type Hooks struct {
	// OnJobEnqueued is called when a job is added to the queue.
	OnJobEnqueued func(job *SpawnJob)

	// OnJobStarted is called when a job starts executing.
	OnJobStarted func(job *SpawnJob)

	// OnJobCompleted is called when a job completes successfully.
	OnJobCompleted func(job *SpawnJob)

	// OnJobFailed is called when a job fails.
	OnJobFailed func(job *SpawnJob, err error)

	// OnJobRetrying is called when a job is about to retry.
	OnJobRetrying func(job *SpawnJob, attempt int)

	// OnBackpressure is called when the queue is experiencing backpressure.
	OnBackpressure func(queueSize int, waitTime time.Duration)
}

// Config configures the scheduler.
type Config struct {
	// MaxConcurrent is the maximum number of concurrent spawn operations.
	MaxConcurrent int `json:"max_concurrent"`

	// GlobalRateLimit is the global rate limiter configuration.
	GlobalRateLimit LimiterConfig `json:"global_rate_limit"`

	// AgentRateLimits is the per-agent rate limiter configuration.
	AgentRateLimits AgentLimiterConfig `json:"agent_rate_limits"`

	// AgentCaps is the per-agent concurrency caps configuration.
	AgentCaps AgentCapsConfig `json:"agent_caps"`

	// FairScheduler is the fair scheduler configuration.
	FairScheduler FairSchedulerConfig `json:"fair_scheduler"`

	// Backoff is the backoff configuration for resource errors.
	Backoff BackoffConfig `json:"backoff"`

	// MaxCompleted is the number of completed jobs to retain for status.
	MaxCompleted int `json:"max_completed"`

	// DefaultRetries is the default number of retries for failed jobs.
	DefaultRetries int `json:"default_retries"`

	// DefaultRetryDelay is the default delay between retries.
	DefaultRetryDelay time.Duration `json:"default_retry_delay"`

	// BackpressureThreshold is the queue size that triggers backpressure alerts.
	BackpressureThreshold int `json:"backpressure_threshold"`
}

// DefaultConfig returns sensible default configuration.
func DefaultConfig() Config {
	return Config{
		MaxConcurrent:         4,
		GlobalRateLimit:       DefaultLimiterConfig(),
		AgentRateLimits:       DefaultAgentLimiterConfig(),
		AgentCaps:             DefaultAgentCapsConfig(),
		FairScheduler:         DefaultFairSchedulerConfig(),
		Backoff:               DefaultBackoffConfig(),
		MaxCompleted:          100,
		DefaultRetries:        3,
		DefaultRetryDelay:     time.Second,
		BackpressureThreshold: 50,
	}
}

// Stats contains scheduler statistics.
type Stats struct {
	TotalSubmitted   int64         `json:"total_submitted"`
	TotalCompleted   int64         `json:"total_completed"`
	TotalFailed      int64         `json:"total_failed"`
	TotalRetried     int64         `json:"total_retried"`
	CurrentQueueSize int           `json:"current_queue_size"`
	CurrentRunning   int           `json:"current_running"`
	AvgQueueTime     time.Duration `json:"avg_queue_time"`
	AvgExecutionTime time.Duration `json:"avg_execution_time"`
	IsPaused         bool          `json:"is_paused"`
	StartedAt        time.Time     `json:"started_at"`
	Uptime           time.Duration `json:"uptime"`
	LimiterStats     LimiterStats  `json:"limiter_stats"`
	QueueStats       QueueStats    `json:"queue_stats"`
	BackoffStats     BackoffStats  `json:"backoff_stats"`
	CapsStats        CapsStats     `json:"caps_stats"`
	InGlobalBackoff  bool          `json:"in_global_backoff"`
	RemainingBackoff time.Duration `json:"remaining_backoff,omitempty"`
}

// Scheduler is the global spawn scheduler that serializes and paces
// all agent dispatch creation operations.
type Scheduler struct {
	mu sync.RWMutex

	config        Config
	queue         *FairScheduler
	globalLimiter *RateLimiter
	agentLimiters *PerAgentLimiter
	agentCaps     *AgentCaps
	backoff       *BackoffController

	// running tracks currently executing jobs.
	running map[string]*SpawnJob

	// completed tracks recently completed jobs for status queries.
	completed    []*SpawnJob
	maxCompleted int

	workers  int
	executor SpawnExecutor
	hooks    Hooks

	// running state
	started   atomic.Bool
	ctx       context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup
	jobNotify chan struct{}

	stats  Stats
	paused atomic.Bool
}

// New creates a new scheduler with the given configuration.
func New(cfg Config) *Scheduler {
	ctx, cancel := context.WithCancel(context.Background())

	s := &Scheduler{
		config:        cfg,
		queue:         NewFairScheduler(cfg.FairScheduler),
		globalLimiter: NewRateLimiter(cfg.GlobalRateLimit),
		agentLimiters: NewPerAgentLimiter(cfg.AgentRateLimits),
		agentCaps:     NewAgentCaps(cfg.AgentCaps),
		backoff:       NewBackoffController(cfg.Backoff),
		running:       make(map[string]*SpawnJob),
		completed:     make([]*SpawnJob, 0, cfg.MaxCompleted),
		maxCompleted:  cfg.MaxCompleted,
		workers:       cfg.MaxConcurrent,
		ctx:           ctx,
		cancel:        cancel,
		jobNotify:     make(chan struct{}, 1),
	}

	// Set scheduler reference for global backoff pause/resume.
	s.backoff.SetScheduler(s)

	return s
}

// SetExecutor sets the function that executes spawn jobs.
func (s *Scheduler) SetExecutor(executor SpawnExecutor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.executor = executor
}

// SetHooks sets the lifecycle hooks.
func (s *Scheduler) SetHooks(hooks Hooks) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hooks = hooks
}

// Start starts the scheduler workers.
func (s *Scheduler) Start() error {
	if s.started.Load() {
		return fmt.Errorf("scheduler already started")
	}

	s.mu.Lock()
	if s.executor == nil {
		s.mu.Unlock()
		return fmt.Errorf("executor not set")
	}
	s.stats.StartedAt = time.Now()
	s.mu.Unlock()

	s.started.Store(true)

	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker(i)
	}

	slog.Info("scheduler started", "workers", s.workers)
	return nil
}

// Stop gracefully stops the scheduler.
func (s *Scheduler) Stop() {
	if !s.started.Load() {
		return
	}

	s.cancel()
	s.wg.Wait()
	s.started.Store(false)

	slog.Info("scheduler stopped")
}

// Submit submits a new spawn job to the scheduler.
func (s *Scheduler) Submit(job *SpawnJob) error {
	if !s.started.Load() {
		return fmt.Errorf("scheduler not started")
	}

	if job.ID == "" {
		job.ID = generateID()
	}
	if job.MaxRetries == 0 {
		job.MaxRetries = s.config.DefaultRetries
	}
	if job.RetryDelay == 0 {
		job.RetryDelay = s.config.DefaultRetryDelay
	}

	job.SetStatus(StatusPending)
	s.queue.Enqueue(job)

	atomic.AddInt64(&s.stats.TotalSubmitted, 1)

	// Check for backpressure.
	queueSize := s.queue.Queue().Len()
	if queueSize >= s.config.BackpressureThreshold {
		if s.hooks.OnBackpressure != nil {
			waitTime := s.globalLimiter.TimeUntilNextToken()
			s.hooks.OnBackpressure(queueSize, waitTime)
		}
	}

	if s.hooks.OnJobEnqueued != nil {
		s.hooks.OnJobEnqueued(job)
	}

	// Notify workers.
	select {
	case s.jobNotify <- struct{}{}:
	default:
	}

	return nil
}

// SubmitBatch submits multiple jobs as a batch.
func (s *Scheduler) SubmitBatch(jobs []*SpawnJob) (string, error) {
	if len(jobs) == 0 {
		return "", nil
	}

	batchID := generateID()
	for _, job := range jobs {
		job.BatchID = batchID
		if err := s.Submit(job); err != nil {
			s.CancelBatch(batchID)
			return "", err
		}
	}

	return batchID, nil
}

// Cancel cancels a job by ID.
func (s *Scheduler) Cancel(jobID string) bool {
	if job := s.queue.Queue().Remove(jobID); job != nil {
		job.Cancel()
		return true
	}

	s.mu.Lock()
	if job, ok := s.running[jobID]; ok {
		job.Cancel()
		s.mu.Unlock()
		return true
	}
	s.mu.Unlock()

	return false
}

// CancelSession cancels all jobs for a session.
func (s *Scheduler) CancelSession(sessionName string) int {
	cancelled := s.queue.Queue().CancelSession(sessionName)

	s.mu.Lock()
	for _, job := range s.running {
		if job.SessionName == sessionName {
			job.Cancel()
			cancelled = append(cancelled, job)
		}
	}
	s.mu.Unlock()

	return len(cancelled)
}

// CancelBatch cancels all jobs in a batch.
func (s *Scheduler) CancelBatch(batchID string) int {
	cancelled := s.queue.Queue().CancelBatch(batchID)

	s.mu.Lock()
	for _, job := range s.running {
		if job.BatchID == batchID {
			job.Cancel()
			cancelled = append(cancelled, job)
		}
	}
	s.mu.Unlock()

	return len(cancelled)
}

// Pause pauses job processing.
func (s *Scheduler) Pause() {
	s.paused.Store(true)
	slog.Info("scheduler paused")
}

// Resume resumes job processing.
func (s *Scheduler) Resume() {
	s.paused.Store(false)
	slog.Info("scheduler resumed")

	select {
	case s.jobNotify <- struct{}{}:
	default:
	}
}

// IsPaused returns true if the scheduler is paused.
func (s *Scheduler) IsPaused() bool {
	return s.paused.Load()
}

// GetJob returns a job by ID (checks queue, running, then completed).
func (s *Scheduler) GetJob(jobID string) *SpawnJob {
	if job := s.queue.Queue().Get(jobID); job != nil {
		return job.Clone()
	}

	s.mu.RLock()
	if job, ok := s.running[jobID]; ok {
		s.mu.RUnlock()
		return job.Clone()
	}
	s.mu.RUnlock()

	s.mu.RLock()
	for _, job := range s.completed {
		if job.ID == jobID {
			s.mu.RUnlock()
			return job.Clone()
		}
	}
	s.mu.RUnlock()

	return nil
}

// GetQueuedJobs returns all queued jobs.
func (s *Scheduler) GetQueuedJobs() []*SpawnJob {
	jobs := s.queue.Queue().ListAll()
	result := make([]*SpawnJob, len(jobs))
	for i, job := range jobs {
		result[i] = job.Clone()
	}
	return result
}

// GetRunningJobs returns all currently running jobs.
func (s *Scheduler) GetRunningJobs() []*SpawnJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := make([]*SpawnJob, 0, len(s.running))
	for _, job := range s.running {
		result = append(result, job.Clone())
	}
	return result
}

// GetRecentCompleted returns recently completed jobs.
func (s *Scheduler) GetRecentCompleted(limit int) []*SpawnJob {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > len(s.completed) {
		limit = len(s.completed)
	}

	result := make([]*SpawnJob, limit)
	for i := 0; i < limit; i++ {
		result[i] = s.completed[len(s.completed)-1-i].Clone()
	}
	return result
}

// Stats returns scheduler statistics.
func (s *Scheduler) GetStats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	stats := s.stats
	stats.CurrentQueueSize = s.queue.Queue().Len()
	stats.CurrentRunning = len(s.running)
	stats.IsPaused = s.paused.Load()
	if !stats.StartedAt.IsZero() {
		stats.Uptime = time.Since(stats.StartedAt)
	}
	stats.LimiterStats = s.globalLimiter.Stats()
	stats.QueueStats = s.queue.Queue().Stats()
	stats.BackoffStats = s.backoff.Stats()
	stats.CapsStats = s.agentCaps.Stats()
	stats.InGlobalBackoff = s.backoff.IsInGlobalBackoff()
	stats.RemainingBackoff = s.backoff.RemainingBackoff()

	return stats
}

// worker is a goroutine that processes jobs from the queue.
func (s *Scheduler) worker(id int) {
	defer s.wg.Done()

	slog.Debug("worker started", "worker_id", id)

	for {
		select {
		case <-s.ctx.Done():
			slog.Debug("worker stopping", "worker_id", id)
			return
		case <-s.jobNotify:
			s.processJobs(id)
		case <-time.After(100 * time.Millisecond):
			s.processJobs(id)
		}
	}
}

// processJobs processes available jobs.
func (s *Scheduler) processJobs(workerID int) {
	for {
		select {
		case <-s.ctx.Done():
			return
		default:
		}

		if s.paused.Load() {
			return
		}

		job := s.queue.TryDequeue()
		if job == nil {
			return
		}

		// Check agent concurrency cap.
		if job.AgentType != "" {
			if !s.agentCaps.TryAcquire(job.AgentType) {
				s.queue.Enqueue(job)

				job = s.findJobWithAvailableCap()
				if job == nil {
					return
				}
			}
		}

		// Wait for per-agent rate limit.
		if job.AgentType != "" {
			if err := s.agentLimiters.Wait(s.ctx, job.AgentType); err != nil {
				s.agentCaps.Release(job.AgentType)
				s.queue.Enqueue(job)
				return
			}
		}

		// Wait for global rate limit.
		if err := s.globalLimiter.Wait(s.ctx); err != nil {
			if job.AgentType != "" {
				s.agentCaps.Release(job.AgentType)
			}
			s.queue.Enqueue(job)
			return
		}

		s.executeJob(workerID, job)
	}
}

// findJobWithAvailableCap tries to find a job whose agent type has available capacity.
func (s *Scheduler) findJobWithAvailableCap() *SpawnJob {
	jobs := s.queue.Queue().ListAll()
	for _, job := range jobs {
		if job.AgentType == "" || s.agentCaps.TryAcquire(job.AgentType) {
			if s.queue.Queue().Remove(job.ID) != nil {
				return job
			}
			if job.AgentType != "" {
				s.agentCaps.Release(job.AgentType)
			}
		}
	}
	return nil
}

// executeJob executes a single job.
func (s *Scheduler) executeJob(workerID int, job *SpawnJob) {
	job.SetStatus(StatusRunning)

	s.mu.Lock()
	s.running[job.ID] = job
	s.mu.Unlock()

	if s.hooks.OnJobStarted != nil {
		s.hooks.OnJobStarted(job)
	}

	slog.Debug("executing job",
		"worker_id", workerID,
		"job_id", job.ID,
		"type", job.Type,
		"session", job.SessionName,
	)

	s.mu.RLock()
	executor := s.executor
	s.mu.RUnlock()

	err := executor(job.Context(), job)

	s.mu.Lock()
	delete(s.running, job.ID)
	s.mu.Unlock()

	s.queue.MarkComplete(job)

	if err != nil {
		if job.IsCancelled() {
			job.SetStatus(StatusCancelled)
			if job.AgentType != "" {
				s.agentCaps.Release(job.AgentType)
			}
		} else {
			s.handleJobError(job, err)
			return
		}
	} else {
		job.SetStatus(StatusCompleted)
		atomic.AddInt64(&s.stats.TotalCompleted, 1)

		s.backoff.RecordSuccess()

		if job.AgentType != "" {
			s.agentCaps.RecordSuccess(job.AgentType)
			s.agentCaps.Release(job.AgentType)
		}

		if s.hooks.OnJobCompleted != nil {
			s.hooks.OnJobCompleted(job)
		}
	}

	s.addToCompleted(job)

	if job.Callback != nil {
		job.Callback(job)
	}
}

// handleJobError classifies the error and decides whether to retry.
func (s *Scheduler) handleJobError(job *SpawnJob, err error) {
	stderrHint := ""
	if hint, ok := job.Metadata["stderr"].(string); ok {
		stderrHint = hint
	}
	exitCode := 0
	if code, ok := job.Metadata["exit_code"].(int); ok {
		exitCode = code
	}

	resErr := ClassifyError(err, exitCode, stderrHint)

	// Record failure for cap cooldown on resource errors.
	if resErr != nil && resErr.Retryable && job.AgentType != "" {
		s.agentCaps.RecordFailure(job.AgentType)
	}

	shouldRetry, backoffDelay := s.backoff.HandleError(job, resErr)

	// Release agent cap before retry delay.
	if job.AgentType != "" {
		s.agentCaps.Release(job.AgentType)
	}

	if (shouldRetry || resErr == nil) && job.CanRetry() {
		job.IncrementRetry()
		atomic.AddInt64(&s.stats.TotalRetried, 1)

		if s.hooks.OnJobRetrying != nil {
			s.hooks.OnJobRetrying(job, job.RetryCount)
		}

		delay := job.RetryDelay
		if backoffDelay > 0 {
			delay = backoffDelay
		}

		slog.Info("retrying job after delay",
			"job_id", job.ID,
			"retry_count", job.RetryCount,
			"delay", delay,
			"resource_error", resErr != nil && resErr.Retryable,
		)

		time.AfterFunc(delay, func() {
			job.SetStatus(StatusPending)
			s.queue.Enqueue(job)
			select {
			case s.jobNotify <- struct{}{}:
			default:
			}
		})
		return
	}

	// No more retries — mark failed.
	job.SetStatus(StatusFailed)
	job.SetError(err)
	atomic.AddInt64(&s.stats.TotalFailed, 1)

	if s.hooks.OnJobFailed != nil {
		s.hooks.OnJobFailed(job, err)
	}

	s.addToCompleted(job)

	if job.Callback != nil {
		job.Callback(job)
	}
}

// addToCompleted adds a job to the completed list with ring-buffer trimming.
func (s *Scheduler) addToCompleted(job *SpawnJob) {
	s.mu.Lock()
	s.completed = append(s.completed, job.Clone())
	if len(s.completed) > s.maxCompleted {
		s.completed = s.completed[len(s.completed)-s.maxCompleted:]
	}
	s.mu.Unlock()
}

// generateID generates a random hex ID.
func generateID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
