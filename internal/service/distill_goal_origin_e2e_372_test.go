package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/scheduler"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/ui"
)

// GitHub #372 (learning-loop design, note 2026-10-17): the #347 refusal (no
// draft skill from a task whose goal a third party wrote) had no test at the
// level where the decision runs. The distiller's entry is unexported and runs
// only from the executor loop after a task completes, so these tests create
// each task through its REAL surface (the webhook router, the GitHub channel
// task creator, the chat action executor, and the UI create-task form as the
// first-party control), then let the real scheduler lease it and the real
// executor run it on SQLite with distillation wired as production wires it
// (container_scheduler.go). A user would see the failure as a draft skill in
// the review queue; that is what is asserted.
//
// Found at: review (#347's review b893 F4; the found-at-level retro,
// docs/audits/2026-10-14-found-at-level-retro.md, lane W8-U). Observable at
// executor-e2e. Regression test at: executor-e2e.

const e2e372Project = "p372"

// e2e372Runtime is a container runtime whose every agent step writes the same
// result.json and exits 0.
type e2e372Runtime struct {
	mu     sync.Mutex
	result string
	n      int
}

func (r *e2e372Runtime) StartContainer(_ context.Context, c *runtime.ContainerConfig) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.n++
	if c.OutputDir != "" {
		if err := os.MkdirAll(c.OutputDir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(c.OutputDir, "result.json"), []byte(r.result), 0o644); err != nil {
			return "", err
		}
	}
	return "c372-" + strings.Repeat("x", r.n), nil
}
func (r *e2e372Runtime) StopContainer(context.Context, string, bool) error { return nil }
func (r *e2e372Runtime) InspectContainer(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *e2e372Runtime) WaitForExit(context.Context, string, time.Duration) (int, error) {
	return 0, nil
}
func (r *e2e372Runtime) GetContainerByTask(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *e2e372Runtime) RemoveContainer(context.Context, string, bool) error { return nil }
func (r *e2e372Runtime) Logs(context.Context, string, int) (string, error)   { return "", nil }

// e2e372LLM is the distiller's model. It counts calls, so a refusal can be
// shown to have happened before the model saw the goal.
type e2e372LLM struct {
	chat.Provider
	calls atomic.Int32
	// reply, when set before the loop starts, replaces the default
	// candidate (GitHub #369: the post-gate skip cases).
	reply string
}

func (l *e2e372LLM) Complete(context.Context, []chat.Message) (*chat.ChatResponse, error) {
	l.calls.Add(1)
	resp := &chat.ChatResponse{}
	resp.Choices = append(resp.Choices, struct {
		Index        int          `json:"index"`
		Message      chat.Message `json:"message"`
		FinishReason string       `json:"finish_reason"`
	}{Message: chat.Message{Content: l.content()}})
	return resp, nil
}

func (l *e2e372LLM) content() string {
	if l.reply != "" {
		return l.reply
	}
	return `{"skip":false,"name":"e2e-372-lesson","description":"what the run taught","body":"# lesson\ndo the thing","domain":"software","roles":["worker"]}`
}

type e2e372 struct {
	db      *sqlite.DB
	reg     *registry.Registry
	tasks   *sqlite.TaskRepository
	skills  *sqlite.SkillRepository
	llm     *e2e372LLM
	metrics *executor.Metrics
	sched   *scheduler.Scheduler
	exec    *executor.Executor
}

func e2e372Registry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{
		"projects/p372.yaml": `
projectId: p372
displayName: Project 372
swarmId: s372
defaultWorkflowId: wf372
defaultPriority: 40
webhooks:
  sources:
    - name: hook
      secret: topsecret
      event_id_path: id
      task_type_template: "Webhook: ${issue.title}"
`,
		"swarms/s372.md":     "---\nswarmId: s372\nroles:\n  - name: worker\n    runtime:\n      image: localhost/vornik-agent:test\n---\n",
		"workflows/wf372.md": "---\nworkflowId: wf372\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n",
	} {
		p := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	require.NotNil(t, reg.GetProject(e2e372Project))
	return reg
}

