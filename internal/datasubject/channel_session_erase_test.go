package datasubject

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GitHub #344 slice 5d-2 (art17 redact design §15.18). Found at: review,
// observed at integration (§15.10.1): every channel_sessions erasure was
// deferred, because a stored-row delete from vornikctl was undone by the
// daemon's next save. With the fence it is deleted or masked here.

type fakeSessions struct {
	rows     map[string]ChannelSession
	laggards []string
	// moveTimes makes the next N guarded writes find the row moved.
	moveTimes int
	writes    int
	deletes   int
}

func (f *fakeSessions) LoadSession(_ context.Context, rowID string) (ChannelSession, bool, error) {
	s, ok := f.rows[rowID]
	return s, ok, nil
}

func (f *fakeSessions) moved() bool {
	if f.moveTimes > 0 {
		f.moveTimes--
		return true
	}
	return false
}

func (f *fakeSessions) DeleteSession(_ context.Context, s ChannelSession) (bool, error) {
	if f.moved() {
		return false, nil
	}
	f.deletes++
	for k, v := range f.rows {
		if v.Kind == s.Kind && v.SessionID == s.SessionID {
			delete(f.rows, k)
		}
	}
	return true, nil
}

func (f *fakeSessions) WriteSession(_ context.Context, s ChannelSession, history []byte) (bool, error) {
	if f.moved() {
		return false, nil
	}
	f.writes++
	k := s.Kind + "/" + s.SessionID
	s.History, s.Version = history, s.Version+1
	f.rows[k] = s
	return true, nil
}

func (f *fakeSessions) FenceLaggards(context.Context) ([]string, error) { return f.laggards, nil }

func sessionExecutor(f *fakeSessions) *Executor {
	return &Executor{
		Rows: &recordingDeleter{}, Artifacts: noArtifacts{}, Sessions: f,
		Redact: &RedactDeps{
			Identifiers: fakeIDs{ids: []string{"jane@example.com", "Jane Doe"}},
			Approve:     approveAll,
			Now:         func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
		},
	}
}

func session(kind, sid, history string) map[string]ChannelSession {
	return map[string]ChannelSession{kind + "/" + sid: {Kind: kind, SessionID: sid, History: []byte(history), Version: 1}}
}

func runSessions(t *testing.T, e *Executor, actions ...Action) *ErasureResult {
	t.Helper()
	res, err := e.Execute(context.Background(), &ErasurePlan{SubjectID: "s", RequestID: "r", Actions: actions})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestSessionErase_DeleteAndRetries(t *testing.T) {
	f := &fakeSessions{rows: session("slack", "D1#main", `[{"role":"user","content":"hi"}]`), moveTimes: 1}
	res := runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "slack/D1#main",
		Disposition: DispositionDelete, Exclusivity: ExclusiveRow})
	if f.deletes != 1 || res.RowsDeleted != 1 || len(res.Deleted) != 1 || len(f.rows) != 0 {
		t.Fatalf("delete after one moved attempt: %+v rows=%v", res, f.rows)
	}

	f = &fakeSessions{rows: session("slack", "D2#main", `[]`), moveTimes: sessionWriteAttempts}
	res = runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "slack/D2#main",
		Disposition: DispositionDelete, Exclusivity: ExclusiveRow})
	if len(res.Deferred) != 1 || res.Deferred[0].Reason != reasonSessionActive || res.Complete() {
		t.Fatalf("three moved attempts must defer as active: %+v", res)
	}
}

func TestSessionErase_ALaggardNodeDefersNamingIt(t *testing.T) {
	f := &fakeSessions{rows: session("slack", "D1#main", `[]`), laggards: []string{"node-old (2026.10.5)"}}
	res := runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "slack/D1#main",
		Disposition: DispositionDelete, Exclusivity: ExclusiveRow})
	if f.deletes != 0 || len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "node-old") {
		t.Fatalf("a laggard node must defer, naming it: %+v", res)
	}
}

