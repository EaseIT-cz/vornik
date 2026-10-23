// Package scheduler provides task scheduling for vornik.
package scheduler

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	mrand "math/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/runtime"
)

// TaskRepository is the interface for task persistence operations.
// This is a subset of persistence.TaskRepository used by the scheduler.
type TaskRepository interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
	LeaseTask(ctx context.Context, opts persistence.LeaseOptions) (*persistence.Task, error)
	RenewLease(ctx context.Context, taskID, leaseID string, extendBySeconds int) error
	ReleaseLease(ctx context.Context, taskID, leaseID string, newStatus persistence.TaskStatus, opts persistence.ReleaseOptions) error
	FindExpiredLeases(ctx context.Context, limit int) ([]*persistence.Task, error)
	CountByStatus(ctx context.Context, projectID string) (map[persistence.TaskStatus]int64, error)
}

// RuntimeManager is the subset of runtime operations the scheduler needs for
// the first end-to-end execution path.
type RuntimeManager interface {
	StartContainer(ctx context.Context, config *runtime.ContainerConfig) (string, error)
	WaitForExit(ctx context.Context, containerID string, timeout time.Duration) (int, error)
	RemoveContainer(ctx context.Context, containerID string, force bool) error
}

// ExecutionRepository is the subset of execution persistence operations used by
// the scheduler-driven execution path.
type ExecutionRepository interface {
	Create(ctx context.Context, execution *persistence.Execution) error
	GetByTaskID(ctx context.Context, taskID string) (*persistence.Execution, error)
	UpdateStatus(ctx context.Context, id string, status persistence.ExecutionStatus) error
	RecordCompletion(ctx context.Context, id string, result []byte) error
	RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error
}

// Executor dispatches task execution outside the scheduler's fallback path.
type Executor interface {
	ExecuteWithContext(ctx context.Context, taskID string) error
	IsExecuting(taskID string) bool
	// Cancel stops a running execution. Used by dispatchViaExecutor's
	// renewal loop when consecutive RenewLease failures escalate to
	// dispatchFailed — the executor goroutine MUST be stopped before
	// the scheduler releases the slot, otherwise a re-leased task
	// can dispatch to a second executor while the original is still
	// running (double-execution).
	Cancel(taskID string) error
}

// RetryHoldSource is implemented by an executor that knows, for a failed
// attempt it left for the scheduler to retry, a time before which the retry
// cannot succeed: the moment the model circuit that rejected it can admit a
// call (permanent-failure design §8.8, GitHub #363). Asserted on the
// scheduler's Executor at the point of use, so an executor that does not
// implement it holds nothing. A miss is (zero, false): no hold.
type RetryHoldSource interface {
	RetryNotBefore(taskID string) (time.Time, bool)
}

// ProjectRegistry provides read-only access to project scheduling
// metadata: concurrency caps + priorities. Both feed the lease query
// — concurrency caps gate eligibility (LLD §4.1), priorities order
// candidates (LLD §4.2).
type ProjectRegistry interface {
	ProjectConcurrencyLimits() map[string]int
	ProjectPriorities() map[string]int
	// ArchivedProjectIDs returns the set of project IDs whose
	// lifecycle is "archived". The lease query excludes them so
	// queued work on an archived project stops dispatching even
	// before the archive-sweeper wipes the rows. Empty / nil
	// disables the filter. Lifecycle archival is a follow-on to
	// the archive feature shipped in commit 202ce2b; nil-returning
	// implementations keep the pre-feature behaviour.
	ArchivedProjectIDs() []string
}

type dispatchOutcome int

const (
	dispatchSucceeded dispatchOutcome = iota
	dispatchFailed
	dispatchPaused
	dispatchTerminated  // task was externally set to a terminal status (e.g. cancelled via UI)
	dispatchInterrupted // scheduler shutdown interrupted in-flight work; requeue without burning an attempt
)

