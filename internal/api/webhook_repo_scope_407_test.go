package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
)

// GitHub #407, production incident 2026-10-16: a GitHub App installed on the
// whole EaseIT-cz organisation delivered every org repository's issue events
// to the headmatch project's generic webhook source. The source filtered only
// on ${action}, nothing consulted the project's repository, and labelled
// vornik-enterprise issues became headmatch issue-fix tasks (about 20, all
// cancelled). Found at: production. These tests drive the real router over
// real SQLite task and webhook-event repositories: the whole path after HMAC.
// Design: github-app-channel-design.md "Generic webhook sources: repository
// scope (2026-10-16, GitHub #407)".

// repoScopeRegistry407 loads a project shaped like headmatch: github.repo set,
// one source filtering on ${action} with no repos of its own (the default
// scope applies) unless sourceRepos is given.
func repoScopeRegistry407(t *testing.T, sourceRepos string) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	project := `
projectId: hm
displayName: Headmatch-like
swarmId: swarm-1
defaultWorkflowId: wf-1
github:
  repo: "acme/own-repo"
webhooks:
  sources:
    - name: github
      secret: topsecret
      task_type_template: "github-event"
      filter: "${action}=labeled,created,completed"
` + sourceRepos
	require.NoError(t, os.WriteFile(filepath.Join(root, "projects", "hm.yaml"), []byte(project), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "swarms", "swarm.md"), []byte("---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "workflows", "wf.md"), []byte("---\nworkflowId: wf-1\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n"), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

type repoScopeRig407 struct {
	server  *Server
	handler http.Handler
	tasks   *sqlite.TaskRepository
	events  *sqlite.WebhookEventRepository
}

func newRepoScopeRig407(t *testing.T, sourceRepos string) repoScopeRig407 {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	events := sqlite.NewWebhookEventRepository(db.DB)
	server := NewServer(
		WithLogger(zerolog.Nop()),
		WithTaskRepository(tasks),
		WithProjectRegistry(repoScopeRegistry407(t, sourceRepos)),
		WithWebhookEventRepository(events),
	)
	server.apiMetrics = NewAPIMetrics(prometheus.NewRegistry())
	return repoScopeRig407{server: server, handler: NewRouter(server, &config.Config{}).Handler(), tasks: tasks, events: events}
}

func (r repoScopeRig407) deliver(t *testing.T, delivery, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/hm/github", bytes.NewReader([]byte(body)))
	req.Header.Set("X-Vornik-Signature", signWebhook([]byte(body), "topsecret"))
	req.Header.Set("X-GitHub-Delivery", delivery)
	rec := httptest.NewRecorder()
	r.handler.ServeHTTP(rec, req)
	return rec
}

func (r repoScopeRig407) scopeCount(outcome string) float64 {
	return testutil.ToFloat64(r.server.apiMetrics.WebhookRepoScopeTotal.WithLabelValues("hm", "github", outcome))
}

func (r repoScopeRig407) taskCount(t *testing.T) int {
	t.Helper()
	pid := "hm"
	got, err := r.tasks.List(context.Background(), persistence.TaskFilter{ProjectID: &pid})
	require.NoError(t, err)
	return len(got)
}

func (r repoScopeRig407) lastEvent(t *testing.T) *persistence.WebhookEvent {
	t.Helper()
	pid := "hm"
	got, err := r.events.List(context.Background(), persistence.WebhookEventFilter{ProjectID: &pid, PageSize: 50})
	require.NoError(t, err)
	require.NotEmpty(t, got)
	return got[0]
}

func labeledIssue407(repo string) string {
	return `{"action":"labeled","label":{"name":"next"},"issue":{"number":165,"title":"x","labels":[{"name":"next"}]},"repository":{"full_name":"` + repo + `"}}`
}

func TestWebhookRepoScope407_RealRouter(t *testing.T) {
	rig := newRepoScopeRig407(t, "")

	// The incident: another repository's labelled issue creates no task.
	rec := rig.deliver(t, "d-other", labeledIssue407("EaseIT-cz/vornik"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "filtered")
	require.Equal(t, 0, rig.taskCount(t), "a delivery for another repository must not create a task")
	ev := rig.lastEvent(t)
	require.Equal(t, persistence.WebhookEventStatusFiltered, ev.Status)
	require.Equal(t, "repo_not_in_scope", ev.ErrorCode)
	require.Contains(t, ev.ErrorMessage, "EaseIT-cz/vornik")

	// The project's own repository, in different case, still creates one.
	rec = rig.deliver(t, "d-own", labeledIssue407("ACME/Own-Repo"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, rig.taskCount(t))

	// A repository-less delivery passes the repository gate (the action
	// filter still applies to it).
	rec = rig.deliver(t, "d-norepo", `{"action":"created","installation":{"id":1}}`)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 2, rig.taskCount(t))

	// A malformed (non-string) full_name cannot match an owner/repo entry:
	// the scoped source drops it rather than reading it as "no repository".
	rec = rig.deliver(t, "d-badname", `{"action":"labeled","repository":{"full_name":42}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 2, rig.taskCount(t))
	require.Equal(t, "repo_not_in_scope", rig.lastEvent(t).ErrorCode)

	// The counter saw every delivery the gate examined: the denominator.
	require.Equal(t, 2.0, rig.scopeCount(webhookRepoScopeDropped))
	require.Equal(t, 1.0, rig.scopeCount(webhookRepoScopeAllowed))
	require.Equal(t, 1.0, rig.scopeCount(webhookRepoScopeNoRepository))
	require.Equal(t, 0.0, rig.scopeCount(webhookRepoScopeUnrestricted))
}

// The job tier's relay door (the DMZ node forwards the verified body)
// applies the same scope: the gate sits in the shared enqueueVerifiedWebhook.
func TestWebhookRepoScope407_RelayDoor(t *testing.T) {
	rig := newRepoScopeRig407(t, "")
	relay := func(delivery, body string) *httptest.ResponseRecorder {
		b, err := json.Marshal(relayEnvelope{ProjectID: "hm", Source: "github", DeliveryID: delivery, Body: []byte(body)})
		require.NoError(t, err)
		rec := httptest.NewRecorder()
		rig.server.RelayWebhook(rec, httptest.NewRequest(http.MethodPost, "/internal/v1/webhook-relay", bytes.NewReader(b)))
		return rec
	}
	rec := relay("r-other", labeledIssue407("EaseIT-cz/vornik"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 0, rig.taskCount(t))
	require.Equal(t, "repo_not_in_scope", rig.lastEvent(t).ErrorCode)
	rec = relay("r-own", labeledIssue407("acme/own-repo"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, rig.taskCount(t))
}

// An explicit repos list replaces the default; "*" accepts every repository.
func TestWebhookRepoScope407_ExplicitReposAndStar(t *testing.T) {
	rig := newRepoScopeRig407(t, "      repos: [\"acme/other\"]\n")
	rec := rig.deliver(t, "d1", labeledIssue407("acme/own-repo"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, 0, rig.taskCount(t), "explicit repos replace the project default")
	rec = rig.deliver(t, "d2", labeledIssue407("acme/other"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, rig.taskCount(t))

	star := newRepoScopeRig407(t, "      repos: [\"*\"]\n")
	rec = star.deliver(t, "d3", labeledIssue407("someone/else"))
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	require.Equal(t, 1, star.taskCount(t))
}