// newE2E372 builds the loop: one SQLite database, the executor with the
// distiller wired as the container wires it, and a running scheduler.
func newE2E372(t *testing.T) *e2e372 {
	t.Helper()
	db := sqlitetest.Memory(t)
	f := &e2e372{
		db:     db,
		reg:    e2e372Registry(t),
		tasks:  sqlite.NewTaskRepository(db.DB),
		skills: sqlite.NewSkillRepository(db.DB),
		llm:    &e2e372LLM{},
	}
	execRepo := sqlite.NewExecutionRepository(db.DB)
	store, err := artifacts.New(artifacts.WithBasePath(t.TempDir()))
	require.NoError(t, err)
	// One clean agent step: a single file_read, which the taint classifier
	// rates as no third-party source, so the step proof holds and only the
	// goal decides.
	rt := &e2e372Runtime{result: `{"status":"COMPLETED","message":"done","toolAudit":[{"tool":"file_read","input":"{\"path\":\"README.md\"}","output":"ok","duration_ms":1}]}`}
	f.metrics = executor.NewMetrics(prometheus.NewRegistry())
	cfg := executor.DefaultConfig()
	cfg.DefaultTimeout = 10 * time.Second
	cfg.RetryDelay = 0
	cfg.ProjectWorkspacePath = t.TempDir()
	f.exec = executor.NewWithOptions(rt, execRepo, sqlite.NewArtifactRepository(db.DB), f.tasks, cfg,
		executor.WithLogger(zerolog.Nop()),
		executor.WithMetrics(f.metrics),
		executor.WithWorkflowResolver(f.reg),
		executor.WithArtifactStore(store),
		executor.WithToolAuditRepository(sqlite.NewToolAuditRepository(db.DB)),
		executor.WithStepOutcomeRepository(sqlite.NewExecutionStepOutcomeRepository(db.DB)),
		executor.WithSkillRepository(f.skills),
		executor.WithSkillDistiller(f.llm),
		executor.WithConversationalLifecycle(
			sqlite.NewTaskMessageRepository(db.DB),
			sqlite.NewTaskScratchpadRepository(db.DB),
			f.tasks,
		),
	)
	scfg := scheduler.DefaultConfig()
	scfg.PollInterval = 20 * time.Millisecond
	scfg.MaxConcurrency = 2
	f.sched = scheduler.NewWithOptions(f.tasks, scfg,
		scheduler.WithRuntimeManager(rt),
		scheduler.WithExecutionRepository(execRepo),
		scheduler.WithExecutor(f.exec),
		scheduler.WithArtifactStore(store),
		scheduler.WithLogger(zerolog.Nop()),
	)
	t.Cleanup(func() {
		_ = f.sched.Stop()
		_ = f.exec.Stop(context.Background())
	})
	return f
}

// start runs the scheduler once the task is in place, so the loop picks up
// exactly the row the surface wrote.
func (f *e2e372) start(t *testing.T) {
	t.Helper()
	require.NoError(t, f.sched.Start())
}

// onlyTask returns the one task the surface created.
func (f *e2e372) onlyTask(t *testing.T) *persistence.Task {
	t.Helper()
	rows, err := f.tasks.List(context.Background(), persistence.TaskFilter{ProjectID: func() *string { p := e2e372Project; return &p }()})
	require.NoError(t, err)
	require.Len(t, rows, 1, "the surface must create exactly one task")
	return rows[0]
}

// attribute models the attribution hazard #347 closes: the day a
// third-party surface records the API key that configured it, the
// 2026-10-12b inference alone would pass the goal. goal_origin is not
// touched (no statement can change it after creation).
func (f *e2e372) attribute(t *testing.T, taskID string) {
	t.Helper()
	_, err := f.db.Exec(`UPDATE tasks SET created_by_api_key_id = 'key-configured-the-source' WHERE id = ?`, taskID)
	require.NoError(t, err)
}

func (f *e2e372) drafts(t *testing.T) int {
	t.Helper()
	rows, err := f.skills.List(context.Background(), e2e372Project, persistence.SkillListFilter{})
	require.NoError(t, err)
	return len(rows)
}

func (f *e2e372) skips() float64 {
	return testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues("goal_third_party")) +
		testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues("goal_origin_unproven")) +
		testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues("goal_origin_unrecorded"))
}

// waitCompleted waits for the loop to complete the task (a task that does
// not complete proves nothing about the distiller).
func (f *e2e372) waitCompleted(t *testing.T, taskID string) {
	t.Helper()
	require.Eventually(t, func() bool {
		row, err := f.tasks.Get(context.Background(), taskID)
		return err == nil && row != nil && row.Status == persistence.TaskStatusCompleted
	}, 20*time.Second, 10*time.Millisecond, "the task must complete in the real loop")
}

// assertRefusedThirdParty waits until the distiller has decided (its skip
// counter moved, so "no draft" is "examined and refused", not "not run
// yet"), then asserts the reason and that nothing reached the review queue.
func (f *e2e372) assertRefusedThirdParty(t *testing.T, taskID, origin string) {
	t.Helper()
	f.waitCompleted(t, taskID)
	row, err := f.tasks.Get(context.Background(), taskID)
	require.NoError(t, err)
	require.Equal(t, origin, row.GoalOrigin, "the surface must have stamped its own origin")
	require.Eventually(t, func() bool { return f.skips() > 0 || f.drafts(t) > 0 },
		20*time.Second, 10*time.Millisecond, "the distiller must run on the completed task")
	require.Equal(t, 0, f.drafts(t), "a %s goal must never become a draft skill (GitHub #347)", origin)
	require.Equal(t, 1.0, testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues("goal_third_party")),
		"refused for the recorded origin, not by the inference")
	require.Zero(t, f.llm.calls.Load(), "the distiller's model must never see a third-party goal")
}