// Config holds scheduler configuration.
type Config struct {
	// MaxConcurrency is the maximum number of tasks to run concurrently (0 = unlimited).
	MaxConcurrency int

	// LeaseDurationSeconds is how long a task lease is valid.
	LeaseDurationSeconds int

	// PollInterval is how often to check for new tasks.
	PollInterval time.Duration

	// RecoveryInterval is how often to check for expired leases.
	RecoveryInterval time.Duration

	// RecoveryBatchSize is the maximum number of expired leases to recover at once.
	RecoveryBatchSize int

	// RecoveryIdleGrace is the minimum continuous duration that
	// `executor.IsExecuting(taskID) == false` must hold before the
	// recovery sweep treats the task as orphaned. A single transient
	// false (the brief gap between step transitions, a goroutine being
	// re-registered, etc.) is not sufficient — the executor must report
	// idle for this long without interruption.
	//
	// Without the grace period, a long-running task whose DB-side lease
	// expires can be silently re-queued mid-execution, allowing a
	// sibling task on the same project to lease in violation of the
	// per-project at-most-N invariant (LLD §4.4 — "tasks rotate
	// round-robin instead of running to completion" symptom).
	//
	// Default 90 seconds = ~3 RecoveryInterval ticks at the default
	// 30s — long enough to swallow any reasonable step-transition gap
	// without delaying real orphan recovery materially.
	RecoveryIdleGrace time.Duration

	// PriorityFloor is the minimum numeric priority value for tasks to consider.
	// Lower numeric priorities are more urgent; this legacy filter excludes
	// tasks with numerically smaller priorities when set above zero.
	PriorityFloor int

	// NOTE (2026-09-02): a `RuntimeImage string` lived here and was DELETED.
	// It was written twice — DefaultConfig and the constructor's empty-check —
	// and read NOWHERE; a repo-wide grep across Go, YAML, JSON and config keys
	// found no reader, no getter, no test stub. Its default was the
	// `test-image:latest` test fixture, so every running daemon held a
	// scheduler config naming an image the release does not ship.
	//
	// The scheduler does not choose the image: the executor does, from its own
	// Config.RuntimeImage (internal/executor/executor.go), read at plan.go.
	// Do not re-add a field here "because the scheduler obviously needs to know
	// its runtime image" — that is the assumption the dead field encoded.
	// See https://docs.vornik.io §11.2.

	// ArtifactStoragePath is the base path used for artifact persistence.
	ArtifactStoragePath string

	// NOTE (2026-10-12, GitHub #142): an `ExecutionTimeout time.Duration`
	// lived here (default 30m, forced positive in NewWithOptions) and was
	// DELETED. Nothing read it and the daemon YAML could not reach it. The
	// scheduler knows no workflow, so it cannot bound an execution without a
	// second teardown path; the executor owns the execution context. The
	// setting now binds there as the default wall-clock cap
	// (`scheduler.execution_timeout` → executor.Config.DefaultMaxWallClock;
	// a workflow's maxWallClock wins). Do not re-add it here. See
	// https://docs.vornik.io §4.12 C1.

	// AbandonedExecutionGrace bounds how long a dispatch keeps its global
	// slot after the task row has left LEASED/RUNNING while the executor
	// still reports the task live: a goroutine wedged in a call with no bound
	// outlives the row its watchdog or canceller already finished. After the
	// grace the slot is released without writing the row or tearing down
	// again (05-scheduler §4.12 C5). Default 10m.
	AbandonedExecutionGrace time.Duration
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() *Config {
	return &Config{
		MaxConcurrency:          10,
		LeaseDurationSeconds:    300, // 5 minutes
		PollInterval:            1 * time.Second,
		RecoveryInterval:        30 * time.Second,
		RecoveryBatchSize:       100,
		RecoveryIdleGrace:       90 * time.Second,
		PriorityFloor:           0,
		ArtifactStoragePath:     "/var/lib/vornik/artifacts",
		AbandonedExecutionGrace: defaultAbandonedExecutionGrace,
	}
}

// Scheduler selects tasks from the queue for execution.
type Scheduler struct {
	config *Config
	repo   TaskRepository
	// configBlocked refuses dispatch for a project whose config apply is
	// unresolved (CA-20). Nil = never blocked.
	configBlocked   func(projectID string) (bool, string)
	metrics         *Metrics
	tracer          trace.Tracer
	runtime         RuntimeManager
	execRepo        ExecutionRepository
	livePub         LivePublisher
	executor        Executor
	artifactStore   *artifacts.Store
	projectRegistry ProjectRegistry
	logger          zerolog.Logger

	// runningCount tracks the number of currently running tasks.
	runningCount int

	// leaseHolder identifies this scheduler instance.
	leaseHolder string

	// mu protects mutable state.
	mu sync.Mutex

	// lastTick is the wall-clock time of the most recent runLoop tick.
	// Read by /readyz via LastTick() so the readiness probe can flag a
	// wedged scheduler (loop stuck on an LLM call, on DB I/O, or on the
	// lock) instead of cheerfully returning 200 while nothing runs.
	// Guarded by mu.
	lastTick time.Time

	// ctx and cancel control the scheduling loop.
	ctx    context.Context
	cancel context.CancelFunc

	// wakeCh lets external callers (operator-driven re-queue from
	// the conversational task lifecycle) hint that new work has
	// landed. Buffered=1 so a burst of wakes coalesces into one
	// extra schedule pass; full is fine because the existing
	// pending wake will cover the burst.
	wakeCh chan struct{}

	// started tracks if the scheduler is running.
	started bool

	// wg tracks active goroutines.
	wg sync.WaitGroup

	// dispatchWg tracks in-flight task dispatch goroutines separately from the
	// main loops so Stop can quiesce scheduling first, then wait for
	// executor-facing work without racing Add against Wait.
	dispatchWg sync.WaitGroup

	// recoveryIdleSince records, per task ID, the wall-clock time at
	// which the recovery sweep first observed `executor.IsExecuting()
	// == false` for that task. The next sweep iteration consults this
	// to apply the RecoveryIdleGrace window — a task is treated as
	// orphaned only after IsExecuting has reported false continuously
	// for that duration. Cleared on the first IsExecuting=true
	// observation OR when the task is recovered. Guarded by mu.
	//
	// LLD §4.4 / §6: this is the field that defends the per-project
	// at-most-N invariant against the rotation symptom — a transient
	// IsExecuting=false (e.g. between step transitions) doesn't
	// re-queue a healthy task.
	recoveryIdleSince map[string]time.Time

	// now is the clock the dispatch watch loop and the config-blocked
	// back-off read. A seam so tests advance time instead of sleeping.
	now func() time.Time
	// watchTicks returns the dispatch watch loop's tick channel and its stop
	// function. A seam for the same reason.
	watchTicks func() (<-chan time.Time, func())
	// watchIterated, when set (tests only), is called once per watch-loop
	// iteration, just before the loop waits for its next tick.
	watchIterated func()
	// blockedUntil holds, per project, when a config-blocked project may be
	// leased again (05-scheduler §4.12 C6). Guarded by mu.
	blockedUntil map[string]time.Time
	// retryHeldUntil holds, per task, when a retry requeued after an open
	// model circuit may be leased (permanent-failure design §8.8 D3). The
	// config-blocked back-off's mechanism, keyed by task. Guarded by mu.
	retryHeldUntil map[string]time.Time
}

// defaultAbandonedExecutionGrace is well above the longest legitimate tail
// after a row leaves RUNNING: the 60s finaliser bound plus the 30s
// container-cleanup bound (05-scheduler §4.12 C5).
const defaultAbandonedExecutionGrace = 10 * time.Minute

// configBlockedBackoff is how long a project whose dispatch was refused for an
// unresolved config apply is left out of the lease query, so the 1s poll does
// not lease and refuse its tasks on every tick (05-scheduler §4.12 C6).
const configBlockedBackoff = 30 * time.Second

// watchTickInterval is the dispatch watch loop's poll period.
const watchTickInterval = 100 * time.Millisecond

func defaultWatchTicks() (<-chan time.Time, func()) {
	t := time.NewTicker(watchTickInterval)
	return t.C, t.Stop
}

// Option is a functional option for configuring the Scheduler.
type Option func(*Scheduler)

// WithMetrics sets the metrics instance for the scheduler.
func WithMetrics(metrics *Metrics) Option {
	return func(s *Scheduler) {
		s.metrics = metrics
	}
}

// WithPrometheusRegistry creates metrics with the given Prometheus registry.
// This is a convenience option that creates a new Metrics instance.
func WithPrometheusRegistry(registry *prometheus.Registry) Option {
	return func(s *Scheduler) {
		if registry != nil {
			s.metrics = NewMetrics(registry)
		}
	}
}

// WithLogger sets the logger used for scheduler execution diagnostics.
func WithLogger(logger zerolog.Logger) Option {
	return func(s *Scheduler) {
		s.logger = logger
	}
}

// WithTracer sets the tracer for the scheduler.
func WithTracer(tracer trace.Tracer) Option {
	return func(s *Scheduler) {
		s.tracer = tracer
	}
}

// WithRuntimeManager sets the runtime manager used to launch containers.
func WithRuntimeManager(runtimeManager RuntimeManager) Option {
	return func(s *Scheduler) {
		s.runtime = runtimeManager
	}
}

// WithExecutionRepository sets the execution repository used to persist runs.
func WithExecutionRepository(repo ExecutionRepository) Option {
	return func(s *Scheduler) {
		s.execRepo = repo
	}
}

// LivePublisher is the live-observation publisher the scheduler reports
// attempt_failed on (live-task-observation design, Change 2026-10-08 C5).
type LivePublisher interface {
	Publish(ctx context.Context, executionID, kind string, payload any) int64
}

// WithLivePublisher wires the publisher; nil publishes nothing.
func WithLivePublisher(pub LivePublisher) Option {
	return func(s *Scheduler) { s.livePub = pub }
}

// WithExecutor sets the standalone executor used for task dispatch.
func WithExecutor(executor Executor) Option {
	return func(s *Scheduler) {
		s.executor = executor
	}
}

// WithArtifactStore sets the artifact store used to persist task outputs.
func WithArtifactStore(store *artifacts.Store) Option {
	return func(s *Scheduler) {
		s.artifactStore = store
	}
}

// WithProjectRegistry sets the project registry for per-project concurrency limits.
// WithConfigBlocked supplies the per-project admission gate the config apply
// journal's unresolved rows feed (LLD 2026-09-13-config-apply-journal §1.4:
// external drift "blocks affected execution and waits for an operator").
//
// Before this, ApplyEngine.BlockedProjects existed and was unit-tested with
// NO PRODUCTION CALLER: startup recovery logged that projects may be
// mid-apply and the scheduler dispatched against that config anyway
// (re-audit 2026-09-15, CA-20). A set nothing reads blocks nothing.
//
// The gate reports (blocked, reason). Nil means never blocked, which is the
// pre-journal behaviour.
func WithConfigBlocked(gate func(projectID string) (bool, string)) Option {
	return func(s *Scheduler) { s.configBlocked = gate }
}

func WithProjectRegistry(reg ProjectRegistry) Option {
	return func(s *Scheduler) {
		s.projectRegistry = reg
	}
}

// WithClock replaces the scheduler's clock: the one the dispatch watch, the
// config-blocked back-off (§4.12 C6) and the retry hold (permanent-failure
// design §8.8) read. A seam for tests that share one clock with the
// executor's circuit breaker; nil keeps time.Now.
func WithClock(now func() time.Time) Option {
	return func(s *Scheduler) {
		if now != nil {
			s.now = now
		}
	}
}

// New creates a new Scheduler instance.
// For backward compatibility, this constructor uses default options.
func New(repo TaskRepository, config *Config) *Scheduler {
	return NewWithOptions(repo, config)
}

// NewWithOptions creates a new Scheduler instance with functional options.
func NewWithOptions(repo TaskRepository, config *Config, opts ...Option) *Scheduler {
	if config == nil {
		config = DefaultConfig()
	}
	s := &Scheduler{
		config:            config,
		repo:              repo,
		leaseHolder:       "scheduler-" + generateID(),
		tracer:            otel.Tracer("vornik.io/vornik/internal/scheduler"),
		logger:            zerolog.Nop(),
		recoveryIdleSince: make(map[string]time.Time),
		wakeCh:            make(chan struct{}, 1),
		now:               time.Now,
		watchTicks:        defaultWatchTicks,
		blockedUntil:      make(map[string]time.Time),
		retryHeldUntil:    make(map[string]time.Time),
	}

	// Apply functional options
	for _, opt := range opts {
		opt(s)
	}

	if s.config.LeaseDurationSeconds <= 0 {
		s.config.LeaseDurationSeconds = 300
	}
	if s.config.PollInterval <= 0 {
		s.config.PollInterval = time.Second
	}
	if s.config.RecoveryInterval <= 0 {
		s.config.RecoveryInterval = 30 * time.Second
	}
	if s.config.RecoveryBatchSize <= 0 {
		s.config.RecoveryBatchSize = 100
	}
	if s.config.RecoveryIdleGrace <= 0 {
		s.config.RecoveryIdleGrace = 90 * time.Second
	}
	if s.config.ArtifactStoragePath == "" {
		s.config.ArtifactStoragePath = "/var/lib/vornik/artifacts"
	}
	if s.config.AbandonedExecutionGrace <= 0 {
		s.config.AbandonedExecutionGrace = defaultAbandonedExecutionGrace
	}

	return s
}

// generateID generates a unique identifier for the scheduler instance.
// A random 4-byte suffix is appended so two schedulers started within the
// same millisecond (e.g. during rapid restarts in tests) get different IDs.
func generateID() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is extraordinarily rare; fall back to nanoseconds.
		return time.Now().Format("20060102-150405.999999999")
	}
	return time.Now().Format("20060102-150405.000") + "-" + hex.EncodeToString(b)
}

