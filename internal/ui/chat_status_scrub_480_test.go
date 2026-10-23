package ui

// GitHub #480 (P1, security; operator decision A5): the UI web chat gave a
// non-admin UI caller the dispatcher tools get_task_status and wait_for_task,
// which handed the chat model a task's raw last_error and the execution's raw
// error_message, and so into its answer. Found at: review (the #466
// adversarial code review, open decision 1); a user reaches it by posting a
// chat message, so the test runs the real api.AuthMiddleware and project gate
// in front of the UI handler, a real dispatcher over real SQLite and a
// scripted model that calls both tools (process-spawn-law design,
// "Amendment 2026-10-21 (#480)").

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/report"
)

const adminKey480UI = "sk-admin-ui-480"

type webChat480 struct {
	handler    http.Handler
	model      *dispatchertest.StatusModel
	projectKey string
}

func newWebChat480(t *testing.T) *webChat480 {
	t.Helper()
	root := writeSwarmFixture(t)
	reg := registry.New()
	require.NoError(t, reg.Load(root))

	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	dispatchertest.SeedFailedTask480(t, tasks, execs, "p1")

	scrub, err := report.NewLineScrubber("")
	require.NoError(t, err)
	model := dispatchertest.NewStatusModel(t)
	agent := dispatcher.NewAgent(model.Provider(), tasks, execs, nil, reg, dispatcher.WithErrorTextScrubber(scrub))

	srv := NewServer(
		WithProjectRegistry(reg),
		WithOnboardingDetector(alreadyOnboardedDetector()),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey480UI}}),
	)
	srv.chatDispatcher = agent

	pk, err := apikey.Generate("p1")
	require.NoError(t, err)
	now := time.Now().UTC()
	lookup := keyLookup466{rows: map[string]*persistence.APIKey{
		apikey.Hash(pk): {ID: "akey-480", ProjectID: "p1", KeyHash: apikey.Hash(pk), CreatedAt: now},
	}}
	h := http.StripPrefix("/ui", srv.Handler())
	h = api.ProjectAuthMiddleware()(h)
	h = api.AuthMiddleware(api.AuthConfig{
		Enabled: true, APIKeyLookup: lookup,
		StaticAPIKeys: map[string][]string{adminKey480UI: nil},
	})(h)
	return &webChat480{handler: h, model: model, projectKey: pk}
}

// post sends one chat message as bearer with the chat cookie and returns the
// rendered page.
func (w *webChat480) post(t *testing.T, bearer, cookie string) string {
	t.Helper()
	form := url.Values{"prompt": {"why did the task fail?"}}
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/p1/chat/messages", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: cookie})
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	return rec.Body.String()
}

func TestWebChat480_NonAdminCallerGetsScrubbedStatus(t *testing.T) {
	w := newWebChat480(t)
	w.post(t, w.projectKey, "cookie-480-a")
	dispatchertest.AssertScrubbed(t, "web chat, project key", w.model.ToolResults())
}

func TestWebChat480_AdminClassCallerReadsRaw(t *testing.T) {
	w := newWebChat480(t)
	w.post(t, adminKey480UI, "cookie-480-b")
	dispatchertest.AssertRaw(t, "web chat, admin key", w.model.ToolResults())
}

// Design review round 1, finding 1: history is keyed by the browser's chat
// cookie, so an admin turn's raw tool results, replayed to the model on a
// later non-admin turn in the same browser, would reach the non-admin. The
// two views keep separate histories.
func TestWebChat480_AdminHistoryIsNotReplayedToNonAdmin(t *testing.T) {
	w := newWebChat480(t)
	w.post(t, adminKey480UI, "cookie-480-c")
	dispatchertest.AssertRaw(t, "the admin turn", w.model.ToolResults())

	body := w.post(t, w.projectKey, "cookie-480-c")
	reqs := w.model.Requests()
	require.NotEmpty(t, reqs)
	last := reqs[len(reqs)-1]
	for _, m := range last.Messages {
		for _, s := range dispatchertest.Secrets480() {
			require.NotContains(t, m.Content, s, "the non-admin turn's model request replayed the admin turn's raw %s message", m.Role)
		}
	}
	for _, s := range dispatchertest.Secrets480() {
		require.NotContains(t, body, s, "the non-admin page rendered the admin turn's history")
	}
}

// Self-review of 5a3f98e95: the cookie is the browser's to set. With the
// operator history at "<cookie>:operator" and the member history at the bare
// cookie, a non-admin in a browser an admin used could set the cookie to
// "<cookie>:operator" and read the admin's raw history as their own. Neither
// key may be a value the other view can produce from any cookie.
func TestWebChat480_CraftedCookieCannotNameTheOperatorHistory(t *testing.T) {
	w := newWebChat480(t)
	w.post(t, adminKey480UI, "cookie-480-e")
	dispatchertest.AssertRaw(t, "the admin turn", w.model.ToolResults())

	for _, crafted := range []string{"cookie-480-e:operator", chatHistoryKey("cookie-480-e", true)} {
		// The page render first (code review round 2, F3): it must not show
		// the admin view's history either.
		req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/chat", nil)
		req.Header.Set("Authorization", "Bearer "+w.projectKey)
		req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: crafted})
		rec := httptest.NewRecorder()
		w.handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `data-testid="chat-empty-state"`, "cookie %q: the page rendered the admin view's history", crafted)

		w.post(t, w.projectKey, crafted)
		reqs := w.model.Requests()
		last := reqs[len(reqs)-1]
		for _, m := range last.Messages {
			for _, s := range dispatchertest.Secrets480() {
				require.NotContains(t, m.Content, s, "cookie %q: a non-admin turn loaded the admin view's history", crafted)
			}
		}
	}
}

// The page render (GET) reads the history of the caller's own view too.
func TestWebChat480_PageRenderReadsOwnViewHistory(t *testing.T) {
	w := newWebChat480(t)
	w.post(t, adminKey480UI, "cookie-480-d")

	get := func(bearer string) string {
		req := httptest.NewRequest(http.MethodGet, "/ui/projects/p1/chat", nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "cookie-480-d"})
		rec := httptest.NewRecorder()
		w.handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	require.NotContains(t, get(adminKey480UI), `data-testid="chat-empty-state"`, "the admin view shows its own history")
	require.Contains(t, get(w.projectKey), `data-testid="chat-empty-state"`, "a non-admin view must not render the admin view's history")
}
