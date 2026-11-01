package datasubject

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Slice 5d-2: erasing a subject from a channel session (GitHub #344).
//
// A channel session's history is also cached in the memory of every daemon
// node that served it. The erasure runs here, in vornikctl, against the stored
// row, guarded on the row's fence version; every fenced node checks that
// version before serving its cached copy and saves only over the version it
// saw, so the deleted or masked session is neither served nor written back.
// Delete for an exclusive or (1)(d)/(e) link; for a shared one the subject's
// own messages are tombstoned where the history attributes them and every
// identifier is masked in the rest, as 5d-1 does for a transcript row.
//
// see LLD § https://docs.vornik.io §15.18

// ChannelSession is one stored session as loaded, with the fence version
// every write is guarded on.
type ChannelSession struct {
	Kind      string
	SessionID string
	History   []byte
	Version   int64
}

// ChannelSessionStore is the store side of 5d-2 (Postgres).
type ChannelSessionStore interface {
	// LoadSession resolves a link row id (<kind>/<session_id>, or a legacy
	// unambiguous bare id) and loads the row; found is false when no row
	// exists.
	LoadSession(ctx context.Context, rowID string) (s ChannelSession, found bool, err error)
	// DeleteSession deletes the row only while it holds version.
	DeleteSession(ctx context.Context, s ChannelSession) (applied bool, err error)
	// WriteSession replaces the history only while the row holds s.Version.
	WriteSession(ctx context.Context, s ChannelSession, history []byte) (applied bool, err error)
	// FenceLaggards names what could write a session back after the erasure:
	// every registered node that does not advertise the fence, or the schema
	// itself when the fence column is missing.
	FenceLaggards(ctx context.Context) ([]string, error)
}

// Kinds of channel-session redaction (RedactedAction.Kind).
const (
	// SessionTombstoned: the subject's own messages were identified and
	// tombstoned, and their identifiers masked in the rest.
	SessionTombstoned = "session_tombstoned"
	// SessionMasked is no longer produced (§15.19); kept so stored reports
	// still read.
	SessionMasked = "session_masked"
	// SessionUnattributed (DeletedRecord.Kind): a shared session whose history
	// records no speaker, deleted in full because the subject's own messages
	// could not be separated (#346 Q5).
	SessionUnattributed = "session_unattributed"
)

// sessionWriteAttempts bounds the retries on a session that keeps moving
// (§15.4: an active conversation is deferred after 3).
const sessionWriteAttempts = 3

const (
	reasonSessionActive = "this conversation was being written to throughout the erasure (it is " +
		"active), so it was left untouched rather than overwritten; re-run the erasure"
	reasonSessionMasking = "the conversation could not be masked safely (an entry is not valid JSON, " +
		"two of its keys would mask to the same key, or an identifier survives in a form literal " +
		"masking does not reach), so nothing was written; this record has NOT been changed"
)

// sessionErasable reports whether 5d-2 can act on a channel session here.
func (e *Executor) sessionErasable() bool { return e != nil && e.Sessions != nil }

// applySessionErase performs one channel_sessions action and records it.
func (e *Executor) applySessionErase(ctx context.Context, a Action, ids []string, idErr error, res *ErasureResult) {
	laggards, err := e.Sessions.FenceLaggards(ctx)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("check the daemon nodes for the session fence: %v", err)})
		return
	}
	if len(laggards) > 0 {
		e.deferRedaction(a, res, "a daemon node that predates the session fence is running ("+
			strings.Join(laggards, ", ")+"); it could write the session back, so this record has NOT "+
			"been changed; re-run once every node is upgraded")
		return
	}
	if a.Disposition == DispositionRedact {
		if e.Redact == nil {
			e.deferRedaction(a, res, "this executor has no redaction capability wired (the review "+
				"gate), so the shared conversation was not masked; it has NOT been changed")
			return
		}
		if idErr != nil {
			e.deferRedaction(a, res, fmt.Sprintf("the subject's identifiers could not be read (%v), so no "+
				"masking could be verified; this row has NOT been changed", idErr))
			return
		}
		if ids = liveIdentifiers(ids); len(ids) == 0 {
			e.deferRedaction(a, res, "the subject has no usable identifiers recorded, so no masking "+
				"could be verified; this row has NOT been changed")
			return
		}
	}
	if chatID, ok := telegramChatID(ctx, e.Sessions, a.RowID); ok {
		defer e.eraseTelegramSaves(a, chatID, res)
	}
	for attempt := 0; attempt < sessionWriteAttempts; attempt++ {
		s, found, err := e.Sessions.LoadSession(ctx, a.RowID)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("load channel session: %v", err)})
			return
		}
		if !found {
			// Nothing stored: there is nothing left to erase.
			res.Deleted = append(res.Deleted, DeletedRecord{Table: a.Table, RowID: a.RowID})
			return
		}
		done, retry := e.eraseSessionOnce(ctx, a, s, ids, res)
		if done || !retry {
			return
		}
	}
	e.deferRedaction(a, res, reasonSessionActive)
}