// Start begins the scheduling loop.
func (s *Scheduler) Start() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return errors.New("scheduler already started")
	}

	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.started = true

	// Start the scheduling loop
	s.wg.Add(1)
	go s.runLoop()

	// Start the recovery loop
	s.wg.Add(1)
	go s.recoveryLoop()

	return nil
}

// Stop shuts down the scheduler.
func (s *Scheduler) Stop() error {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return nil
	}
	s.cancel()
	s.started = false
	s.mu.Unlock()

	// Wait for goroutines to finish
	s.wg.Wait()
	s.dispatchWg.Wait()

	return nil
}

// runLoop is the main scheduling loop.
func (s *Scheduler) runLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			s.lastTick = time.Now()
			s.mu.Unlock()
			s.schedule()
		case <-s.wakeCh:
			// Caller (operator-driven re-queue, etc.) hinted that
			// new work landed. Do a scheduling pass right away
			// instead of waiting for the next tick.
			s.mu.Lock()
			s.lastTick = time.Now()
			s.mu.Unlock()
			s.schedule()
		}
	}
}

// Wake hints the scheduler to scan for newly-queued tasks now.
// Non-blocking: if a wake is already buffered, the second call is
// a no-op (the buffered wake will still trigger one extra
// schedule pass). Used by the conversational task lifecycle's
// API handlers after re-queueing a task — keeps operator-perceived
// latency in the low-100ms range instead of one PollInterval.
func (s *Scheduler) Wake() {
	if s == nil {
		return
	}
	select {
	case s.wakeCh <- struct{}{}:
	default:
		// channel full; the existing pending wake will cover us.
	}
}

// LastTick returns the wall-clock time of the most recent runLoop tick,
// or a zero time when the scheduler has not yet ticked (not started, or
// ticker has not fired). Used by /readyz to detect a wedged loop.
func (s *Scheduler) LastTick() time.Time {
	if s == nil {
		return time.Time{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTick
}

// PollInterval returns the configured scheduler tick interval. Paired
// with LastTick for the readiness "scheduler hasn't ticked in > 2× poll"
// check, so the caller doesn't need to know the raw Config shape.
func (s *Scheduler) PollInterval() time.Duration {
	if s == nil || s.config == nil {
		return 0
	}
	return s.config.PollInterval
}

// recoveryLoop checks for and recovers expired leases.
func (s *Scheduler) recoveryLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.config.RecoveryInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.recoverExpiredLeases()
		}
	}
}

// schedule attempts to lease and dispatch tasks.
//
// Concurrency: two scheduler triggers (the periodic tick + a
// Wake() coalesced call) can land in this function simultaneously.
// The original code read runningCount under the lock, released
// the lock, then leased + incremented in a follow-up critical
// section — leaving a TOCTOU window where both goroutines saw
// the same "available" value and over-dispatched by up to N-1
// tasks beyond MaxConcurrency. Each surplus task consumed a
// container slot and a budget-counting lease row.
//
// Fix: reserve the slot atomically BEFORE the lease attempt.
// If the lease fails (no work, or a transient repo error), we
// roll back the reservation. While the DB call is in flight,
// the running count reflects the in-progress reservation, so a
// concurrent schedule() observes one fewer available slot — no
// double-spend.
func (s *Scheduler) schedule() {
	s.metrics.RecordLoop()
	for {
		s.mu.Lock()
		maxConcurrency := s.config.MaxConcurrency
		if maxConcurrency > 0 && s.runningCount >= maxConcurrency {
			s.mu.Unlock()
			return
		}
		// Reserve a slot under the lock so concurrent callers see
		// the bump immediately. If the lease fails we release it
		// at the bottom of the loop.
		s.runningCount++
		s.mu.Unlock()

		task, err := s.leaseTaskWithContext(s.baseContext())
		if err != nil {
			// Lease failed — release the reservation. Anything
			// other than "no work available" is worth logging.
			s.mu.Lock()
			s.runningCount--
			s.mu.Unlock()
			if err != persistence.ErrNoTasksAvailable && err != persistence.ErrNotFound {
				s.logger.Error().Err(err).Msg("scheduler: failed to lease task")
			}
			return
		}

		// Reservation upgraded to a real running task. dispatchWg
		// pairs with the dispatchTask goroutine's wg.Done.
		s.dispatchWg.Add(1)
		go s.dispatchTask(task)

		// Loop continues so a single schedule() call can fill any
		// remaining capacity in one pass when MaxConcurrency > 1.
		// Without MaxConcurrency configured the original code
		// looped exactly once; preserve that by breaking out.
		if maxConcurrency == 0 {
			return
		}
	}
}

