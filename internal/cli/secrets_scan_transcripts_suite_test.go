package cli

// GitHub #414 (1): `vornikctl secrets scan-history` never rescanned
// chat_audit_log.hallucination_signals_json, chat_audit_log.chat_id/user_id or
// task_messages (content, metadata, author_id), so a row written before the
// #395 seam kept any credential it held, and the dry run could not say how many
// such rows there were. Found at: review (#395's §9.2.7). A user observes it at
// integration: the stored rows still hold the credential after the operator
// runs the rescan. This suite drives the scanners the command runs against a
// real database, and runs on BOTH drivers: SQLite in the default lane
// (secrets_scan_transcripts_sqlite_test.go) and Postgres under
// -tags=integration (secrets_scan_transcripts_integration_test.go).
// Design: chat-audit retention and redaction design §10.2.

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secrets"
)

// Synthetic credentials of the right shape; never real.
const (
	gh414  = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	aws414 = "AKIAQ3EGUZJ7RLW4XN2P"
	pw414  = "hunter2-long-enough-414"
	// cutRun414 is a benign run at a signal's cut edge: the edge-run rule
	// fires on it, and only --rules all (or cut_token) may rewrite it.
	cutRun414 = "AbCdEfGhIjKlMnOpQrSt"
)

var hash414 = strings.Repeat("0123456789abcdef", 4)

// transcriptRig is one driver's database plus how to make a task on it.
type transcriptRig struct {
	env        scanEnv
	createTask func(t *testing.T, id, project string)
	// orphans is true when the driver can hold a task message whose task is
	// gone (SQLite: no foreign keys before #361).
	orphans bool
}

