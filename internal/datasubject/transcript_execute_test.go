package datasubject

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Slice 5d-1b (GitHub #344; design 2026-07-29-art17-redact-and-reembed
// §15.14, "Engine (unit)"). Found at: review (the 5d design). The store is
// faked here; its guarded write is pinned on both drivers by
// repotest.RunTranscriptRedactSuite.

type fakeTranscripts struct {
	turns    map[string]TranscriptTurn
	authors  map[string][]ThreadAuthor
	payloads map[string][]TaskPayload
	writes   map[string]TurnWrite
	paid     map[string]string
	outcome  TurnResult
	payOut   map[string]TurnResult
}

func newFakeTranscripts() *fakeTranscripts {
	return &fakeTranscripts{turns: map[string]TranscriptTurn{}, authors: map[string][]ThreadAuthor{},
		payloads: map[string][]TaskPayload{}, writes: map[string]TurnWrite{}, paid: map[string]string{},
		outcome: TurnResult{Outcome: TranscriptApplied}, payOut: map[string]TurnResult{}}
}

func (f *fakeTranscripts) LoadTurn(_ context.Context, _ LinkableTable, id string) (TranscriptTurn, error) {
	t, ok := f.turns[id]
	if !ok {
		return TranscriptTurn{}, errors.New("no such row")
	}
	return t, nil
}

func (f *fakeTranscripts) ThreadAuthors(_ context.Context, _ LinkableTable, key string) ([]ThreadAuthor, error) {
	return f.authors[key], nil
}

func (f *fakeTranscripts) ApplyTurn(_ context.Context, prior TranscriptTurn, w TurnWrite) (TurnResult, error) {
	if f.outcome.Outcome == TranscriptApplied {
		f.writes[prior.RowID] = w
	}
	return f.outcome, nil
}

func (f *fakeTranscripts) OriginPayloads(_ context.Context, id string) ([]TaskPayload, error) {
	return f.payloads[id], nil
}

func (f *fakeTranscripts) ApplyPayload(_ context.Context, p TaskPayload, payload string) (TurnResult, error) {
	if out, ok := f.payOut[p.TaskID]; ok {
		return out, nil
	}
	f.paid[p.TaskID] = payload
	return TurnResult{Outcome: TranscriptApplied}, nil
}

type fixedIDs []string

func (f fixedIDs) Identifiers(context.Context, string) ([]string, error) { return f, nil }

var anna = fixedIDs{"telegram:4242", "Anna Novak", "anna.novak@mail.test"}

func str(s string) *string { return &s }

func transcriptExecutor(store *fakeTranscripts, ids IdentifierSource) *Executor {
	return &Executor{
		Rows: &recordingDeleter{}, Artifacts: noArtifacts{},
		Redact: &RedactDeps{Transcripts: store, Identifiers: ids, ApplyWithoutReview: true,
			Now: func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) }},
	}
}

func transcriptPlan(table LinkableTable, rows ...string) *ErasurePlan {
	p := &ErasurePlan{SubjectID: "s1", RequestID: "r1"}
	for _, r := range rows {
		p.Actions = append(p.Actions, Action{Table: table, RowID: r, Disposition: DispositionRedact})
	}
	return p
}

// message is a task message of task-1.
func message(id, author, content string, metadata *string) TranscriptTurn {
	const task = "task-1"
	return TranscriptTurn{Table: TableTaskMessages, RowID: id, Author: author, ThreadKey: task, TaskID: task,
		Fields: map[string]*string{"content": str(content), "metadata": metadata}}
}

func chatTurn(id, chat, user, msg, resp string) TranscriptTurn {
	return TranscriptTurn{Table: TableChatAuditLog, RowID: id, Author: user, ChatID: chat, ThreadKey: chat,
		Fields: map[string]*string{"user_message": str(msg), "response": str(resp),
			"tool_calls_json": str("[]"), "hallucination_signals_json": str("")}}
}

func run(t *testing.T, e *Executor, plan *ErasurePlan) *ErasureResult {
	t.Helper()
	res, err := e.Execute(context.Background(), plan)
	if err != nil {
		t.Fatalf("execute: %v (%+v)", err, res)
	}
	return res
}

