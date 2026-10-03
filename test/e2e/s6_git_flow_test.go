//go:build e2e
// +build e2e

package e2e_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/aidisclosure"
	"vornik.io/vornik/internal/executor"
	forgeh "vornik.io/vornik/internal/executor/handlers/forge"
	"vornik.io/vornik/internal/forge"
	"vornik.io/vornik/internal/forge/github"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/runtime"
	"vornik.io/vornik/internal/spawn"
)

// Process-spawn law S6 (https://docs.vornik.io,
// "S6 — git state an agent can write", compatibility matrix): git is the
// product's most critical capability, so every flow S6 touches keeps a
// real-git test written BEFORE the change. These two drive the executor end to
// end on a real repository: a task runs through Execute() with a fake runtime
// standing in for the agent container, its worktree is merged back into the
// project, and (second test) the forge publish chain pushes the merged result
// to a temp bare remote through the REAL GitHub provider's push path.

// s6DaemonGitHome gives the test a private HOME with an empty .gitconfig, so
// daemon git reads no global config of the developer's, and composes and
// registers the daemon's git config from it, as the daemon's startup does
// (S6-D4).
func s6DaemonGitHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Cleanup(spawn.RegisterGitGlobalConfig(spawn.GitGlobalConfig()))
	if _, err := spawn.ComposeGitConfig(context.Background(), t.TempDir()); err != nil {
		t.Fatalf("compose the daemon's git config: %v", err)
	}
	return home
}

// s6Git runs git for the TEST's own setup and assertions (not daemon code).
func s6Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// s6Runtime is the agent container stand-in: it records the config the
// executor started it with, writes agent.txt into the mounted project dir (the
// task's worktree) WITHOUT committing, so the daemon's auto-commit and
// merge-back must carry it, and writes a completed result.json.
type s6Runtime struct {
	mu      sync.Mutex
	configs []runtime.ContainerConfig
}

func (r *s6Runtime) StartContainer(_ context.Context, c *runtime.ContainerConfig) (string, error) {
	r.mu.Lock()
	r.configs = append(r.configs, *c)
	r.mu.Unlock()
	if c.ProjectDir != "" {
		if err := os.WriteFile(filepath.Join(c.ProjectDir, "agent.txt"), []byte("written by the agent\n"), 0o644); err != nil {
			return "", err
		}
	}
	if c.OutputDir != "" {
		_ = os.MkdirAll(c.OutputDir, 0o755)
		if err := os.WriteFile(filepath.Join(c.OutputDir, "result.json"), []byte(`{"status":"COMPLETED"}`), 0o644); err != nil {
			return "", err
		}
	}
	return "s6-container-" + c.TaskID, nil
}
func (r *s6Runtime) StopContainer(context.Context, string, bool) error { return nil }
func (r *s6Runtime) InspectContainer(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *s6Runtime) WaitForExit(context.Context, string, time.Duration) (int, error) { return 0, nil }
func (r *s6Runtime) GetContainerByTask(context.Context, string) (*runtime.Container, error) {
	return nil, nil
}
func (r *s6Runtime) RemoveContainer(context.Context, string, bool) error { return nil }
func (r *s6Runtime) Logs(context.Context, string, int) (string, error)   { return "", nil }
func (r *s6Runtime) started() []runtime.ContainerConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.ContainerConfig(nil), r.configs...)
}

// s6Resolver is a one-project, one-role, one-agent-step workflow.
type s6Resolver struct{ projectID string }

func (s s6Resolver) GetProject(id string) *registry.Project {
	if id != s.projectID {
		return nil
	}
	return &registry.Project{ID: id, SwarmID: "s6-swarm", DefaultWorkflowID: "s6-wf"}
}
func (s6Resolver) GetSwarm(id string) *registry.Swarm {
	if id != "s6-swarm" {
		return nil
	}
	return &registry.Swarm{ID: id, Roles: []registry.SwarmRole{{
		Name: "worker", Runtime: registry.SwarmRoleRuntime{Image: "localhost/vornik-agent:test"},
	}}}
}
func (s6Resolver) GetWorkflow(id string) *registry.Workflow {
	if id != "s6-wf" {
		return nil
	}
	return &registry.Workflow{
		ID:         id,
		Entrypoint: "run",
		Steps:      map[string]registry.WorkflowStep{"run": {Type: "agent", Role: "worker", OnSuccess: "done"}},
		Terminals:  map[string]registry.WorkflowTerminal{"done": {Status: "COMPLETED"}},
	}
}

