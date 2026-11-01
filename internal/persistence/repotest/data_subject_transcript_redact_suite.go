package repotest

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/subjectstore"
)

// Slice 5d-1a (GitHub #344; design 2026-07-29-art17-redact-and-reembed
// §15.14): the transcript store's guarded write, on both drivers. Each case is
// a property the erasure report's claims rest on: a row written is a row whose
// every column held what the engine read and whose tasks were all settled;
// anything else writes nothing and says which it was.

// TranscriptRedactStore is the surface the suite drives.
type TranscriptRedactStore interface {
	datasubject.TranscriptRedactor
	SetTranscriptHooks(h subjectstore.TranscriptHooks)
}

// SQLExec runs a statement with $n placeholders on the driver under test, and
// binds a timestamp in the driver's form.
type SQLExec interface {
	Exec(ctx context.Context, query string, args ...any) error
	QueryString(ctx context.Context, query string, args ...any) (*string, error)
	Time(t time.Time) any
}

type transcriptFixture struct {
	t    *testing.T
	ctx  context.Context
	db   SQLExec
	proj string
	base time.Time
}

func (f *transcriptFixture) at(i int) any {
	return f.db.Time(f.base.Add(time.Duration(i) * time.Second))
}

func (f *transcriptFixture) exec(q string, args ...any) {
	f.t.Helper()
	if err := f.db.Exec(f.ctx, q, args...); err != nil {
		f.t.Fatalf("exec %q: %v", q, err)
	}
}

func (f *transcriptFixture) task(id, parent, chatTurn string, status persistence.TaskStatus, payload *string) {
	f.t.Helper()
	f.exec(`INSERT INTO tasks (id, project_id, status, parent_task_id, chat_turn_id, payload, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $7)`, id, f.proj, string(status), nullStr(parent), nullStr(chatTurn),
		ptrAny(payload), f.at(0))
}

func (f *transcriptFixture) message(id, taskID, author, content string, metadata *string, i int) {
	f.t.Helper()
	f.exec(`INSERT INTO task_messages (id, task_id, author_kind, author_id, message_kind, content, metadata, created_at)
		VALUES ($1, $2, 'operator', $3, 'message', $4, $5, $6)`, id, taskID, nullStr(author), content, ptrAny(metadata), f.at(i))
}