func TestTranscript_OwnTurnIsTombstonedUnderAPseudonym(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m1"] = message("m1", "telegram:4242", "my number is 777 123 456", nil)
	f.authors["task-1"] = []ThreadAuthor{{Author: "bob", FirstAt: time.Unix(1, 0), FirstID: "m0"},
		{Author: "telegram:4242", FirstAt: time.Unix(2, 0), FirstID: "m1"}}
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m1"))
	w := f.writes["m1"]
	if got := *w.Fields["content"]; got != "[message removed at the request of a participant on 2026-10-30]" {
		t.Fatalf("content = %q", got)
	}
	if w.Author != "removed-participant-2" || w.Fields["metadata"] != nil {
		t.Fatalf("write = %+v", w)
	}
	if len(f.writes) != 1 || len(res.Redacted) != 1 || res.Redacted[0].Kind != TranscriptTombstoned || !res.Complete() {
		t.Fatalf("result = %+v", res)
	}
	n := res.Narrative(nil)
	if n.Entries[0].Outcome != NarrativeTombstoned || len(n.Limitations) != 3 {
		t.Fatalf("narrative = %+v", n)
	}
}

func TestTranscript_OtherParticipantTurnIsMaskedByteForByte(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m2"] = message("m2", "bob", "Ask anna novak, or ANNA NOVAK's mail anna.novak@mail.test. Annabel stays.", nil)
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m2"))
	got := *f.writes["m2"].Fields["content"]
	want := "Ask [person removed], or [person removed]'s mail [person removed]. Annabel stays." + maskSuffix
	if got != want {
		t.Fatalf("content =\n%q\nwant\n%q", got, want)
	}
	if f.writes["m2"].Author != "bob" || res.Redacted[0].Kind != TranscriptMasked {
		t.Fatalf("write = %+v, result = %+v", f.writes["m2"], res)
	}
	if res.Narrative(nil).Entries[0].Outcome != NarrativeMasked {
		t.Fatal("an unmatched author's turn must say masked, not removed")
	}
}

func TestTranscript_JSONColumnsAreWalkedAndVerified(t *testing.T) {
	f := newFakeTranscripts()
	turn := chatTurn("c1", "group-9", "bob", "send it", "done")
	turn.Fields["tool_calls_json"] = str(`[{"name":"send_mail","args":"{\"to\":\"anna.novak@mail.test\"}"}]`)
	turn.Fields["hallucination_signals_json"] = str(`[{"sentence":"I mailed Anna Novak.","anna.novak@mail.test":1}]`)
	f.turns["c1"] = turn
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableChatAuditLog, "c1"))
	w := f.writes["c1"]
	for _, col := range []string{"tool_calls_json", "hallucination_signals_json"} {
		if v := *w.Fields[col]; strings.Contains(strings.ToLower(v), "anna") || !strings.Contains(v, maskToken) {
			t.Errorf("%s = %s", col, v)
		}
	}
	if w.ChatID != nil {
		t.Fatal("a group chat id must not change")
	}
	if !res.Complete() {
		t.Fatalf("result = %+v", res)
	}
}

func TestTranscript_MetadataThatIsNotJSONIsDeferred(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m3"] = message("m3", "bob", "x", str(`{"to": "anna.novak@mail.test"} trailing`))
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m3"))
	if len(f.writes) != 0 || len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "could not be masked safely") {
		t.Fatalf("result = %+v", res)
	}
}

func TestTranscript_OriginPayloadsAreMaskedThroughTheSubtree(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["c2"] = chatTurn("c2", "telegram:4242", "telegram:4242", "book for Anna Novak", "booked")
	f.payloads["c2"] = []TaskPayload{
		{TaskID: "origin", Status: persistence.TaskStatusCompleted, Payload: str(`{"goal":"book for Anna Novak"}`)},
		{TaskID: "child", Status: persistence.TaskStatusCompleted, Payload: str(`{"goal":"mail anna.novak@mail.test"}`)},
		{TaskID: "grandchild", Status: persistence.TaskStatusCompleted, Payload: str(`{"goal":"confirm with Anna Novak"}`)},
		{TaskID: "live", Status: persistence.TaskStatusRunning, Payload: str(`{"goal":"Anna Novak again"}`)},
		{TaskID: "broken", Status: persistence.TaskStatusCompleted, Payload: str(`not json`)},
		{TaskID: "empty", Status: persistence.TaskStatusCompleted},
		{TaskID: "clean", Status: persistence.TaskStatusCompleted, Payload: str(`{"goal":"weather"}`)},
	}
	f.payOut["live"] = TurnResult{Outcome: TranscriptLive, LiveTaskID: "live"}
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableChatAuditLog, "c2"))
	for _, id := range []string{"origin", "child", "grandchild"} {
		if p := f.paid[id]; p == "" || strings.Contains(strings.ToLower(p), "anna") {
			t.Errorf("%s payload = %q", id, p)
		}
	}
	if _, ok := f.paid["clean"]; ok {
		t.Error("an unchanged payload was rewritten")
	}
	deferred := map[string]DeferredAction{}
	for _, d := range res.Deferred {
		deferred[d.RowID] = d
	}
	if deferred["live"].LiveTaskID != "live" || deferred["broken"].Table != TableTasks || len(deferred) != 2 {
		t.Fatalf("deferred = %+v", res.Deferred)
	}
	// The DM chat id is the subject's identifier: pseudonymised.
	if w := f.writes["c2"]; w.ChatID == nil || *w.ChatID != "removed-participant-1" {
		t.Fatalf("chat id = %v", w.ChatID)
	}
}

