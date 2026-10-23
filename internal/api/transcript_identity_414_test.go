package api

// GitHub #414 (2) — caller-controlled identity fields were stored unscanned
// and unbounded: chat_audit_log.chat_id ("api:" + the raw User-Agent) and
// task_messages.author_id (the REST authorId). Found at: review (#395's
// §9.2.7). A user observes it at integration: the stored row holds the
// credential. Each test drives the REAL writer through the decorator the
// container installs into REAL SQLite and reads the stored bytes back.
// Design: 2026-09-04-chat-audit-retention-and-redaction-design.md §10.2.3.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
)

func proxyAuditChatID414(t *testing.T, userAgent string) string {
	t.Helper()
	db := sqlitetest.Memory(t)
	s := &Server{logger: zerolog.Nop(),
		chatAuditRepo: auditredact.NewChatAudit(sqlite.NewChatAuditRepository(db.DB), detector395(t), nil, nil)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("User-Agent", userAgent)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m",
		[]chat.Message{{Role: "user", Content: "hi"}}, oneChoice395("hello"), 0)
	var chatID string
	require.NoError(t, db.DB.QueryRow(`SELECT chat_id FROM chat_audit_log`).Scan(&chatID))
	return chatID
}

// A User-Agent carrying a key, a control byte and invalid UTF-8.
func TestChatProxyAudit414_UserAgentIsBoundedAndScanned(t *testing.T) {
	chatID := proxyAuditChatID414(t, "evil-client/1.0 \x01 key="+aws395+" \xff\xfe tail")
	assertNoFragment395(t, "chat_id", chatID, aws395)
	require.True(t, utf8.ValidString(chatID), "chat_id must be valid UTF-8: %q", chatID)
	require.NotContains(t, chatID, "\x01")
	require.True(t, strings.HasPrefix(chatID, "api:evil-client/1.0"), "the product token survives: %q", chatID)
}

// A User-Agent longer than the bound is capped, and the cap drops a straddling
// secret-alphabet run whole; one that is nothing but such a run bounds to
// empty and is stored as anonymous (review a68b F8).
func TestChatProxyAudit414_UserAgentIsCapped(t *testing.T) {
	long := proxyAuditChatID414(t, "client/2 "+strings.Repeat("word ", 60))
	require.LessOrEqual(t, len(long), len("api:")+auditredact.IdentityLimit)

	blob := proxyAuditChatID414(t, strings.Repeat("Ab9", 70))
	require.Equal(t, "api:anonymous", blob)
}

// failingChatAudit fails every Insert, so the proxy's failure line runs.
type failingChatAudit struct {
	persistence.ChatAuditRepository
}

func (failingChatAudit) Insert(context.Context, *persistence.ChatAuditEntry) error {
	return errors.New("boom")
}

// The proxy's insert-failure log line held the raw User-Agent (review a68b F1).
func TestChatProxyAudit414_InsertFailureLogCarriesNoUserAgent(t *testing.T) {
	var buf bytes.Buffer
	db := sqlitetest.Memory(t)
	inner := failingChatAudit{sqlite.NewChatAuditRepository(db.DB)}
	s := &Server{logger: zerolog.New(&buf),
		chatAuditRepo: auditredact.NewChatAudit(inner, detector395(t), nil, nil)}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.Header.Set("User-Agent", "client "+aws395)
	s.recordChatAPIAudit(context.Background(), r, time.Now(), "m",
		[]chat.Message{{Role: "user", Content: "hi"}}, oneChoice395("hello"), 0)
	require.Contains(t, buf.String(), "Insert failed", "the failure line ran")
	assertNoFragment395(t, "log", buf.String(), aws395)
}

// storedAuthor414 returns where the client's authorId is stored. Since
// GitHub #432 that is the metadata label, never author_id (which must stay
// empty here: these requests carry no principal); the #414 bound and scan
// still apply to the label.
func storedAuthor414(t *testing.T, db *sql.DB) string {
	t.Helper()
	var author, meta sql.NullString
	require.NoError(t, db.QueryRow(`SELECT author_id, metadata FROM task_messages ORDER BY created_at DESC LIMIT 1`).Scan(&author, &meta))
	require.Empty(t, author.String, "#432: a client authorId is never the author of record")
	var m map[string]string
	require.NoError(t, json.Unmarshal([]byte(meta.String), &m), meta.String)
	return m[ClientSuppliedAuthorKey]
}

// The REST authorId was stored verbatim.
func TestTaskMessages414_AuthorIDIsScanned(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	body := `{"kind":"message","content":"hello","authorId":"ops-` + aws395 + `"}`
	rec := httptest.NewRecorder()
	rig.srv.PostTaskMessage(rec, httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(body)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	author := storedAuthor414(t, rig.db)
	assertNoFragment395(t, "author_id", author, aws395)
	require.True(t, strings.HasPrefix(author, "ops-"), "the handle around the finding survives: %q", author)
}

func TestTaskMessages414_AuthorIDIsBounded(t *testing.T) {
	rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
	body := `{"kind":"message","content":"hello","authorId":"` + strings.Repeat("name ", 60) + `"}`
	rec := httptest.NewRecorder()
	rig.srv.PostTaskMessage(rec, httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(body)))
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.LessOrEqual(t, len(storedAuthor414(t, rig.db)), auditredact.IdentityLimit)
}
