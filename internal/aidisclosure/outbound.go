package aidisclosure

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// FooterSeparator is the one spelling of the per-message rule (design §3): a
// plain-text horizontal rule on its own line, so the notice reads as a distinct
// statement and survives being quoted or read without markup.
const FooterSeparator = "\n\n---\n"

// WithFooter appends n to body under FooterSeparator. An empty (or
// whitespace-only) body yields the notice alone: a file mail with no caption
// still carries AI-authored content — the file — and must still disclose.
func WithFooter(body string, n Notice) string {
	if strings.TrimSpace(body) == "" {
		return n.Text
	}
	return body + FooterSeparator + n.Text
}

// Outbound describes one message leaving the system on a channel, for Disclose.
//
// Design §11.4.2 (GitHub #342): every outbound that does not pass through
// ChannelReceiver — notifications, composed mail, file sends — routes through
// Disclose, so the cadence decision and the footer exist once.
type Outbound struct {
	// Channel is the conversation.Channel Name(); it selects the cadence.
	Channel string
	// Scope is the per-session identity the notice is remembered against (the
	// session id, or the DisclosureScoper scope). Empty on a per-session channel
	// means "serve, do not record".
	Scope string
	// Body is the content to be sent.
	Body string
	// Authored selects PublicationNotice for the per-message footer: the push
	// ORIGINATES content rather than replying in a conversation (§11.4.3).
	Authored bool
	// SendNotice delivers the notice as its own message on a per-session
	// channel. Required there; a nil SendNotice fails closed.
	SendNotice func(ctx context.Context, text string) error
	// Served and Failed are optional metrics hooks (design §4.1). Failed
	// receives "read" | "send" | "write" | "unrecorded".
	Served func(c Cadence)
	Failed func(stage string)
}

// ErrNoNoticeSender is returned when a per-session outbound owes the notice but
// gave no way to deliver it. Failing closed is the §4 row-1 direction.
var ErrNoNoticeSender = errors.New("aidisclosure: per-session outbound owes the AI notice but has no notice sender")

// defaultService is what a nil *Service resolves to: compiled defaults, no
// store. An unwired call site therefore over-discloses rather than going
// silent — the §5 "no off switch" rule applied to wiring mistakes.
var defaultService = New(Config{}, nil)

func (s *Service) orDefault() *Service {
	if s == nil {
		return defaultService
	}
	return s
}

// Disclose applies the channel's cadence to an outbound and returns the body to
// send.
//
//   - per-message: the body with the notice footer (publication notice when
//     Authored). Never touches the store.
//   - per-session: if the notice is owed for Scope, SendNotice delivers it first
//     as its own message. A SendNotice error is returned and the caller MUST NOT
//     send the body (fail closed). A store read error serves anyway; a store
//     write error is reported through Failed and ignored. Body is returned
//     unchanged.
func (s *Service) Disclose(ctx context.Context, o Outbound) (string, error) {
	s = s.orDefault()
	served := func(c Cadence) {
		if o.Served != nil {
			o.Served(c)
		}
	}
	failed := func(stage string) {
		if o.Failed != nil {
			o.Failed(stage)
		}
	}

	if CadenceFor(o.Channel) == CadencePerMessage {
		n := s.notice
		if o.Authored {
			n = s.publication
		}
		served(CadencePerMessage)
		return WithFooter(o.Body, n), nil
	}

	notice, serve := s.notice, true
	if strings.TrimSpace(o.Scope) != "" {
		var err error
		notice, serve, err = s.Ensure(ctx, o.Channel, o.Scope)
		if err != nil {
			failed("read") // serve is true on a read error
		}
	}
	if !serve {
		return o.Body, nil
	}
	if o.SendNotice == nil {
		failed("send")
		return "", ErrNoNoticeSender
	}
	if err := o.SendNotice(ctx, notice.Text); err != nil {
		failed("send")
		return "", fmt.Errorf("aidisclosure: AI Act Art 50 notice could not be delivered on %s; "+
			"refusing to send undisclosed content: %w", o.Channel, err)
	}
	served(CadencePerSession)
	if strings.TrimSpace(o.Scope) == "" {
		// Served, but with no identity there is no Art 99 row. Observable, so a
		// site that lands here regularly reads as an evidence hole (§11.4.2).
		failed("unrecorded")
		return o.Body, nil
	}
	if err := s.Record(ctx, o.Channel, o.Scope, notice); err != nil {
		failed("write")
	}
	return o.Body, nil
}
