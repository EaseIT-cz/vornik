package ui

// GitHub #466 (security, the rest of #440): the UI task page rendered the
// task's raw last_error, every execution's raw error_message and the stored
// post-mortem summary, and the execution page its raw error_message, to every
// caller the project check admits, so a project-scoped key read on /ui what
// the scrubbed log panel withholds. Found at: review (the #440 lane's open
// decision 1); a user observes it on the page, so the test runs the real
// api.AuthMiddleware with a DB project key and the project gate in front of
// the UI handler, over real SQLite (process-spawn-law design, "Amendment
// 2026-10-21 (#466)").

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/report"
)

const adminKey466UI = "sk-admin-ui-466"

var (
	token466UI = "ghp_" + strings.Repeat("W2eR5tY8uI", 4)[:36]
	aws466UI   = "AKIA" + "IOSFODNN7EXAMPLQ"
	slack466UI = "xoxb-" + "1234567890-abcdefghij"
	// summaryToken466UI appears only in the stored post-mortem summary.
	summaryToken466UI = "ghp_" + strings.Repeat("S7dF3gH6jK", 4)[:36]
	// detailToken466UI appears only in a step outcome's error_detail.
	detailToken466UI = "ghp_" + strings.Repeat("D1oP4aS7dF", 4)[:36]
	// cpcToken466UI appears only in cross-project call rows.
	cpcToken466UI = "ghp_" + strings.Repeat("C9pX2cR5vB", 4)[:36]
	// A token after "Bearer", a key at the start of a line, a key used as
	// a JSON object key, and a clean line.
	errText466UI = "step build failed: Authorization: Bearer " + token466UI +
		"\n" + aws466UI + "\n{\"" + slack466UI + "\":1}\nexit status 1"
)

type keyLookup466 struct {
	rows map[string]*persistence.APIKey
}

func (k keyLookup466) LookupActiveByHash(_ context.Context, h string) (*persistence.APIKey, error) {
	if r, ok := k.rows[h]; ok {
		return r, nil
	}
	return nil, persistence.ErrAPIKeyNotFound
}

// cpcStub466 serves cross-project call rows by id (the SQLite repository is
// a stub that refuses every call).
type cpcStub466 struct {
	persistence.CrossProjectCallRepository
	rows map[string]*persistence.CrossProjectCall
}

