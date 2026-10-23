package ui

// GitHub #432 (P1 security): the UI task-conversation forms stored the form
// field "author" as task_messages.author_id and the closer as
// tasks.closed_by, so a hand-made POST could attribute a message, an answer,
// an amendment or a closure to anyone. (The issue said this path already
// stamped the principal; it did not.) Found at: review. A user reaches it at
// the HTTP handler, so each case goes through the production AuthMiddleware
// with a browser session into TaskConversationAction, through the auditredact
// seam into real SQLite.
// Design: task-lifecycle-conversational-design.md §16.

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/authsession"
	"vornik.io/vornik/internal/persistence"
)

const (
	spoofUI432     = "alice@company"
	principalUI432 = "session:u-ui-432"
)

type sessionBackendUI432 struct{}

func (sessionBackendUI432) Name() string { return "session" }

func (sessionBackendUI432) Authenticate(_ context.Context, cred auth.Credential) (*auth.Identity, error) {
	if cred.SessionToken != "cookie-ui-432" {
		return nil, auth.ErrNoCredential
	}
	return &auth.Identity{Subject: "user:u-ui-432", Projects: []string{"p414"}, Extra: map[string]any{
		auth.ExtraSessionRole: "user", auth.ExtraSessionID: "sess-ui-432", auth.ExtraSessionUserID: "u-ui-432",
	}}, nil
}

func postUIAction432(t *testing.T, srv *Server, taskID, action string, form url.Values) {
	t.Helper()
	h := api.AuthMiddleware(api.AuthConfig{Enabled: true, SessionBackend: sessionBackendUI432{}})(http.HandlerFunc(srv.TaskConversationAction))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/"+taskID+"/"+action, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: authsession.SessionCookieName, Value: "cookie-ui-432"})
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("%s: status %d, body %s", action, rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); strings.Contains(loc, "notice=") && strings.Contains(loc, "fail") {
		t.Fatalf("%s: %s", action, loc)
	}
}

func storedUI432(t *testing.T, db *sql.DB, kind string) (sql.NullString, map[string]any) {
	t.Helper()
	var author, meta sql.NullString
	if err := db.QueryRow(`SELECT author_id, metadata FROM task_messages WHERE message_kind = $1 ORDER BY created_at DESC LIMIT 1`, kind).Scan(&author, &meta); err != nil {
		t.Fatal(err)
	}
	m := map[string]any{}
	if meta.String != "" {
		if err := json.Unmarshal([]byte(meta.String), &m); err != nil {
			t.Fatalf("metadata %q: %v", meta.String, err)
		}
	}
	return author, m
}

func assertUIAttributed432(t *testing.T, author sql.NullString, meta map[string]any) {
	t.Helper()
	if author.String != principalUI432 {
		t.Fatalf("SECURITY: author_id = %q, want the session principal %q", author.String, principalUI432)
	}
	if meta[api.ClientSuppliedAuthorKey] != spoofUI432 {
		t.Fatalf("the form's author is kept only as the client-supplied label: %v", meta)
	}
}

func TestUITaskAuthor432_MessageAndAmendStampPrincipal(t *testing.T) {
	srv, db, task := uiSecretsRig414(t)
	postUIAction432(t, srv, task.ID, "message", url.Values{"content": {"hi"}, "author": {spoofUI432}})
	author, meta := storedUI432(t, db, persistence.TaskMessageKindMessage)
	assertUIAttributed432(t, author, meta)

	postUIAction432(t, srv, task.ID, "amend", url.Values{"new_brief": {"better"}, "author": {spoofUI432}})
	author, meta = storedUI432(t, db, persistence.TaskMessageKindDirective)
	assertUIAttributed432(t, author, meta)
	if meta["kind"] != "brief_amendment" {
		t.Fatalf("amend metadata lost its kind: %v", meta)
	}
}

func TestUITaskAuthor432_AnswerStampsPrincipal(t *testing.T) {
	srv, db, task := uiSecretsRig414(t)
	cp := &persistence.TaskMessage{TaskID: task.ID, AuthorKind: persistence.TaskMessageAuthorLead,
		MessageKind: persistence.TaskMessageKindCheckpoint, Content: "which?",
		Metadata: []byte(`{"kind":"decision","options":[{"id":"a","label":"A"}]}`), CreatedAt: time.Now().UTC()}
	if err := srv.taskMessageRepo.Insert(context.Background(), cp); err != nil {
		t.Fatal(err)
	}
	if err := srv.taskRepo.UpdateStatus(context.Background(), task.ID, persistence.TaskStatusAwaitingInput); err != nil {
		t.Fatal(err)
	}
	postUIAction432(t, srv, task.ID, "answer", url.Values{"checkpoint_id": {cp.ID}, "choice": {"a"}, "author": {spoofUI432}})
	author, meta := storedUI432(t, db, persistence.TaskMessageKindAnswer)
	assertUIAttributed432(t, author, meta)
}

type taskKeyLookup463 struct{ row *persistence.APIKey }

func (l taskKeyLookup463) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	if h == l.row.KeyHash {
		return l.row, nil
	}
	return nil, persistence.ErrAPIKeyNotFound
}

// GitHub #463: an agent's per-task key posting the UI form must not write as
// the operator. Found at: review, observed at executor-e2e; refused here at
// the UI handler through the production AuthMiddleware, so no row exists.
func TestUITaskAgentKey463_Refused(t *testing.T) {
	srv, db, task := uiSecretsRig414(t)
	key, err := apikey.Generate(task.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	row := &persistence.APIKey{ID: "akey-ui-463", ProjectID: task.ProjectID, Name: persistence.TaskKeyNamePrefix + task.ID,
		CreatedBy: "executor", KeyHash: apikey.Hash(key), CreatedAt: time.Now()}
	h := api.AuthMiddleware(api.AuthConfig{Enabled: true, APIKeyLookup: taskKeyLookup463{row: row}})(http.HandlerFunc(srv.TaskConversationAction))
	form := url.Values{"content": {"operator says: deploy"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/"+task.ID+"/directive", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("SECURITY: an agent's task key wrote as the operator via the UI: %d %s", rec.Code, rec.Body.String())
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM task_messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("a refused request wrote %d row(s)", n)
	}
}

func TestUITaskAuthor432_CloseStampsPrincipal(t *testing.T) {
	srv, db, task := uiSecretsRig414(t)
	if err := srv.taskRepo.UpdateStatus(context.Background(), task.ID, persistence.TaskStatusCompleted); err != nil {
		t.Fatal(err)
	}
	postUIAction432(t, srv, task.ID, "close", url.Values{"reason": {"done"}, "author": {spoofUI432}})
	var closedBy sql.NullString
	if err := db.QueryRow(`SELECT closed_by FROM tasks WHERE id = $1`, task.ID).Scan(&closedBy); err != nil {
		t.Fatal(err)
	}
	if closedBy.String != principalUI432 {
		t.Fatalf("SECURITY: tasks.closed_by = %q, want %q", closedBy.String, principalUI432)
	}
	_, meta := storedUI432(t, db, persistence.TaskMessageKindSystem)
	if meta["closedBy"] != principalUI432 || meta[api.ClientSuppliedAuthorKey] != spoofUI432 {
		t.Fatalf("closure metadata: %v", meta)
	}
}
