package api

// GitHub #432 (P1 security): the task-conversation handlers stored the
// client's authorId as task_messages.author_id and the closer as
// tasks.closed_by, so any project-scoped key could attribute a message, an
// answer, an amendment, a summary or a closure to any named human. #414 only
// bounded and scanned that string. Found at: review (security audit). A user
// or attacker reaches it at the HTTP handler, so each test sends the request
// through the production middleware (AuthMiddleware with a DB-key lookup,
// then ProjectAuthMiddleware, then apiV1ProjectsHandler) into real SQLite
// through the auditredact seam, and reads the stored row back.
// Design: task-lifecycle-conversational-design.md §17.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

const (
	spoof432     = "alice@company"
	keyRowID432  = "akey-432"
	principal432 = "api_key_id:" + keyRowID432
)

type authorRig432 struct {
	taskMessagesRig395
	chain  http.Handler
	bearer string
	key    string // the DB key, presented or not
}

// newAuthorRig432 wires the production middleware in front of the real
// project router. authOn=false is the auth-disabled branch with no
// credential presented.
func newAuthorRig432(t *testing.T, status persistence.TaskStatus, authOn bool) authorRig432 {
	t.Helper()
	rig := newTaskMessagesRig395(t, status)
	key, err := apikey.Generate(tcProjectID)
	require.NoError(t, err)
	lookup := &multiKeyLookup{byHash: map[string]*persistence.APIKey{
		apikey.Hash(key): {ID: keyRowID432, ProjectID: tcProjectID, KeyHash: apikey.Hash(key), CreatedAt: time.Now()},
	}}
	var h http.Handler = http.HandlerFunc(rig.srv.apiV1ProjectsHandler)
	h = ProjectAuthMiddleware()(h)
	h = AuthMiddleware(AuthConfig{Enabled: authOn, APIKeyLookup: lookup})(h)
	bearer := key
	if !authOn {
		bearer = ""
	}
	return authorRig432{taskMessagesRig395: rig, chain: h, bearer: bearer, key: key}
}

func (r authorRig432) post(t *testing.T, suffix, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, tcURL(suffix), strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if r.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+r.bearer)
	}
	rec := httptest.NewRecorder()
	r.chain.ServeHTTP(rec, req)
	return rec
}

// row returns the newest message of kind: its author_id and decoded metadata.
func (r authorRig432) row(t *testing.T, kind string) (sql.NullString, map[string]any) {
	t.Helper()
	var author, meta sql.NullString
	require.NoError(t, r.db.QueryRow(`SELECT author_id, metadata FROM task_messages WHERE message_kind = ? ORDER BY created_at DESC LIMIT 1`, kind).
		Scan(&author, &meta))
	m := map[string]any{}
	if meta.Valid && meta.String != "" {
		require.NoError(t, json.Unmarshal([]byte(meta.String), &m), meta.String)
	}
	return author, m
}

// assertAttributed432: the author of record is the key, and the client's
// value survives only as the client-supplied label.
func assertAttributed432(t *testing.T, author sql.NullString, meta map[string]any, want string) {
	t.Helper()
	require.True(t, author.Valid, "author_id must be set")
	require.Equal(t, want, author.String, "author_id must be the authenticated principal")
	require.NotContains(t, author.String, "alice", "SECURITY: the caller chose the author")
	require.Equal(t, spoof432, meta[ClientSuppliedAuthorKey], "the client's value is kept only as the client-supplied label: %v", meta)
}

func TestTaskAuthor432_PostTaskMessageStampsPrincipal(t *testing.T) {
	for _, kind := range []string{persistence.TaskMessageKindMessage, persistence.TaskMessageKindDirective} {
		t.Run(kind, func(t *testing.T) {
			rig := newAuthorRig432(t, persistence.TaskStatusRunning, true)
			rec := rig.post(t, "/messages", `{"kind":"`+kind+`","content":"hi","authorId":"`+spoof432+`","metadata":{"a":1}}`)
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			author, meta := rig.row(t, kind)
			assertAttributed432(t, author, meta, principal432)
			require.EqualValues(t, 1, meta["a"], "the caller's own metadata survives")
		})
	}
}

func TestTaskAuthor432_AnswerCheckpointStampsPrincipal(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusAwaitingInput, true)
	cp := &persistence.TaskMessage{TaskID: tcTaskID, AuthorKind: persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "which?",
		Metadata: []byte(`{"kind":"decision","options":[{"id":"a","label":"A"}]}`), CreatedAt: time.Now().UTC()}
	require.NoError(t, rig.repo.Insert(context.Background(), cp))
	// A caller that also plants the label key in its metadata does not get
	// to keep a second, different claim: authorId wins.
	rec := rig.post(t, "/messages/"+cp.ID+"/answer",
		`{"content":"a","choice":"a","authorId":"`+spoof432+`","metadata":{"`+ClientSuppliedAuthorKey+`":"bob"}}`)
	require.Less(t, rec.Code, 300, rec.Body.String())
	author, meta := rig.row(t, persistence.TaskMessageKindAnswer)
	assertAttributed432(t, author, meta, principal432)
}