func det414(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func strong414(t *testing.T) ruleSelection {
	t.Helper()
	sel, err := parseRuleSelection("strong", secrets.EffectivePatterns(nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

type chatRow414 struct{ chatID, userID, userMessage, response, toolCalls, signals string }

func readChat414(t *testing.T, db *sql.DB, id string) chatRow414 {
	t.Helper()
	var r chatRow414
	if err := db.QueryRow(`SELECT chat_id, user_id, user_message, response, tool_calls_json,
		COALESCE(hallucination_signals_json, '') FROM chat_audit_log WHERE id = $1`, id).
		Scan(&r.chatID, &r.userID, &r.userMessage, &r.response, &r.toolCalls, &r.signals); err != nil {
		t.Fatalf("read chat row %s: %v", id, err)
	}
	return r
}

type msgRow414 struct {
	content, author string
	metadata        []byte
}

func readMsg414(t *testing.T, db *sql.DB, id string) msgRow414 {
	t.Helper()
	var r msgRow414
	var author sql.NullString
	if err := db.QueryRow(`SELECT content, author_id, metadata FROM task_messages WHERE id = $1`, id).
		Scan(&r.content, &author, &r.metadata); err != nil {
		t.Fatalf("read message %s: %v", id, err)
	}
	r.author = author.String
	return r
}

func noFragment414(t *testing.T, where, stored, secret string) {
	t.Helper()
	for i := 0; i+10 <= len(secret); i++ {
		if strings.Contains(stored, secret[i:i+10]) {
			t.Fatalf("%s still holds a fragment of the secret (%q): %s", where, secret[i:i+10], stored)
		}
	}
}

// runTranscriptRescanSuite seeds one dirty and one clean row in each store and
// checks the dry run, the strong apply and the all apply.
func runTranscriptRescanSuite(t *testing.T, rig transcriptRig, project string) {
	ctx := context.Background()
	db := rig.env.db
	now := rig.env.bindTime(time.Now().UTC())
	det := det414(t)

	dirtyChat, cleanChat := project+"-chat-dirty", project+"-chat-clean"
	signals := `[{"claim_value":"https://x.test/` + gh414[:20] + `…","detail":"d","sentence":"plain words"},` +
		`{"claim_value":"https://docs.example.test/` + cutRun414 + `…","detail":"d","sentence":"more words"}]`
	for _, row := range []struct{ id, chatID, userID, response, signals string }{
		{dirtyChat, "api:client " + aws414, "email:" + aws414 + "@x.test", "reply " + gh414 + " done", signals},
		{cleanChat, "telegram:-1001234567890", "", "an ordinary answer", ""},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO chat_audit_log
			(id, ts, chat_id, user_id, project_id, user_message, response, tool_calls_json, hallucination_signals_json)
			VALUES ($1, $2, $3, $4, $5, '', $6, '[]', $7)`,
			row.id, now, row.chatID, row.userID, project, row.response, row.signals); err != nil {
			t.Fatalf("seed chat row: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM chat_audit_log WHERE project_id = $1`, project)
	})

	taskID := project + "-task"
	rig.createTask(t, taskID, project)
	dirtyMsg, cleanMsg := project+"-msg-dirty", project+"-msg-clean"
	meta := `{"kind":"note","auth":{"password":"` + pw414 + `"},"source_set_hash":"` + hash414 + `"}`
	for _, m := range []struct {
		id, content string
		author      any
		meta        string
	}{
		{dirtyMsg, "deploy with " + aws414, "ops-" + gh414, meta},
		{cleanMsg, "hello", nil, `{"kind":"note"}`},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO task_messages
			(id, task_id, author_kind, author_id, message_kind, content, metadata, created_at)
			VALUES ($1, $2, 'operator', $3, 'message', $4, $5, $6)`,
			m.id, taskID, m.author, m.content, rig.env.bindJSON([]byte(m.meta)), now); err != nil {
			t.Fatalf("seed message: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM task_messages WHERE task_id = $1`, taskID)
		_, _ = db.Exec(`DELETE FROM secret_redaction_audit WHERE task_id = $1`, taskID)
	})

	beforeChat, beforeMsg := readChat414(t, db, dirtyChat), readMsg414(t, db, dirtyMsg)
	cleanChatBefore, cleanMsgBefore := readChat414(t, db, cleanChat), readMsg414(t, db, cleanMsg)

	// Dry run: the denominator, and not one byte changed.
	chatRes, err := scanChatAuditHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
	if err != nil {
		t.Fatalf("chat dry run: %v", err)
	}
	msgRes, err := scanTaskMessagesHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
	if err != nil {
		t.Fatalf("task_messages dry run: %v", err)
	}
	if chatRes.RowsScanned != 2 || chatRes.RowsMatched != 1 {
		t.Errorf("chat_audit_log examined/matched = %d/%d, want 2/1", chatRes.RowsScanned, chatRes.RowsMatched)
	}
	if msgRes.RowsScanned != 2 || msgRes.RowsMatched != 1 {
		t.Errorf("task_messages examined/matched = %d/%d, want 2/1", msgRes.RowsScanned, msgRes.RowsMatched)
	}
	for col, want := range map[string][2]int{
		"chat_id": {2, 1}, "user_id": {1, 1}, "response": {2, 1}, "user_message": {0, 0},
		"tool_calls_json": {0, 0}, "hallucination_signals_json": {1, 1},
	} {
		if got := [2]int{chatRes.ExaminedByColumn[col], chatRes.MatchedByColumn[col]}; got != want {
			t.Errorf("chat_audit_log %s examined/matched = %v, want %v", col, got, want)
		}
	}
	for col, want := range map[string][2]int{"content": {2, 1}, "author_id": {1, 1}, "metadata": {2, 1}} {
		if got := [2]int{msgRes.ExaminedByColumn[col], msgRes.MatchedByColumn[col]}; got != want {
			t.Errorf("task_messages %s examined/matched = %v, want %v", col, got, want)
		}
	}
	if chatRes.ExcludedByType["cut_token"] == 0 {
		t.Errorf("the cut-edge run must be counted as found but not selected: %v", chatRes.ExcludedByType)
	}
	if readChat414(t, db, dirtyChat) != beforeChat {
		t.Fatal("a dry run changed chat_audit_log")
	}
	if got := readMsg414(t, db, dirtyMsg); got.content != beforeMsg.content || got.author != beforeMsg.author ||
		string(got.metadata) != string(beforeMsg.metadata) {
		t.Fatal("a dry run changed task_messages")
	}

	// Scope: another project examines none of these rows.
	other, err := scanTaskMessagesHistory(ctx, rig.env, det, project+"-other", time.Time{}, false, strong414(t), 0)
	if err != nil || other.RowsScanned != 0 {
		t.Fatalf("--project scoping: examined %d, err %v", other.RowsScanned, err)
	}

	// Apply, strong.
	if _, err := scanChatAuditHistory(ctx, rig.env, det, project, time.Time{}, true, strong414(t), 0); err != nil {
		t.Fatalf("chat apply: %v", err)
	}
	if _, err := scanTaskMessagesHistory(ctx, rig.env, det, project, time.Time{}, true, strong414(t), 0); err != nil {
		t.Fatalf("task_messages apply: %v", err)
	}
	chat := readChat414(t, db, dirtyChat)
	noFragment414(t, "chat_id", chat.chatID, aws414)
	noFragment414(t, "user_id", chat.userID, aws414)
	noFragment414(t, "response", chat.response, gh414)
	noFragment414(t, "hallucination_signals_json", chat.signals, gh414)
	if !strings.HasPrefix(chat.chatID, "api:client ") || !strings.HasPrefix(chat.userID, "email:") {
		t.Errorf("identity prefixes must survive: %q / %q", chat.chatID, chat.userID)
	}
	if !strings.Contains(chat.signals, cutRun414) {
		t.Errorf("under --rules strong the cut-edge run is kept: %s", chat.signals)
	}
	var sigs []map[string]any
	if err := json.Unmarshal([]byte(chat.signals), &sigs); err != nil || len(sigs) != 2 {
		t.Fatalf("signals must stay a two-element JSON array: %v %s", err, chat.signals)
	}
	if readChat414(t, db, cleanChat) != cleanChatBefore {
		t.Error("a clean chat row must be left byte-identical")
	}

	msg := readMsg414(t, db, dirtyMsg)
	noFragment414(t, "content", msg.content, aws414)
	noFragment414(t, "author_id", msg.author, gh414)
	noFragment414(t, "metadata", string(msg.metadata), pw414)
	var m map[string]any
	if err := json.Unmarshal(msg.metadata, &m); err != nil {
		t.Fatalf("metadata must still decode: %v (%s)", err, msg.metadata)
	}
	if m["source_set_hash"] != hash414 || m["kind"] != "note" {
		t.Errorf("machine-read metadata must survive: %v", m)
	}
	if got := readMsg414(t, db, cleanMsg); got.content != cleanMsgBefore.content ||
		string(got.metadata) != string(cleanMsgBefore.metadata) || got.author != "" {
		t.Error("a clean message must be left byte-identical")
	}
	var events int
	if err := db.QueryRow(`SELECT COALESCE(SUM(count), 0) FROM secret_redaction_audit
		WHERE task_id = $1 AND source = 'scan'`, taskID).Scan(&events); err != nil || events < 3 {
		t.Errorf("task_messages redactions must be recorded per task: %d (%v)", events, err)
	}

	// A second strong pass finds the cut-edge run and nothing it would write.
	again, err := scanTaskMessagesHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
	if err != nil || again.RowsMatched != 0 {
		t.Errorf("a re-run over redacted messages must match nothing: %d (%v)", again.RowsMatched, err)
	}

	// Apply, all: the cut-edge run goes too.
	if _, err := scanChatAuditHistory(ctx, rig.env, det, project, time.Time{}, true, ruleSelection{spec: ruleSpecAll, all: true}, 0); err != nil {
		t.Fatalf("chat apply all: %v", err)
	}
	if chat := readChat414(t, db, dirtyChat); strings.Contains(chat.signals, cutRun414) {
		t.Errorf("under --rules all the cut-edge run is replaced: %s", chat.signals)
	}

	runStepOutcomeRescan425(t, rig, project)

	if rig.orphans {
		orphan := project + "-msg-orphan"
		if _, err := db.ExecContext(ctx, `INSERT INTO task_messages
			(id, task_id, author_kind, message_kind, content, created_at)
			VALUES ($1, $2, 'operator', 'message', $3, $4)`, orphan, project+"-gone", "key "+aws414, now); err != nil {
			t.Fatalf("seed orphan: %v", err)
		}
		t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM task_messages WHERE id = $1`, orphan) })
		all, err := scanTaskMessagesHistory(ctx, rig.env, det, "", time.Time{}, false, strong414(t), 0)
		if err != nil || all.MatchedByColumn["content"] != 1 {
			t.Errorf("without --project an orphan message is examined: %v (%v)", all.MatchedByColumn, err)
		}
		scoped, err := scanTaskMessagesHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
		if err != nil || scoped.RowsMatched != 0 {
			t.Errorf("with --project an orphan is not in the project: %d (%v)", scoped.RowsMatched, err)
		}
	}
}

// newTask414 is the minimum task row a repository accepts.
func newTask414(id, project string) *persistence.Task {
	wf := "wf"
	now := time.Now().UTC()
	return &persistence.Task{ID: id, ProjectID: project, WorkflowID: &wf, Status: persistence.TaskStatusCompleted,
		Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now}
}

// runStepOutcomeRescan425 is GitHub #425: execution_step_outcomes.
// hallucination_signals had no scan-history target, so rows written before its
// write seam keep any credential they quote and the dry run cannot say how
// many signal values there are. Found at: review (#414's §10.2.5); observed at
// integration (the stored row). Design: chat-audit retention and redaction
// design §11.2.2. Runs inside the transcript suite, so on both drivers.
func runStepOutcomeRescan425(t *testing.T, rig transcriptRig, project string) {
	t.Helper()
	ctx := context.Background()
	db := rig.env.db
	now := rig.env.bindTime(time.Now().UTC())
	det := det414(t)

	dirty, clean, empty := project+"-out-dirty", project+"-out-clean", project+"-out-empty"
	dirtySignals := `[{"detector":"url_not_fetched","claim_value":"https://x.test/?k=` + gh414 + `","sentence":"see ` + aws414 + `"},` +
		`{"detector":"path_not_found","claim_value":"https://docs.example.test/` + cutRun414 + `…","sentence":"words"}]`
	for _, row := range []struct {
		id      string
		signals any
	}{
		{dirty, rig.env.bindJSON([]byte(dirtySignals))},
		{clean, rig.env.bindJSON([]byte(`[{"detector":"url_not_fetched","claim_value":"https://example.test/a","sentence":"plain"}]`))},
		{empty, nil},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO execution_step_outcomes
			(id, project_id, task_id, execution_id, step_id, role, model, outcome, recorded_at, hallucination_signals)
			VALUES ($1, $2, $3, $4, 'write', 'writer', 'm', 'ok', $5, $6)`,
			row.id, project, project+"-task", project+"-exec", now, row.signals); err != nil {
			t.Fatalf("seed outcome row: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM execution_step_outcomes WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM secret_redaction_audit WHERE task_id = $1`, project+"-task")
	})

	read := func(id string) string {
		t.Helper()
		var b []byte
		if err := db.QueryRow(`SELECT hallucination_signals FROM execution_step_outcomes WHERE id = $1`, id).Scan(&b); err != nil {
			t.Fatalf("read outcome %s: %v", id, err)
		}
		return string(b)
	}
	before, cleanBefore := read(dirty), read(clean)

	res, err := scanStepOutcomesHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
	if err != nil {
		t.Fatalf("step outcome dry run: %v", err)
	}
	if res.RowsScanned != 3 || res.RowsMatched != 1 {
		t.Errorf("execution_step_outcomes examined/matched = %d/%d, want 3/1", res.RowsScanned, res.RowsMatched)
	}
	if got := [2]int{res.ExaminedByColumn["hallucination_signals"], res.MatchedByColumn["hallucination_signals"]}; got != [2]int{2, 1} {
		t.Errorf("hallucination_signals examined/matched = %v, want [2 1]", got)
	}
	if res.ExcludedByType["cut_token"] == 0 {
		t.Errorf("the cut-edge run is found but not selected under strong: %v", res.ExcludedByType)
	}
	if read(dirty) != before {
		t.Fatal("a dry run changed execution_step_outcomes")
	}
	other, err := scanStepOutcomesHistory(ctx, rig.env, det, project+"-other", time.Time{}, false, strong414(t), 0)
	if err != nil || other.RowsScanned != 0 {
		t.Fatalf("--project scoping: examined %d, err %v", other.RowsScanned, err)
	}

	if _, err := scanStepOutcomesHistory(ctx, rig.env, det, project, time.Time{}, true, strong414(t), 0); err != nil {
		t.Fatalf("step outcome apply: %v", err)
	}
	got := read(dirty)
	noFragment414(t, "hallucination_signals", got, gh414)
	noFragment414(t, "hallucination_signals", got, aws414)
	if !strings.Contains(got, cutRun414) {
		t.Errorf("under --rules strong the cut-edge run is kept: %s", got)
	}
	var sigs []map[string]any
	if err := json.Unmarshal([]byte(got), &sigs); err != nil || len(sigs) != 2 {
		t.Fatalf("signals must stay a two-element JSON array: %v %s", err, got)
	}
	if sigs[0]["detector"] != "url_not_fetched" && sigs[1]["detector"] != "url_not_fetched" {
		t.Errorf("the signals' other fields must survive: %v", sigs)
	}
	if read(clean) != cleanBefore {
		t.Error("a clean outcome row must be left byte-identical")
	}
	var events int
	if err := db.QueryRow(`SELECT COALESCE(SUM(count), 0) FROM secret_redaction_audit
		WHERE task_id = $1 AND source = 'scan'`, project+"-task").Scan(&events); err != nil || events < 2 {
		t.Errorf("step outcome redactions must be recorded per task: %d (%v)", events, err)
	}

	// A second strong pass selects nothing: what is left is the kept run.
	again, err := scanStepOutcomesHistory(ctx, rig.env, det, project, time.Time{}, false, strong414(t), 0)
	if err != nil || len(again.SelectedByType) != 0 {
		t.Errorf("a re-run over redacted signals selects nothing: %v (%v)", again.SelectedByType, err)
	}

	if _, err := scanStepOutcomesHistory(ctx, rig.env, det, project, time.Time{}, true, ruleSelection{spec: ruleSpecAll, all: true}, 0); err != nil {
		t.Fatalf("step outcome apply all: %v", err)
	}
	if got := read(dirty); strings.Contains(got, cutRun414) {
		t.Errorf("under --rules all the cut-edge run is replaced: %s", got)
	}
}
