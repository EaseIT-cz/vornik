// Package watchdog detects in-flight executions that have stopped
// making forward progress and either logs them or marks them failed
// so the dashboard surfaces the problem.
//
// "Forward progress" is defined as a write to executions.updated_at —
// the executor's saveCheckpoint between every workflow step bumps
// this column on every step boundary, so a fresh updated_at is
// strong evidence the executor is alive and still walking the
// workflow. An execution where status=RUNNING but updated_at is
// older than the configured stuck threshold is the watchdog's
// signal: the executor goroutine is hung, crashed silently, or
// trapped in a too-long step.
//
// Distinct from existing recovery layers:
//   - Context-deadline TIMEOUT fires from inside the executor when a
//     step exceeds its configured timeout. The watchdog catches the
//     case where no timeout was configured, or where the executor
//     itself is unable to fire its own timeout.
//   - LEASE_EXPIRED fires from the scheduler's recovery loop when the
//     task lease has elapsed. Lease durations are typically much
//     longer (hours) than the watchdog threshold (minutes) — the
//     watchdog is the faster signal.
//
// Action policy defaults to fail (was warn-only until 2026-05-13;
// flipped after a ghost-RUNNING incident on ibkr-trader where an
// operator cancelled live tasks thinking two were running in
// parallel because the prior retry's execution row was still
// RUNNING in the table). The "fail" action marks both the execution
// row and the task row as terminal with class STUCK_EXECUTION and,
// since 2026-10-12 (GitHub #142, 05-scheduler §4.12 C2/C3), names the
// case — stuck (the task's lease is still renewed: a live goroutine not
// advancing) or abandoned (no live lease: nothing is running it) — and
// asks the executor to stop the execution through the cancel
// primitive's post-write sequence (taskcancel.AfterTerminal). Until then
// it did NOT try to stop anything. A goroutine genuinely hung in a call
// with no bound still won't respond; the scheduler releases its slot
// after AbandonedExecutionGrace (§4.12 C5).
// Operators with legitimately long-running steps can opt back to
// warn via WatchdogConfig in the daemon YAML.
package watchdog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/stepoutcome"
	"vornik.io/vornik/internal/taskcancel"
)

// Action is the policy applied when the watchdog finds a stuck
// execution. Two values: "warn" logs and increments a counter; "fail"
// additionally marks the row terminal so the operator dashboard
// stops showing it as in-flight.
type Action string

const (
	ActionWarn Action = "warn"
	ActionFail Action = "fail"
)

// IsValid reports whether the action string is one the watchdog
// understands. Unknown values are coerced to ActionWarn at startup
// so a config typo doesn't silently turn the watchdog off.
func (a Action) IsValid() bool {
	return a == ActionWarn || a == ActionFail
}

// ExecutionRepository is the narrow set of execution-row operations
// the watchdog uses. Defined locally so the watchdog package can be
// tested without dragging in the full persistence dependency graph.
type ExecutionRepository interface {
	List(ctx context.Context, filter persistence.ExecutionFilter) ([]*persistence.Execution, error)
	RecordFailure(ctx context.Context, id string, errorMessage, errorCode string) error
	// SupersedeOrphanPausedExecutions finalizes PAUSED executions whose
	// parent task is already terminal — the global backstop for the
	// orphan-PAUSED leak. Run once per watchdog scan.
	SupersedeOrphanPausedExecutions(ctx context.Context) (int64, error)
}

// StepOutcomeRepository is the narrow surface for the pending-outcome
// backstop. Optional: when nil, that reconcile leg is skipped.
//
// The leg exists because SweepPending is scoped to the ONE execution its
// caller is closing, and only three paths call it (executor handleSuccess /
// handleFailure / handleCancelled). Every path that terminalises a task's
// OTHER executions — cascadeOrphanExecutions, supersedeStaleExecutions and
// this watchdog's own orphan-PAUSED sweep — leaves the outcome rows beneath
// them at pending_validation forever. Measured 2026-08-18: 393 of adaptive's
// 673 step outcomes over 30 days, every one the lead's delegating `route`
// step, on executions that were CANCELLED while their task COMPLETED. Since
// pending_validation counts as neither ok nor failure, the highest-volume
// workflow in the fleet had no usable step-outcome record.
//
// Converging on the invariant here repairs rows already stranded, which no
// call-site hook can, and covers crash recovery and future terminal paths.
type StepOutcomeRepository interface {
	// SweepPendingForTerminalExecutions relabels pending_validation rows whose
	// execution is already terminal, oldest-first within a bounded batch, and
	// returns how many it relabelled. Idempotent.
	SweepPendingForTerminalExecutions(ctx context.Context, olderThan time.Duration, limit int) (int64, error)
}

// StuckStepRecorder writes the row for the step of an execution the watchdog
// failed as stuck (outcome-quality-design §S8.8, GitHub #369). Optional: when
// nil, no step row is written (as before 2026-10-17).
type StuckStepRecorder interface {
	// RecordStuckStepOutcome inserts o unless a row for (execution, step) was
	// recorded at or after since, or o.ID exists; reports whether it inserted.
	RecordStuckStepOutcome(ctx context.Context, o *persistence.ExecutionStepOutcome, since time.Time) (bool, error)
}

// InFlightStepSource is what the executor knows about the agent step an
// execution is running in this process. Optional: without it (or when it does
// not know the execution) the watchdog reads the step from the execution row.
type InFlightStepSource interface {
	InFlightStep(executionID string) (stepoutcome.InFlightStep, bool)
}

