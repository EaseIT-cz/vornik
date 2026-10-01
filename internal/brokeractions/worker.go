// Package brokeractions executes broker write actions a person approved in
// /inbox — https://docs.vornik.io
// design.md §5.4.
//
// The worker is the only thing that ever calls a broker write tool. It claims
// an approved row (a single-winner CAS), re-checks every gate immediately
// before the call, calls the tool exactly once with the stored bytes, and
// records what happened. Nothing retries a write: an outcome it cannot be sure
// of is recorded unknown, for an operator to resolve.
package brokeractions

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
)

// Caller makes one tool call (mcp.Manager.CallToolOnce).
type Caller interface {
	CallToolOnce(ctx context.Context, projectID, qualifiedName, argsJSON string) (text string, isError bool, err error)
}

// Config wires the worker.
type Config struct {
	Repo   persistence.BrokerActionRepository
	Caller Caller
	// WritesOn reports the daemon's broker.writes, read at call time.
	WritesOn func() bool
	// ToolDeclared reports why the project's configuration no longer allows
	// the tool as a broker write (server not broker_write, tool not in its
	// allowed_tools), or nil. Read at call time.
	ToolDeclared func(projectID, tool string) error
	// Timeout bounds one tool call (broker.action_timeout).
	Timeout time.Duration
	// Redact scrubs the tool's response before it is stored. Nil means no
	// scanning is available (secret scanning off, or its detector failed to
	// build): the response is stored as returned and OnUnscanned is told.
	Redact func([]byte) []byte
	// OnUnscanned is told each time a tool response is stored without
	// being scanned. Optional.
	OnUnscanned func()
	// OnFinishError is told when an outcome could not be recorded; the row
	// stays executing for an operator. Optional.
	OnFinishError func(a *persistence.BrokerAction, status string)
	// Gauge receives the stuck-row counts after each sweep. Optional.
	Gauge func([]persistence.BrokerActionStuck)
	// OnTransition is told about each terminal outcome (the push hook).
	// Optional.
	OnTransition func(a *persistence.BrokerAction)
	// OnChange is told after the worker changes an action's state (each
	// terminal write, and an expiry sweep that expired rows): the push
	// outbox's kick (design §7a). Optional; must not block.
	OnChange func()
	// Interval between sweeps; default one minute.
	Interval time.Duration
	Logger   zerolog.Logger
	Now      func() time.Time
}

const (
	// outcomeCapBytes caps the stored tool response.
	outcomeCapBytes = 16 * 1024
	// DefaultActionTimeout is broker.action_timeout's default.
	DefaultActionTimeout = 60 * time.Second
)

// Worker executes approved broker actions.
type Worker struct {
	cfg  Config
	kick chan string
}

// New builds a worker.
func New(cfg Config) *Worker {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	if cfg.Timeout <= 0 {
		// A zero timeout would expire every call before it is sent and
		// record each action unknown.
		cfg.Timeout = DefaultActionTimeout
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Worker{cfg: cfg, kick: make(chan string, 64)}
}

// Kick asks the worker to execute an action now (the approve handler calls
// it). Non-blocking: a full queue is caught by the next sweep. Nil-safe.
func (w *Worker) Kick(actionID string) {
	if w == nil {
		return
	}
	select {
	case w.kick <- actionID:
	default:
	}
}

// Run promotes the staged rows of tasks that completed before a crash, then
// executes kicked actions and sweeps on an interval until ctx ends.
func (w *Worker) Run(ctx context.Context) {
	if n, err := w.cfg.Repo.PromoteStagedOfCompletedTasks(ctx); err != nil {
		w.cfg.Logger.Warn().Err(err).Msg("broker actions: startup promotion failed")
	} else if n > 0 {
		w.cfg.Logger.Info().Int64("actions", n).Msg("broker actions: promoted staged proposals of completed tasks")
	}
	// Sweep once now: approved rows a restart left behind should not wait a
	// full interval.
	w.Sweep(ctx)
	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-w.kick:
			w.Execute(ctx, id)
		case <-ticker.C:
			w.Sweep(ctx)
		}
	}
}

// Sweep expires overdue approvals, discards orphaned staged rows, executes
// any approved row left behind (a restart, or a full kick queue), and feeds
// the stuck-row gauge.
func (w *Worker) Sweep(ctx context.Context) {
	now := w.cfg.Now()
	if n, err := w.cfg.Repo.ExpireDue(ctx, now); err != nil {
		w.cfg.Logger.Warn().Err(err).Msg("broker actions: expiry sweep failed")
	} else if n > 0 {
		w.changed()
	}
	if _, err := w.cfg.Repo.DiscardStagedOrphans(ctx); err != nil {
		w.cfg.Logger.Warn().Err(err).Msg("broker actions: orphan sweep failed")
	}
	if approved, err := w.cfg.Repo.ListByStatus(ctx, "", persistence.BrokerActionApproved, 50); err == nil {
		for _, a := range approved {
			w.Execute(ctx, a.ActionID)
		}
	}
	if w.cfg.Gauge != nil {
		if stuck, err := w.cfg.Repo.CountStuck(ctx, now, persistence.BrokerActionStuckAfter, persistence.BrokerActionApprovedStuckAfter); err == nil {
			w.cfg.Gauge(stuck)
		}
	}
}