func TestSessionErase_NoStoreIsTheLiveSessionDeferral(t *testing.T) {
	e := sessionExecutor(&fakeSessions{})
	e.Sessions = nil
	res := runSessions(t, e, Action{Table: TableChannelSessions, RowID: "webchat/w", Disposition: DispositionDelete})
	if len(res.Deferred) != 1 || res.Deferred[0].Reason != reasonLiveSession {
		t.Fatalf("no fenced store (SQLite) must keep the deferral: %+v", res)
	}
}

func historyOf(t *testing.T, f *fakeSessions, key string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := json.Unmarshal(f.rows[key].History, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestSessionErase_EmailOwnEntryTombstonedOthersMasked(t *testing.T) {
	h := `[{"role":"user","content":"From: Jane Doe <jane@example.com>\n\nplease call me"},` +
		`{"role":"assistant","content":"Sure Jane Doe, noted.","tool_calls":[{"id":"c1","type":"function",` +
		`"function":{"name":"remember","arguments":"{\"who\":\"jane@example.com\",\"n\":5}"}}]},` +
		`{"role":"user","content":"From: bob@example.com\n\nI spoke to Jane Doe today"}]`
	f := &fakeSessions{rows: session("email", "<m1@x>", h)}
	res := runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "email/<m1@x>",
		Disposition: DispositionRedact, Exclusivity: SharedRow})
	if len(res.Redacted) != 1 || res.Redacted[0].Kind != SessionTombstoned {
		t.Fatalf("result: %+v", res)
	}
	got := historyOf(t, f, "email/<m1@x>")
	if c := got[0]["content"].(string); !strings.HasPrefix(c, tombstoneStart+"2026-10-30") {
		t.Errorf("own entry not tombstoned: %q", c)
	}
	if c := got[2]["content"].(string); strings.Contains(c, "Jane") || !strings.Contains(c, "bob@example.com") {
		t.Errorf("another participant's entry: %q", c)
	}
	args := got[1]["tool_calls"].([]any)[0].(map[string]any)["function"].(map[string]any)["arguments"].(string)
	if strings.Contains(args, "jane@example.com") || !strings.Contains(args, `"n":5`) {
		t.Errorf("tool call arguments not masked as JSON: %s", args)
	}
	if err := VerifyRedaction([]string{"jane@example.com", "Jane Doe"}, string(f.rows["email/<m1@x>"].History)); err != nil {
		t.Errorf("an identifier survives: %v", err)
	}
}

func TestSessionErase_WebchatEveryUserEntryIsOwn(t *testing.T) {
	f := &fakeSessions{rows: session("webchat", "w1", `[{"role":"user","content":"I am in Prague"},{"role":"assistant","content":"ok"}]`)}
	res := runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "webchat/w1",
		Disposition: DispositionRedact, Exclusivity: SharedRow})
	got := historyOf(t, f, "webchat/w1")
	if len(res.Redacted) != 1 || res.Redacted[0].Kind != SessionTombstoned ||
		!strings.HasPrefix(got[0]["content"].(string), tombstoneStart) || got[1]["content"] != "ok" {
		t.Fatalf("webchat: %+v %v", res, got)
	}
}

// #346 decision Q5 (art17 redact design §15.19): a shared session whose
// history records no speaker cannot have the subject's own words removed, so
// it is deleted in full and the report says why.
func TestSessionErase_UnattributedSharedSessionIsDeleted(t *testing.T) {
	f := &fakeSessions{rows: session("slack", "T/C#1.1", `[{"role":"user","content":"jane@example.com here, my plan is X"}]`)}
	res := runSessions(t, sessionExecutor(f), Action{Table: TableChannelSessions, RowID: "slack/T/C#1.1",
		Disposition: DispositionRedact, Exclusivity: SharedRow})
	if f.deletes != 1 || f.writes != 0 || len(f.rows) != 0 {
		t.Fatalf("an unattributed shared session must be deleted: deletes=%d writes=%d", f.deletes, f.writes)
	}
	if len(res.Deleted) != 1 || res.Deleted[0].Kind != SessionUnattributed || len(res.Redacted) != 0 {
		t.Fatalf("result: %+v", res)
	}
	if n := res.Narrative(nil); n.Entries[0].Outcome != NarrativeSessionUnattributed {
		t.Fatalf("narrative: %+v", n.Entries)
	}
}