// ParentUnblockSweeper re-checks parents stranded in WAITING_FOR_CHILDREN.
// Optional: when nil, that reconcile leg is skipped.
//
// Audited 2026-08-18: four paths terminalise a CHILD task without driving the
// parent-unblock hook — the UI's execution-cancel (ui/execution_actions.go
// cancelOne), the chat cancel action (chat/parser.go, which holds no executor
// reference at all), and this watchdog's own approval-timeout and stuck-fail
// legs. executor.Cancel cascades orphan EXECUTIONS but likewise never calls the
// hook. That is occurrences #3-#6 of a bug class already at #2 (#1: an
// operator-CLOSED child never woke its parent, 2026-05-21; #2: child-cancel
// across three entry points, 2026-06-07 / 3f1f3e76).
//
// The pre-existing backstop ran at DAEMON STARTUP ONLY, so a stranded parent
// waited for a restart. Running the same idempotent sweep per tick contains
// every one of those paths without touching their call sites, and can never
// change a fate the startup sweep would not have reached anyway.
type ParentUnblockSweeper interface {
	// SweepStuckWaitingForChildren walks WAITING_FOR_CHILDREN parents and
	// re-runs the unblock decision on each. Idempotent: a parent whose
	// children are still in flight is left alone.
	SweepStuckWaitingForChildren(ctx context.Context)
}

// TaskRepository is the watchdog's required surface on the task
// table — used only when Action=fail to flip the task row terminal
// alongside the execution row. The Get/Update pair mirrors what
// executor.handleFailure does.
type TaskRepository interface {
	Get(ctx context.Context, id string) (*persistence.Task, error)
	Update(ctx context.Context, task *persistence.Task) error
	// List + UpdateStatus back the approval-timeout sweep: find tasks
	// parked in AWAITING_APPROVAL, cancel the ones past the timeout.
	List(ctx context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error)
	UpdateStatus(ctx context.Context, id string, status persistence.TaskStatus) error
	// TransitionConditional gates the approval-timeout cancel on the live
	// status, so an approval landing between List and the write is not
	// overwritten (scheduler design §4.10).
	TransitionConditional(ctx context.Context, id string, from []persistence.TaskStatus, to persistence.TaskStatus, opts persistence.TransitionOpts) (bool, error)
}

// Config holds watchdog tunables. Zero-value defaults are applied at
// New(), so the daemon can wire a partially-populated Config and
// still get a working watchdog.
type Config struct {
	// Enabled gates the periodic scan. When false, Start() is a
	// no-op so a daemon can ship the watchdog dark and turn it on
	// per-environment without code changes.
	Enabled bool
	// Interval is the period between scans. Default: 60s.
	// Shorter intervals catch stuck rows sooner at the cost of
	// repeated SELECTs against the executions table.
	Interval time.Duration
	// StuckThreshold is the maximum allowed gap between now and an
	// execution's updated_at before the watchdog fires. Default:
	// 30m. Should be longer than the slowest legitimate step in
	// any workflow plus a comfortable margin — false positives
	// here are noisier than missed detections.
	StuckThreshold time.Duration
	// Action is what to do when a stuck row is found. Default:
	// ActionWarn. Operators escalate to ActionFail only after they
	// trust the threshold isn't tripping on legitimate slow steps.
	Action Action
	// ApprovalTimeout cancels tasks parked in AWAITING_APPROVAL longer
	// than this — so the operator-action inbox can't accumulate stale
	// approvals forever. 0 disables (no expiry). Default wired from
	// autonomy.approval_timeout_hours (96h). Uses the same leader-gated
	// scan loop as the stuck-execution detector.
	ApprovalTimeout time.Duration
	// ReservationStaleAfter bounds how long a budget reservation
	// (trading-hardening §1) may stay unsettled before the sweep reaps it
	// regardless of task state — the backstop that guarantees a leaked
	// reservation (task row never created, or a settle that never ran)
	// can't block a project's hard cap forever. The sweep ALSO settles
	// reservations whose task already went terminal. 0 → defaultReservationStaleAfter.
	ReservationStaleAfter time.Duration
}

// DefaultConfig returns a watchdog configuration with the action set
// to fail. The original default was warn-only (ship-dark posture
// while operators learned the threshold), but warn-only left ghost
// RUNNING rows in the executions table that confused operators into
// cancelling tasks they thought had multiple concurrent agents
// running (observed 2026-05-13: ibkr-trader execution row stayed
// RUNNING for >12 min while the scheduler had already moved on to
// the next retry attempt — operator saw "two RUNNING" in the
// dashboard and cancelled, when in fact only the latest was a live
// goroutine). The 30-minute threshold is much longer than any
// legitimate single step in the workflows we ship, so flipping the
// default to fail is safe: a real stuck row gets cleaned up, the
// dashboard reflects reality, and operators with workloads that
// legitimately need >30m can opt back to warn via WatchdogConfig.
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		Interval:       60 * time.Second,
		StuckThreshold: 30 * time.Minute,
		Action:         ActionFail,
	}
}

// Metrics groups the Prometheus counters the watchdog publishes.
// detected fires once per (execution_id, observed updated_at) pair —
// not per tick — so a long-stuck row contributes 1 to the counter,
// not N for the number of times we observed it. failed fires only
// when Action=fail and the row was successfully marked terminal.
type Metrics struct {
	detected prometheus.Counter
	failed   prometheus.Counter
	// orphansSwept counts PAUSED executions finalized by the
	// orphan-PAUSED reconcile backstop (parent task already terminal).
	orphansSwept prometheus.Counter
	// approvalsExpired counts tasks cancelled by the approval-timeout
	// sweep (parked in AWAITING_APPROVAL past the configured timeout).
	approvalsExpired prometheus.Counter
	// reservationsSwept counts budget reservations settled by the
	// terminal-and-stale sweep (terminal task, or older than the stale bound).
	reservationsSwept prometheus.Counter
	// pendingOutcomesSwept counts step-outcome rows relabelled from
	// pending_validation by the terminal-execution backstop. A persistently
	// non-zero rate means some terminal path is still not sweeping its own
	// rows, which is the defect this backstop was added to contain.
	pendingOutcomesSwept prometheus.Counter
	// stuckStepOutcomes counts, by result, the step-outcome row a stuck-fail
	// writes for the step that hung (outcome-quality §S8.8): recorded,
	// already_recorded (the step had a row since the checkpoint), no_step, or
	// error. Every stuck-fail with a recorder wired lands in one.
	stuckStepOutcomes *prometheus.CounterVec
}