// baseContext returns the scheduler context or a safe background fallback.
// Direct unit tests call internal methods before Start(), so nil must not escape.
func (s *Scheduler) baseContext() context.Context {
	if s != nil && s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// leaseTaskWithContext attempts to lease a single task with tracing.
func (s *Scheduler) leaseTaskWithContext(ctx context.Context) (*persistence.Task, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, span := s.tracer.Start(ctx, "scheduler.lease")
	defer span.End()

	startLease := time.Now()

	opts := persistence.LeaseOptions{
		LeaseHolder:          s.leaseHolder,
		LeaseDurationSeconds: s.config.LeaseDurationSeconds,
		PriorityFloor:        s.config.PriorityFloor,
	}
	if s.projectRegistry != nil {
		opts.ProjectConcurrencyLimits = s.projectRegistry.ProjectConcurrencyLimits()
		opts.ProjectPriorities = s.projectRegistry.ProjectPriorities()
		// 50 matches the project schema's documented default for
		// DefaultPriority (project.go validates 0..100). Projects
		// missing from the priorities map fall back to this so an
		// unconfigured project doesn't accidentally become
		// highest-priority.
		opts.ProjectPriorityDefault = 50
		// Archived-project hard-guard. The archive feature stops
		// new task creation at the UI/API boundary but doesn't
		// prevent already-queued tasks from leasing — operators
		// would expect "archive" to stop activity immediately,
		// not "stop new work and let pending work finish".
		opts.ExcludedProjects = s.projectRegistry.ArchivedProjectIDs()
	}
	// Config-blocked projects inside their back-off (§4.12 C6). Copied, not
	// appended in place: the registry's slice is not ours to grow.
	if backedOff := s.configBackedOffProjects(); len(backedOff) > 0 {
		opts.ExcludedProjects = append(slices.Clone(opts.ExcludedProjects), backedOff...)
	}
	// Retries held until their model's circuit can admit a call (§8.8 D3).
	opts.ExcludedTaskIDs = s.retryHeldTasks()

	task, err := s.repo.LeaseTask(ctx, opts)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return nil, err
	}
	// The exclusion list above was read before the query ran. A retry hold
	// taken in between (taskFailed holds, then releases the task QUEUED) is
	// not in it, so the query can lease the task the hold was for. Put it
	// back unchanged; the next poll excludes it (§8.8 D3).
	if s.retryHeld(task.ID) {
		if relErr := s.putBackHeld(ctx, task); relErr != nil {
			s.logger.Warn().Err(relErr).Str("task_id", task.ID).
				Msg("scheduler: could not put back a retry leased inside its hold; lease recovery will requeue it")
		}
		return nil, persistence.ErrNoTasksAvailable
	}

	// Record lease acquisition metrics
	if s.metrics != nil {
		s.metrics.RecordLeaseAcquired(task.ProjectID)
		s.metrics.RecordLeaseDuration(task.ProjectID, time.Since(startLease).Seconds())
		// Queue residency: time from task creation to lease. Recorded here —
		// the scheduler is the authoritative lease owner — rather than on the
		// (now dead) queue.Lease() path that used to emit it.
		if !task.CreatedAt.IsZero() {
			s.metrics.RecordQueueWait(task.ProjectID, time.Since(task.CreatedAt).Seconds())
		}
	}

	// Set span attributes
	span.SetAttributes(
		attribute.String("task_id", task.ID),
		attribute.String("project_id", task.ProjectID),
	)
	span.SetStatus(codes.Ok, "lease acquired")

	return task, nil
}

// leaseTask attempts to lease a single task.
func (s *Scheduler) leaseTask() (*persistence.Task, error) {
	return s.leaseTaskWithContext(s.baseContext())
}

// dispatchTask handles a leased task with tracing.
func (s *Scheduler) dispatchTask(task *persistence.Task) {
	defer s.dispatchWg.Done()

	ctx, span := s.tracer.Start(s.baseContext(), "scheduler.dispatch",
		trace.WithAttributes(
			attribute.String("task_id", task.ID),
			attribute.String("project_id", task.ProjectID),
		),
	)
	defer span.End()

	// Record task scheduled metric
	if s.metrics != nil {
		s.metrics.RecordTaskScheduled(task.ProjectID)
	}

	// Config-apply admission (§1.4). A project with an unresolved journal row
	// — half-applied, or externally drifted — must not execute against config
	// nobody has resolved. The task goes back to QUEUED with the reason on the
	// row, and the project is backed off so the poll does not lease and refuse
	// it every tick; once the operator resolves the row it runs.
	//
	// It used to go back to PENDING with no reason. The lease query takes only
	// QUEUED and nothing moves a PENDING task with no paused execution back, so
	// the task never ran even after the journal was resolved (05-scheduler
	// §4.12 as-built 6 and C6; GitHub #142, outcome-quality §S3 #14).
	if s.configBlocked != nil {
		if blocked, reason := s.configBlocked(task.ProjectID); blocked {
			s.logger.Warn().Str("task_id", task.ID).Str("project_id", task.ProjectID).
				Str("reason", reason).
				Msg("scheduler: dispatch refused, the project's configuration apply is unresolved")
			if err := s.releaseConfigBlocked(task, reason); err != nil {
				span.RecordError(fmt.Errorf("blocked-project lease release failed: %w", err))
			}
			return
		}
	}

	if s.executor != nil {
		outcome, errorMsg := s.dispatchViaExecutor(ctx, task)
		if err := s.releaseExecutorLease(task, outcome, errorMsg); err != nil {
			span.RecordError(fmt.Errorf("executor lease release failed: %w", err))
			span.SetStatus(codes.Error, err.Error())
			return
		}
		if outcome == dispatchFailed {
			span.RecordError(errors.New(errorMsg))
			span.SetStatus(codes.Error, errorMsg)
			return
		}
		return
	}

	// Fallback until all callers always wire a standalone executor.
	_ = s.completeTask(task, true, "")
}

// releaseConfigBlocked frees the slot of a dispatch refused for an unresolved
// config apply, puts the task back to QUEUED with the reason and its remedy on
// the row (attempt unchanged), and backs the project off for
// configBlockedBackoff (05-scheduler §4.12 C6).
func (s *Scheduler) releaseConfigBlocked(task *persistence.Task, reason string) error {
	s.decrementRunning()
	s.mu.Lock()
	s.blockedUntil[task.ProjectID] = s.now().Add(configBlockedBackoff)
	s.mu.Unlock()
	return s.repo.ReleaseLease(s.baseContext(), task.ID, taskLeaseID(task), persistence.TaskStatusQueued, persistence.ReleaseOptions{
		Error: "waiting: " + reason + ". Dispatch resumes on its own once the operator resolves the project's config apply journal",
	})
}

// configBackedOffProjects lists the projects still inside their
// config-blocked back-off, pruning the ones whose back-off has passed.
func (s *Scheduler) configBackedOffProjects() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []string
	for p, until := range s.blockedUntil {
		if now.Before(until) {
			out = append(out, p)
			continue
		}
		delete(s.blockedUntil, p)
	}
	return out
}

// retryHeldTasks lists the tasks whose retry hold has not yet passed,
// pruning the ones that have (permanent-failure design §8.8 D3).
func (s *Scheduler) retryHeldTasks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var out []string
	for id, until := range s.retryHeldUntil {
		if now.Before(until) {
			out = append(out, id)
			continue
		}
		delete(s.retryHeldUntil, id)
	}
	return out
}

// retryHeld reports whether taskID is inside its retry hold now.
func (s *Scheduler) retryHeld(taskID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	until, ok := s.retryHeldUntil[taskID]
	return ok && s.now().Before(until)
}