func (f *transcriptFixture) turn(id, chatID, user, msg, resp, promptHash string, i int) {
	f.t.Helper()
	f.exec(`INSERT INTO chat_audit_log (id, ts, chat_id, user_id, project_id, system_prompt_hash, user_message, response)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, id, f.at(i), chatID, user, f.proj, promptHash, msg, resp)
}

func (f *transcriptFixture) prompt(body string) string {
	f.t.Helper()
	h := persistence.HashChatSystemPrompt(body)
	f.exec(`INSERT INTO chat_system_prompts (hash, body, created_at) VALUES ($1, $2, $3) ON CONFLICT (hash) DO NOTHING`,
		h, body, f.at(0))
	return h
}

func (f *transcriptFixture) str(q string, args ...any) *string {
	f.t.Helper()
	v, err := f.db.QueryString(f.ctx, q, args...)
	if err != nil {
		f.t.Fatalf("query %q: %v", q, err)
	}
	return v
}

func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func ptrAny(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func sp(s string) *string { return &s }

// ownWrite tombstones a task message's content and renames its author,
// keeping every other column.
func ownWrite(turn datasubject.TranscriptTurn, content, author string) datasubject.TurnWrite {
	fields := map[string]*string{}
	for k, v := range turn.Fields {
		fields[k] = v
	}
	fields["content"] = sp(content)
	return datasubject.TurnWrite{Fields: fields, Author: author}
}

// RunTranscriptRedactSuite pins the 5d-1a store on one driver.
func RunTranscriptRedactSuite(t *testing.T, s TranscriptRedactStore, db SQLExec) {
	t.Helper()
	cases := []struct {
		name string
		run  func(*testing.T, TranscriptRedactStore, *transcriptFixture)
	}{
		{"a NULL-metadata row is applied, not version-changed", transcriptCase01},
		{"JSON metadata round-trips through the guard", transcriptCase02},
		{"a column changed after load is version-changed and nothing is written", transcriptCase03},
		{"two erasures race on one row: one applies, the other is version-changed", transcriptCase04},
		{"a task started between load and apply makes the row live", transcriptCase05},
		{"a live child under a settled parent makes the row live, naming the child", transcriptCase06},
		{"a chat turn's origin task subtree is its live anchor", transcriptCase07},
		{"prompt re-key: a settled referrer moves, a live one is deferred, the old body waits for it", transcriptCase08},
		{"a referrer that arrives during the re-key keeps the old body and is reported", transcriptCase09},
		{"thread authors are ordered by first turn, a tie by row id", transcriptCase10},
		{"origin payloads reach the subtree through parent_task_id, and nothing else", transcriptCase11},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, s, &transcriptFixture{t: t, ctx: context.Background(), db: db, proj: uniqueID("proj"),
				base: time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)})
		})
	}
}

// transcriptCase01: a NULL-metadata row is applied, not version-changed.
func transcriptCase01(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task, msg := uniqueID("task"), uniqueID("msg")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(msg, task, "telegram:1", "hello from anna", nil, 1)
	turn, err := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	if err != nil {
		t.Fatal(err)
	}
	if turn.Fields["metadata"] != nil || turn.TaskID != task || turn.Author != "telegram:1" {
		t.Fatalf("loaded %+v", turn)
	}
	res, err := s.ApplyTurn(f.ctx, turn, ownWrite(turn, "[removed]", "removed-participant-1"))
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if got := f.str(`SELECT content FROM task_messages WHERE id = $1`, msg); got == nil || *got != "[removed]" {
		t.Fatalf("content = %v", got)
	}
	if got := f.str(`SELECT author_id FROM task_messages WHERE id = $1`, msg); got == nil || *got != "removed-participant-1" {
		t.Fatalf("author = %v", got)
	}
	if got := f.str(`SELECT CAST(metadata AS TEXT) FROM task_messages WHERE id = $1`, msg); got != nil {
		t.Fatalf("metadata = %q, want NULL kept", *got)
	}
}

// transcriptCase02: JSON metadata round-trips through the guard.
func transcriptCase02(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task, msg := uniqueID("task"), uniqueID("msg")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(msg, task, "", "x", sp(`{"from": "anna"}`), 1)
	turn, err := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	if err != nil {
		t.Fatal(err)
	}
	w := ownWrite(turn, "x", "")
	w.Fields["metadata"] = sp(`{"from": "[person removed]"}`)
	res, err := s.ApplyTurn(f.ctx, turn, w)
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	got, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	if m := got.Fields["metadata"]; m == nil || *m != `{"from": "[person removed]"}` {
		t.Fatalf("metadata = %v", m)
	}
}

// transcriptCase03: a column changed after load is version-changed and nothing is written.
func transcriptCase03(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task, msg := uniqueID("task"), uniqueID("msg")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(msg, task, "telegram:1", "original", nil, 1)
	turn, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	f.exec(`UPDATE task_messages SET content = 'edited' WHERE id = $1`, msg)
	res, err := s.ApplyTurn(f.ctx, turn, ownWrite(turn, "[removed]", "removed-participant-1"))
	if err != nil || res.Outcome != datasubject.TranscriptVersionChanged {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if got := f.str(`SELECT content FROM task_messages WHERE id = $1`, msg); *got != "edited" {
		t.Fatalf("content = %q", *got)
	}
}

// transcriptCase04: two erasures race on one row: one applies, the other is version-changed.
func transcriptCase04(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task, msg := uniqueID("task"), uniqueID("msg")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(msg, task, "telegram:1", "anna and bob", nil, 1)
	a, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	b, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	r1, err1 := s.ApplyTurn(f.ctx, a, ownWrite(a, "[person removed] and bob", "telegram:1"))
	r2, err2 := s.ApplyTurn(f.ctx, b, ownWrite(b, "anna and [person removed]", "telegram:1"))
	if err1 != nil || err2 != nil || r1.Outcome != datasubject.TranscriptApplied || r2.Outcome != datasubject.TranscriptVersionChanged {
		t.Fatalf("got %+v/%v, %+v/%v", r1, err1, r2, err2)
	}
}

// transcriptCase05: a task started between load and apply makes the row live.
func transcriptCase05(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task, msg := uniqueID("task"), uniqueID("msg")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(msg, task, "telegram:1", "hi", nil, 1)
	turn, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	f.exec(`UPDATE tasks SET status = 'RUNNING' WHERE id = $1`, task)
	res, err := s.ApplyTurn(f.ctx, turn, ownWrite(turn, "[removed]", "removed-participant-1"))
	if err != nil || res.Outcome != datasubject.TranscriptLive || res.LiveTaskID != task {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if got := f.str(`SELECT content FROM task_messages WHERE id = $1`, msg); *got != "hi" {
		t.Fatalf("content = %q", *got)
	}
}

// transcriptCase06: a live child under a settled parent makes the row live, naming the child.
func transcriptCase06(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	parent, child, grand, msg := uniqueID("task"), uniqueID("task"), uniqueID("task"), uniqueID("msg")
	f.task(parent, "", "", persistence.TaskStatusCompleted, nil)
	f.task(child, parent, "", persistence.TaskStatusCompleted, nil)
	f.task(grand, child, "", persistence.TaskStatusAwaitingInput, nil)
	f.message(msg, parent, "telegram:1", "hi", nil, 1)
	turn, _ := s.LoadTurn(f.ctx, datasubject.TableTaskMessages, msg)
	res, err := s.ApplyTurn(f.ctx, turn, ownWrite(turn, "[removed]", "removed-participant-1"))
	if err != nil || res.Outcome != datasubject.TranscriptLive || res.LiveTaskID != grand {
		t.Fatalf("apply = %+v, %v", res, err)
	}
}

// transcriptCase07: a chat turn's origin task subtree is its live anchor.
func transcriptCase07(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	row, origin, child := uniqueID("chat"), uniqueID("task"), uniqueID("task")
	f.turn(row, "dm-1", "telegram:1", "book it", "booked", "", 1)
	f.task(origin, "", row, persistence.TaskStatusCompleted, nil)
	f.task(child, origin, "", persistence.TaskStatusRunning, nil)
	turn, err := s.LoadTurn(f.ctx, datasubject.TableChatAuditLog, row)
	if err != nil {
		t.Fatal(err)
	}
	w := datasubject.TurnWrite{Fields: turn.Fields, Author: "telegram:1"}
	res, err := s.ApplyTurn(f.ctx, turn, w)
	if err != nil || res.Outcome != datasubject.TranscriptLive || res.LiveTaskID != child {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	f.exec(`UPDATE tasks SET status = 'COMPLETED' WHERE id = $1`, child)
	dm := "removed-participant-1"
	w.Fields = map[string]*string{}
	for k, v := range turn.Fields {
		w.Fields[k] = v
	}
	w.Fields["user_message"] = sp("[removed]")
	w.Author, w.ChatID = dm, &dm
	res, err = s.ApplyTurn(f.ctx, turn, w)
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("apply after settle = %+v, %v", res, err)
	}
	got, _ := s.LoadTurn(f.ctx, datasubject.TableChatAuditLog, row)
	if got.ChatID != dm || got.Author != dm || *got.Fields["user_message"] != "[removed]" || *got.Fields["response"] != "booked" {
		t.Fatalf("after = %+v", got)
	}
}

// transcriptCase08: prompt re-key: a settled referrer moves, a live one is deferred, the old body waits for it.
func transcriptCase08(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	body := "You are helping anna.example@mail.test " + uniqueID("p")
	old := f.prompt(body)
	row, settled, live, liveTask := uniqueID("chat"), uniqueID("chat"), uniqueID("chat"), uniqueID("task")
	f.turn(row, "dm-1", "telegram:1", "hi", "hello", old, 1)
	f.turn(settled, "dm-2", "telegram:2", "q", "a", old, 2)
	f.turn(live, "dm-3", "telegram:3", "q", "a", old, 3)
	f.task(liveTask, "", live, persistence.TaskStatusRunning, nil)
	turn, _ := s.LoadTurn(f.ctx, datasubject.TableChatAuditLog, row)
	if turn.PromptBody == nil || *turn.PromptBody != body {
		t.Fatalf("prompt body = %v", turn.PromptBody)
	}
	masked := "You are helping [person removed] " + body[len("You are helping anna.example@mail.test "):]
	w := datasubject.TurnWrite{Fields: turn.Fields, Author: turn.Author, Prompt: &datasubject.PromptRekey{OldHash: old, NewBody: masked}}
	res, err := s.ApplyTurn(f.ctx, turn, w)
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	newHash := persistence.HashChatSystemPrompt(masked)
	if res.NewPromptHash != newHash || res.Repointed != 1 || res.OldPromptDeleted ||
		len(res.DeferredReferrers) != 1 || res.DeferredReferrers[0].RowID != live || res.DeferredReferrers[0].LiveTaskID != liveTask {
		t.Fatalf("re-key = %+v", res)
	}
	for id, want := range map[string]string{row: newHash, settled: newHash, live: old} {
		if got := f.str(`SELECT system_prompt_hash FROM chat_audit_log WHERE id = $1`, id); *got != want {
			t.Errorf("%s hash = %s, want %s", id, *got, want)
		}
	}
	if f.str(`SELECT body FROM chat_system_prompts WHERE hash = $1`, old) == nil {
		t.Fatal("old body deleted while a live referrer still uses it")
	}
	// A re-run after the task settles moves it and deletes the old body.
	f.exec(`UPDATE tasks SET status = 'COMPLETED' WHERE id = $1`, liveTask)
	lt, _ := s.LoadTurn(f.ctx, datasubject.TableChatAuditLog, live)
	res, err = s.ApplyTurn(f.ctx, lt, datasubject.TurnWrite{Fields: lt.Fields, Author: lt.Author,
		Prompt: &datasubject.PromptRekey{OldHash: old, NewBody: masked}})
	if err != nil || res.Outcome != datasubject.TranscriptApplied || !res.OldPromptDeleted {
		t.Fatalf("re-run = %+v, %v", res, err)
	}
	if f.str(`SELECT body FROM chat_system_prompts WHERE hash = $1`, old) != nil {
		t.Fatal("old body survives with no referrer")
	}
}

// transcriptCase09: a referrer that arrives during the re-key keeps the old body and is reported.
func transcriptCase09(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	body := "profile of anna " + uniqueID("p")
	old := f.prompt(body)
	row, arrived := uniqueID("chat"), uniqueID("chat")
	f.turn(row, "dm-1", "telegram:1", "hi", "hello", old, 1)
	s.SetTranscriptHooks(subjectstore.TranscriptHooks{AfterOldPromptDelete: func() {
		// The audit writers upsert the body before the row (SavePrompt).
		f.prompt(body)
		f.turn(arrived, "dm-9", "telegram:9", "new", "turn", old, 9)
	}})
	defer s.SetTranscriptHooks(subjectstore.TranscriptHooks{})
	turn, _ := s.LoadTurn(f.ctx, datasubject.TableChatAuditLog, row)
	res, err := s.ApplyTurn(f.ctx, turn, datasubject.TurnWrite{Fields: turn.Fields, Author: turn.Author,
		Prompt: &datasubject.PromptRekey{OldHash: old, NewBody: "profile of [person removed]"}})
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if len(res.DeferredReferrers) != 1 || res.DeferredReferrers[0].RowID != arrived || !res.DeferredReferrers[0].Arrived {
		t.Fatalf("deferred = %+v", res.DeferredReferrers)
	}
	if f.str(`SELECT body FROM chat_system_prompts WHERE hash = $1`, old) == nil {
		t.Fatal("the arrived referrer's body dangles")
	}
}

// transcriptCase10: thread authors are ordered by first turn, a tie by row id.
func transcriptCase10(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	task := uniqueID("task")
	f.task(task, "", "", persistence.TaskStatusCompleted, nil)
	f.message(task+"-b", task, "bob", "1", nil, 1)
	f.message(task+"-a", task, "anna", "2", nil, 1)
	f.message(task+"-c", task, "carol", "3", nil, 0)
	f.message(task+"-d", task, "bob", "4", nil, 5)
	f.message(task+"-e", task, "", "agent", nil, 0)
	got, err := s.ThreadAuthors(f.ctx, datasubject.TableTaskMessages, task)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, a := range got {
		names = append(names, a.Author)
	}
	if len(names) != 3 || names[0] != "carol" || names[1] != "anna" || names[2] != "bob" {
		t.Fatalf("order = %v", names)
	}
}

// transcriptCase11: origin payloads reach the subtree through parent_task_id, and nothing else.
func transcriptCase11(t *testing.T, s TranscriptRedactStore, f *transcriptFixture) {
	row, other := uniqueID("chat"), uniqueID("chat")
	f.turn(row, "dm-1", "telegram:1", "hi", "ok", "", 1)
	f.turn(other, "dm-1", "telegram:1", "hi", "ok", "", 2)
	origin, child, grand, unrelated, nullTask := uniqueID("task"), uniqueID("task"), uniqueID("task"), uniqueID("task"), uniqueID("task")
	f.task(origin, "", row, persistence.TaskStatusCompleted, sp(`{"goal": "mail anna"}`))
	f.task(child, origin, "", persistence.TaskStatusCompleted, sp(`{"goal": "draft for anna"}`))
	f.task(grand, child, "", persistence.TaskStatusRunning, sp(`{"goal": "send to anna"}`))
	f.task(nullTask, origin, "", persistence.TaskStatusCompleted, nil)
	f.task(unrelated, "", other, persistence.TaskStatusCompleted, sp(`{"goal": "anna again"}`))
	got, err := s.OriginPayloads(f.ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]datasubject.TaskPayload{}
	for _, p := range got {
		seen[p.TaskID] = p
	}
	if len(seen) != 4 || seen[unrelated].TaskID != "" || seen[nullTask].Payload != nil || seen[grand].Status != persistence.TaskStatusRunning {
		t.Fatalf("payloads = %+v", got)
	}
	res, err := s.ApplyPayload(f.ctx, seen[child], `{"goal": "draft for [person removed]"}`)
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("child = %+v, %v", res, err)
	}
	res, err = s.ApplyPayload(f.ctx, seen[grand], `{"goal": "send to [person removed]"}`)
	if err != nil || res.Outcome != datasubject.TranscriptLive || res.LiveTaskID != grand {
		t.Fatalf("live grandchild = %+v, %v", res, err)
	}
	// A settled origin is written on its own status; its live grandchild's
	// payload is a separate copy and waits on its own.
	res, err = s.ApplyPayload(f.ctx, seen[origin], `{"goal": "mail [person removed]"}`)
	if err != nil || res.Outcome != datasubject.TranscriptApplied {
		t.Fatalf("origin = %+v, %v", res, err)
	}
	stale := seen[child]
	res, err = s.ApplyPayload(f.ctx, stale, `{"goal": "x"}`)
	if err != nil || res.Outcome != datasubject.TranscriptVersionChanged {
		t.Fatalf("stale = %+v, %v", res, err)
	}
	if got := f.str(`SELECT CAST(payload AS TEXT) FROM tasks WHERE id = $1`, unrelated); got == nil || *got != `{"goal": "anna again"}` {
		t.Fatalf("unrelated payload = %v", got)
	}
}