func TestTranscript_MaskedOtherParticipantOriginMasksItsPayloadToo(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["c3"] = chatTurn("c3", "group-1", "bob", "remind Anna Novak", "ok")
	f.payloads["c3"] = []TaskPayload{{TaskID: "t", Status: persistence.TaskStatusCompleted, Payload: str(`{"goal":"remind Anna Novak"}`)}}
	run(t, transcriptExecutor(f, anna), transcriptPlan(TableChatAuditLog, "c3"))
	if p := f.paid["t"]; !strings.Contains(p, maskToken) {
		t.Fatalf("payload = %q", p)
	}
}

func TestTranscript_PseudonymIsStableAndCollapsesTheSubject(t *testing.T) {
	f := newFakeTranscripts()
	base := time.Unix(100, 0)
	f.turns["a"] = message("a", "Anna Novak", "first", nil)
	f.turns["b"] = message("b", "telegram:4242", "second", nil)
	f.authors["task-1"] = []ThreadAuthor{
		{Author: "carol", FirstAt: base, FirstID: "x2"},
		{Author: "Anna Novak", FirstAt: base, FirstID: "x1"}, // tie with carol: smaller id first
		{Author: "dave", FirstAt: base.Add(time.Second), FirstID: "x3"},
		{Author: "telegram:4242", FirstAt: base.Add(2 * time.Second), FirstID: "x4"},
	}
	run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "a", "b"))
	if f.writes["a"].Author != "removed-participant-1" || f.writes["b"].Author != "removed-participant-1" {
		t.Fatalf("authors = %q, %q", f.writes["a"].Author, f.writes["b"].Author)
	}

	// A re-run after a partial erasure: "a" was tombstoned, "b" was deferred.
	// The tombstoned row's pseudonym is reused although the subject's
	// remaining first turn is later.
	f2 := newFakeTranscripts()
	done := message("a", "removed-participant-1", tombstone(base), nil)
	f2.turns["a"] = done
	f2.turns["b"] = message("b", "telegram:4242", "second", nil)
	f2.authors["task-1"] = []ThreadAuthor{
		{Author: "removed-participant-1", FirstAt: base, FirstID: "x1"},
		{Author: "carol", FirstAt: base, FirstID: "x2"},
		{Author: "dave", FirstAt: base.Add(time.Second), FirstID: "x3"},
		{Author: "telegram:4242", FirstAt: base.Add(2 * time.Second), FirstID: "x4"},
	}
	res := run(t, transcriptExecutor(f2, anna), transcriptPlan(TableTaskMessages, "a", "b"))
	if f2.writes["b"].Author != "removed-participant-1" {
		t.Fatalf("re-run pseudonym = %q", f2.writes["b"].Author)
	}
	if _, wrote := f2.writes["a"]; wrote {
		t.Fatal("an already tombstoned row was written again")
	}
	if len(res.Redacted) != 2 || !res.Complete() {
		t.Fatalf("re-run result = %+v", res)
	}
}

func TestTranscript_DirtyVerificationWritesNothing(t *testing.T) {
	f := newFakeTranscripts()
	// Obfuscated: literal masking does not reach it; the floor's variants do.
	f.turns["m4"] = message("m4", "bob", "write to anna.novak at mail.test", nil)
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m4"))
	if len(f.writes) != 0 || len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "did not verify clean") {
		t.Fatalf("result = %+v", res)
	}
}

func TestTranscript_NoReviewDefers(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m5"] = message("m5", "bob", "hi Anna Novak", nil)
	e := transcriptExecutor(f, anna)
	e.Redact.ApplyWithoutReview = false
	res := run(t, e, transcriptPlan(TableTaskMessages, "m5"))
	if len(f.writes) != 0 || len(res.Deferred) != 1 || res.Deferred[0].Reason != ErrReviewRequired.Error() {
		t.Fatalf("result = %+v", res)
	}
	var seen RedactionProposal
	e.Redact.Approve = func(p RedactionProposal) (bool, error) { seen = p; return true, nil }
	run(t, e, transcriptPlan(TableTaskMessages, "m5"))
	if seen.Model != TranscriptMaskModel || !strings.Contains(seen.After, maskToken) || len(f.writes) != 1 {
		t.Fatalf("proposal = %+v", seen)
	}
}

