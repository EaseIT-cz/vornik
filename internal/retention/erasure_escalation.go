package retention

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/datasubject"
)

// The Art 12(3) escalation pass (GitHub #344 slice 5d-1c; art17 redact design
// §15.16). A shared transcript row that a running task still holds is
// deferred by the erasure, and the request stays open. Seven days before the
// response deadline the operator is told, once per row and deadline, so the
// task can be finished or cancelled and the erasure re-run, or an extension
// claimed while it is still lawful. The pass decides nothing: it never writes
// a report, never touches a transcript and never extends a request.
//
// see LLD § https://docs.vornik.io §15.16

// EscalationSource is what the pass reads and records, on either driver.
type EscalationSource interface {
	ListLiveRequests(ctx context.Context) ([]datasubject.Request, error)
	GetRequestReport(ctx context.Context, id string) (reportJSON, hash string, err error)
	EscalationRaised(ctx context.Context, k datasubject.EscalationKey) (bool, error)
	RecordEscalation(ctx context.Context, k datasubject.EscalationKey, raisedAt time.Time) error
	TaskStatus(ctx context.Context, taskID string) (string, error)
}

// Escalator sends one operator alert and says whether it was sent
// (steering.OperatorAlertNotifier.SendOperator).
type Escalator interface {
	SendOperator(ctx context.Context, subject, body string) error
}

// ErasureEscalation is the pass. One per process.
type ErasureEscalation struct {
	src    EscalationSource
	esc    Escalator
	logger zerolog.Logger
	// SetUnsent receives the number of due rows that could not be sent on
	// each run (the vornik_erasure_escalations_unsent gauge). Nil is allowed.
	SetUnsent func(int)

	mu sync.Mutex
	// lastLogged rate-limits the unsent warning to once a day per row. It is
	// per process: after a leader change the new leader may log a row the
	// previous one logged that day, which is harmless because the gauge, not
	// the log, is the signal (review ef8f).
	lastLogged map[datasubject.EscalationKey]time.Time
}

// NewErasureEscalation builds the pass.
func NewErasureEscalation(src EscalationSource, esc Escalator, logger zerolog.Logger) *ErasureEscalation {
	return &ErasureEscalation{src: src, esc: esc, logger: logger, lastLogged: map[datasubject.EscalationKey]time.Time{}}
}

// Escalations is what one run did.
type Escalations struct {
	// Due counts rows inside the margin whose task is still live.
	Due int
	// Raised counts alerts sent and recorded this run.
	Raised int
	// Unsent counts due rows whose alert could not be sent.
	Unsent int
	// Logged counts unsent warnings written to the log this run.
	Logged int
}

const unsentLogInterval = 24 * time.Hour

// Run checks every live erasure request once.
func (p *ErasureEscalation) Run(ctx context.Context, now time.Time) (Escalations, error) {
	var out Escalations
	reqs, err := p.src.ListLiveRequests(ctx)
	if err != nil {
		return out, fmt.Errorf("retention: list live requests: %w", err)
	}
	var errs []error
	for _, req := range reqs {
		if req.Kind != datasubject.RequestErasure {
			continue
		}
		deadline := req.Deadline()
		if now.Before(deadline.Add(-datasubject.EscalationMargin)) {
			continue
		}
		if err := p.runRequest(ctx, req, deadline, now, &out); err != nil {
			errs = append(errs, err)
		}
	}
	if p.SetUnsent != nil {
		p.SetUnsent(out.Unsent)
	}
	return out, errors.Join(errs...)
}

func (p *ErasureEscalation) runRequest(ctx context.Context, req datasubject.Request, deadline, now time.Time, out *Escalations) error {
	blob, _, err := p.src.GetRequestReport(ctx, req.ID)
	if err != nil || strings.TrimSpace(blob) == "" {
		return nil // no erasure has run yet: nothing was deferred
	}
	var report struct {
		Result struct {
			Deferred []datasubject.DeferredAction `json:"deferred"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(blob), &report); err != nil {
		return fmt.Errorf("retention: read the erasure report of %s: %w", req.ID, err)
	}
	var errs []error
	for _, d := range report.Result.Deferred {
		if d.LiveTaskID == "" {
			continue
		}
		live, err := p.taskLive(ctx, d.LiveTaskID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !live {
			continue
		}
		out.Due++
		k := datasubject.EscalationKey{RequestID: req.ID, Table: d.Table, RowID: d.RowID, Deadline: deadline}
		raised, err := p.src.EscalationRaised(ctx, k)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if raised {
			continue
		}
		subject, body := escalationText(req, d, deadline)
		if err := p.esc.SendOperator(ctx, subject, body); err != nil {
			out.Unsent++
			p.logUnsent(k, err, now, out)
			continue
		}
		// Recorded only after the send, so a failed send retries.
		if err := p.src.RecordEscalation(ctx, k, now); err != nil {
			errs = append(errs, err)
			continue
		}
		out.Raised++
	}
	return errors.Join(errs...)
}

func (p *ErasureEscalation) taskLive(ctx context.Context, taskID string) (bool, error) {
	status, err := p.src.TaskStatus(ctx, taskID)
	if err != nil {
		return false, err
	}
	for _, s := range datasubject.TranscriptLiveStatuses() {
		if status == s {
			return true, nil
		}
	}
	return false, nil
}

func (p *ErasureEscalation) logUnsent(k datasubject.EscalationKey, err error, now time.Time, out *Escalations) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if last, ok := p.lastLogged[k]; ok && now.Sub(last) < unsentLogInterval {
		return
	}
	p.lastLogged[k] = now
	out.Logged++
	p.logger.Warn().Err(err).Str("request", k.RequestID).Str("row", string(k.Table)+"/"+k.RowID).
		Time("deadline", k.Deadline).
		Msg("erasure escalation: a deferred row is still held by a running task near the Art 12(3) deadline, " +
			"and the operator alert could not be sent")
}

// escalationText is the alert. It names the request, the record, the task and
// the deadline, and what the operator can do; never the record's content.
func escalationText(req datasubject.Request, d datasubject.DeferredAction, deadline time.Time) (subject, body string) {
	subject = fmt.Sprintf("GDPR erasure %s: a record is still held by running task %s", req.ID, d.LiveTaskID)
	var b strings.Builder
	fmt.Fprintf(&b, "Request %s must be answered by %s (Art 12(3)).\n", req.ID, deadline.UTC().Format("2006-01-02 15:04 UTC"))
	fmt.Fprintf(&b, "Record %s/%s was not erased because task %s is still running.\n", d.Table, d.RowID, d.LiveTaskID)
	b.WriteString("Finish or cancel that task, then re-run: vornikctl subject erase " + req.ID + "\n")
	if !req.Extended {
		b.WriteString("If the request cannot be completed in time, an Art 12(3) extension is available (up to two more " +
			"months) only if the subject is told of it, with the reasons, within one month of receipt " +
			"(by " + datasubject.FirstMonthEnd(req.OpenedAt).UTC().Format("2006-01-02 15:04 UTC") + "). Record it with: " +
			"vornikctl subject extend " + req.ID + " --reason <why> --notified-at <when the subject was told>\n")
	}
	return subject, b.String()
}
