package api

// GitHub #395 — transcript secret scanning had gaps at write time.
// Found at: review (the #344 security follow-up, Art 17 redact design
// §15.11.4a). Observed at: integration — rows persisted holding a credential.
// Each test drives a REAL writer (the chat-proxy audit writer, the REST task
// message handlers) through the decorator the container installs into a REAL
// SQLite table, then reads the stored bytes back.
// Design: 2026-09-04-chat-audit-retention-and-redaction-design.md §9.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/taintlineage"
)

// Synthetic credentials of the right shape; never real.
const (
	gh395  = "ghp_R7mK2pX9vQ4tL8wZ3nB6cJ1hF5yD0sGaE2uT"
	aws395 = "AKIAQ3EGUZJ7RLW4XN2P"
	pw395  = "correct-horse-battery-staple-395"
)

func detector395(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{
		Patterns:  secrets.EffectivePatterns(nil, nil),
		Allowlist: secrets.DefaultAllowlist(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, d.Scan([]byte("x "+gh395+" y")), "fixture must match github_pat")
	require.NotEmpty(t, d.Scan([]byte("x "+aws395+" y")), "fixture must match aws_access_key")
	return d
}

func assertNoFragment395(t *testing.T, field, stored, secret string) {
	t.Helper()
	for i := 0; i+10 <= len(secret); i++ {
		if strings.Contains(stored, secret[i:i+10]) {
			t.Fatalf("%s stores a fragment of the secret (%q):\n%s", field, secret[i:i+10], stored)
		}
	}
}

func oneChoice395(text string) *chat.ChatResponse {
	resp := &chat.ChatResponse{Model: "m"}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Role: "assistant", Content: text}})
	return resp
}

// The proxy writer cut user_message and response at 500 bytes BEFORE the
// decorator scanned them, with a byte slice that could split a rune.
func TestChatProxyAudit395_SecretAtTheCutAndASplitRune(t *testing.T) {
	db := sqlitetest.Memory(t)
	s := &Server{logger: zerolog.Nop(),
		chatAuditRepo: auditredact.NewChatAudit(sqlite.NewChatAuditRepository(db.DB), detector395(t), nil, nil)}
	// The token starts before the stored limit and ends after it.
	user := strings.Repeat("u", 469) + " " + gh395 + " rest"
	// A 4-byte rune spans byte 486, where the old cut fell (500 minus the
	// 14-byte "…(truncated)" suffix).
	reply := strings.Repeat("r", 484) + "😀" + strings.Repeat("z", 100)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m",
		[]chat.Message{{Role: "user", Content: user}}, oneChoice395(reply), 0)

	var um, resp string
	require.NoError(t, db.DB.QueryRow(`SELECT user_message, response FROM chat_audit_log`).Scan(&um, &resp))
	assertNoFragment395(t, "user_message", um, gh395)
	require.True(t, utf8.ValidString(resp), "the cut must not split a rune: %q", resp[len(resp)-20:])
	require.LessOrEqual(t, len(um), 500)
	require.LessOrEqual(t, len(resp), 500)
}

// taskMessagesRig395 is the REST server over real SQLite task and task-message
// stores, with the task-message store behind the decorator the container wires.
type taskMessagesRig395 struct {
	srv  *Server
	db   *sql.DB
	repo persistence.TaskMessageRepository
}

func newTaskMessagesRig395(t *testing.T, status persistence.TaskStatus) taskMessagesRig395 {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	require.NoError(t, tasks.Create(context.Background(), &persistence.Task{
		ID: tcTaskID, ProjectID: tcProjectID, Status: status, MaxAttempts: 3,
		Payload: []byte(`{}`),
	}))
	repo := auditredact.NewTaskMessages(sqlite.NewTaskMessageRepository(db.DB), detector395(t), nil, nil)
	return taskMessagesRig395{srv: &Server{logger: zerolog.Nop(), taskRepo: tasks, taskMessageRepo: repo}, db: db.DB, repo: repo}
}

func (r taskMessagesRig395) stored(t *testing.T, kind string) (content, metadata string) {
	t.Helper()
	var meta sql.NullString
	require.NoError(t, r.db.QueryRow(`SELECT content, metadata FROM task_messages WHERE message_kind = ? ORDER BY created_at DESC LIMIT 1`, kind).
		Scan(&content, &meta))
	return content, meta.String
}

