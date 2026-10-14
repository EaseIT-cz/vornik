package telegram

import (
	"context"
	"strconv"

	"vornik.io/vornik/internal/aidisclosure"
)

// SetAIDisclosure wires the daemon's Art 50 disclosure service, so the
// per-chat "already disclosed" state is the one ChannelReceiver uses. Nil (the
// default) still discloses, with the compiled defaults and no memory.
func (b *Bot) SetAIDisclosure(d *aidisclosure.Service) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.disclosure = d
}

// WithAIDisclosure is SetAIDisclosure as a constructor option.
func WithAIDisclosure(d *aidisclosure.Service) BotOption {
	return func(b *Bot) { b.disclosure = d }
}

func (b *Bot) aiDisclosure() *aidisclosure.Service {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.disclosure
}

// discloseChat makes sure chatID has been told it is dealing with an AI system
// before the bot pushes AI-authored content into it outside a ChannelReceiver
// turn — a completion notice, an artifact, a summary (Art 50 design 2026-07-28
// §11.4, GitHub #342).
//
// Telegram is per-session: the notice is its own message, once per chat, scoped
// by the chat id exactly as ChannelReceiver scopes it (channel.go uses the chat
// id as SessionID), so a chat disclosed in a turn is not told again here. A
// non-nil error means the notice could not be delivered; the caller MUST NOT
// send the content (fail closed, design §4).
func (b *Bot) discloseChat(ctx context.Context, chatID int64) error {
	_, err := b.aiDisclosure().Disclose(ctx, aidisclosure.Outbound{
		Channel: "telegram",
		Scope:   strconv.FormatInt(chatID, 10),
		SendNotice: func(ctx context.Context, text string) error {
			return b.sendMessage(ctx, chatID, text)
		},
	})
	return err
}

// discloseForumThread is discloseChat for the forum supergroup: the notice is
// remembered against the forum chat, and delivered into the thread the content
// is about to land in, because that is where the person is reading.
func (b *Bot) discloseForumThread(ctx context.Context, threadID int64) error {
	_, err := b.aiDisclosure().Disclose(ctx, aidisclosure.Outbound{
		Channel: "telegram",
		Scope:   strconv.FormatInt(b.forumChatID, 10),
		SendNotice: func(ctx context.Context, text string) error {
			_, err := b.sendForumMessage(ctx, threadID, text)
			return err
		},
	})
	return err
}