func TestTranscript_LiveRowsAndReferrersDeferWithTheirTask(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m6"] = message("m6", "telegram:4242", "x", nil)
	f.outcome = TurnResult{Outcome: TranscriptLive, LiveTaskID: "task-9"}
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m6"))
	if len(res.Deferred) != 1 || res.Deferred[0].LiveTaskID != "task-9" || res.Complete() {
		t.Fatalf("live = %+v", res)
	}

	f2 := newFakeTranscripts()
	turn := chatTurn("c4", "group-1", "telegram:4242", "x", "y")
	turn.PromptHash, turn.PromptBody = "h-old", str("You help Anna Novak.")
	f2.turns["c4"] = turn
	f2.outcome = TurnResult{Outcome: TranscriptApplied, DeferredReferrers: []ReferrerDeferral{
		{RowID: "c5", LiveTaskID: "task-5"}, {RowID: "c6", Arrived: true}}}
	res = run(t, transcriptExecutor(f2, anna), transcriptPlan(TableChatAuditLog, "c4"))
	if p := f2.writes["c4"].Prompt; p == nil || p.OldHash != "h-old" || p.NewBody != "You help [person removed]." {
		t.Fatalf("prompt = %+v", p)
	}
	if len(res.Deferred) != 2 || res.Deferred[0].Table != TableChatSystemPrompts || res.Deferred[0].LiveTaskID != "task-5" ||
		!strings.Contains(res.Deferred[1].Reason, "must be erased there") {
		t.Fatalf("referrers = %+v", res.Deferred)
	}
}

func TestTranscript_WithoutTheStoreTranscriptsStillDefer(t *testing.T) {
	e := transcriptExecutor(newFakeTranscripts(), anna)
	e.Redact.Transcripts = nil
	res := run(t, e, transcriptPlan(TableTaskMessages, "m7"))
	if len(res.Deferred) != 1 || res.Deferred[0].Reason != reasonNoRedactionPath {
		t.Fatalf("result = %+v", res)
	}
}

func TestMaskText_WordBoundaries(t *testing.T) {
	cases := map[string]string{
		"Jo said hi":         "[person removed] said hi",
		"a job for jo":       "a job for [person removed]",
		"+420 777 1 called":  "[person removed] called",
		"x+420 777 1 y":      "x[person removed] y", // a non-word edge needs no boundary
		"x+420 777 1y":       "x+420 777 1y",        // "1y" is one word: left for the floor
		"JO, Jo and jo.":     "[person removed], [person removed] and [person removed].",
		"nothing about them": "nothing about them",
	}
	for in, want := range cases {
		if got := maskText(in, []string{"Jo", "+420 777 1"}); got != want {
			t.Errorf("maskText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Review 6648 F3: two object keys that mask to the same key would keep one
// value and drop the other silently. The row is deferred instead.
func TestTranscript_JSONKeyCollisionIsDeferred(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m8"] = message("m8", "bob", "x", str(`{"Anna Novak": "1", "anna.novak@mail.test": "2"}`))
	res := run(t, transcriptExecutor(f, anna), transcriptPlan(TableTaskMessages, "m8"))
	if len(f.writes) != 0 || len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "could not be masked safely") {
		t.Fatalf("result = %+v", res)
	}
}

// Security review of cc464bb70: the floor walked JSON strings only, so an
// identifier stored as a JSON number (a phone number) was neither masked nor
// verified, and the row was reported clean. The whole document is verified
// too; such a row defers.
func TestTranscript_IdentifierAsAJSONNumberDefers(t *testing.T) {
	f := newFakeTranscripts()
	f.turns["m9"] = message("m9", "bob", "call her", str(`{"phone": 420777123456, "note": "ok"}`))
	ids := fixedIDs{"telegram:4242", "420777123456"}
	res := run(t, transcriptExecutor(f, ids), transcriptPlan(TableTaskMessages, "m9"))
	if len(f.writes) != 0 || len(res.Redacted) != 0 || len(res.Deferred) != 1 ||
		!strings.Contains(res.Deferred[0].Reason, "did not verify clean") {
		t.Fatalf("result = %+v", res)
	}

	f2 := newFakeTranscripts()
	f2.turns["c9"] = chatTurn("c9", "group-1", "bob", "book it", "ok")
	f2.payloads["c9"] = []TaskPayload{{TaskID: "t9", Status: "COMPLETED", Payload: str(`{"goal": "call", "to": 420777123456}`)}}
	res = run(t, transcriptExecutor(f2, ids), transcriptPlan(TableChatAuditLog, "c9"))
	if _, wrote := f2.paid["t9"]; wrote {
		t.Fatal("a payload holding the identifier as a number was written")
	}
	deferred := false
	for _, d := range res.Deferred {
		deferred = deferred || d.RowID == "t9"
	}
	if !deferred {
		t.Fatalf("payload not deferred: %+v", res)
	}
}