// NewMetrics registers watchdog counters on the provided registry.
// Returns nil when the registry is nil so tests can opt out.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	if reg == nil {
		return nil
	}
	m := &Metrics{
		detected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_stuck_executions_detected_total",
			Help: "Number of distinct stuck-execution events the watchdog has observed (deduped per execution_id + observed updated_at).",
		}),
		failed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_stuck_executions_failed_total",
			Help: "Number of stuck executions the watchdog has marked terminal (fires only when action=fail).",
		}),
		orphansSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_orphan_paused_swept_total",
			Help: "Number of orphan PAUSED executions (parent task already terminal) finalized by the reconcile backstop.",
		}),
		approvalsExpired: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_approvals_expired_total",
			Help: "Number of tasks cancelled by the approval-timeout sweep (parked in AWAITING_APPROVAL past the timeout).",
		}),
		reservationsSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_budget_reservations_swept_total",
			Help: "Number of budget reservations settled by the terminal-and-stale sweep (terminal task or older than the stale bound).",
		}),
		pendingOutcomesSwept: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vornik_watchdog_pending_outcomes_swept_total",
			Help: "Number of step-outcome rows relabelled from pending_validation because their execution was already terminal.",
		}),
	}
	m.stuckStepOutcomes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vornik_watchdog_stuck_step_outcomes_total",
		Help: "Step-outcome rows for the step of an execution the watchdog failed as stuck, by result: recorded, already_recorded (the step had a row since its last checkpoint), no_step (no step in flight), error.",
	}, []string{"result"})
	reg.MustRegister(m.detected, m.failed, m.orphansSwept, m.approvalsExpired, m.reservationsSwept, m.pendingOutcomesSwept, m.stuckStepOutcomes)
	return m
}

// Watchdog runs the periodic scan as a single background goroutine.
// Lifecycle mirrors scheduler.Scheduler: Start() spawns the loop,
// Stop() cancels and waits for it to drain.
type Watchdog struct {
	cfg           Config
	execRepo      ExecutionRepository
	taskRepo      TaskRepository        // optional; required only when Action=fail
	outcomeRepo   StepOutcomeRepository // optional; nil skips the pending-outcome leg
	parentSweeper ParentUnblockSweeper  // optional; nil skips the stuck-parent leg
	// stuckSteps writes the step row for a stuck-fail; inFlight names the step
	// (§S8.8). Both optional.
	stuckSteps StuckStepRecorder
	inFlight   InFlightStepSource
	// taskTeardown is the cancel primitive's post-write work for the
	// approval-timeout cancel (GitHub #304); nil writes only.
	taskTeardown taskcancel.Teardown
	logger       zerolog.Logger
	metrics      *Metrics
	now          func() time.Time
	// leaderGate gates each scan on the elected leader so two
	// daemons don't both detect + act on the same stuck row.
	// Especially important when cfg.Action=fail — duplicate
	// detection would result in two FailExecution calls and
	// confusing audit chatter. Nil-safe.
	leaderGate LeaderGate

	// reservationSweeper settles budget reservations whose task went
	// terminal or that have gone stale. Optional — nil disables the sweep
	// (the prompt settle in the executor still runs). Wired via
	// SetReservationSweeper so the constructor signature stays stable.
	reservationSweeper ReservationSweeper

	mu      sync.Mutex
	started bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	// seenLock guards the dedupe table. The map keys executions by
	// ID; the value is the updated_at the watchdog last observed for
	// that row. A re-observation with the same updated_at is silenced;
	// a fresh updated_at clears the entry (the executor unstuck and
	// resumed checkpointing). When a row leaves RUNNING the entry
	// also clears.
	seenLock sync.Mutex
	seen     map[string]time.Time
}

// New constructs a Watchdog. Zero-valued Config fields fall back to
// DefaultConfig() values; an unknown Action is coerced to ActionWarn
// (a typo in the daemon YAML must not silently disable the
// watchdog). Returns nil when execRepo is nil — the watchdog has
// nothing to scan without it.
func New(cfg Config, execRepo ExecutionRepository, taskRepo TaskRepository, logger zerolog.Logger, metrics *Metrics) *Watchdog {
	if execRepo == nil {
		return nil
	}
	def := DefaultConfig()
	if cfg.Interval <= 0 {
		cfg.Interval = def.Interval
	}
	if cfg.StuckThreshold <= 0 {
		cfg.StuckThreshold = def.StuckThreshold
	}
	if !cfg.Action.IsValid() {
		cfg.Action = def.Action
	}
	return &Watchdog{
		cfg:      cfg,
		execRepo: execRepo,
		taskRepo: taskRepo,
		logger:   logger,
		metrics:  metrics,
		now:      func() time.Time { return time.Now().UTC() },
		seen:     make(map[string]time.Time),
	}
}

// Enabled reports whether the watchdog will actually scan when
// Start() is called. Exposed for tests + diagnostics so callers
// who built the watchdog via initWatchdog wiring can verify the
// effective config (e.g. that a missing `watchdog:` block in
// the operator YAML still leaves the safety net armed).
func (w *Watchdog) Enabled() bool {
	if w == nil {
		return false
	}
	return w.cfg.Enabled
}

