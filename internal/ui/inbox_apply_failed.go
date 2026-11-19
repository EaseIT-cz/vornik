package ui

import (
	"context"
	"net/http"
	"time"
)

// Approved requests whose change could not be applied, shown in /inbox for
// an operator-wide viewer (GitHub #669 item 5, design 22 §22.2.16b). The
// approver-device service reads, masks and cuts them (one implementation,
// the phone's); the console renders them, with a Dismiss (GitHub #806,
// §22.2.16d). Retry is the phone's: the device holds the approval.

// ApplyFailedCard is one approved request that could not be applied.
type ApplyFailedCard struct {
	ID         string
	Sentence   string
	ApprovedAt time.Time
	Reason     string
	Retryable  bool
}

// WithAgentApplyFailed wires the failed list; more says rows were left out.
// Optional: nil hides the section.
func WithAgentApplyFailed(list func(context.Context) (rows []ApplyFailedCard, more bool, err error)) ServerOption {
	return func(s *Server) { s.agentApplyFailed = list }
}

type applyFailedView struct {
	ID, Sentence, Approved, Reason string
	Retryable                      bool
}

// loadAgentApplyFailed lists the failed requests for the operator. A
// project-scoped viewer sees none, as for the pending requests. A load error
// is logged and reported as failed, so the page never says "All clear" over
// a list it could not read (the #167 rule; code review a1f5 F6).
func (s *Server) loadAgentApplyFailed(r *http.Request) (rows []applyFailedView, more, failed bool) {
	if s.agentApplyFailed == nil || scopeQueryIDs(r) != nil {
		return nil, false, false
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	cards, more, err := s.agentApplyFailed(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("inbox: failed approver-device requests list failed; the page says so")
		return nil, false, true
	}
	out := make([]applyFailedView, 0, len(cards))
	for _, c := range cards {
		v := applyFailedView{ID: c.ID, Sentence: c.Sentence, Reason: c.Reason, Retryable: c.Retryable}
		if !c.ApprovedAt.IsZero() {
			v.Approved = humanizeSince(time.Since(c.ApprovedAt)) + " ago"
		}
		out = append(out, v)
	}
	return out, more, false
}
