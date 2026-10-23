package ui

// GitHub #496 item 2 (P2, security): the web chat's dispatcher session pins
// the URL's project into Request.AllowedProjects, but webchat.SessionStore.Load
// did so only after two early returns (no registry; the registry no longer
// holding the project), on which the list stayed empty, and an empty list
// means "no restriction" to every dispatcher tool. A key scoped to project p1,
// posting to p1's chat while a configuration reload removed p1 between the
// handler's check and the store's load, could have the model read project
// p2's task. Found at: review (the #491 code review's lead); a user reaches
// it by posting a chat message, so the test runs the real api.AuthMiddleware
// and project gate in front of the UI handler, a real dispatcher over real
// SQLite and the #480 scripted model, which calls get_task_status and
// wait_for_task on p2's task (process-spawn-law design, "Amendment
// 2026-10-21 (#496)").

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
	"vornik.io/vornik/internal/webchat"
)

func TestWebChat496_ScopedKeyCannotReachAnotherProjectsTask(t *testing.T) {
	for _, tc := range []struct {
		name string
		// storeRegistryWithoutP1 gives p1's chat store a registry snapshot
		// that no longer holds p1, as a reload between the handler's check
		// and the store's load leaves it.
		storeRegistryWithoutP1 bool
	}{
		{name: "registry holds p1"},
		{name: "p1 gone from the registry at load", storeRegistryWithoutP1: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeSwarmFixture(t)
			reg := registry.New()
			require.NoError(t, reg.Load(root))
			require.NotNil(t, reg.GetProject("p1"))

			db := sqlitetest.Memory(t)
			tasks := sqlite.NewTaskRepository(db.DB)
			execs := sqlite.NewExecutionRepository(db.DB)
			// The task the model asks about belongs to p2, not to p1.
			dispatchertest.SeedFailedTask480(t, tasks, execs, "p2")

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
			if tc.storeRegistryWithoutP1 {
				srv.chatStores = map[string]*webchat.SessionStore{"p1": webchat.NewSessionStore(registry.New(), "p1")}
			}

			pk, err := apikey.Generate("p1")
			require.NoError(t, err)
			lookup := keyLookup466{rows: map[string]*persistence.APIKey{
				apikey.Hash(pk): {ID: "akey-496", ProjectID: "p1", KeyHash: apikey.Hash(pk), CreatedAt: time.Now().UTC()},
			}}
			h := http.StripPrefix("/ui", srv.Handler())
			h = api.ProjectAuthMiddleware()(h)
			h = api.AuthMiddleware(api.AuthConfig{Enabled: true, APIKeyLookup: lookup})(h)

			form := url.Values{"prompt": {"why did task " + dispatchertest.TaskID480 + " fail?"}}
			req := httptest.NewRequest(http.MethodPost, "/ui/projects/p1/chat/messages", strings.NewReader(form.Encode()))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Authorization", "Bearer "+pk)
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			req.AddCookie(&http.Cookie{Name: chatSessionCookie, Value: "cookie-496"})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

			results := model.ToolResults()
			require.Len(t, results, 2, "both tools must have been called: %q", results)
			for _, r := range results {
				require.Contains(t, r, "not permitted for this session", "a p1-scoped chat read p2's task: %q", r)
				require.NotContains(t, r, dispatchertest.CleanLine480, "a line of p2's error text reached p1's chat")
				require.NotContains(t, r, "FAILED", "p2's task status reached p1's chat")
			}
		})
	}
}