// s6RunTaskToMergeBack runs one task through Execute() against the project at
// <root>/<projectID> and waits for it to complete. It returns what the fake
// runtime was started with.
func s6RunTaskToMergeBack(t *testing.T, root, projectID, taskID string, payload []byte) []runtime.ContainerConfig {
	t.Helper()
	if err := spawn.RegisterWorkspaceRoot(root); err != nil {
		t.Fatal(err)
	}
	db := sqlitetest.Memory(t)
	taskRepo := sqlite.NewTaskRepository(db.DB)
	execRepo := sqlite.NewExecutionRepository(db.DB)
	artifactRepo := sqlite.NewArtifactRepository(db.DB)

	cfg := executor.DefaultConfig()
	cfg.ProjectWorkspacePath = root
	cfg.RetryDelay = 0
	rt := &s6Runtime{}
	e := executor.NewWithOptions(rt, execRepo, artifactRepo, taskRepo, cfg)
	e.SetWorkflowResolver(s6Resolver{projectID: projectID})

	if payload == nil {
		payload = []byte(`{"taskType":"x","context":{"prompt":"write agent.txt"}}`)
	}
	now := time.Now()
	if err := taskRepo.Create(context.Background(), &persistence.Task{
		ID: taskID, ProjectID: projectID, Status: persistence.TaskStatusLeased,
		Priority: 50, CreationSource: persistence.TaskCreationSourceUser,
		Attempt: 1, MaxAttempts: 1, Payload: payload, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create task: %v", err)
	}
	if err := e.Execute(taskID); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		task, err := taskRepo.Get(context.Background(), taskID)
		if err == nil && task != nil && task.Status == persistence.TaskStatusCompleted {
			break
		}
		if err == nil && task != nil && (task.Status == persistence.TaskStatusFailed || task.Status == persistence.TaskStatusCancelled) {
			t.Fatalf("task ended %s: %v", task.Status, derefStr(task.LastError))
		}
		if time.Now().After(deadline) {
			status := "?"
			if task != nil {
				status = string(task.Status)
			}
			t.Fatalf("task did not complete in time (status %s)", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	return rt.started()
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// assertMergedBack checks the post-merge-back state of the project: agent.txt
// is on HEAD through a merge commit, the worktree and its branch are gone,
// and the checkout is clean.
func assertMergedBack(t *testing.T, project, taskID string, started []runtime.ContainerConfig) {
	t.Helper()
	if len(started) != 1 {
		t.Fatalf("want exactly one container start, got %d", len(started))
	}
	c := started[0]
	if want := filepath.Join(project, ".worktrees", taskID); c.ProjectDir != want {
		t.Errorf("container ProjectDir = %q, want the task worktree %q", c.ProjectDir, want)
	}
	if want := filepath.Join(project, ".git"); c.ProjectGitDir != want {
		t.Errorf("container ProjectGitDir = %q, want %q", c.ProjectGitDir, want)
	}
	if got := s6Git(t, project, "show", "HEAD:agent.txt"); got != "written by the agent" {
		t.Errorf("HEAD:agent.txt = %q", got)
	}
	parents := strings.Fields(s6Git(t, project, "rev-list", "--parents", "-n", "1", "HEAD"))
	if len(parents) != 3 {
		t.Errorf("HEAD is not a merge commit (rev-list --parents: %v)", parents)
	}
	if _, err := os.Stat(filepath.Join(project, ".worktrees", taskID)); !os.IsNotExist(err) {
		t.Errorf("worktree directory still present after merge-back (stat err %v)", err)
	}
	if got := s6Git(t, project, "branch", "--list", "worktree/"+taskID); got != "" {
		t.Errorf("branch worktree/%s still present: %q", taskID, got)
	}
	if got := s6Git(t, project, "status", "--porcelain"); got != "" {
		t.Errorf("project checkout is not clean after merge-back:\n%s", got)
	}
}

// TestS6E2E_WorktreeToMergeBackThroughExecute: the task runs in its own
// worktree, the agent's uncommitted file is auto-committed and merged back into
// the project, and the worktree and branch are cleaned up.
func TestS6E2E_WorktreeToMergeBackThroughExecute(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	s6DaemonGitHome(t)
	root := t.TempDir()
	const projectID = "s6proj"
	project := filepath.Join(root, projectID)
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	s6Git(t, project, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s6Git(t, project, "add", "-A")
	s6Git(t, project, "commit", "-q", "-m", "seed")

	const taskID = "task_s6_e2e_merge"
	started := s6RunTaskToMergeBack(t, root, projectID, taskID, nil)
	assertMergedBack(t, project, taskID, started)
}

// s6Discloser supplies the publication notice the publish step requires.
type s6Discloser struct{}

func (s6Discloser) PublicationNotice() aidisclosure.Notice {
	return aidisclosure.Notice{Text: "AI-generated (test notice)"}
}

// s6Resolve returns the one provider for every project.
type s6Resolve struct{ p forge.ForgeProvider }

func (r s6Resolve) ForgeProvider(context.Context, string) (forge.ForgeProvider, error) {
	return r.p, nil
}

// s6Source is the service's forgePublishSource: the project clone at its HEAD.
type s6Source struct{ dir string }

func (s s6Source) PublishSource(ctx context.Context, _ *persistence.Task) (string, string, error) {
	sha, err := forgeh.WorkspaceHead(ctx, s.dir)
	return s.dir, sha, err
}

// TestS6E2E_ForgePublishChain: the forge publish chain end to end on real git.
// The child's work merges back into the project clone through Execute(), then
// forge.open_change_request reads the clone's HEAD (WorkspaceHead), counts the
// commits beyond the base, and pushes the branch through the real GitHub
// provider's git push to a temp bare remote; the API side is an httptest fake.
func TestS6E2E_ForgePublishChain(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	s6DaemonGitHome(t)
	root := t.TempDir()
	const projectID = "s6forge"
	bare := filepath.Join(root, "remote.git")
	project := filepath.Join(root, projectID)
	s6Git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	s6Git(t, root, "clone", "-q", bare, project)
	if err := os.WriteFile(filepath.Join(project, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s6Git(t, project, "add", "-A")
	s6Git(t, project, "commit", "-q", "-m", "seed")
	s6Git(t, project, "push", "-q", "origin", "main")

	job := forge.ForgeJob{Provider: forge.ProviderGitHub, Repo: "o/r", Action: "opened", Number: 7, DefaultBranch: "main"}
	payload, err := json.Marshal(map[string]any{
		"taskType":  "x",
		"context":   map[string]string{"prompt": "fix issue #7"},
		"forge_job": job,
	})
	if err != nil {
		t.Fatal(err)
	}
	const taskID = "task_s6_e2e_publish"
	started := s6RunTaskToMergeBack(t, root, projectID, taskID, payload)
	assertMergedBack(t, project, taskID, started)
	head := s6Git(t, project, "rev-parse", "HEAD")

	// The GitHub API fake: installation-token mint, open-PR list (none), create.
	var mu sync.Mutex
	var created map[string]any
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "tok", "expires_at": time.Now().Add(time.Hour)})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls":
			_, _ = w.Write([]byte("[]"))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/pulls":
			mu.Lock()
			_ = json.NewDecoder(r.Body).Decode(&created)
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":42,"html_url":"https://example.invalid/o/r/pull/42","state":"open"}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer api.Close()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	provider, err := github.New(forge.GitHubConfig{AppID: 1, InstallationID: 2, PrivateKeyPath: keyPath, APIBaseURL: api.URL})
	if err != nil {
		t.Fatalf("github.New: %v", err)
	}

	h := forgeh.NewOpenChangeRequestHandler(s6Resolve{p: provider}, s6Source{dir: project}, nil, nil, s6Discloser{})
	res, err := h.Execute(context.Background(), executor.SystemStepInput{
		Task: &persistence.Task{ID: taskID, ProjectID: projectID, Payload: payload},
	})
	if err != nil {
		t.Fatalf("open_change_request: %v", err)
	}
	var out struct {
		State  string `json:"state"`
		Branch string `json:"branch"`
		CRURL  string `json:"cr_url"`
	}
	if err := json.Unmarshal(res.Result, &out); err != nil {
		t.Fatalf("result %s: %v", res.Result, err)
	}
	if out.State != "opened" {
		t.Fatalf("state = %q, want opened (result %s)", out.State, res.Result)
	}
	if out.Branch != "fix/issue-7" {
		t.Errorf("branch = %q, want fix/issue-7", out.Branch)
	}
	if got := s6Git(t, bare, "rev-parse", "refs/heads/"+out.Branch); got != head {
		t.Errorf("remote %s = %s, want the project HEAD %s", out.Branch, got, head)
	}
	mu.Lock()
	defer mu.Unlock()
	if created == nil || created["head"] != out.Branch || created["base"] != "main" {
		t.Errorf("create-PR body = %v", created)
	}
}