// The REST writer stored caller metadata verbatim and nothing scanned content.
func TestTaskMessages395_PostTaskMessageScansContentAndMetadata(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	body := `{"kind":"message","content":"deploy with ` + gh395 + ` please",` +
		`"metadata":{"password":"` + pw395 + `","note":"key ` + aws395 + `","nested":{"apiKey":"` + pw395 + `"},"reason":"keep me"}}`
	rec := httptest.NewRecorder()
	rig.srv.PostTaskMessage(rec, httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(body)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	content, meta := rig.stored(t, persistence.TaskMessageKindMessage)
	assertNoFragment395(t, "content", content, gh395)
	assertNoFragment395(t, "metadata", meta, pw395)
	assertNoFragment395(t, "metadata", meta, aws395)
	require.Contains(t, meta, "keep me", "an innocuous value survives")
	require.Contains(t, content, "deploy with")
}

// AnswerCheckpoint merges every caller metadata key with the choice.
func TestTaskMessages395_AnswerCheckpointScansCallerMetadata(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusAwaitingInput)
	cp := &persistence.TaskMessage{TaskID: tcTaskID, AuthorKind: persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "which?",
		Metadata: []byte(`{"kind":"decision","options":[{"id":"a","label":"A"}]}`), CreatedAt: time.Now().UTC()}
	require.NoError(t, rig.repo.Insert(context.Background(), cp))

	body := `{"content":"go with a","choice":"a","metadata":{"token":"` + pw395 + `","comment":"use ` + gh395 + `"}}`
	rec := httptest.NewRecorder()
	rig.srv.AnswerCheckpoint(rec, httptest.NewRequest(http.MethodPost, tcURL("/messages/"+cp.ID+"/answer"), strings.NewReader(body)))
	require.Less(t, rec.Code, 300, rec.Body.String())

	_, meta := rig.stored(t, persistence.TaskMessageKindAnswer)
	assertNoFragment395(t, "metadata", meta, pw395)
	assertNoFragment395(t, "metadata", meta, gh395)
	require.Contains(t, meta, `"choice":"a"`, "the choice a reader decodes survives")
}

// A taint-park style marker: a source URL carrying a key is redacted, while the
// hex source_set_hash the latch reads survives byte-identical.
func TestTaskMessages395_TaintLatchHashSurvivesWhileASourceKeyIsRedacted(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	hash := strings.Repeat("0123456789abcdef", 4)
	meta := `{"kind":"` + taintlineage.LatchMarkerKind + `","source_set_hash":"` + hash +
		`","sources":["https://api.example.test/q?api_key=` + pw395 + `","https://x.test/` + gh395 + `"]}`
	msg := &persistence.TaskMessage{TaskID: tcTaskID, AuthorKind: persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindSystem, Content: "latched", Metadata: []byte(meta), CreatedAt: time.Now().UTC()}
	require.NoError(t, rig.repo.Insert(context.Background(), msg))

	_, stored := rig.stored(t, persistence.TaskMessageKindSystem)
	assertNoFragment395(t, "metadata", stored, gh395)
	assertNoFragment395(t, "metadata", stored, pw395)
	got, ok := taintlineage.ParseLatchHash([]byte(stored))
	require.True(t, ok, "the latch still parses: %s", stored)
	require.Equal(t, hash, got)
}

// Metadata with nothing to redact is stored byte-identical (no re-encoding).
func TestTaskMessages395_CleanMetadataIsStoredUntouched(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	meta := `{"reason":"moved",   "from":"RUNNING","n":12345678901234567890}`
	msg := &persistence.TaskMessage{TaskID: tcTaskID, AuthorKind: persistence.TaskMessageAuthorSystem,
		MessageKind: persistence.TaskMessageKindNote, Content: "fine", Metadata: []byte(meta), CreatedAt: time.Now().UTC()}
	require.NoError(t, rig.repo.Insert(context.Background(), msg))
	_, stored := rig.stored(t, persistence.TaskMessageKindNote)
	require.Equal(t, meta, stored)
}

// The REST 64 KiB cap runs BEFORE the seam: it must not hand the seam the head
// of a credential whose tail it cut, nor split a rune.
func TestTaskMessages395_RestCapDropsAStraddlingToken(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	content := strings.Repeat("w", maxMessageContentBytes-21) + " " + gh395 + " tail"
	body := `{"kind":"message","content":"` + content + `"}`
	rec := httptest.NewRecorder()
	rig.srv.PostTaskMessage(rec, httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(body)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	stored, _ := rig.stored(t, persistence.TaskMessageKindMessage)
	assertNoFragment395(t, "content", stored, gh395)
	require.LessOrEqual(t, len(stored), maxMessageContentBytes)
	require.True(t, utf8.ValidString(stored))
}