func e2e372Sign(body []byte) string {
	mac := hmac.New(sha256.New, []byte("topsecret"))
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// surfaces create one task each through the real code a user reaches.
var e2e372Surfaces = map[string]struct {
	origin string
	create func(t *testing.T, f *e2e372)
}{
	"webhook": {persistence.TaskGoalOriginWebhook, func(t *testing.T, f *e2e372) {
		server := api.NewServer(api.WithLogger(zerolog.Nop()), api.WithTaskRepository(f.tasks),
			api.WithProjectRegistry(f.reg), api.WithWebhookEventRepository(sqlite.NewWebhookEventRepository(f.db.DB)))
		router := api.NewRouter(server, &config.Config{})
		// The sender claims a first-party origin; it is payload, never the marker.
		body := []byte(`{"id":"evt-372","goal_origin":"rest","issue":{"title":"Fix login"}}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/"+e2e372Project+"/hook", bytes.NewReader(body))
		req.Header.Set("X-Vornik-Signature", e2e372Sign(body))
		rec := httptest.NewRecorder()
		router.Handler().ServeHTTP(rec, req)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	}},
	"github": {persistence.TaskGoalOriginGitHub, func(t *testing.T, f *e2e372) {
		g := newGitHubTaskCreator(f.tasks, f.reg.GetProject(e2e372Project), nil, zerolog.Nop())
		require.NoError(t, g.Create(context.Background(), github.TaskCreationEvent{
			Kind: "issues.labeled", SessionID: "acme/api#issues/372", Title: "bug", Body: "an outsider wrote this",
			Labels: []string{"vornik-task"}, SenderLogin: "outsider", Repo: "acme/api", Number: 372,
			IdempotencyKey: "github-app:d-372",
		}))
	}},
	"chat": {persistence.TaskGoalOriginChat, func(t *testing.T, f *e2e372) {
		res, err := chat.ExecuteAction(context.Background(), chat.Action{
			Type: chat.ActionCreateTask, Project: e2e372Project, Type_: "research",
			Input: map[string]any{"goal_origin": "rest", "context": map[string]any{"prompt": "summarise what the email said"}},
		}, f.tasks, nil, nil)
		require.NoError(t, err)
		require.True(t, res.Success, res.Message)
	}},
}

func TestDistillE2E_ThirdPartyGoalIsNeverDistilled(t *testing.T) {
	for name, s := range e2e372Surfaces {
		t.Run(name, func(t *testing.T) {
			f := newE2E372(t)
			s.create(t, f)
			task := f.onlyTask(t)
			f.start(t)
			f.assertRefusedThirdParty(t, task.ID, s.origin)
		})
		// The attribution regression the inference alone would pass: the
		// same surface, the row then attributed to a key. Without the
		// recorded-origin check this one produces a draft.
		t.Run(name+" attributed to a key", func(t *testing.T) {
			f := newE2E372(t)
			s.create(t, f)
			task := f.onlyTask(t)
			f.attribute(t, task.ID)
			f.start(t)
			f.assertRefusedThirdParty(t, task.ID, s.origin)
		})
	}
}

// The control: the same loop, a goal the signed-in operator typed in the web
// UI form (first-party, distillable without a key, operator decision
// 2026-10-13), produces a draft. Without it, the refusals above could pass on
// a loop that never distils anything.
func TestDistillE2E_UIFormGoalIsDistilled(t *testing.T) {
	f := newE2E372(t)
	creator := taskcreate.New(taskcreate.WithTaskRepository(f.tasks), taskcreate.WithProjectRegistry(f.reg),
		taskcreate.WithRateLimiter(ratelimit.New()))
	srv := ui.NewServer(ui.WithProjectRegistry(f.reg), ui.WithTaskRepository(f.tasks), ui.WithTaskCreator(creator))
	form := url.Values{}
	form.Set("prompt", "Write the release checklist")
	form.Set("workflowId", "wf372")
	form.Set("taskType", "research")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/"+e2e372Project+"/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ProjectCreateTaskSubmit(rec, req, e2e372Project)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	task := f.onlyTask(t)
	require.Equal(t, persistence.TaskGoalOriginUI, task.GoalOrigin)

	f.start(t)
	f.waitCompleted(t, task.ID)
	require.Eventually(t, func() bool { return f.drafts(t) == 1 }, 20*time.Second, 10*time.Millisecond,
		"a ui-form goal with a clean run must produce one draft skill")
	rows, err := f.skills.List(context.Background(), e2e372Project, persistence.SkillListFilter{})
	require.NoError(t, err)
	require.Equal(t, persistence.SkillMaturityDraft, rows[0].Maturity)
	require.Equal(t, task.ID, rows[0].OriginTask)
	require.Equal(t, int32(1), f.llm.calls.Load())
}