// putBackHeld returns a task leased inside its retry hold to QUEUED as it
// was: attempt, error and class unchanged (an empty Attempt keeps the
// column; KeepErrorClass keeps the class).
func (s *Scheduler) putBackHeld(ctx context.Context, task *persistence.Task) error {
	opts := persistence.ReleaseOptions{KeepErrorClass: true}
	if task.LastError != nil {
		opts.Error = *task.LastError
	}
	return s.repo.ReleaseLease(ctx, task.ID, taskLeaseID(task), persistence.TaskStatusQueued, opts)
}

// holdRetry asks the executor when the retry of taskID can first succeed and,
// when that is after now, holds the task out of the lease query until then.
// It returns the suffix the release appends to the stored error, so the
// QUEUED task says why it is not running, or "" when nothing is held. Called
// BEFORE the release to QUEUED, so a poll that starts after the release
// excludes the task; one already in flight is caught by the check after
// LeaseTask (permanent-failure design §8.8 D3 and §8.8.4, GitHub #363).
func (s *Scheduler) holdRetry(taskID string) string {
	src, ok := s.executor.(RetryHoldSource)
	if !ok {
		return ""
	}
	until, ok := src.RetryNotBefore(taskID)
	now := s.now()
	if !ok || !until.After(now) {
		return ""
	}
	s.mu.Lock()
	s.retryHeldUntil[taskID] = until
	s.mu.Unlock()
	s.logger.Info().Str("task_id", taskID).Time("held_until", until).
		Dur("hold", until.Sub(now)).
		Msg("scheduler: retry held until the model's circuit can admit a call")
	return "; retry held until " + until.UTC().Format(time.RFC3339) +
		": the model's circuit cannot admit a call before then"
}

func (s *Scheduler) dispatchViaExecutor(ctx context.Context, task *persistence.Task) (dispatchOutcome, string) {
	if err := s.executor.ExecuteWithContext(ctx, task.ID); err != nil {
		return dispatchFailed, err.Error()
	}

	ticks, stopTicks := s.watchTicks()
	defer stopTicks()
	lastRenewal := s.now()
	// Lease-renewal jitter: every dispatch picks an independent
	// interval in [base*0.75, base*1.25] where base = LeaseDuration/2.
	// Without jitter, many tasks leased at the same tick (e.g. a
	// project with concurrency=20 firing all 20 dispatches in one
	// scheduler cycle) all renew their leases at exactly base
	// seconds later, hammering the DB with N concurrent
	// UPDATE-with-row-lock calls. The jitter spreads the renewal
	// load across the half-lease window. The interval is chosen
	// once per dispatch (not per renewal) so each task's renewal
	// cadence stays predictable but globally desynchronised.
	renewInterval := computeJitteredRenewInterval(s.config.LeaseDurationSeconds)
	// Tolerate transient RenewLease failures — a single DB blip
	// must not abort the dispatch watch and re-queue the task while
	// the original executor goroutine is still running (that opens
	// a double-execution window: scheduler re-leases to a second
	// executor, original keeps writing to the workspace + burning
	// LLM budget). Only escalate to dispatchFailed after several
	// consecutive failures spanning at least one full lease.
	const maxRenewFailures = 3
	renewFailures := 0
	// leaseReleased flips true when a renewal fails with
	// ErrLeaseNotFound AND the task has already left RUNNING/LEASED —
	// i.e. the executor itself deliberately cleared the lease as part
	// of a benign in-flight transition (lead checkpoint / external_wait
	// → AWAITING_INPUT / AWAITING_EXTERNAL, pause → PAUSED, closure →
	// COMPLETED) or an operator cancelled. The renewal failure there is
	// expected, NOT a lost lease, so escalating to executor.Cancel
	// would clobber the benign at-rest status with CANCELLED. Once set,
	// we stop renewing and just wait for IsExecuting to drop; the
	// post-loop status reload classifies the real outcome.
	//
	// Bug (T-…2d9fe309771eb138, 2026-06-03): a lead checkpoint flipped
	// the task RUNNING → AWAITING_INPUT (ClearLease), the watch loop
	// kept renewing the now-cleared lease, hit 3 ErrLeaseNotFound
	// failures ~1s apart, and cancelled the task the operator was about
	// to answer — surfacing as "input requested, then cancelled instead
	// of continuing."
	leaseReleased := false
	// releasedAt is when the loop first saw the row out of LEASED/RUNNING.
	// A goroutine still live AbandonedExecutionGrace after that has outlived
	// its row — wedged in a call with no bound after the watchdog or a
	// canceller finished it — and the slot is released (§4.12 C5).
	var releasedAt time.Time

	for s.executor.IsExecuting(task.ID) {
		if leaseReleased {
			if held := s.now().Sub(releasedAt); held >= s.config.AbandonedExecutionGrace {
				s.logger.Error().
					Str("task_id", task.ID).
					Str("project_id", task.ProjectID).
					Dur("held_after_row_left_running", held).
					Msg("scheduler: the executor still reports this task live long after its row left RUNNING; " +
						"releasing the slot without writing the row or tearing down again (the writer that moved the row owns both)")
				if s.metrics != nil {
					s.metrics.RecordAbandonedDispatch(task.ProjectID)
				}
				return dispatchTerminated, ""
			}
		}
		if leaseID := taskLeaseID(task); !leaseReleased && leaseID != "" && s.now().Sub(lastRenewal) >= renewInterval {
			err := s.repo.RenewLease(ctx, task.ID, leaseID, s.config.LeaseDurationSeconds)
			switch {
			case err == nil:
				renewFailures = 0
				lastRenewal = s.now()
			case errors.Is(err, persistence.ErrLeaseNotFound) && s.leaseClearedByTransition(ctx, task.ID):
				// Deliberate lease-clear by a benign transition (or an
				// operator cancel). Stop renewing; do NOT escalate.
				s.logger.Info().
					Str("task_id", task.ID).
					Msg("scheduler: lease cleared by deliberate transition out of RUNNING/LEASED; stopping renewal without cancel")
				leaseReleased = true
				releasedAt = s.now()
			default:
				renewFailures++
				s.logger.Warn().Err(err).
					Str("task_id", task.ID).
					Int("consecutive_failures", renewFailures).
					Int("max_failures", maxRenewFailures).
					Msg("scheduler: lease renewal failed; will retry")
				if renewFailures >= maxRenewFailures {
					// Cancel the executor before releasing the slot
					// so we never re-dispatch a task that's still
					// running in another goroutine.
					if cancelErr := s.executor.Cancel(task.ID); cancelErr != nil {
						s.logger.Warn().Err(cancelErr).Str("task_id", task.ID).
							Msg("scheduler: executor.Cancel after renewal-failure escalation returned error")
					}
					return dispatchFailed, fmt.Sprintf("failed to renew task lease (%d consecutive failures): %v", renewFailures, err)
				}
				// Schedule the next retry attempt sooner than the
				// full renewInterval so we recover quickly after a
				// transient blip.
				lastRenewal = s.now().Add(-renewInterval + time.Second)
			}
		}

		if s.watchIterated != nil {
			s.watchIterated()
		}
		select {
		case <-ctx.Done():
			return dispatchInterrupted, ctx.Err().Error()
		case <-ticks:
		}
	}

	current, err := s.repo.Get(ctx, task.ID)
	if err != nil {
		return dispatchFailed, fmt.Sprintf("failed to reload task after executor completion: %v", err)
	}

	switch current.Status {
	case persistence.TaskStatusCompleted:
		return dispatchSucceeded, ""
	case persistence.TaskStatusCancelled:
		// Task was externally cancelled while the executor was running.
		// Do not overwrite the CANCELLED status — just release the slot.
		return dispatchTerminated, ""
	case persistence.TaskStatusFailed:
		if current.LastError != nil && *current.LastError != "" {
			return dispatchFailed, *current.LastError
		}
		return dispatchFailed, fmt.Sprintf("task finished with status %s", current.Status)
	default:
		if s.execRepo != nil {
			exec, execErr := s.execRepo.GetByTaskID(ctx, task.ID)
			if execErr == nil && exec != nil && exec.Status == persistence.ExecutionStatusPaused {
				return dispatchPaused, ""
			}
		}
		return dispatchFailed, nonTerminalNotePrefix + string(current.Status)
	}
}