func (c cpcStub466) Get(_ context.Context, id string) (*persistence.CrossProjectCall, error) {
	if r, ok := c.rows[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, persistence.ErrNotFound
}

func (c cpcStub466) GetByCalleeTaskID(_ context.Context, _ string) (*persistence.CrossProjectCall, error) {
	return nil, persistence.ErrNotFound
}

// cpcTasks466 is the SQLite task repository with cross_project_call_id
// overlaid on Get and GetChildren.
type cpcTasks466 struct {
	*sqlite.TaskRepository
	cpcByTask map[string]string
}

func (c cpcTasks466) overlay(t *persistence.Task) *persistence.Task {
	if t != nil {
		if id, ok := c.cpcByTask[t.ID]; ok {
			t.CrossProjectCallID = &id
		}
	}
	return t
}

func (c cpcTasks466) Get(ctx context.Context, id string) (*persistence.Task, error) {
	t, err := c.TaskRepository.Get(ctx, id)
	return c.overlay(t), err
}

func (c cpcTasks466) GetChildren(ctx context.Context, id string) ([]*persistence.Task, error) {
	kids, err := c.TaskRepository.GetChildren(ctx, id)
	for _, k := range kids {
		c.overlay(k)
	}
	return kids, err
}

// The #466 doubles answer absence as production does (repository miss
// contract design).
func TestUIDoubles466_MissContract(t *testing.T) {
	ctx := context.Background()
	repotest.AssertMiss(t, "CrossProjectCallRepository.Get", func() (*persistence.CrossProjectCall, error) {
		return cpcStub466{}.Get(ctx, "cpc-absent")
	})
	repotest.AssertMiss(t, "CrossProjectCallRepository.GetByCalleeTaskID", func() (*persistence.CrossProjectCall, error) {
		return cpcStub466{}.GetByCalleeTaskID(ctx, "task-absent")
	})
	tasks := cpcTasks466{TaskRepository: sqlite.NewTaskRepository(sqlitetest.Memory(t).DB)}
	repotest.AssertMiss(t, "TaskRepository.Get", func() (*persistence.Task, error) {
		return tasks.Get(ctx, "task-absent")
	})
}

type uiChain466 struct {
	handler    http.Handler
	srv        *Server
	projectKey string
}

func newUIChain466(t *testing.T, scrub bool) *uiChain466 {
	t.Helper()
	return newUIChain466With(t, scrub, &countingExplainer491{})
}

// newUIChain466With is newUIChain466 with the post-mortem explainer given
// (GitHub #491).
func newUIChain466With(t *testing.T, scrub bool, explainer PostMortemExplainer) *uiChain466 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	pms := sqlite.NewTaskPostMortemRepository(db.DB)
	wf := "wf-466"
	now := time.Now().UTC()
	le := errText466UI
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: "t466", ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), LastError: &le, CreatedAt: now, UpdatedAt: now,
	}))
	em := errText466UI
	start := now.Add(-time.Minute)
	require.NoError(t, execs.Create(ctx, &persistence.Execution{
		ID: "e466", TaskID: "t466", ProjectID: "alpha", WorkflowID: wf,
		Status: persistence.ExecutionStatusFailed, ErrorMessage: &em, StartedAt: &start, CompletedAt: &now,
	}))
	// A summary stored before #439 scrubbed the explainer's evidence.
	require.NoError(t, pms.Record(ctx, &persistence.TaskPostMortem{
		TaskID: "t466", ProjectID: "alpha", Summary: "The step failed because the token " + summaryToken466UI + " was rejected.",
		Model: "m", RecordedAt: now,
	}))

	// Coordinator review 2026-10-21 (sibling-path parity): a step outcome's
	// error_detail is rendered on the execution page, the replay page and
	// the live page's seeded timeline. A running task carries the live page.
	outcomes := sqlite.NewExecutionStepOutcomeRepository(db.DB)
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: "t466live", ProjectID: "alpha", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now,
	}))
	require.NoError(t, execs.Create(ctx, &persistence.Execution{
		ID: "e466live", TaskID: "t466live", ProjectID: "alpha", WorkflowID: wf,
		Status: persistence.ExecutionStatusRunning, StartedAt: &start,
	}))
	for i, execID := range []string{"e466", "e466live"} {
		require.NoError(t, outcomes.Record(ctx, &persistence.ExecutionStepOutcome{
			ID: "o466-" + execID, ProjectID: "alpha", TaskID: []string{"t466", "t466live"}[i], ExecutionID: execID,
			StepID: "build", Role: "coder", Outcome: "tool_error", ErrorClass: "tool_error",
			ErrorDetail: "auth failed for " + detailToken466UI + "\n" + errText466UI, RecordedAt: now,
		}))
	}

	// Coordinator review 2026-10-21 (MEDIUM): a cross-project call row's
	// error_message carries the raw failure reason (MarkRejected with
	// err.Error()), and the replay page renders it for the incoming call and
	// each outbound hop. t466 is a callee (cpc-in) and has a callee child of
	// its own (cpc-out).
	inID, outID := "cpc-in", "cpc-out"
	parent := "t466"
	require.NoError(t, tasks.Create(ctx, &persistence.Task{
		ID: "t466child", ProjectID: "beta", WorkflowID: &wf, Status: persistence.TaskStatusFailed,
		Attempt: 1, MaxAttempts: 1, Payload: []byte(`{}`), ParentTaskID: &parent, CrossProjectCallID: &outID,
		CreatedAt: now, UpdatedAt: now,
	}))
	cpcErr := "callee task creation failed: " + cpcToken466UI
	cpcs := cpcStub466{rows: map[string]*persistence.CrossProjectCall{
		inID:  {ID: inID, CallerTaskID: "t0", CallerProject: "zeta", CalleeProject: "alpha", Status: persistence.CPCStatusRejected, ErrorMessage: &cpcErr, CreatedAt: now},
		outID: {ID: outID, CallerTaskID: "t466", CallerProject: "alpha", CalleeProject: "beta", Status: persistence.CPCStatusRejected, ErrorMessage: &cpcErr, CreatedAt: now},
	}}

	// The SQLite task repository does not persist cross_project_call_id
	// (the feature is Postgres-only), so the fixture overlays it.
	taskView := cpcTasks466{TaskRepository: tasks, cpcByTask: map[string]string{"t466": inID, "t466child": outID}}

	opts := []ServerOption{
		WithCrossProjectCallRepository(cpcs),
		WithTaskRepository(taskView), WithExecutionRepository(execs), WithPostMortemRepository(pms),
		WithStepOutcomeRepository(outcomes),
		WithLLMUsageRepository(sqlite.NewTaskLLMUsageRepository(db.DB)),
		WithToolAuditRepository(sqlite.NewToolAuditRepository(db.DB)),
		WithArtifactRepository(sqlite.NewArtifactRepository(db.DB)),
		WithTaskMessageRepository(sqlite.NewTaskMessageRepository(db.DB)),
		WithOperatorCapability(config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey466UI}}),
		WithPostMortemExplainer(explainer),
	}
	if scrub {
		s, err := report.NewLineScrubber("")
		require.NoError(t, err)
		opts = append(opts, WithErrorTextScrubber(s))
	}
	srv := NewServer(opts...)

	pk, err := apikey.Generate("alpha")
	require.NoError(t, err)
	lookup := keyLookup466{rows: map[string]*persistence.APIKey{
		apikey.Hash(pk): {ID: "akey-466", ProjectID: "alpha", KeyHash: apikey.Hash(pk), CreatedAt: now},
	}}
	// The service mounts the UI under /ui and strips the prefix
	// (uiSubtreeHandler) inside the project gate.
	h := http.StripPrefix("/ui", srv.Handler())
	h = api.ProjectAuthMiddleware()(h)
	h = api.AuthMiddleware(api.AuthConfig{
		Enabled: true, APIKeyLookup: lookup,
		// staticKey491UI: configured, unscoped and not admin-class (#491).
		StaticAPIKeys: map[string][]string{adminKey466UI: nil, staticKey491UI: nil},
	})(h)
	return &uiChain466{handler: h, srv: srv, projectKey: pk}
}

