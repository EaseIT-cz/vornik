package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/registry"
)

// GitHub #305 (2026-10-08): the companion list tool took no project argument
// and its decode dropped unknown fields, so Hermes's agent-admin key (home
// hermes--home) asking to list prague-venues silently got hermes--home's
// tasks. LLD 21 §A, "list takes a project, and tool arguments are strict".

// listFixture is a server whose registry holds two projects of namespace
// hermes and one of namespace other, and whose task repository records the
// project of every List call and answers one companion task per project.
type listFixture struct {
	srv      *Server
	keyRepo  *memAPIKeyRepo
	listed   []string
	calls    int
	adminRaw string
	plainRaw string
}

func newListFixture(t *testing.T) *listFixture {
	t.Helper()
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"hermes--home":          {ID: "hermes--home", Broker: true},
		"hermes--prague-venues": {ID: "hermes--prague-venues", Broker: true},
		"other--x":              {ID: "other--x", Broker: true},
		"alpha":                 {ID: "alpha"},
	})
	f := &listFixture{keyRepo: &memAPIKeyRepo{}}
	taskRepo := &mocks.MockTaskRepository{
		ListFunc: func(_ context.Context, filter persistence.TaskFilter) ([]*persistence.Task, error) {
			f.calls++
			projects := filter.ProjectIDs
			if filter.ProjectID != nil {
				projects = []string{*filter.ProjectID}
			}
			var out []*persistence.Task
			for i, p := range projects {
				f.listed = append(f.listed, p)
				wf := "wf"
				out = append(out, &persistence.Task{ID: "task-of-" + p, ProjectID: p, WorkflowID: &wf,
					Status: persistence.TaskStatusCompleted, CreationSource: persistence.TaskCreationSourceCompanion,
					CreatedAt: time.Now().Add(-time.Duration(i) * time.Minute)})
			}
			if filter.PageSize > 0 && len(out) > filter.PageSize {
				out = out[:filter.PageSize] // as the drivers' LIMIT does
			}
			return out, nil
		},
	}
	f.srv = &Server{logger: zerolog.Nop(), apiKeyRepo: f.keyRepo, taskRepo: taskRepo, projectRegistry: reg}
	f.adminRaw, _ = seedCompanionKey(t, f.keyRepo, "hermes--home", nil)
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentAdmin = true
	f.keyRepo.rows[len(f.keyRepo.rows)-1].AgentNamespace = "hermes"
	f.plainRaw, _ = seedCompanionKey(t, f.keyRepo, "alpha", nil)
	return f
}

func (f *listFixture) list(t *testing.T, raw string, args map[string]any) (map[string]any, string, bool) {
	t.Helper()
	text, isErr := callTool(t, f.srv, raw, "list", args)
	if isErr {
		return nil, text, true
	}
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(text), &out), text)
	return out, text, false
}

func TestCompanionList_AdminKeyListsNamedProject(t *testing.T) {
	for _, p := range []string{"prague-venues", "hermes--prague-venues", "  prague-venues  "} {
		f := newListFixture(t)
		out, text, isErr := f.list(t, f.adminRaw, map[string]any{"project": p})
		require.False(t, isErr, text)
		assert.Equal(t, "hermes--prague-venues", out["project"], "project %q", p)
		assert.Equal(t, []string{"hermes--prague-venues"}, f.listed, "project %q", p)
		assert.Contains(t, text, "task-of-hermes--prague-venues")
	}
}

func TestCompanionList_AdminKeyForeignProjectRefused(t *testing.T) {
	for _, p := range []string{"other--x", "hermes--other--x", "nonexistent", "hermes--nonexistent"} {
		f := newListFixture(t)
		_, text, isErr := f.list(t, f.adminRaw, map[string]any{"project": p})
		require.True(t, isErr, "project %q must be refused: %s", p, text)
		assert.Empty(t, f.listed, "no task row may be read for a refused project %q", p)
	}
	f := newListFixture(t)
	_, text, _ := f.list(t, f.adminRaw, map[string]any{"project": "other--x"})
	assert.Contains(t, text, "not in your namespace")
	_, text, _ = f.list(t, f.adminRaw, map[string]any{"project": "nonexistent"})
	assert.Contains(t, text, "there is no project")
}

func TestCompanionList_PlainKeyOtherProjectRefused(t *testing.T) {
	f := newListFixture(t)
	_, text, isErr := f.list(t, f.plainRaw, map[string]any{"project": "hermes--prague-venues"})
	require.True(t, isErr, text)
	assert.Contains(t, text, "only its own project")
	assert.Empty(t, f.listed)

	out, text, isErr := f.list(t, f.plainRaw, map[string]any{"project": "alpha"})
	require.False(t, isErr, text)
	assert.Equal(t, "alpha", out["project"])
	assert.Equal(t, []string{"alpha"}, f.listed)
}

func TestCompanionList_NoProjectUnchanged(t *testing.T) {
	for _, args := range []map[string]any{nil, {}, {"project": ""}, {"limit": 5}} {
		f := newListFixture(t)
		out, text, isErr := f.list(t, f.adminRaw, args)
		require.False(t, isErr, text)
		assert.Equal(t, "hermes--home", out["project"])
		assert.Equal(t, []string{"hermes--home"}, f.listed)
		assert.Equal(t, []string{"project", "tasks", "total"}, sortedKeys(out))
	}
}

func TestCompanionList_UnknownFieldRefused(t *testing.T) {
	f := newListFixture(t)
	_, text, isErr := f.list(t, f.adminRaw, map[string]any{"projekt": "prague-venues"})
	require.True(t, isErr, text)
	assert.Contains(t, text, `"projekt"`)
	assert.Contains(t, text, "limit, status, project")
	assert.Empty(t, f.listed)
}

// GitHub #305: the two flags a bundle reads before sending project to list,
// and before relying on a refusal of an unknown argument.
func TestGetCapabilities_StrictArgsAndListProject(t *testing.T) {
	srv, _, _ := newCompanionMCPServer(t)
	rec := httptest.NewRecorder()
	srv.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	f := decodeCapabilities(t, rec.Body.Bytes()).Features
	assert.True(t, f["companion-strict-args"])
	assert.True(t, f["companion-list-project"])
	assert.True(t, f["companion-list-all-projects"], "GitHub #324: list(project: \"*\")")

	bare := &Server{logger: zerolog.Nop()}
	rec = httptest.NewRecorder()
	bare.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil))
	f = decodeCapabilities(t, rec.Body.Bytes()).Features
	v, ok := f["companion-list-project"]
	assert.True(t, ok, "the flag must always be present")
	assert.False(t, v, "a daemon with no companion endpoint does not advertise it")
	v, ok = f["companion-list-all-projects"]
	assert.True(t, ok, "the flag must always be present")
	assert.False(t, v)
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