// Start begins the periodic scan in a background goroutine. Returns
// nil immediately when Enabled=false so the daemon can call Start()
// unconditionally and the operator's config knob does the gating.
// Calling Start() twice returns an error.
func (w *Watchdog) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.cfg.Enabled {
		w.logger.Info().Msg("watchdog disabled by config — periodic stuck-execution scan will not run")
		return nil
	}
	if w.started {
		return fmt.Errorf("watchdog already started")
	}
	w.ctx, w.cancel = context.WithCancel(context.Background())
	w.started = true
	w.wg.Add(1)
	go w.runLoop()
	w.logger.Info().
		Dur("interval", w.cfg.Interval).
		Dur("stuck_threshold", w.cfg.StuckThreshold).
		Str("action", string(w.cfg.Action)).
		Msg("watchdog started")
	return nil
}

// Stop signals the loop to exit and waits for it to drain. Safe to
// call multiple times; subsequent calls after the first are no-ops.
func (w *Watchdog) Stop() error {
	w.mu.Lock()
	if !w.started {
		w.mu.Unlock()
		return nil
	}
	w.cancel()
	w.started = false
	w.mu.Unlock()
	w.wg.Wait()
	return nil
}

// runLoop is the periodic scan. Each tick lists RUNNING executions,
// filters those whose updated_at is older than now-StuckThreshold,
// and dispatches each through the configured action.
func (w *Watchdog) runLoop() {
	defer w.wg.Done()

	// Fire one scan immediately so a stuck row caught at startup
	// doesn't have to wait an interval before being surfaced.
	w.scanOnce()

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-ticker.C:
			if w.leaderGate != nil && !w.leaderGate.IsLeader() {
				continue
			}
			w.scanOnce()
		}
	}
}

// LeaderGate is the narrow contract Watchdog uses to skip scans
// on non-leader daemons. Satisfied by *leaderelection.Elector;
// defined locally to keep the watchdog package independent.
type LeaderGate interface {
	IsLeader() bool
}

// SetLeaderGate wires the elected-leader gate. Called by the
// service container after the Watchdog is constructed. nil
// clears (single-process default).
func (w *Watchdog) SetLeaderGate(g LeaderGate) {
	if w == nil {
		return
	}
	w.leaderGate = g
}

// scanOnce performs a single watchdog scan. Exported test surface so
// unit tests can drive the loop deterministically without sleeping
// for an interval.
func (w *Watchdog) scanOnce() {
	parent := w.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	running := persistence.ExecutionStatusRunning
	rows, err := w.execRepo.List(ctx, persistence.ExecutionFilter{
		Status:   &running,
		PageSize: 500, // bound the scan; a deployment with 500+ concurrently-RUNNING is itself a problem
	})
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: failed to list RUNNING executions for scan")
		return
	}

	now := w.now()
	threshold := now.Add(-w.cfg.StuckThreshold)
	currentRunning := make(map[string]struct{}, len(rows))

	for _, exec := range rows {
		if exec == nil {
			continue
		}
		currentRunning[exec.ID] = struct{}{}
		if !exec.UpdatedAt.Before(threshold) {
			// Fresh checkpoint — clear any prior dedupe so this
			// execution can be reported again if it re-sticks.
			w.clearSeen(exec.ID)
			continue
		}
		w.handleStuck(ctx, exec, now)
	}

	w.pruneSeen(currentRunning)

	// Orphan-PAUSED reconcile backstop. Independent of the stuck-RUNNING
	// scan above: finalize PAUSED executions whose parent task already went
	// terminal (the adaptive-route leak the per-task cascade misses on
	// CLOSED / odd-cancel paths). Best-effort — a failure is logged, never
	// fatal; the next tick retries.
	w.reconcileOrphanPaused(ctx)
	w.reconcilePendingOutcomes(ctx)

	// Approval-timeout sweep: cancel tasks parked in AWAITING_APPROVAL past
	// the configured timeout so the operator-action inbox doesn't accrue
	// stale approvals. Best-effort; no-op when disabled / no task repo.
	w.sweepExpiredApprovals(ctx)

	// Budget-reservation sweep: settle reservations whose task went terminal
	// (prompt settle missed, e.g. a crash) or that have gone stale. The
	// backstop that keeps a leaked reservation from blocking a hard cap
	// forever. Best-effort; no-op when no sweeper is wired.
	w.sweepBudgetReservations(ctx)

	// Stranded-parent reconcile LAST, deliberately. Both of this scan's own
	// terminalising legs — handleStuck (action=fail) above and
	// sweepExpiredApprovals just now — can mark a CHILD task terminal without
	// driving the parent-unblock hook, so running this after them means a
	// parent stranded by THIS tick is recovered by THIS tick rather than
	// waiting for the next one.
	w.reconcileStuckParents(ctx)
}

// defaultReservationStaleAfter bounds an unsettled reservation's lifetime
// when Config.ReservationStaleAfter is unset. Comfortably longer than any
// legitimate task run (the executor settles promptly on terminal), so the
// stale path only ever reaps genuinely-leaked rows.
const defaultReservationStaleAfter = 6 * time.Hour

// ReservationSweeper is the narrow contract the watchdog needs to reap
// budget reservations. Satisfied by persistence.BudgetReservationRepository.
type ReservationSweeper interface {
	SweepTerminalAndStale(ctx context.Context, staleCutoff, now time.Time) (int64, error)
}

// SetReservationSweeper wires the budget-reservation sweeper. Called by the
// service container after construction. nil disables the sweep.
func (w *Watchdog) SetReservationSweeper(s ReservationSweeper) {
	if w == nil {
		return
	}
	w.reservationSweeper = s
}

