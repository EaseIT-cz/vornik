package dispatcher

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/chat"
)

// Steering (dispatcher turn-context design §9, GitHub #645): a message the
// user sends while a turn runs is added to that turn at its next model step,
// so a correction ("stop doing X, do Y") changes the turn in progress instead
// of waiting behind it.

// steerPrefix marks a steered message for the model.
const steerPrefix = "[Sent while you were working] "

// SteerInbox holds the messages sent to one running turn. The channel offers
// into it; the turn drains it; Close ends it and returns what was never read.
type SteerInbox struct {
	mu      sync.Mutex
	closed  bool
	pending []steerItem
	// drained are items read into the turn but not yet answered: a turn
	// that then fails hands them back (review cd48 F2).
	drained []steerItem
}

type steerItem struct {
	text string
	ref  any // the channel's own handle, returned by Close for a leftover
}

// NewSteerInbox returns an open inbox.
func NewSteerInbox() *SteerInbox { return &SteerInbox{} }

// Offer adds a message; false once the inbox is closed, so the caller takes
// its normal path.
func (s *SteerInbox) Offer(text string, ref any) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.pending = append(s.pending, steerItem{text: text, ref: ref})
	return true
}

// Drain returns the texts offered since the last drain.
func (s *SteerInbox) Drain() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.pending))
	for _, it := range s.pending {
		out = append(out, it.text)
	}
	s.drained = append(s.drained, s.pending...)
	s.pending = nil
	return out
}

// markAnswered records that the turn answered everything drained so far.
func (s *SteerInbox) markAnswered() {
	s.mu.Lock()
	s.drained = nil
	s.mu.Unlock()
}

// Close ends the inbox and returns the handles of messages the turn did not
// answer: those never drained and, when requeueUnanswered, those drained but
// not answered because the turn failed. After Close every Offer is refused,
// so nothing is lost and nothing is answered twice.
func (s *SteerInbox) Close(requeueUnanswered bool) []any {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	items := s.pending
	if requeueUnanswered {
		items = append(append([]steerItem(nil), s.drained...), s.pending...)
	}
	out := make([]any, 0, len(items))
	for _, it := range items {
		out = append(out, it.ref)
	}
	s.pending, s.drained = nil, nil
	return out
}

// markSteeredAnswered is called when the turn returns its final answer. Any
// other return keeps the drained messages, so a failed turn hands them back.
func markSteeredAnswered(ctx context.Context) {
	if in := steeringFrom(ctx); in != nil {
		in.markAnswered()
	}
}

type steerKey struct{}

// WithSteering attaches a turn's inbox to its context.
func WithSteering(ctx context.Context, inbox *SteerInbox) context.Context {
	if inbox == nil {
		return ctx
	}
	return context.WithValue(ctx, steerKey{}, inbox)
}

func steeringFrom(ctx context.Context) *SteerInbox {
	s, _ := ctx.Value(steerKey{}).(*SteerInbox)
	return s
}

// appendSteered appends the inbox's messages as user messages; it reports
// whether any arrived.
func appendSteered(ctx context.Context, msgs []chat.Message) ([]chat.Message, bool) {
	inbox := steeringFrom(ctx)
	if inbox == nil {
		return msgs, false
	}
	texts := inbox.Drain()
	for _, t := range texts {
		msgs = append(msgs, chat.Message{Role: "user", Content: steerPrefix + t})
	}
	return msgs, len(texts) > 0
}