func (c *uiChain466) get(t *testing.T, path, bearer string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	b, _ := io.ReadAll(rec.Body)
	require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, string(b))
	body := html.UnescapeString(string(b))
	// The page must render the error block at all, raw or withheld, or every
	// NotContains below would pass vacuously.
	require.True(t, strings.Contains(body, "step build failed") || strings.Contains(body, report.WithheldLine),
		"%s: the error block is not rendered", path)
	return body
}

var uiPaths466 = []string{"/ui/tasks/t466", "/ui/executions/e466", "/ui/executions/e466/replay", "/ui/tasks/t466live/live"}

func assertNoSecret466UI(t *testing.T, where, body string) {
	t.Helper()
	for _, s := range []string{token466UI, aws466UI, slack466UI, summaryToken466UI, detailToken466UI, cpcToken466UI} {
		assert.NotContains(t, body, s, "%s served a planted secret to a project-scoped caller", where)
	}
}

func TestTaskPage466_ProjectKeySeesNoSecret(t *testing.T) {
	c := newUIChain466(t, true)
	for _, p := range uiPaths466 {
		body := c.get(t, p, c.projectKey)
		assertNoSecret466UI(t, p, body)
		assert.Contains(t, body, report.WithheldLine, p)
		assert.Contains(t, body, "exit status 1", "%s: a line with no secret is still shown", p)

		assert.Contains(t, c.get(t, p, adminKey466UI), token466UI, "%s: the operator view must stay unchanged", p)
	}
	assert.Contains(t, c.get(t, "/ui/tasks/t466", adminKey466UI), summaryToken466UI,
		"the fixture must render the stored summary to the operator")
	for _, p := range uiPaths466[1:] {
		assert.Contains(t, c.get(t, p, adminKey466UI), detailToken466UI,
			"%s: the fixture must render the step outcome's detail to the operator", p)
	}
	replay := c.get(t, "/ui/executions/e466/replay", adminKey466UI)
	assert.Contains(t, replay, summaryToken466UI, "the replay page renders the stored summary too")
	// The outbound hop's error is rendered; the incoming breadcrumb shows no
	// error text today, but its row is scrubbed too (the timeline value).
	assert.Contains(t, replay, cpcToken466UI, "the replay page renders the outbound hop's error to the operator")
}

// Fail closed: no scrubber wired, the non-admin caller sees no error line.
func TestTaskPage466_NoScrubberWithholds(t *testing.T) {
	c := newUIChain466(t, false)
	for _, p := range uiPaths466 {
		body := c.get(t, p, c.projectKey)
		assertNoSecret466UI(t, p, body)
		assert.NotContains(t, body, "exit status 1", p)
	}
}