// nonTerminalNotePrefix starts the scheduler's own note for a task the
// executor left LEASED or RUNNING. releaseExecutorLease recognises its own
// constant to look for the step error the execution row holds (GitHub #98);
// it never parses text from anywhere else.
const nonTerminalNotePrefix = "task left executor in non-terminal status "

// nonTerminalDetail is the error stored for a task the executor left
// non-terminal: "step <id>: <execution error>" when the newest execution holds
// an error, else note unchanged (permanent-failure design, amendment
// 2026-10-08). The class is still decided from note by the caller.
func (s *Scheduler) nonTerminalDetail(taskID, note string) string {
	if s.execRepo == nil || !strings.HasPrefix(note, nonTerminalNotePrefix) {
		return note
	}
	ctx, cancel := context.WithTimeout(s.baseContext(), 5*time.Second)
	defer cancel()
	exec, err := s.execRepo.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil || exec.Status == persistence.ExecutionStatusPaused ||
		exec.ErrorMessage == nil || strings.TrimSpace(*exec.ErrorMessage) == "" {
		return note
	}
	msg := strings.TrimSpace(*exec.ErrorMessage)
	if exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return msg
	}
	prefix := "step " + *exec.CurrentStepID
	if strings.HasPrefix(msg, prefix) {
		return msg
	}
	return prefix + ": " + msg
}

func (s *Scheduler) releaseExecutorLease(task *persistence.Task, outcome dispatchOutcome, errorMsg string) error {
	switch outcome {
	case dispatchSucceeded:
		return s.TaskCompleted(task.ID, taskLeaseID(task), true, "")
	case dispatchPaused:
		// Decrement before any fallible operation so the slot is always
		// freed, even if the subsequent repo calls fail.
		s.decrementRunning()
		current, err := s.repo.Get(s.baseContext(), task.ID)
		if err != nil {
			return fmt.Errorf("failed to reload paused task: %w", err)
		}
		releaseStatus := current.Status
		if releaseStatus == persistence.TaskStatusLeased || releaseStatus == persistence.TaskStatusRunning {
			releaseStatus = persistence.TaskStatusPending
		}
		return s.repo.ReleaseLease(s.baseContext(), task.ID, taskLeaseID(task), releaseStatus, persistence.ReleaseOptions{})
	case dispatchTerminated:
		// Task reached a terminal state externally (e.g. cancelled via UI).
		// Only decrement the running slot — the status and lease were already
		// managed by whichever path made the terminal transition.
		s.decrementRunning()
		return nil
	case dispatchInterrupted:
		s.decrementRunning()
		// Even when s.ctx is cancelled during shutdown we still need to release
		// the lease, but must not block the shutdown path indefinitely if the
		// DB is also draining. Use a detached context with a short deadline.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if s.execRepo != nil {
			exec, err := s.execRepo.GetByTaskID(shutdownCtx, task.ID)
			if err == nil && exec != nil && exec.Status == persistence.ExecutionStatusPaused {
				return nil
			}
		}
		current, err := s.repo.Get(shutdownCtx, task.ID)
		if err != nil {
			return fmt.Errorf("failed to reload interrupted task: %w", err)
		}
		if isTerminalTaskStatus(current.Status) {
			return nil
		}
		return s.repo.ReleaseLease(shutdownCtx, task.ID, taskLeaseID(task), persistence.TaskStatusQueued, persistence.ReleaseOptions{
			Error: errorMsgOrStatus(errorMsg, persistence.TaskStatusQueued),
		})
	default:
		status := persistence.TaskStatusFailed
		note := errorMsgOrStatus(errorMsg, status)
		return s.taskFailed(task.ID, taskLeaseID(task), note, s.nonTerminalDetail(task.ID, note))
	}
}

func isTerminalTaskStatus(status persistence.TaskStatus) bool {
	switch status {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled:
		return true
	default:
		return false
	}
}