func TestSessionErase_ADirtyResultWritesNothing(t *testing.T) {
	// The identifier held as a JSON number: no string reaches it.
	e := sessionExecutor(&fakeSessions{})
	e.Redact.Identifiers = fakeIDs{ids: []string{"420777123456"}}
	f := &fakeSessions{rows: session("email", "1", `[{"role":"assistant","content":"x","meta":420777123456}]`)}
	e.Sessions = f
	res := runSessions(t, e, Action{Table: TableChannelSessions, RowID: "email/1",
		Disposition: DispositionRedact, Exclusivity: SharedRow})
	if f.writes != 0 || len(res.Deferred) != 1 || res.Deferred[0].Reason != reasonSessionMasking {
		t.Fatalf("a dirty result must defer and write nothing: %+v", res)
	}
}

func TestSessionErase_AMissingRowIsAlreadyErased(t *testing.T) {
	res := runSessions(t, sessionExecutor(&fakeSessions{rows: map[string]ChannelSession{}}),
		Action{Table: TableChannelSessions, RowID: "slack/gone", Disposition: DispositionDelete})
	if len(res.Deleted) != 1 || res.RowsDeleted != 0 || !res.Complete() {
		t.Fatalf("missing row: %+v", res)
	}
}

// §15.18.1: Telegram's named saves are reached from the same erasure.
func writeSave(t *testing.T, root, chatID, name, content string) string {
	t.Helper()
	dir := filepath.Join(root, "saves", chatID)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTelegramSaves_DeletedWithTheSession(t *testing.T) {
	root := t.TempDir()
	p := writeSave(t, root, "77", "trip", `{"chat_id":77,"messages":[{"role":"user","content":"x"}]}`)
	other := writeSave(t, root, "78", "keep", `{"chat_id":78,"messages":[]}`)
	e := sessionExecutor(&fakeSessions{rows: session("telegram", "77", `[]`)})
	e.TelegramSaves = &TelegramSaves{Root: root}
	res := runSessions(t, e, Action{Table: TableChannelSessions, RowID: "telegram/77",
		Disposition: DispositionDelete, Exclusivity: ExclusiveRow})
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the chat's save was not removed")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("another chat's save was touched")
	}
	if len(res.Deleted) != 2 || !strings.Contains(res.Deleted[1].RowID, "saved copy trip") {
		t.Fatalf("report: %+v", res.Deleted)
	}
}

// Telegram records no speaker, so a redaction of a Telegram session deletes
// it, and its saved copies with it (§15.19).
func TestTelegramSaves_DeletedOnARedactionToo(t *testing.T) {
	root := t.TempDir()
	p := writeSave(t, root, "77", "a", `{"chat_id":77,"messages":[{"role":"assistant","content":"Jane Doe called"}]}`)
	e := sessionExecutor(&fakeSessions{rows: session("telegram", "77", `[]`)})
	e.TelegramSaves = &TelegramSaves{Root: root}
	res := runSessions(t, e, Action{Table: TableChannelSessions, RowID: "telegram/77",
		Disposition: DispositionRedact, Exclusivity: SharedRow})
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the save was not removed")
	}
	if len(res.Deleted) != 2 {
		t.Fatalf("report: %+v", res.Deleted)
	}
}

func TestTelegramSaves_UnreachableDirectoryDefersNamingIt(t *testing.T) {
	e := sessionExecutor(&fakeSessions{rows: session("telegram", "77", `[]`)})
	e.TelegramSaves = &TelegramSaves{Root: "/nonexistent/vornik-test"}
	res := runSessions(t, e, Action{Table: TableChannelSessions, RowID: "telegram/77",
		Disposition: DispositionDelete, Exclusivity: ExclusiveRow})
	if len(res.Deferred) != 1 || !strings.Contains(res.Deferred[0].Reason, "/nonexistent/vornik-test/saves/77") {
		t.Fatalf("unreachable: %+v", res.Deferred)
	}
}
