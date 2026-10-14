package datasubject

import (
	"fmt"
	"strings"
)

// channel_sessions row identity and its Art 15(4) default.
//
// see LLD § https://docs.vornik.io §15.10

// channelSessionKinds are the session kinds the daemon writes today, one per
// call site of channelSessionPersister in internal/service. Closed so that a
// link to a kind nothing writes is refused when it is made, rather than
// resolving to nothing at export time. A new channel kind is added here.
var channelSessionKinds = map[string]bool{
	"slack": true, "email": true, "github": true, "webchat": true, "telegram": true,
}

// ParseChannelSessionRowID splits a channel_sessions link row id of the form
// "<kind>/<session_id>". The primary key is (kind, session_id), so a session id
// alone is ambiguous. The split is on the FIRST slash: kinds contain none and
// GitHub session ids ("owner/repo#issues/N") do.
func ParseChannelSessionRowID(rowID string) (kind, sessionID string, err error) {
	k, id, ok := strings.Cut(rowID, "/")
	if !ok || !channelSessionKinds[k] || strings.TrimSpace(id) == "" {
		return "", "", fmt.Errorf("datasubject: channel_sessions row id %q must be <kind>/<session_id> "+
			"with kind one of slack, email, github, webchat, telegram", rowID)
	}
	return k, id, nil
}

// ChannelSessionSingleSpeaker reports whether a session of this kind belongs to
// one person. A session's history carries no per-entry speaker, and Slack
// threads, email threads, GitHub issues and Telegram groups are multi-party by
// construction, so only a web chat session (one browser, one person) may be
// exported verbatim; every other kind is treated as shared under Art 15(4).
func ChannelSessionSingleSpeaker(kind string) bool { return kind == "webchat" }
