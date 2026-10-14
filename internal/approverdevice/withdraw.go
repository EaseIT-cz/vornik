package approverdevice

import (
	"context"
	"errors"
	"log"

	"vornik.io/vornik/internal/persistence"
)

// Withdrawal of a pending request the daemon knows no longer applies
// (agent-administered design §9.2d, GitHub #264). The race with a device
// decision is settled by the repository: WithdrawRequest and Decide are each
// one statement guarded on status = 'pending', so exactly one transitions.

// ErrNotWithdrawable refuses a withdrawal of a request whose kind has not
// opted in (RegisterWithdrawable).
var ErrNotWithdrawable = errors.New("approverdevice: this kind of request cannot be withdrawn")

// WithdrawOutcome is what a withdrawal found.
type WithdrawOutcome string

// Withdrawal outcomes.
const (
	// WithdrawDone: this call withdrew the request.
	WithdrawDone WithdrawOutcome = "withdrawn"
	// WithdrawAlready: it was withdrawn before; nothing changed or fired.
	WithdrawAlready WithdrawOutcome = "already_withdrawn"
	// WithdrawTooLate: it had already ended otherwise (Status says how); the
	// decision stands and the caller handles it.
	WithdrawTooLate WithdrawOutcome = "too_late"
)

// WithdrawResult is a withdrawal's outcome and, for WithdrawTooLate, the
// status the request ended in.
type WithdrawResult struct {
	Outcome WithdrawOutcome
	Status  string
}

// RegisterWithdrawable lets requests of kind be withdrawn. Withdrawal is
// opt-in: a kind whose asker reads the status itself (host_action), whose
// claim flow reads only rejected/expired (device_enrollment), or that has its
// own state row (broker_action, memory_retention) is not registered.
func (s *Service) RegisterWithdrawable(kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.withdrawable == nil {
		s.withdrawable = map[string]bool{}
	}
	s.withdrawable[kind] = true
}

// Withdraw ends a pending request that no longer applies, with by (a daemon
// actor constant, e.g. "system:agent-admin-cover") and a one-sentence reason
// built by the daemon from a fixed template, never from agent prose. It is
// daemon-side only: no HTTP route, agent verb or companion tool reaches it,
// and none may be added without a design review (review 0df6 F6).
//
// It is idempotent: a repeat answers WithdrawAlready and fires nothing. A
// request already decided or expired answers WithdrawTooLate with its
// status. The terminal observers see "withdrawn"; no push is sent (design
// §9.2d item 3).
func (s *Service) Withdraw(ctx context.Context, id, by, reason string) (WithdrawResult, error) {
	if _, err := persistence.ValidWithdrawal(by, reason); err != nil {
		return WithdrawResult{}, err
	}
	r, err := s.repo.GetRequest(ctx, id)
	if err != nil {
		return WithdrawResult{}, err
	}
	s.mu.RLock()
	ok := s.withdrawable[r.Kind]
	s.mu.RUnlock()
	if !ok {
		return WithdrawResult{}, ErrNotWithdrawable
	}
	s.seam("withdraw", id)
	err = s.repo.WithdrawRequest(ctx, id, by, reason, s.now())
	if errors.Is(err, persistence.ErrApprovalNoTransition) {
		now, gerr := s.repo.GetRequest(ctx, id)
		if gerr != nil {
			return WithdrawResult{}, gerr
		}
		if now.Status == persistence.ApprovalWithdrawn {
			return WithdrawResult{Outcome: WithdrawAlready, Status: now.Status}, nil
		}
		return WithdrawResult{Outcome: WithdrawTooLate, Status: now.Status}, nil
	}
	if err != nil {
		return WithdrawResult{}, err
	}
	done, err := s.repo.GetRequest(ctx, id)
	if err != nil {
		// Withdrawn; only the observers' signal is lost, and the row is
		// authoritative (design §20.2). Say so (review b4f2 F5).
		log.Printf("approverdevice: request withdrawn id=%s by=%s; re-reading it for the observers failed: %v", id, by, err)
		return WithdrawResult{Outcome: WithdrawDone, Status: persistence.ApprovalWithdrawn}, nil
	}
	log.Printf("approverdevice: request withdrawn id=%s kind=%s by=%s reason=%q", done.ID, done.Kind, by, done.WithdrawReason)
	s.notifyTerminal(ctx, *done, persistence.ApprovalWithdrawn)
	return WithdrawResult{Outcome: WithdrawDone, Status: persistence.ApprovalWithdrawn}, nil
}

// seam runs the test hook between reading a request and its guarded
// transition, so a test can force the competing call in between
// deterministically. Nil in production.
func (s *Service) seam(op, id string) {
	if s.beforeTransition != nil {
		s.beforeTransition(op, id)
	}
}