// Execute runs one approved action, if this worker wins its claim. A panic
// anywhere in it is contained: the claimed row stays executing, which the
// stuck gauge and doctor surface, and the worker goes on to the next action
// (review-20260930-bd00 F3).
func (w *Worker) Execute(ctx context.Context, actionID string) {
	defer func() {
		if r := recover(); r != nil {
			w.cfg.Logger.Error().Str("action_id", actionID).Interface("panic", r).
				Msg("broker actions: panic while executing; the row stays executing for an operator")
		}
	}()
	won, err := w.cfg.Repo.ClaimForExecution(ctx, actionID, w.cfg.Now())
	if err != nil || !won {
		return // not approved, expired, or another worker has it
	}
	a, err := w.cfg.Repo.Get(ctx, actionID)
	if err != nil {
		// Claimed but unreadable: nothing was sent. The row stays
		// executing, which the stuck gauge surfaces for an operator.
		w.cfg.Logger.Error().Err(err).Str("action_id", actionID).Msg("broker actions: claimed row could not be read")
		return
	}
	if reason := w.recheck(a); reason != "" {
		w.finish(ctx, a, persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError,
			outcomeDoc("refused", reason))
		return
	}
	callCtx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	text, isErr, err := w.cfg.Caller.CallToolOnce(callCtx, a.ProjectID, a.Tool, string(a.ArgsJSON))
	status, class := classify(callCtx, isErr, err)
	body := text
	if err != nil {
		body = err.Error()
	}
	w.finish(ctx, a, status, class, outcomeDoc("response", w.redact(capString(body, outcomeCapBytes))))
}

// redact scrubs a tool response, or reports that it could not.
func (w *Worker) redact(s string) string {
	if w.cfg.Redact == nil {
		if w.cfg.OnUnscanned != nil {
			w.cfg.OnUnscanned()
		}
		return s
	}
	return string(w.cfg.Redact([]byte(s)))
}

// recheck returns why the action may not be sent now, or "".
func (w *Worker) recheck(a *persistence.BrokerAction) string {
	if w.cfg.WritesOn == nil || !w.cfg.WritesOn() {
		return "broker.writes is off"
	}
	if w.cfg.ToolDeclared != nil {
		if err := w.cfg.ToolDeclared(a.ProjectID, a.Tool); err != nil {
			return "tool no longer declared as a broker write: " + err.Error()
		}
	}
	if sum, err := approval.CanonicalSHA256(a.ArgsJSON); err != nil || sum != a.ArgsSHA256 {
		return "stored arguments no longer match the approved hash"
	}
	return ""
}

// classify maps one call to its terminal state (design §5.4 step 4). When in
// doubt it is unknown: failed tells the operator nothing was sent.
func classify(callCtx context.Context, isToolError bool, err error) (string, string) {
	switch {
	case err == nil && !isToolError:
		return persistence.BrokerActionExecuted, persistence.BrokerOutcomeOK
	case err == nil:
		return persistence.BrokerActionFailed, persistence.BrokerOutcomeToolError
	case errors.Is(err, mcp.ErrNotSent):
		return persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError
	case errors.Is(callCtx.Err(), context.DeadlineExceeded):
		return persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout
	default:
		return persistence.BrokerActionUnknown, persistence.BrokerOutcomeTransportError
	}
}

func (w *Worker) finish(ctx context.Context, a *persistence.BrokerAction, status, class string, outcome []byte) {
	if err := w.cfg.Repo.Finish(ctx, a.ActionID, status, class, outcome, w.cfg.Now()); err != nil {
		w.cfg.Logger.Error().Err(err).Str("action_id", a.ActionID).Str("status", status).
			Msg("broker actions: outcome could not be recorded; the row stays executing for an operator")
		if w.cfg.OnFinishError != nil {
			w.cfg.OnFinishError(a, status)
		}
		return
	}
	w.cfg.Logger.Info().Str("action_id", a.ActionID).Str("project", a.ProjectID).Str("tool", a.Tool).
		Str("status", status).Str("outcome_class", class).Msg("broker action finished")
	w.changed()
	// Success-only: OnTransition feeds actions_finished_total and
	// OnFinishError feeds finish_failed_total, so each terminal write
	// counts in exactly one of them (review-20260930-8985 N1).
	if w.cfg.OnTransition != nil {
		a.Status, a.OutcomeClass = status, class
		w.cfg.OnTransition(a)
	}
}

func outcomeDoc(key, value string) []byte {
	b, _ := json.Marshal(map[string]string{key: value})
	return b
}

// capString cuts s to at most n bytes without splitting a character
// (review-20260930-bd00 F10).
func capString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func (w *Worker) changed() {
	if w.cfg.OnChange != nil {
		w.cfg.OnChange()
	}
}
