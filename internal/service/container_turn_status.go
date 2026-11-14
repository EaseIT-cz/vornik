package service

import (
	"context"
	"sync/atomic"

	"vornik.io/vornik/internal/chatorigin"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/webchat"
)

// turnStatusReporter routes the dispatcher's in-flight progress to the originating
// channel, when that channel can display it.
//
// OPERATOR REQUEST 2026-07-30: "on slack there is a mechanism to display statuses — for
// thinking, tool calling, etc — it can be rewritten and doesn't have to use any of the
// context." That mechanism is assistant.threads.setStatus, and it is deliberately NOT
// what this uses: it needs `assistant:write`, a manifest change and a reinstall, and it
// converts DMs into assistant threads. The Slack channel instead rewrites the progress
// placeholder it already owns, which needs no new scope.
//
// The dispatcher knows nothing about channels, so the resolution lives here: name →
// conversation.Channel → optional TurnStatusChannel. A channel that cannot display a
// status is simply skipped. Slack rewrites its placeholder; Telegram, since #642,
// shows the line above its streamed reply.
type turnStatusReporter struct {
	resolver chatorigin.ChannelResolver
	// webChat is the UI's sink for web chat turns (web-chat-design.md §2.3).
	// Web chat builds a channel per turn and registers none, so its lines
	// go here, never through the resolver, whose other callers (reminder
	// and completion delivery) keep resolving web chat to nothing. Set once
	// the UI server exists; until then a web chat line is dropped.
	webChat atomic.Pointer[webChatSinkBox]
}

// webChatTurnStatusSink is what *ui.Server implements for web chat turns.
type webChatTurnStatusSink interface {
	SetWebChatTurnStatus(historyKey, line string)
}

type webChatSinkBox struct{ sink webChatTurnStatusSink }

// setWebChatSink binds the UI's sink; the reporter is built before the UI.
func (r *turnStatusReporter) setWebChatSink(s webChatTurnStatusSink) {
	r.webChat.Store(&webChatSinkBox{sink: s})
}

// ReportTurnStatus implements dispatcher.TurnStatusReporter.
//
// Best-effort and non-blocking by contract: this is called from inside the tool loop, so
// a slow or failing status update must never add latency to the very turn it is
// reporting on.
func (r *turnStatusReporter) ReportTurnStatus(ctx context.Context, channelName, sessionID, status string) {
	if r == nil || channelName == "" || sessionID == "" {
		return
	}
	if channelName == webchat.ChannelName {
		// The web chat's session id is its history key (web-chat-design.md §2.3).
		if b := r.webChat.Load(); b != nil {
			b.sink.SetWebChatTurnStatus(sessionID, status)
		}
		return
	}
	if r.resolver == nil {
		return
	}
	ch := r.resolver.ResolveChannel(channelName)
	if ch == nil {
		return
	}
	display, ok := ch.(conversation.TurnStatusChannel)
	if !ok {
		// Email, GitHub: no transient-status surface. Not an error.
		return
	}
	display.SetTurnStatus(ctx, sessionID, status)
}

// Compile-time guard against signature drift between the two packages.
var _ dispatcher.TurnStatusReporter = (*turnStatusReporter)(nil)
