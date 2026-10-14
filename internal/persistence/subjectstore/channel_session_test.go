package subjectstore

import (
	"context"
	"slices"
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// audit 2026-10-12: channel_sessions.history was neither exported (Art 15/20)
// nor erased (Art 17) — the collector read only active_project. Design
// 2026-07-29-art17-redact-and-reembed §15.10, GitHub #344.
func TestCollectorSpec_ChannelSessionsReadsHistory(t *testing.T) {
	spec := collectorSpecs[datasubject.TableChannelSessions]
	if !slices.Contains(spec.contentCols, "history") {
		t.Fatalf("the export must include the conversation history, got columns %v", spec.contentCols)
	}
}

// audit 2026-10-12: DeleteRow ran `DELETE FROM channel_sessions WHERE session_id
// = $1`, removing every kind's row with that id, and the running daemon re-saves
// a deleted session from memory on its next message. It must refuse — before
// any database access, so this needs no database.
func TestDeleteRow_RefusesChannelSessions(t *testing.T) {
	r := &Repository{}
	err := r.DeleteRow(context.Background(), datasubject.TableChannelSessions, "slack/C1.123")
	if err == nil || !strings.Contains(err.Error(), "live conversation state") {
		t.Fatalf("a channel session must not be deletable as a plain row, got %v", err)
	}
}