// recoverExpiredLeases finds and recovers tasks with expired leases.
func (s *Scheduler) recoverExpiredLeases() {
	ctx := s.baseContext()
	tasks, err := s.repo.FindExpiredLeases(ctx, s.config.RecoveryBatchSize)
	if err != nil {
		s.logger.Error().Err(err).Msg("scheduler: failed to find expired leases")
		return
	}

	if len(tasks) > 0 {
		s.logger.Info().Int("count", len(tasks)).Msg("scheduler: recovering expired leases")
	}

	now := time.Now()
	// Snapshot the dynamic grace once for the whole batch — if we
	// recomputed per-task, completing tasks could shrink the running
	// count mid-batch and cause two identically-stuck tasks at the
	// same physical moment to get different grace windows. Same
	// sweep, same grace.
	graceForBatch := s.dynamicRecoveryGrace()
	for _, task := range tasks {
		// Record lease expired metric
		if s.metrics != nil {
			s.metrics.RecordLeaseExpired(task.ProjectID)
		}

		if s.executor != nil && s.executor.IsExecuting(task.ID) {
			// Active executor — clear any prior idle observation (the
			// task came back to life, e.g. between step transitions)
			// and renew the lease. LLD §6: this is the common case
			// for any step longer than LeaseDurationSeconds.
			s.clearRecoveryIdleSince(task.ID)
			if leaseID := taskLeaseID(task); leaseID != "" {
				if err := s.repo.RenewLease(ctx, task.ID, leaseID, s.config.LeaseDurationSeconds); err == nil {
					s.logger.Warn().
						Str("task_id", task.ID).
						Str("project_id", task.ProjectID).
						Str("lease_id", leaseID).
						Msg("scheduler: expired lease still belongs to active executor; renewed instead of recovering")
					continue
				} else {
					s.logger.Warn().Err(err).
						Str("task_id", task.ID).
						Str("project_id", task.ProjectID).
						Str("lease_id", leaseID).
						Msg("scheduler: failed to renew active expired lease; falling back to recovery")
				}
			}
		} else if s.executor != nil {
			// Executor reports the task as not executing. Apply the
			// grace period before treating it as orphaned (LLD §4.4 /
			// §6 — the per-project at-most-N invariant depends on
			// this). A single transient IsExecuting=false (the brief
			// gap between step transitions, or a goroutine being
			// re-registered) doesn't recover; only a continuous run
			// of false observations longer than RecoveryIdleGrace
			// does.
			//
			// Without this check, a long-running task whose DB-side
			// lease expired could be silently re-queued mid-execution,
			// allowing a sibling task on the same project to lease in
			// violation of the per-project cap — the "tasks rotate
			// round-robin" failure mode the user reported.
			if firstSeen := s.recordRecoveryIdleSince(task.ID, now); now.Sub(firstSeen) < graceForBatch {
				s.logger.Warn().
					Str("task_id", task.ID).
					Str("project_id", task.ProjectID).
					Dur("idle_for", now.Sub(firstSeen)).
					Dur("grace", graceForBatch).
					Dur("base_grace", s.config.RecoveryIdleGrace).
					Int("running_count", s.RunningCount()).
					Msg("scheduler: lease expired but executor recently reported idle within grace window; deferring recovery")
				continue
			}
		}

		// Diagnostic snapshot — added 2026-04 while the lease recovery
		// root cause was unknown (that item closed 2026-04-22 with the
		// lifecycle instrumentation, 3ca47547c). Capture everything we'd need
		// to diagnose the next occurrence: lease identity, acquisition
		// time, planned expiry, actual-vs-planned hold, attempt count.
		// This runs at Warn so it surfaces in default-level logs even
		// without operator intervention.
		var (
			leasedBy  string
			leasedAt  time.Time
			expiresAt time.Time
			leaseID   string
		)
		if task.LeasedBy != nil {
			leasedBy = *task.LeasedBy
		}
		if task.LeasedAt != nil {
			leasedAt = *task.LeasedAt
		}
		if task.LeaseExpiresAt != nil {
			expiresAt = *task.LeaseExpiresAt
		}
		if task.LeaseID != nil {
			leaseID = *task.LeaseID
		}
		heldFor := time.Duration(0)
		if !leasedAt.IsZero() {
			heldFor = now.Sub(leasedAt)
		}
		overdueBy := time.Duration(0)
		if !expiresAt.IsZero() {
			overdueBy = now.Sub(expiresAt)
		}
		s.logger.Warn().
			Str("task_id", task.ID).
			Str("project_id", task.ProjectID).
			Str("status", string(task.Status)).
			Str("lease_id", leaseID).
			Str("leased_by", leasedBy).
			Time("leased_at", leasedAt).
			Time("lease_expires_at", expiresAt).
			Dur("held_for", heldFor).
			Dur("overdue_by", overdueBy).
			Int("attempt", task.Attempt).
			Int("max_attempts", task.MaxAttempts).
			Msg("scheduler: lease recovery diagnostic snapshot")

		// Normalize MaxAttempts==0 (legacy / unconfigured) to 1, matching
		// TaskCompleted's same guard. Without this, a task with
		// MaxAttempts=0 whose lease expires would be re-queued forever:
		// the terminal predicate `nextAttempt > MaxAttempts` is always
		// false when MaxAttempts==0, and Postgres' `COALESCE(NULLIF($5,0))`
		// preserves the existing 0.
		maxAttempts := task.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		nextAttempt := task.Attempt + 1
		newStatus := persistence.TaskStatusQueued
		releaseOpts := persistence.ReleaseOptions{
			Attempt:     nextAttempt,
			MaxAttempts: maxAttempts,
			Error:       "task lease expired during execution",
			ErrorClass:  persistence.TaskFailureClassLeaseExpired,
		}
		if nextAttempt > maxAttempts {
			newStatus = persistence.TaskStatusFailed
			releaseOpts.Attempt = maxAttempts
		}

		err := s.repo.ReleaseLease(ctx, task.ID, taskLeaseID(task), newStatus, releaseOpts)
		if err != nil {
			s.logger.Error().Err(err).Str("task_id", task.ID).Msg("scheduler: failed to release expired lease")
			continue
		}
		// Recovery committed; the idle observation tracked under
		// recoveryIdleSince is consumed and must not influence
		// future ticks (the next time this task ID appears, it'll
		// be a fresh execution after re-lease).
		s.clearRecoveryIdleSince(task.ID)

		s.logger.Info().
			Str("task_id", task.ID).
			Str("project_id", task.ProjectID).
			Str("status", string(newStatus)).
			Int("attempt", releaseOpts.Attempt).
			Msg("scheduler: recovered expired lease")

		// Record recovery metric
		if s.metrics != nil {
			s.metrics.RecordRecovery(task.ProjectID)
		}
	}
}

// TaskCompleted should be called when a task finishes execution.
// This decrements the running count and updates the task status.
// Failed tasks are re-queued if persistence.TaskShouldRetry allows it — attempts
// remain AND the failure class is not one that can never succeed on a re-run.
func (s *Scheduler) TaskCompleted(taskID, leaseID string, success bool, errorMsg string) error {
	if success {
		s.decrementRunning()
		return s.repo.ReleaseLease(s.baseContext(), taskID, leaseID, persistence.TaskStatusCompleted, persistence.ReleaseOptions{})
	}
	return s.taskFailed(taskID, leaseID, errorMsg, errorMsg)
}

// taskFailed releases a failed attempt. note is what the classifier reads;
// detail is the error stored on the task. They differ only for a task the
// executor left non-terminal, where detail names the failed step and note is
// the scheduler's own text (GitHub #98): classifying the step's free text
// would stamp classes such as BUDGET_BLOCKED from words in it.
func (s *Scheduler) taskFailed(taskID, leaseID, note, detail string) error {
	s.decrementRunning()
	errorMsg := note // the classifier input; detail is what is stored

	// Check if the task has retries remaining.
	task, err := s.repo.Get(s.baseContext(), taskID)
	if err != nil || task == nil {
		// Can't look up the task — fail permanently.
		return s.repo.ReleaseLease(s.baseContext(), taskID, leaseID, persistence.TaskStatusFailed, persistence.ReleaseOptions{
			Error:      detail,
			ErrorClass: classifySchedulerFailure(nil, errorMsg),
		})
	}

	maxAttempts := task.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 1
	}
	attempt := task.Attempt
	if attempt <= 0 {
		attempt = 1
	}
	// Classify BEFORE deciding, not only on the terminal branch. Pre-fix this
	// asked only about the budget, so a permanent failure — a GitHub 404 on a
	// PR that does not exist — burned every attempt before anything looked at
	// what had actually gone wrong.
	class := classifySchedulerFailure(task, errorMsg)
	if class == "" && task.LastErrorClass != nil {
		// classifySchedulerFailure returns "" to mean "a precise class is
		// already on the row" — read it, since that is the class the executor
		// stamped and the one the retry decision must honour.
		class = *task.LastErrorClass
	}
	if persistence.TaskShouldRetry(attempt, maxAttempts, class) {
		// The hold is taken before the release, which makes the task leasable.
		held := s.holdRetry(taskID)
		err := s.repo.ReleaseLease(s.baseContext(), taskID, leaseID, persistence.TaskStatusQueued, persistence.ReleaseOptions{
			Attempt:     attempt + 1,
			MaxAttempts: maxAttempts,
			Error:       detail + held,
		})
		if err == nil {
			s.publishAttemptFailed(taskID, attempt, class, true)
		}
		return err
	}

	// Terminal. Stamp a class so `task explain` and `playbook show` have something to
	// resolve — an empty column here was the 2026-07-30 defect. An empty return from the
	// classifier means it deferred to a precise class already on the row, so the release
	// asks the repository to keep it: an empty class alone clears the column (both
	// drivers), which erased the executor's class until 2026-10-03 (permanent-failure
	// design, amendment 2026-10-03). "" is the classifier's only defer signal.
	cls := classifySchedulerFailure(task, errorMsg)
	err = s.repo.ReleaseLease(s.baseContext(), taskID, leaseID, persistence.TaskStatusFailed, persistence.ReleaseOptions{
		Error:          detail,
		ErrorClass:     cls,
		KeepErrorClass: cls == "",
	})
	if err == nil {
		published := cls
		if published == "" {
			published = class
		}
		s.publishAttemptFailed(taskID, attempt, published, false)
	}
	return err
}