// sweepBudgetReservations settles terminal-task + stale reservations. The
// stale bound falls back to defaultReservationStaleAfter when unset.
func (w *Watchdog) sweepBudgetReservations(ctx context.Context) {
	if w.reservationSweeper == nil {
		return
	}
	stale := w.cfg.ReservationStaleAfter
	if stale <= 0 {
		stale = defaultReservationStaleAfter
	}
	now := w.now()
	n, err := w.reservationSweeper.SweepTerminalAndStale(ctx, now.Add(-stale), now)
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: budget-reservation sweep failed; retry next tick")
		return
	}
	if n > 0 {
		if w.metrics != nil {
			w.metrics.reservationsSwept.Add(float64(n))
		}
		w.logger.Info().Int64("settled", n).Msg("watchdog: settled terminal/stale budget reservations")
	}
}

// sweepExpiredApprovals cancels tasks that have sat in AWAITING_APPROVAL
// longer than cfg.ApprovalTimeout. The task's updated_at is the entry time
// (status changes touch it; a task awaiting approval sees no other writes),
// so it's the age proxy. Best-effort + idempotent: a cancelled task leaves
// the AWAITING_APPROVAL set, so a re-run finds nothing.
func (w *Watchdog) sweepExpiredApprovals(ctx context.Context) {
	if w.cfg.ApprovalTimeout <= 0 || w.taskRepo == nil {
		return
	}
	status := persistence.TaskStatusAwaitingApproval
	tasks, err := w.taskRepo.List(ctx, persistence.TaskFilter{Status: &status, PageSize: 500})
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: approval-timeout list failed; retry next tick")
		return
	}
	cutoff := w.now().Add(-w.cfg.ApprovalTimeout)
	expired := 0
	for _, t := range tasks {
		if t == nil || !t.UpdatedAt.Before(cutoff) {
			continue
		}
		// Through the cancel primitive (05-scheduler §4.11, GitHub #304): once
		// the row moved, the task's parent is unblocked and its children are
		// cancelled. An approval-parked task has no live execution, so the
		// teardown's stop is a no-op.
		//
		// The row records why and what to do (outcome-quality LLD §S5 #17,
		// GitHub #142): without it the task read like an operator's cancel.
		reason := approvalTimeoutReason(w.cfg.ApprovalTimeout)
		class := persistence.TaskFailureClassCancelled
		outcome, err := taskcancel.CancelWith(ctx, w.taskRepo, w.taskTeardown, t.ID,
			[]persistence.TaskStatus{persistence.TaskStatusAwaitingApproval},
			persistence.TransitionOpts{LastError: &reason, LastErrorClass: &class})
		if err != nil {
			w.logger.Warn().Err(err).Str("task_id", t.ID).Msg("watchdog: approval-timeout cancel failed")
			continue
		}
		if outcome.TeardownErr != nil {
			w.logger.Warn().Err(outcome.TeardownErr).Str("task_id", t.ID).
				Msg("watchdog: approval-timeout cancel could not stop an execution")
		}
		if !outcome.Transitioned {
			// Approved (or otherwise moved on) since List read it: no longer
			// awaiting approval, so the timeout no longer applies.
			continue
		}
		expired++
		w.logger.Info().
			Str("task_id", t.ID).
			Str("project_id", t.ProjectID).
			Dur("approval_age", w.now().Sub(t.UpdatedAt)).
			Dur("timeout", w.cfg.ApprovalTimeout).
			Msg("watchdog: cancelled task — approval timed out (no operator decision within the window)")
	}
	if expired > 0 && w.metrics != nil && w.metrics.approvalsExpired != nil {
		w.metrics.approvalsExpired.Add(float64(expired))
	}
}

// approvalTimeoutReason is the last_error an approval-timeout cancel records:
// what happened, and the two things an operator can do about it.
func approvalTimeoutReason(timeout time.Duration) string {
	window := timeout.String()
	if timeout > 0 && timeout%time.Hour == 0 {
		window = fmt.Sprintf("%dh", int64(timeout/time.Hour))
	}
	return fmt.Sprintf("approval timed out: nobody approved or rejected this task within %s, so it was cancelled. "+
		"Re-submit it to run it, or raise autonomy.approval_timeout_hours to give approvers longer.", window)
}

// reconcileOrphanPaused finalizes orphan PAUSED executions whose parent task
// is terminal. Best-effort + idempotent (a no-op once none remain).
func (w *Watchdog) reconcileOrphanPaused(ctx context.Context) {
	n, err := w.execRepo.SupersedeOrphanPausedExecutions(ctx)
	if err != nil {
		w.logger.Warn().Err(err).Msg("watchdog: orphan-PAUSED reconcile failed; orphans linger until next tick")
		return
	}
	if n > 0 {
		if w.metrics != nil && w.metrics.orphansSwept != nil {
			w.metrics.orphansSwept.Add(float64(n))
		}
		w.logger.Info().Int64("orphans_swept", n).
			Msg("watchdog: finalized orphan PAUSED executions (parent task already terminal)")
	}
}

// pendingOutcomeSettleGrace is how long an execution must have been terminal
// before its pending rows are considered stranded. The executor writes an
// execution's terminal status around its own SweepPending call, so a shorter
// grace would race that window and relabel a row the correct path was about
// to finalize properly.
const pendingOutcomeSettleGrace = 10 * time.Minute

// pendingOutcomeBatchLimit bounds one tick's relabelling. Mirrors the score
// reconciler: a bounded oldest-first batch keeps a large historical backlog
// from monopolising a scan, and drains over successive ticks.
const pendingOutcomeBatchLimit = 500

// WithStepOutcomeRepository wires the pending-outcome backstop. Chainable so
// the daemon can opt in without changing New's signature; returns the receiver
// unchanged when w is nil so a disabled watchdog stays disabled.
func (w *Watchdog) WithStepOutcomeRepository(repo StepOutcomeRepository) *Watchdog {
	if w == nil {
		return w
	}
	w.outcomeRepo = repo
	return w
}

