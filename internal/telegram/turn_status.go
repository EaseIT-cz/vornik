package telegram

import (
	"context"
	"strings"
	"sync"

	"vornik.io/vornik/internal/conversation"
)

// SetTurnStatus implements conversation.TurnStatusChannel (conversation-channel
// design, update 2026-11-05, GitHub #642): the turn's one progress line is
// shown above the chat's open streamed reply and removed when the reply
// closes. With no open stream (voice mode, the one-shot path) it does nothing,
// as the interface allows. Display only: it never enters history.
func (c *Channel) SetTurnStatus(_ context.Context, sessionID, status string) {
	status = strings.TrimSpace(status)
	if status == "" {
		return
	}
	chatID, err := parseTelegramSessionID(sessionID)
	if err != nil {
		return
	}
	if s := c.bot.openStreams.get(chatID); s != nil {
		s.setStatus(status)
	}
}

var _ conversation.TurnStatusChannel = (*Channel)(nil)

// openStreamRegistry maps a chat to its open streamed reply. It lives on the
// Bot because the turn-status reporter may resolve a different Channel value
// for the same bot. Nothing serializes turns per chat, so a newer stream
// replaces an older one; Close removes the entry only if it is still its own.
type openStreamRegistry struct {
	mu sync.Mutex
	m  map[int64]*telegramStream
}

func (r *openStreamRegistry) put(chatID int64, s *telegramStream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[int64]*telegramStream{}
	}
	r.m[chatID] = s
}

func (r *openStreamRegistry) get(chatID int64) *telegramStream {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.m[chatID]
}

func (r *openStreamRegistry) remove(chatID int64, s *telegramStream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m[chatID] == s {
		delete(r.m, chatID)
	}
}
