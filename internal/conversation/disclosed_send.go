package conversation

import (
	"context"
	"strings"

	"vornik.io/vornik/internal/aidisclosure"
)

// Sender is the slice of Channel the disclosure path needs: a name (which
// selects the Art 50 cadence) and Send.
type Sender interface {
	Name() string
	Send(ctx context.Context, msg ChannelMessage) (string, error)
}

// DisclosureScopeFor returns the identity the Art 50 notice is remembered
// against for msg on ch: the channel's DisclosureScoper scope when it declares
// one, else the SessionID. Shared by ChannelReceiver and SendDisclosed so a chat
// disclosed in a turn is not re-disclosed by a later push, and vice versa.
func DisclosureScopeFor(ch any, msg ChannelMessage) string {
	if scoper, ok := ch.(DisclosureScoper); ok {
		if declared := strings.TrimSpace(scoper.DisclosureScope(msg)); declared != "" {
			return declared
		}
	}
	return msg.SessionID
}

// SendDisclosed sends msg on ch with the EU AI Act Art 50 disclosure applied
// at the channel's cadence — the path every outbound that does not pass through
// ChannelReceiver must take (design 2026-07-28 §11.4, GitHub #342).
//
// Per-message channels (email, github-app) get the notice as a footer in the
// body; authored selects the publication wording. Per-session channels
// (telegram, slack, web-chat) get the conversational notice as its own message
// first, once per scope, sharing the receiver's store. If the notice cannot be
// delivered, msg is NOT sent and the error is returned (fail closed). A nil d
// discloses with the compiled defaults.
func SendDisclosed(ctx context.Context, d *aidisclosure.Service, ch Sender, msg ChannelMessage, authored bool) (string, error) {
	channel := ch.Name()
	// When a per-session notice is owed, it leads the SAME message, set off by
	// the rule, rather than going as a separate post: two posts back-to-back
	// trip per-channel rate limits (Slack's local bucket is 1/s, burst 1) and
	// the content would be refused after the notice went out. The receiver
	// never hit this because an LLM turn separates its two sends (design
	// §11.4.2, amended 2026-10-12). The store is recorded only after this one
	// send succeeds, so a failed post leaves the person still owed the notice.
	var (
		sentID  string
		sentAll bool
	)
	body, err := d.Disclose(ctx, aidisclosure.Outbound{
		Channel:  channel,
		Scope:    DisclosureScopeFor(ch, msg),
		Body:     msg.Text,
		Authored: authored,
		SendNotice: func(ctx context.Context, text string) error {
			withNotice := msg
			withNotice.Text = text
			if strings.TrimSpace(msg.Text) != "" {
				withNotice.Text = text + aidisclosure.FooterSeparator + msg.Text
			}
			id, err := ch.Send(ctx, withNotice)
			if err != nil {
				return err
			}
			sentID, sentAll = id, true
			return nil
		},
	})
	if err != nil {
		return "", err
	}
	if sentAll {
		return sentID, nil
	}
	msg.Text = body
	return ch.Send(ctx, msg)
}