// eraseSessionOnce makes one guarded attempt. done reports a recorded
// outcome; retry reports that the row moved and may be re-loaded.
func (e *Executor) eraseSessionOnce(ctx context.Context, a Action, s ChannelSession, ids []string, res *ErasureResult) (done, retry bool) {
	if a.Disposition != DispositionRedact || !ChannelSessionAttributed(s.Kind) {
		applied, err := e.Sessions.DeleteSession(ctx, s)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("delete channel session: %v", err)})
			return true, false
		}
		if applied {
			res.RowsDeleted++
			d := DeletedRecord{Table: a.Table, RowID: a.RowID}
			if a.Disposition == DispositionRedact {
				d.Kind = SessionUnattributed
			}
			res.Deleted = append(res.Deleted, d)
		}
		return applied, !applied
	}
	masked, own, err := maskSessionHistory(s.Kind, s.History, ids, tombstone(e.now()))
	if err == nil {
		err = verifyJSONStrings(string(masked), ids)
	}
	if err != nil {
		e.deferRedaction(a, res, reasonSessionMasking)
		return true, false
	}
	kind := TranscriptMasked
	if own > 0 || ChannelSessionSingleSpeaker(s.Kind) {
		kind = SessionTombstoned
	}
	before, after := string(s.History), string(masked)
	changed := before != after
	if changed && !e.reviewApproved(a, before, after, ids, TranscriptMaskModel, res) {
		return true, false
	}
	if changed {
		applied, err := e.Sessions.WriteSession(ctx, s, masked)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("write channel session: %v", err)})
			return true, false
		}
		if !applied {
			return false, true
		}
	}
	res.Redacted = append(res.Redacted, RedactedAction{
		Table: a.Table, RowID: a.RowID, BeforeHash: hashFields(map[string]*string{"history": &before}),
		AfterHash: hashFields(map[string]*string{"history": &after}), Model: TranscriptMaskModel, Kind: kind,
		Verified: true, ReviewBypassed: e.Redact != nil && e.Redact.ApplyWithoutReview && changed,
	})
	return true, false
}

// maskSessionHistory builds the masked history: the subject's own user
// entries (by an exact From: match, or every user entry of a single-speaker
// kind) have their content replaced by the tombstone; every other string,
// in every entry, is masked. own counts the tombstoned entries.
func maskSessionHistory(kind string, history []byte, ids []string, mark string) ([]byte, int, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(history, &entries); err != nil {
		return nil, 0, fmt.Errorf("history is not a JSON array: %w", err)
	}
	own := 0
	out := make([]any, len(entries))
	for i, raw := range entries {
		entry, err := decodeJSON(raw)
		if err != nil {
			return nil, 0, err
		}
		obj, isObj := entry.(map[string]any)
		if isObj && obj["role"] == "user" && (ChannelSessionSingleSpeaker(kind) || fromMatches(obj["content"], ids)) {
			obj["content"] = mark
			own++
		}
		masked, err := walkJSONKeysChecked(entry, func(s string) string {
			if s == mark {
				return s
			}
			return maskSessionString(s, ids)
		})
		if err != nil {
			return nil, 0, err
		}
		out[i] = masked
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, 0, err
	}
	return b, own, nil
}

// maskSessionString masks one string. A tool call's arguments are JSON
// inside a string: masked as JSON where they parse, as text where not.
func maskSessionString(s string, ids []string) string {
	if t := strings.TrimSpace(s); strings.HasPrefix(t, "{") || strings.HasPrefix(t, "[") {
		if m, err := maskJSON(s, ids); err == nil {
			return m
		}
	}
	return maskText(s, ids)
}

func decodeJSON(raw []byte) (any, error) {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// fromMatches is O-5d-7 for a channel entry: the content's first line is
// "From: <v>" (enrichUserContent's envelope) and v, or the address inside
// "<...>" of v, equals one of the identifiers exactly.
func fromMatches(content any, ids []string) bool {
	text := firstText(content)
	line, _, _ := strings.Cut(text, "\n")
	v, ok := strings.CutPrefix(line, "From: ")
	if !ok {
		return false
	}
	v = strings.TrimSpace(v)
	addr := v
	if i, j := strings.LastIndex(v, "<"), strings.LastIndex(v, ">"); i >= 0 && j > i {
		addr = strings.TrimSpace(v[i+1 : j])
	}
	return isOwnTurn(v, ids) || isOwnTurn(addr, ids)
}

// firstText is a message content's text: the string itself, or the first
// text block of a multimodal content array.
func firstText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		for _, b := range c {
			if m, ok := b.(map[string]any); ok {
				if t, ok := m["text"].(string); ok {
					return t
				}
			}
		}
	}
	return ""
}
