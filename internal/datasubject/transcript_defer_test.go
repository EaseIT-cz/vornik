package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Slice 5d-0 and the channel_sessions interim
// (https://docs.vornik.io §15.10).
//
// audit 2026-10-12: with a 5c redactor wired, a shared transcript row
// (chat_audit_log / task_messages) was handed to ChunkRedactor.LoadChunk, which
// reads only project_memory_chunks, so the row ended FAILED ("memory chunk …
// not found") instead of DEFERRED, and Execute returned ErrPartialErasure
// (GitHub #344, 5d as-built §15.1).

// countingRedactor records every store call, so a test can assert that no store
// was touched for a record type the redactor cannot handle.
type countingRedactor struct {
	loads   []string
	redacts []string
}

func (c *countingRedactor) LoadChunk(_ context.Context, id string) (string, string, error) {
	c.loads = append(c.loads, id)
	return "", "", errNotAChunk
}

func (c *countingRedactor) RedactChunk(_ context.Context, id, _, _ string) (RedactionResult, error) {
	c.redacts = append(c.redacts, id)
	return RedactionResult{Outcome: RedactionApplied}, nil
}

// errNotAChunk is what the real store says for a non-chunk id.
var errNotAChunk = errors.New("memory chunk not found")

func wiredExecutor(r ChunkRedactor, w *fakeRewriter, del RowDeleter) *Executor {
	return &Executor{
		Rows: del, Artifacts: noArtifacts{},
		Redact: &RedactDeps{
			Redactor: r, Rewriter: w, Identifiers: fakeIDs{ids: []string{"jane@example.com"}},
			Approve: approveAll,
		},
	}
}

func TestExecute_SharedTranscriptWithRedactorWiredIsDeferredNotFailed(t *testing.T) {
	r := &countingRedactor{}
	w := &fakeRewriter{out: "rewritten"}
	e := wiredExecutor(r, w, &recordingDeleter{})
	plan := &ErasurePlan{SubjectID: "subj-1", RequestID: "req-1", Actions: []Action{
		{Table: TableTaskMessages, RowID: "tm-1", Disposition: DispositionRedact, Exclusivity: SharedRow},
		{Table: TableChatAuditLog, RowID: "cal-1", Disposition: DispositionRedact, Exclusivity: SharedRow},
	}}

	res, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("a record type with no redaction path is a missing capability, not a fault; got %v", err)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("shared transcripts must not end FAILED, got %+v", res.Failed)
	}
	if len(res.Deferred) != 2 {
		t.Fatalf("both shared transcripts must be DEFERRED, got %+v", res.Deferred)
	}
	for _, d := range res.Deferred {
		if !strings.Contains(d.Reason, "no redaction path for this record type") ||
			!strings.Contains(d.Reason, "NOT been changed") {
			t.Errorf("%s/%s: reason must name the missing path and say nothing changed, got %q",
				d.Table, d.RowID, d.Reason)
		}
	}
	if len(r.loads) != 0 || len(r.redacts) != 0 || len(w.seen) != 0 {
		t.Errorf("no store or model may be touched for a transcript row: loads=%v redacts=%v rewrites=%d",
			r.loads, r.redacts, len(w.seen))
	}
	if res.Complete() {
		t.Error("a deferred row must keep the request un-actioned")
	}
	// Visible in the request report.
	n := res.Narrative(plan)
	deferred := 0
	for _, entry := range n.Entries {
		if entry.Outcome == NarrativeDeferred && strings.Contains(entry.Detail, "no redaction path") {
			deferred++
		}
	}
	if deferred != 2 {
		t.Errorf("the report must list both rows as deferred with their reason, got %+v", n.Entries)
	}
}

// The deferral happens even with no redactor wired, with the more specific
// reason: the record type has no path at all, wired or not.
func TestExecute_SharedTranscriptWithoutRedactorNamesTheRecordType(t *testing.T) {
	e := &Executor{Rows: &recordingDeleter{}, Artifacts: noArtifacts{}}
	plan := &ErasurePlan{Actions: []Action{
		{Table: TableTaskMessages, RowID: "tm-1", Disposition: DispositionRedact},
	}}
	res, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "no redaction path for this record type") {
		t.Fatalf("got %+v", res.Deferred)
	}
}

// Memory chunks still take the 5c path: the guard must not swallow them.
func TestExecute_ChunkRedactionStillReachesTheRedactor(t *testing.T) {
	r := newFakeRedactor()
	r.content["c1"] = "Called jane@example.com today."
	r.hash["c1"] = "h1"
	w := &fakeRewriter{out: "Called the client today."}
	e, _ := redactExecutor(r, w, []string{"jane@example.com"}, approveAll)
	res, err := e.Execute(context.Background(), redactPlan("c1"))
	if err != nil || len(res.Redacted) != 1 {
		t.Fatalf("chunk redaction must still apply: err=%v res=%+v", err, res)
	}
}

// audit 2026-10-12: channel_sessions.history is live conversation state. The
// running daemon keeps each session in memory and re-saves it on the next
// message, so a stored-row delete from `vornikctl subject erase` was undone while
// the report said "deleted". Every channel_sessions action — DELETE included —
// is deferred until a daemon-side erase exists (§15.10 item 5, O-5d-5).
func TestExecute_ChannelSessionErasureIsAlwaysDeferred(t *testing.T) {
	r := &countingRedactor{}
	del := &recordingDeleter{}
	e := wiredExecutor(r, &fakeRewriter{out: "x"}, del)
	plan := &ErasurePlan{Actions: []Action{
		{Table: TableChannelSessions, RowID: "slack/C1.123", Disposition: DispositionDelete, Exclusivity: ExclusiveRow},
		{Table: TableChannelSessions, RowID: "webchat/abc", Disposition: DispositionRedact, Exclusivity: SharedRow},
		// webchat is exported verbatim, yet its erasure is still deferred: the
		// export/erasure asymmetry is deliberate (§15.10 item 5).
		{Table: TableChannelSessions, RowID: "webchat/solo", Disposition: DispositionDelete, Exclusivity: ExclusiveRow},
	}}
	res, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(del.deleted) != 0 || len(r.loads) != 0 {
		t.Fatalf("no store may be touched for a channel session: deleted=%v loads=%v", del.deleted, r.loads)
	}
	if res.RowsDeleted != 0 || len(res.Deleted) != 0 {
		t.Errorf("a session must never be reported deleted, got %+v", res.Deleted)
	}
	if len(res.Deferred) != 3 {
		t.Fatalf("every session action must be deferred, got %+v", res.Deferred)
	}
	for _, d := range res.Deferred {
		// The session check precedes the generic 5d-0 check (§15.10.2 "Order").
		if d.Reason != reasonLiveSession {
			t.Errorf("%s: reason must be the live-state one, got %q", d.RowID, d.Reason)
		}
	}
	if res.Complete() {
		t.Error("a deferred session must keep the request open (Art 12(3) clock keeps running)")
	}
}