// reconcilePendingOutcomes relabels step-outcome rows left at
// pending_validation under an already-terminal execution. Best-effort and
// idempotent, exactly like reconcileOrphanPaused: a failure is logged and the
// next tick retries, because this leg must never take down the scan that
// detects stuck executions.
func (w *Watchdog) reconcilePendingOutcomes(ctx context.Context) {
	if w.outcomeRepo == nil {
		return
	}
	n, err := w.outcomeRepo.SweepPendingForTerminalExecutions(ctx, pendingOutcomeSettleGrace, pendingOutcomeBatchLimit)
	if err != nil {
		w.logger.Warn().Err(err).
			Msg("watchdog: pending-outcome reconcile failed; rows stay pending_validation until next tick")
		return
	}
	if n > 0 {
		if w.metrics != nil && w.metrics.pendingOutcomesSwept != nil {
			w.metrics.pendingOutcomesSwept.Add(float64(n))
		}
		w.logger.Info().Int64("rows_swept", n).
			Msg("watchdog: relabelled pending_validation outcomes under terminal executions")
	}
}

// WithTaskTeardown wires what a cancel owes once the row moved (05-scheduler
// §4.11; GitHub #304): the approval-timeout sweep cancels through
// taskcancel.Cancel, which then unblocks the task's parent and cancels its
// children. Nil keeps the write only (the stranded-parent sweep stays the
// backstop). Chainable, like WithParentUnblockSweeper.
func (w *Watchdog) WithTaskTeardown(td taskcancel.Teardown) *Watchdog {
	if w == nil {
		return w
	}
	w.taskTeardown = td
	return w
}

// WithStuckStepRecorder wires the step row a stuck-fail writes (§S8.8).
// Chainable; nil keeps the as-built behaviour (no step row).
func (w *Watchdog) WithStuckStepRecorder(r StuckStepRecorder) *Watchdog {
	if w == nil {
		return w
	}
	w.stuckSteps = r
	return w
}

// WithInFlightStepSource wires the executor's knowledge of the agent step an
// execution is running, for the stuck step row's step, role and model.
// Chainable; nil falls back to the execution row.
func (w *Watchdog) WithInFlightStepSource(s InFlightStepSource) *Watchdog {
	if w == nil {
		return w
	}
	w.inFlight = s
	return w
}

// WithParentUnblockSweeper wires the stranded-parent backstop. Chainable so the
// daemon can opt in without changing New's signature.
func (w *Watchdog) WithParentUnblockSweeper(sweeper ParentUnblockSweeper) *Watchdog {
	if w == nil {
		return w
	}
	w.parentSweeper = sweeper
	return w
}

// reconcileStuckParents re-runs the parent-unblock decision for every parent
// still in WAITING_FOR_CHILDREN. Idempotent and best-effort, like the other
// reconcile legs: the sweep short-circuits on any parent whose children are not
// all terminal, so the worst case is a no-op per entry.
func (w *Watchdog) reconcileStuckParents(ctx context.Context) {
	if w.parentSweeper == nil {
		return
	}
	w.parentSweeper.SweepStuckWaitingForChildren(ctx)
}

// handleStuck applies the configured action to a single stuck
// execution row. Dedupe by (id, updated_at) so a long-stuck row
// only fires the action on first detection plus any subsequent
// re-stick — not on every tick.
func (w *Watchdog) handleStuck(ctx context.Context, exec *persistence.Execution, now time.Time) {
	if w.hasSeen(exec.ID, exec.UpdatedAt) {
		return // already reported at this updated_at — silenced
	}
	age := now.Sub(exec.UpdatedAt)
	logEvent := w.logger.Warn().
		Str("execution_id", exec.ID).
		Str("task_id", exec.TaskID).
		Str("project_id", exec.ProjectID).
		Dur("checkpoint_age", age).
		Time("last_updated_at", exec.UpdatedAt).
		Str("action", string(w.cfg.Action))

	if w.cfg.Action == ActionFail {
		if err := w.markFailed(ctx, exec, age); err != nil {
			logEvent.Err(err).Msg("watchdog: stuck execution detected — fail action errored, row left as-is")
			return
		}
		w.markSeen(exec.ID, exec.UpdatedAt)
		if w.metrics != nil {
			w.metrics.detected.Inc()
		}
		if w.metrics != nil {
			w.metrics.failed.Inc()
		}
		logEvent.Msg("watchdog: stuck execution detected — marked FAILED with class STUCK_EXECUTION")
		return
	}
	w.markSeen(exec.ID, exec.UpdatedAt)
	if w.metrics != nil {
		w.metrics.detected.Inc()
	}
	logEvent.Msg("watchdog: stuck execution detected — warn-only, row left RUNNING")
}

// Stuck-execution verdicts (05-scheduler §4.12 C2, GitHub #142). The task's
// lease tells them apart: a dispatcher renews it only while its executor
// reports the task live, so a lease still in the future means a live goroutine
// whose checkpoint stopped advancing, and an absent or past lease means no
// process holds the execution at all.
const (
	verdictStuck     = "stuck"
	verdictAbandoned = "abandoned"
	verdictUnknown   = "unknown" // no task row to read the lease from
)

// stuckFailableStatuses is the from-set of the stuck-execution task write:
// the statuses in which an execution runs (LEASED, RUNNING), plus PENDING, the
// status a task is left in when no dispatcher drives it any more (an approval
// pause parks its execution PAUSED, so it is never listed here)
// (05-scheduler §4.12 C3).
var stuckFailableStatuses = []persistence.TaskStatus{
	persistence.TaskStatusPending, persistence.TaskStatusLeased, persistence.TaskStatusRunning,
}

// stuckVerdict reads the verdict from the task row's lease at now.
func stuckVerdict(task *persistence.Task, now time.Time) string {
	if task == nil {
		return verdictUnknown
	}
	if task.LeaseExpiresAt != nil && task.LeaseExpiresAt.After(now) {
		return verdictStuck
	}
	return verdictAbandoned
}