func TestTaskAuthor432_AmendBriefStampsPrincipal(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusRunning, true)
	rec := rig.post(t, "/amend", `{"newBrief":"do it better","reason":"r","authorId":"`+spoof432+`"}`)
	require.Less(t, rec.Code, 300, rec.Body.String())
	author, meta := rig.row(t, persistence.TaskMessageKindDirective)
	assertAttributed432(t, author, meta, principal432)
	require.Equal(t, "brief_amendment", meta["kind"])
}

func TestTaskAuthor432_SummarizeThreadStampsPrincipal(t *testing.T) {
	// GitHub #431 (merged alongside #432): summarize is an agent-only call,
	// admitted only for the running task's own per-task key, so a project
	// key is refused before any author is chosen. The agent-key case and
	// its attribution are covered by the TestSummarize431_* tests.
	rig := newAuthorRig432(t, persistence.TaskStatusRunning, true)
	rec := rig.post(t, "/summarize", `{"messageIds":["m1"],"summary":"short","authorId":"`+spoof432+`"}`)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
}

func TestTaskAuthor432_CloseTaskStampsPrincipal(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusCompleted, true)
	rec := rig.post(t, "/close", `{"reason":"done","authorId":"`+spoof432+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, principal432, resp["closedBy"], "the response names the principal")

	var closedBy sql.NullString
	require.NoError(t, rig.db.QueryRow(`SELECT closed_by FROM tasks WHERE id = ?`, tcTaskID).Scan(&closedBy))
	require.Equal(t, principal432, closedBy.String, "tasks.closed_by (the CSV export column) is the principal")

	_, meta := rig.row(t, persistence.TaskMessageKindSystem)
	require.Equal(t, principal432, meta["closedBy"])
	require.Equal(t, spoof432, meta[ClientSuppliedAuthorKey])
}

// The label must be merged into metadata. Metadata that is not a JSON object
// cannot hold it, and storing it nowhere would drop the claim silently.
func TestTaskAuthor432_NonObjectMetadataWithAuthorIsRejected(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusRunning, true)
	rec := rig.post(t, "/messages", `{"kind":"message","content":"hi","authorId":"`+spoof432+`","metadata":[1,2]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	var n int
	require.NoError(t, rig.db.QueryRow(`SELECT COUNT(*) FROM task_messages`).Scan(&n))
	require.Zero(t, n, "a rejected request writes no row")
}

// Auth off and no credential: there is no principal, so the author of record
// keeps the handler's default, and the client value is still only a label.
func TestTaskAuthor432_AuthDisabledNoCredentialKeepsDefaults(t *testing.T) {
	t.Run("message", func(t *testing.T) {
		rig := newAuthorRig432(t, persistence.TaskStatusRunning, false)
		rec := rig.post(t, "/messages", `{"kind":"message","content":"hi","authorId":"`+spoof432+`"}`)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		author, meta := rig.row(t, persistence.TaskMessageKindMessage)
		require.False(t, author.Valid && author.String != "", "no principal: author_id stays empty, got %q", author.String)
		require.Equal(t, spoof432, meta[ClientSuppliedAuthorKey])
	})
	t.Run("amend", func(t *testing.T) {
		rig := newAuthorRig432(t, persistence.TaskStatusRunning, false)
		rec := rig.post(t, "/amend", `{"newBrief":"b","authorId":"`+spoof432+`"}`)
		require.Less(t, rec.Code, 300, rec.Body.String())
		author, meta := rig.row(t, persistence.TaskMessageKindDirective)
		require.False(t, author.Valid && author.String != "", "no principal: author_id stays empty, got %q", author.String)
		require.Equal(t, spoof432, meta[ClientSuppliedAuthorKey])
	})
	t.Run("answer", func(t *testing.T) {
		rig := newAuthorRig432(t, persistence.TaskStatusAwaitingInput, false)
		cp := &persistence.TaskMessage{TaskID: tcTaskID, AuthorKind: persistence.TaskMessageAuthorLead,
			MessageKind: persistence.TaskMessageKindCheckpoint, Content: "which?", CreatedAt: time.Now().UTC()}
		require.NoError(t, rig.repo.Insert(context.Background(), cp))
		rec := rig.post(t, "/messages/"+cp.ID+"/answer", `{"content":"a","authorId":"`+spoof432+`"}`)
		require.Less(t, rec.Code, 300, rec.Body.String())
		author, meta := rig.row(t, persistence.TaskMessageKindAnswer)
		require.False(t, author.Valid && author.String != "", "no principal: author_id stays empty, got %q", author.String)
		require.Equal(t, spoof432, meta[ClientSuppliedAuthorKey])
	})
	t.Run("summarize", func(t *testing.T) {
		// GitHub #431: without the running task's own key there is no
		// summarize, auth on or off.
		rig := newAuthorRig432(t, persistence.TaskStatusRunning, false)
		rec := rig.post(t, "/summarize", `{"messageIds":["m1"],"summary":"s","authorId":"`+spoof432+`"}`)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	})
	t.Run("close", func(t *testing.T) {
		rig := newAuthorRig432(t, persistence.TaskStatusCompleted, false)
		rec := rig.post(t, "/close", `{"authorId":"`+spoof432+`"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var closedBy sql.NullString
		require.NoError(t, rig.db.QueryRow(`SELECT closed_by FROM tasks WHERE id = ?`, tcTaskID).Scan(&closedBy))
		require.Equal(t, "operator", closedBy.String)
	})
}