// publishAttemptFailed reports a released failed attempt on that attempt's
// execution stream (live-task-observation design, Change 2026-10-08 C5). The
// payload has no free text: the class is closed at the source, and anything
// outside the task failure vocabulary is published as UNKNOWN. Best effort:
// with no publisher, no execution repository or no execution, nothing is sent.
func (s *Scheduler) publishAttemptFailed(taskID string, attempt int, class string, willRetry bool) {
	if s.livePub == nil || s.execRepo == nil {
		return
	}
	ctx, cancel := context.WithTimeout(s.baseContext(), 5*time.Second)
	defer cancel()
	exec, err := s.execRepo.GetByTaskID(ctx, taskID)
	if err != nil || exec == nil {
		return
	}
	class = livepubsub.ClosedTaskClass(class)
	step := ""
	if exec.CurrentStepID != nil {
		step = *exec.CurrentStepID
	}
	s.livePub.Publish(ctx, exec.ID, livepubsub.KindAttemptFailed, livepubsub.AttemptFailedPayload{
		Attempt: attempt, StepID: step, ErrorClass: class, WillRetry: willRetry,
	})
}

func (s *Scheduler) completeTask(task *persistence.Task, success bool, errorMsg string) error {
	leaseID := taskLeaseID(task)

	status := persistence.TaskStatusCompleted
	opts := persistence.ReleaseOptions{}

	if !success {
		maxAttempts := task.MaxAttempts
		if maxAttempts <= 0 {
			maxAttempts = 1
		}
		attempt := task.Attempt
		if attempt <= 0 {
			attempt = 1
		}
		class := classifySchedulerFailure(task, errorMsg)
		if class == "" && task.LastErrorClass != nil {
			class = *task.LastErrorClass
		}
		if persistence.TaskShouldRetry(attempt, maxAttempts, class) {
			status = persistence.TaskStatusQueued
			opts.Attempt = attempt + 1
			opts.MaxAttempts = maxAttempts
			opts.Error = errorMsg
		} else {
			status = persistence.TaskStatusFailed
			opts.Attempt = attempt
			opts.MaxAttempts = maxAttempts
			opts.Error = errorMsg
		}
	}

	s.decrementRunning()

	return s.repo.ReleaseLease(s.baseContext(), task.ID, leaseID, status, opts)
}

func (s *Scheduler) decrementRunning() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningCount > 0 {
		s.runningCount--
	}
}

// recordRecoveryIdleSince stamps `now` against `taskID` if no prior
// idle observation exists, then returns the EARLIEST observation
// time (which may be from this call or from a previous tick). The
// recovery sweep compares `now - returned` against
// RecoveryIdleGrace to decide whether the executor has been idle
// long enough to treat the task as orphaned.
func (s *Scheduler) recordRecoveryIdleSince(taskID string, now time.Time) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.recoveryIdleSince == nil {
		s.recoveryIdleSince = make(map[string]time.Time)
	}
	if existing, ok := s.recoveryIdleSince[taskID]; ok {
		return existing
	}
	s.recoveryIdleSince[taskID] = now
	return now
}

// clearRecoveryIdleSince removes any idle observation for taskID.
// Called when the executor reports `IsExecuting=true` (the task
// came back to life — the prior false was transient) or when the
// task is recovered (its idle window is consumed).
func (s *Scheduler) clearRecoveryIdleSince(taskID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recoveryIdleSince, taskID)
}

func taskLeaseID(task *persistence.Task) string {
	if task == nil || task.LeaseID == nil {
		return ""
	}
	return *task.LeaseID
}

// leaseClearedByTransition reports whether a RenewLease ErrLeaseNotFound
// is the result of a deliberate transition out of RUNNING/LEASED rather
// than a genuine lost lease. It reloads the task and returns true when
// the current status is anything other than RUNNING or LEASED — meaning
// the executor (lead hand-off → AWAITING_INPUT / AWAITING_EXTERNAL,
// pause → PAUSED, closure → COMPLETED) or an operator (cancel) cleared
// the lease on purpose while the execution goroutine is still
// finalizing. On any reload error it returns false so the caller treats
// the failure as a real lost lease and keeps its escalation safety net
// (avoids masking a true double-execution window behind a DB blip).
func (s *Scheduler) leaseClearedByTransition(ctx context.Context, taskID string) bool {
	cur, err := s.repo.Get(ctx, taskID)
	if err != nil || cur == nil {
		return false
	}
	return cur.Status != persistence.TaskStatusRunning &&
		cur.Status != persistence.TaskStatusLeased
}

// dynamicRecoveryGrace scales the configured RecoveryIdleGrace by the
// scheduler's current active-execution count. Under high churn (many
// concurrent executions), the false-positive rate of IsExecuting=false
// observations rises — a step transition, a brief goroutine
// re-registration gap — and a fixed grace can amplify those false
// positives into spurious recoveries that re-lease still-running tasks.
//
// Scaling:
//   - 0-5 running: grace = base (no scaling needed at low load)
//   - 6-15 running: grace = base * 1.5
//   - 16+ running: grace = base * 2 (capped)
//
// The cap protects against pathological values when running_count
// briefly spikes — without it, a 100-task burst would push the
// recovery loop's grace into 5+ minutes territory and stall lease
// recovery for legitimately stuck tasks. Symmetric tuning to the
// jitter window: the renewal cadence is up to 1.25× base, so the
// recovery sweep's grace must always exceed peak renewal interval
// + a safety margin.
func (s *Scheduler) dynamicRecoveryGrace() time.Duration {
	base := s.config.RecoveryIdleGrace
	if base <= 0 {
		base = 90 * time.Second
	}
	running := s.RunningCount()
	switch {
	case running >= 16:
		return base * 2
	case running >= 6:
		return base * 3 / 2
	default:
		return base
	}
}

// computeJitteredRenewInterval returns the per-dispatch lease renewal
// cadence with ±25% jitter applied. Base = leaseDuration/2 (so two
// renewals fit per lease, with a comfortable margin against
// transient renewal failures). Jitter desynchronises renewals
// across concurrently-dispatched tasks so the DB doesn't see a
// thundering-herd UPDATE pattern when many tasks lease at the
// same tick. Each call to this function returns an independent
// random interval — the dispatcher picks one per dispatch and
// reuses it for the lifetime of that dispatch's renewal loop.
//
// Ranges: with the default LeaseDurationSeconds=300, base = 150s
// and the jittered interval lands in [112.5s, 187.5s]. A 100ms
// floor protects sub-second test configs from underflowing to 0
// or to the dispatch poll cadence.
func computeJitteredRenewInterval(leaseDurationSeconds int) time.Duration {
	base := time.Duration(leaseDurationSeconds) * time.Second / 2
	if base <= 0 {
		return 100 * time.Millisecond
	}
	// 50% spread: half is on each side of base. r is uniform in
	// [0, 1) so r-0.5 is in [-0.5, +0.5), and the spread amount is
	// base/4 (a quarter of base, applied either side, totaling 50%
	// peak-to-peak). math/rand's package-level Source is seeded
	// once at process start (Go 1.20+) — no per-process drift.
	jitter := time.Duration((mrand.Float64() - 0.5) * float64(base) / 2)
	out := base + jitter
	if out < 100*time.Millisecond {
		out = 100 * time.Millisecond
	}
	return out
}

func errorMsgOrStatus(errorMsg string, status persistence.TaskStatus) string {
	if errorMsg != "" {
		return errorMsg
	}
	if status == persistence.TaskStatusFailed {
		return "executor reported task failure"
	}
	return ""
}

// RunningCount returns the current number of running tasks.
func (s *Scheduler) RunningCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runningCount
}

// IsStarted returns whether the scheduler is currently running.
func (s *Scheduler) IsStarted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}