// stuckReason is the failure text for a stuck execution. The prefix is kept
// verbatim (logs and searches key on it); the rest says which case this is and
// what to do (outcome-quality LLD §S2 C7; 05-scheduler §4.12 C2).
func stuckReason(exec *persistence.Execution, age time.Duration, verdict string) string {
	prefix := fmt.Sprintf("watchdog: execution stuck — no checkpoint advance in %s (last update %s). ",
		age.Truncate(time.Second), exec.UpdatedAt.UTC().Format(time.RFC3339))
	switch verdict {
	case verdictAbandoned:
		return prefix + "Its task holds no live lease, so nothing was running it (a daemon restart or crash " +
			"left the row behind); retry the task if its work is still wanted"
	case verdictStuck:
		return prefix + "Its task's lease is still renewed, so the executor is alive but not advancing: " +
			"the step, its model call or the executor stopped making progress; the execution was asked to stop. " +
			"Check the step's container log and the model's health (vornikctl doctor) before retrying"
	default:
		return prefix + "The step, its model call or the executor stopped making progress; " +
			"check the step's container log and the model's health (vornikctl doctor) before retrying"
	}
}

// markFailed flips both the execution and (when wired) the task row
// to terminal with class STUCK_EXECUTION, then — only when its task write
// moved the row — tears the execution down through the cancel primitive's
// post-write sequence (taskcancel.AfterTerminal, no child cascade): the live
// container is asked to stop once and the parent unblock runs (05-scheduler
// §4.12 C3, GitHub #142). Before 2026-10-12 it wrote the rows and nothing
// else, so the container ran on and the scheduler slot stayed held.
//
// Either side failing is surfaced to the caller so the metric increment +
// the success-log happen only on the all-or-nothing path.
func (w *Watchdog) markFailed(ctx context.Context, exec *persistence.Execution, age time.Duration) error {
	var (
		task    *persistence.Task
		taskErr error
	)
	if w.taskRepo != nil && exec.TaskID != "" {
		task, taskErr = w.taskRepo.Get(ctx, exec.TaskID)
	}
	verdict := verdictUnknown
	if taskErr == nil {
		verdict = stuckVerdict(task, w.now())
	}
	msg := stuckReason(exec, age, verdict)
	if err := w.execRepo.RecordFailure(ctx, exec.ID, msg, persistence.TaskFailureClassStuckExecution); err != nil {
		return fmt.Errorf("execution RecordFailure: %w", err)
	}
	// The step that hung gets its row before the teardown is asked for, so
	// the executor's late row (the step returning because it was stopped)
	// finds the execution already failed and is not written (§S8.8).
	w.recordStuckStep(ctx, exec, verdict, msg)
	if w.taskRepo == nil || exec.TaskID == "" {
		return nil
	}
	// The task vanished between the scan and this Get — deleted with its
	// project, or pruned. There is nothing left to flip, and the execution row
	// is already marked, so this is a clean finish and NOT an error.
	//
	// It used to be one. The short-circuit was written as `task == nil`, which
	// TaskRepository.Get never returns: it is MissErrNotFound. So a vanished
	// task took the error path, and that path is expensive — handleStuck logs
	// "row left as-is" (false: RecordFailure above already flipped it), skips
	// markSeen so the same execution is re-reported and re-flipped on every
	// sweep, and never increments the detected/failed counters, so the stuck
	// count under-reports. Found 2026-08-22 by tightening the package's task
	// double, which had been answering a miss with (nil, nil).
	if errors.Is(taskErr, persistence.ErrNotFound) {
		return nil
	}
	if taskErr != nil {
		// A real lookup failure. Best-effort task-side update; an execution row
		// marked FAILED with a still-RUNNING task row will surface as a
		// discrepancy in the UI which an operator can investigate.
		return fmt.Errorf("task Get: %w", taskErr)
	}
	if task == nil {
		return nil // depth: a future backend that answers a miss with a bare nil
	}
	// Defensive: if the task is already in a terminal state, don't
	// regress it. Watchdog only runs on RUNNING executions, but a
	// race with the executor's own handleFailure could land us
	// here just after the task transitioned.
	switch task.Status {
	case persistence.TaskStatusCompleted, persistence.TaskStatusFailed,
		persistence.TaskStatusCancelled, persistence.TaskStatusClosed:
		return nil
	}
	// Conditional, not a full-row Update: a status another writer set between
	// the Get above and this write is not overwritten (05-scheduler §4.12 C3).
	// From PENDING, LEASED or RUNNING only (stuckFailableStatuses).
	// A task §6 recovery already re-queued, or one parked (AWAITING_*,
	// WAITING_FOR_CHILDREN, PAUSED) behind a leftover RUNNING execution row,
	// is not this scan's to fail; the full-row Update used to fail them too.
	// The lease is kept on purpose — the dispatch's own release still decides
	// retry or terminal on it when the goroutine returns.
	errClass := persistence.TaskFailureClassStuckExecution
	moved, err := w.taskRepo.TransitionConditional(ctx, task.ID, stuckFailableStatuses,
		persistence.TaskStatusFailed, persistence.TransitionOpts{LastError: &msg, LastErrorClass: &errClass})
	if err != nil {
		return fmt.Errorf("task transition: %w", err)
	}
	if !moved {
		return nil // another writer finished the task; it owns the teardown
	}
	out := taskcancel.AfterTerminal(ctx, w.taskTeardown, task.ID, false)
	ev := w.logger.Info()
	if out.TeardownErr != nil {
		ev = w.logger.Warn().Err(out.TeardownErr)
	}
	ev.Str("task_id", task.ID).
		Str("execution_id", exec.ID).
		Str("verdict", verdict).
		Bool("torn_down", out.TornDown).
		Msg("watchdog: stuck execution failed; teardown requested through the cancel primitive")
	return nil
}

