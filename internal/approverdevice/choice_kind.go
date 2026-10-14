package approverdevice

import (
	"context"
	"fmt"

	"vornik.io/vornik/internal/persistence"
)

// FileRequestPush stores a request the daemon files for itself (not an
// agent's) and pushes subject with the sentence and the link: no values, no
// decision buttons (§9.3). A duplicate id is the repository's error.
func (s *Service) FileRequestPush(ctx context.Context, r persistence.AgentApprovalRequestRow, subject string) error {
	if err := s.repo.CreateRequest(ctx, r); err != nil {
		return err
	}
	s.push(ctx, subject, fmt.Sprintf("%s\nDecide on your approver device: %s/ui/approve/%s\n%s", r.Sentence, s.origin, r.ID, browserLine))
	return nil
}

// Choice kinds: a request answered with one of a fixed set of choices
// instead of approve and reject, through the same guarded path host_action
// uses (DecideChoice: offer check, token rotation, rendered-hash binding,
// DecidedChoice). The first is config_journal_drift, keep or restore an
// interrupted config apply (config-apply journal design §13.3, GitHub #302).
// host_action keeps its own choices (HostChoices) and page.

// Choice is one answer a choice kind's page offers: the posted value and
// the button's label.
type Choice struct {
	Value, Label string
}

// ChoicesFunc returns the choices a request of a choice kind offers, in
// page order. Every choice approves: a choice kind has no deny unless the
// kind offers one named ChoiceDeny.
type ChoicesFunc func(r persistence.AgentApprovalRequestRow) []Choice

// RegisterChoices makes kind a choice kind answered with fn's choices.
func (s *Service) RegisterChoices(kind string, fn ChoicesFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.choices == nil {
		s.choices = map[string]ChoicesFunc{}
	}
	s.choices[kind] = fn
}

func (s *Service) choicesFunc(kind string) (ChoicesFunc, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	fn, ok := s.choices[kind]
	return fn, ok
}

// isChoiceKind reports whether a request is decided by a choice: host_action
// or a registered choice kind.
func (s *Service) isChoiceKind(kind string) bool {
	if kind == persistence.ApprovalKindHostAction {
		return true
	}
	_, ok := s.choicesFunc(kind)
	return ok
}

// choiceValues is the set of values a request offers from the device.
func (s *Service) choiceValues(r persistence.AgentApprovalRequestRow) []string {
	if r.Kind == persistence.ApprovalKindHostAction {
		return HostChoices(r)
	}
	var out []string
	for _, c := range s.pageChoices(r) {
		out = append(out, c.Value)
	}
	return out
}

// pageChoices is a registered choice kind's choices; nil for any other kind.
func (s *Service) pageChoices(r persistence.AgentApprovalRequestRow) []Choice {
	fn, ok := s.choicesFunc(r.Kind)
	if !ok || fn == nil {
		return nil
	}
	return fn(r)
}
