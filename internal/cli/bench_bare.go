package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/spawn"
)

// vornikctl bench agent bare (benchmark design, Amendment 2026-11-16, GitHub
// #650): the bare-model baseline. The same model, alone, on the same borrowed
// items, graded by the same hidden suites in the agent image, so harness lift
// is the swarm's pass rate minus this one's pass@1.

var (
	bareTasksPath     string
	bareMode          string
	bareRepeats       int
	bareMatchedTokens int
	bareEndpoint      string
	bareModel         string
	bareImage         string
	bareJournalPath   string
	bareArmName       string
	bareRunID         string
)

var benchAgentBareCmd = &cobra.Command{
	Use:   "bare",
	Short: "Run the bare-model baseline on a task set's seeded items",
	Long: `Runs the arm's model alone on every task that has a seed (a borrowed
exercise's stubs) and grades each reply with the task's hidden acceptance
suite, inside the agent image without network, exactly as a swarm arm is
graded.

  --mode bare-pass1    one call, no test feedback (the comparison with the
                       swarm, which never sees the tests either)
  --mode bare-pass2    a second call shown the grader's output after a model
                       failure (Aider's protocol)
  --mode bare-matched  independent calls until --matched-tokens (the swarm
                       arm's median tokens per item) is spent; solved if any
                       passes

The journal records scaffold=<mode>, so bench agent compare refuses to set it
against a swarm journal. The endpoint and model default to
VORNIK_BENCH_MODEL_ENDPOINT and VORNIK_BENCH_MODEL; the key is read from
VORNIK_BENCH_MODEL_API_KEY.`,
	RunE: runBenchAgentBare,
}

func init() {
	f := benchAgentBareCmd.Flags()
	f.StringVar(&bareTasksPath, "tasks", "", "task set (only tasks with a seed and a suite run)")
	f.StringVar(&bareMode, "mode", agentbench.ScaffoldBarePass1, "bare-pass1, bare-pass2 or bare-matched")
	f.IntVar(&bareRepeats, "repeats", 3, "runs per task")
	f.IntVar(&bareMatchedTokens, "matched-tokens", 0, "bare-matched: the swarm arm's median tokens per item")
	f.StringVar(&bareEndpoint, "endpoint", os.Getenv("VORNIK_BENCH_MODEL_ENDPOINT"), "OpenAI-compatible endpoint (…/v1)")
	f.StringVar(&bareModel, "model", os.Getenv("VORNIK_BENCH_MODEL"), "model ID to send")
	f.StringVar(&bareImage, "acceptance-image", os.Getenv("VORNIK_BENCH_AGENT_IMAGE"), "agent image the hidden suites run in")
	f.StringVar(&bareJournalPath, "journal", "bare-journal.json", "where to write the journal")
	f.StringVar(&bareArmName, "arm", "", "arm name (required)")
	f.StringVar(&bareRunID, "run-id", "", "identifier for this run")
	benchAgentCmd.AddCommand(benchAgentBareCmd)
}

func runBenchAgentBare(cmd *cobra.Command, _ []string) error {
	if bareArmName == "" || bareEndpoint == "" || bareModel == "" || bareImage == "" {
		return errors.New("--arm, --endpoint, --model and --acceptance-image are required")
	}
	all, err := loadTaskSet(bareTasksPath)
	if err != nil {
		return err
	}
	var tasks []agentbench.TaskSpec
	for _, t := range all {
		if t.Seed != "" && t.Acceptance != "" {
			tasks = append(tasks, t)
		}
	}
	if len(tasks) == 0 {
		return fmt.Errorf("task set %s has no task with a seed and a suite: the bare baseline completes seeded items", bareTasksPath)
	}
	if err := agentbench.ValidateTaskTargets(tasks, true); err != nil {
		return err
	}
	// The digests describe the whole task-set file, as a swarm arm's do, so
	// a bare and a swarm journal of one set share them and only the scaffold
	// axis tells them apart (review-20261116-a5b7 L2).
	ids := make([]string, 0, len(all))
	bodies := make(map[string]string, len(all))
	for _, t := range all {
		body, err := agentbench.TaskBody(t)
		if err != nil {
			return err
		}
		ids = append(ids, t.ID)
		bodies[t.ID] = body
	}
	suites, err := agentbench.AcceptanceSetDigest(all)
	if err != nil {
		return err
	}
	root, err := os.MkdirTemp("", "vornik-bare-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := spawn.RegisterWorkspaceRoot(root); err != nil {
		return err
	}
	cfg := agentbench.BareConfig{
		RunID: bareRunID, Tasks: tasks, Repeats: bareRepeats, Mode: bareMode, MatchedTokens: bareMatchedTokens,
		Model: httpBareModel{endpoint: bareEndpoint, model: bareModel, apiKey: os.Getenv("VORNIK_BENCH_MODEL_API_KEY")},
		Arm: agentbench.ArmFields{
			HarnessVersion: agentbench.HarnessVersion, Name: bareArmName,
			Models:           map[string]string{"bare": bareModel},
			AgentImages:      map[string]string{"acceptance": bareImage},
			ContextPolicy:    "bare",
			TaskSetSHA256:    agentbench.TaskSetDigest(ids, bodies),
			AcceptanceSHA256: suites,
		},
		NewWorkspace: func(ctx context.Context, spec agentbench.TaskSpec) (agentbench.BareWorkspace, error) {
			return newGitBareWorkspace(ctx, root, spec, bareImage)
		},
	}
	j, runErr := agentbench.RunBare(cmd.Context(), cfg)
	if runErr != nil && len(j.TaskRuns) == 0 {
		return runErr
	}
	// A cancelled arm still writes what it finished, then reports the
	// cancellation (review-20261116-8777 F2).
	f, err := os.Create(filepath.Clean(bareJournalPath))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := j.Write(f); err != nil {
		return err
	}
	passed := 0
	for _, r := range j.TaskRuns {
		if r.Succeeded {
			passed++
		}
	}
	_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s: %d of %d item runs passed (%d tasks x %d repeats); journal %s\n",
		bareMode, passed, len(j.TaskRuns), len(tasks), bareRepeats, bareJournalPath)
	if runErr != nil {
		return fmt.Errorf("the arm stopped after %d item runs (journal written): %w", len(j.TaskRuns), runErr)
	}
	return nil
}