// Results of the stuck step row (vornik_watchdog_stuck_step_outcomes_total).
const (
	stuckStepRecorded        = "recorded"
	stuckStepAlreadyRecorded = "already_recorded"
	stuckStepNoStep          = "no_step"
	stuckStepError           = "error"
)

// stuckStepOf names the step of exec that was in flight: the executor's own
// record when this process runs it, else the execution row's current step,
// with its role from the workflow snapshot (outcome-quality §S8.8).
func (w *Watchdog) stuckStepOf(exec *persistence.Execution) (stepoutcome.InFlightStep, bool) {
	if w.inFlight != nil {
		if s, ok := w.inFlight.InFlightStep(exec.ID); ok && s.StepID != "" {
			return s, true
		}
	}
	if exec.CurrentStepID == nil || *exec.CurrentStepID == "" {
		return stepoutcome.InFlightStep{}, false
	}
	s := stepoutcome.InFlightStep{StepID: *exec.CurrentStepID}
	s.Role, s.Model = snapshotStepRole(exec.WorkflowSnapshot, s.StepID)
	return s, true
}

// snapshotStepRole reads a step's role from the execution's workflow snapshot
// (the workflow marshalled as JSON at execution start), spelled as the
// executor records it: an agent or plan step its role and no model (the
// model is resolved only while the step runs), a system step "system" with
// its handler, a gate step "gate". Empty when the snapshot is absent or does
// not name the step.
func snapshotStepRole(snapshot []byte, stepID string) (role, model string) {
	if len(snapshot) == 0 {
		return "", ""
	}
	var wf struct {
		Steps map[string]struct {
			Type    string
			Role    string
			Handler string
		}
	}
	if json.Unmarshal(snapshot, &wf) != nil {
		return "", ""
	}
	st, ok := wf.Steps[stepID]
	if !ok {
		return "", ""
	}
	switch st.Type {
	case "system":
		return stepoutcome.RoleSystem, st.Handler
	case "gate":
		return stepoutcome.RoleGate, ""
	}
	return st.Role, ""
}

// recordStuckStep writes the row for the step of exec that was in flight when
// the watchdog failed it (outcome-quality-design §S8.8, GitHub #369): outcome
// failed for a stuck execution (a live goroutine that stopped advancing),
// orphaned when nothing was running it, class stuck_execution. At most one row
// per execution (its id), and none when the step already has a row since the
// last checkpoint. Best-effort: a failure is logged and counted, and the
// caller still fails the task.
func (w *Watchdog) recordStuckStep(ctx context.Context, exec *persistence.Execution, verdict, reason string) {
	if w.stuckSteps == nil {
		return
	}
	step, ok := w.stuckStepOf(exec)
	if !ok {
		w.countStuckStep(stuckStepNoStep)
		w.logger.Info().Str("execution_id", exec.ID).
			Msg("watchdog: stuck execution had no step in flight; no step-outcome row written")
		return
	}
	outcome := stepoutcome.Failed
	if verdict != verdictStuck {
		outcome = stepoutcome.Orphaned
	}
	now := w.now()
	row := &persistence.ExecutionStepOutcome{
		ID:          "outc_stuck_" + exec.ID,
		ProjectID:   exec.ProjectID,
		TaskID:      exec.TaskID,
		ExecutionID: exec.ID,
		StepID:      step.StepID,
		Role:        step.Role,
		Model:       step.Model,
		Outcome:     string(outcome),
		ErrorClass:  stepoutcome.ClassStuckExecution,
		ErrorDetail: reason,
		FinalizedAt: &now,
		RecordedAt:  now,
	}
	if !step.StartedAt.IsZero() {
		d := now.Sub(step.StartedAt).Milliseconds()
		row.DurationMS = &d
	}
	inserted, err := w.stuckSteps.RecordStuckStepOutcome(ctx, row, exec.UpdatedAt)
	ev := w.logger.Info()
	result := stuckStepRecorded
	switch {
	case err != nil:
		result = stuckStepError
		ev = w.logger.Warn().Err(err)
	case !inserted:
		result = stuckStepAlreadyRecorded
	}
	w.countStuckStep(result)
	ev.Str("execution_id", exec.ID).
		Str("step", step.StepID).
		Str("role", step.Role).
		Str("model", step.Model).
		Str("outcome", string(outcome)).
		Str("result", result).
		Msg("watchdog: step-outcome row for the stuck execution's step")
}

func (w *Watchdog) countStuckStep(result string) {
	if w.metrics != nil && w.metrics.stuckStepOutcomes != nil {
		w.metrics.stuckStepOutcomes.WithLabelValues(result).Inc()
	}
}

// markSeen records (id, updatedAt) and returns true when this is the
// first observation at this updated_at — i.e. the caller should
// fire the action. Returns false on a repeat observation.
func (w *Watchdog) markSeen(id string, updatedAt time.Time) bool {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	if prev, ok := w.seen[id]; ok && prev.Equal(updatedAt) {
		return false
	}
	w.seen[id] = updatedAt
	return true
}

func (w *Watchdog) hasSeen(id string, updatedAt time.Time) bool {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	prev, ok := w.seen[id]
	return ok && prev.Equal(updatedAt)
}

// clearSeen removes the dedupe entry for an execution that has
// freshened its checkpoint. Idempotent.
func (w *Watchdog) clearSeen(id string) {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	delete(w.seen, id)
}

// pruneSeen drops dedupe entries for executions that are no longer
// RUNNING (completed, failed, cancelled) so the map doesn't grow
// without bound across the daemon's lifetime.
func (w *Watchdog) pruneSeen(stillRunning map[string]struct{}) {
	w.seenLock.Lock()
	defer w.seenLock.Unlock()
	for id := range w.seen {
		if _, ok := stillRunning[id]; !ok {
			delete(w.seen, id)
		}
	}
}