// A browser session is attributed to its user, a static key to its hash
// prefix; neither to the client's value, and never to the bearer itself.
func TestTaskAuthor432_SessionAndStaticKeyPrincipals(t *testing.T) {
	t.Run("session", func(t *testing.T) {
		rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
		sb := &stubSessionBackend{token: "cookie-432", projects: []string{tcProjectID}, role: "user", sessID: "sess-432", userID: "u-432"}
		h := AuthMiddleware(AuthConfig{Enabled: true, SessionBackend: sb})(ProjectAuthMiddleware()(http.HandlerFunc(rig.srv.apiV1ProjectsHandler)))
		req := httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(`{"content":"hi","authorId":"`+spoof432+`"}`))
		req.AddCookie(&http.Cookie{Name: "vornik_session", Value: "cookie-432"})
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		author, meta := authorRig432{taskMessagesRig395: rig}.row(t, persistence.TaskMessageKindMessage)
		assertAttributed432(t, author, meta, "session:u-432")
	})
	t.Run("static key", func(t *testing.T) {
		rig := newTaskMessagesRig395(t, persistence.TaskStatusRunning)
		const static = "static-key-432-abcdefghijklmnop"
		h := AuthMiddleware(AuthConfig{Enabled: true, StaticAPIKeys: map[string][]string{static: {tcProjectID}}})(ProjectAuthMiddleware()(http.HandlerFunc(rig.srv.apiV1ProjectsHandler)))
		req := httptest.NewRequest(http.MethodPost, tcURL("/messages"), strings.NewReader(`{"content":"hi","authorId":"`+spoof432+`"}`))
		req.Header.Set("Authorization", "Bearer "+static)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		author, meta := authorRig432{taskMessagesRig395: rig}.row(t, persistence.TaskMessageKindMessage)
		assertAttributed432(t, author, meta, "api_key_sha256:"+apikey.Hash(static)[:16])
		require.NotContains(t, author.String, static, "SECURITY: the bearer is never stored")
	})
}

func TestWithClientAuthorLabel432(t *testing.T) {
	got, err := WithClientAuthorLabel(nil, "")
	require.NoError(t, err)
	require.Nil(t, got, "no label: metadata untouched")
	got, err = WithClientAuthorLabel([]byte(`[1]`), "")
	require.NoError(t, err)
	require.Equal(t, `[1]`, string(got), "no label: even non-object metadata is untouched")
	got, err = WithClientAuthorLabel([]byte(`null`), "x")
	require.NoError(t, err)
	require.JSONEq(t, `{"clientSuppliedAuthor":"x"}`, string(got))
	got, err = WithClientAuthorLabel([]byte(`{"clientSuppliedAuthor":"bob","k":1}`), "x")
	require.NoError(t, err)
	require.JSONEq(t, `{"clientSuppliedAuthor":"x","k":1}`, string(got), "the request's authorId overwrites a planted label")
	for _, bad := range []string{`[1]`, `"s"`, `3`, `{`} {
		_, err = WithClientAuthorLabel([]byte(bad), "x")
		require.ErrorIs(t, err, ErrClientAuthorMetadata, bad)
	}
}

// Auth off, but the caller presents a DB key: serveAuthDisabled still
// resolves it for attribution, so the key is the author of record.
func TestTaskAuthor432_AuthDisabledWithDBKeyStampsPrincipal(t *testing.T) {
	rig := newAuthorRig432(t, persistence.TaskStatusCompleted, false)
	rig.bearer = rig.key
	rec := rig.post(t, "/close", `{"authorId":"`+spoof432+`"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var closedBy sql.NullString
	require.NoError(t, rig.db.QueryRow(`SELECT closed_by FROM tasks WHERE id = ?`, tcTaskID).Scan(&closedBy))
	require.Equal(t, principal432, closedBy.String)
	rec = rig.post(t, "/messages", `{"content":"hi","authorId":"`+spoof432+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	author, meta := rig.row(t, persistence.TaskMessageKindMessage)
	assertAttributed432(t, author, meta, principal432)
}
