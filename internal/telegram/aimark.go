package telegram

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/aimark"
)

// aiMarkHolder holds the EU AI Act Art 50(2) marker (GitHub #117; Art 50
// design §14.8 B, rows 11, 13).
type aiMarkHolder struct {
	mu     sync.Mutex
	marker *aimark.Marker
}

// SetAIMarker sets the marker; nil records nothing.
func (b *Bot) SetAIMarker(m *aimark.Marker) {
	b.aiMark.mu.Lock()
	defer b.aiMark.mu.Unlock()
	b.aiMark.marker = m
}

// recordAIText writes the ledger row for text Telegram accepted. A Bot API
// text message has no field to carry a mark, so the ledger row (found by
// the detection endpoint from the text's hash) is the mark. Called after a
// successful send, with the text as sent. A failure is logged and counted by
// the marker; the message is already delivered.
func (b *Bot) recordAIText(ctx context.Context, text string) {
	b.aiMark.mu.Lock()
	m := b.aiMark.marker
	b.aiMark.mu.Unlock()
	if m == nil || text == "" {
		return
	}
	_, _, _ = m.New(ctx, []byte(text), "telegram", "")
}

// recordAIAudio writes the ledger row for a voice reply Telegram accepted
// (row 12, opt-in): the clip's bytes as sent. As with text, the ledger row is
// the mark.
func (b *Bot) recordAIAudio(ctx context.Context, audio []byte) {
	b.aiMark.mu.Lock()
	m := b.aiMark.marker
	b.aiMark.mu.Unlock()
	if m == nil || len(audio) == 0 {
		return
	}
	_, _, _ = m.New(ctx, audio, "telegram_voice", "")
}

// editText is the text an edit actually sends: Telegram's 4096-character
// limit, cut as editMessageText cuts it.
func editText(text string) string {
	if len(text) > 4096 {
		return text[:4093] + "..."
	}
	return text
}