// gitBareWorkspace is one fresh repository seeded by the same Prepare a swarm
// arm uses, committed once after the reply is written, and graded by the same
// podman grader, which exports that HEAD.
type gitBareWorkspace struct {
	ws     gitWorkspace
	target string
	grader podmanGrader
}

func newGitBareWorkspace(ctx context.Context, root string, spec agentbench.TaskSpec, image string) (*gitBareWorkspace, error) {
	dir, err := os.MkdirTemp(root, "item-")
	if err != nil {
		return nil, err
	}
	ws := gitWorkspace{dir: dir}
	if _, err := ws.Prepare(ctx, spec, 1); err != nil {
		return nil, err
	}
	return &gitBareWorkspace{ws: ws, target: filepath.Clean(spec.Targets[0]),
		grader: podmanGrader{workspace: dir, image: image, memoryLimit: oneSlotMemoryLimit()}}, nil
}

func (w *gitBareWorkspace) Write(rel, content string) error {
	return os.WriteFile(filepath.Join(w.ws.dir, w.target, filepath.FromSlash(rel)), []byte(content), 0o644)
}

func (w *gitBareWorkspace) Commit(ctx context.Context) error {
	if _, err := w.ws.git(ctx, "add", "-A"); err != nil {
		return err
	}
	_, err := w.ws.git(ctx, "-c", "user.name=vornik-bench", "-c", "user.email=bench@vornik.io",
		"commit", "-q", "--no-gpg-sign", "--allow-empty", "-m", "bare reply")
	return err
}

func (w *gitBareWorkspace) Grade(ctx context.Context, spec agentbench.TaskSpec) agentbench.AcceptanceResult {
	return w.grader.Grade(ctx, spec)
}

func (w *gitBareWorkspace) Close() { _ = os.RemoveAll(w.ws.dir) }

// httpBareModel is one chat completion against the operator's own endpoint,
// as the model-window probe calls it: an explicit timeout and a capped read.
type httpBareModel struct {
	endpoint, model, apiKey string
	client                  *http.Client
}

const (
	bareCallTimeout = 20 * time.Minute // a long completion on one GPU slot
	bareReplyLimit  = 8 << 20
)

func (m httpBareModel) Complete(ctx context.Context, msgs []agentbench.BareMessage) (agentbench.BareReply, error) {
	body, err := json.Marshal(map[string]any{"model": m.model, "messages": msgs})
	if err != nil {
		return agentbench.BareReply{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, bareCallTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, m.endpoint+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return agentbench.BareReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	client := m.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return agentbench.BareReply{}, fmt.Errorf("bare model call: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, bareReplyLimit))
	if err != nil {
		return agentbench.BareReply{}, fmt.Errorf("bare model call: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return agentbench.BareReply{}, fmt.Errorf("bare model call: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || len(out.Choices) == 0 {
		return agentbench.BareReply{}, fmt.Errorf("bare model call: no choice in the reply: %v", err)
	}
	return agentbench.BareReply{Text: out.Choices[0].Message.Content, Tokens: out.Usage.TotalTokens}, nil
}
