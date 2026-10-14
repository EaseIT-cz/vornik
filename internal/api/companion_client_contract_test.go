package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentbench"
	"vornik.io/vornik/internal/membench"
	"vornik.io/vornik/internal/persistence"
)

// The in-repo Go CLIENTS of the companion MCP surface, driven against the
// REAL handler (LLD 21 §A amendment 2026-10-14, GitHub #305 follow-up).
//
// Found at: production-like. The 2026.10.6 release memory benchmark
// (membench daemon at aa2dc8d5d, 2026-10-14, membench-2026.10.6.log) died on
// every recall with `recall: unknown argument "max_tokens"`: #305 made the
// tools strict, the harness had always sent an argument recall never
// declared, and the harness's own fake server accepted anything. The census
// that #305 shipped covered the plugin bundles only, so the Go clients (the
// memory benchmark and the agent benchmark, whose delegate sends "project")
// were never examined.
//
// This test cannot be fooled by a permissive fake: the server IS the
// daemon's handler, and every call the clients make is also re-checked
// against the declared-argument registry, so a tolerated legacy name still
// fails here.

// contractRecorder wraps the real companion handler, stamping the bearer the
// way AuthMiddleware does and recording each call and its answer.
type contractRecorder struct {
	mu    sync.Mutex
	calls []contractCall
}

type contractCall struct {
	Tool    string
	Args    json.RawMessage
	Text    string
	IsError bool
}

func (c *contractRecorder) wrap(t *testing.T, srv *Server, raw string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/mcp/companion" {
			// Admin routes a client falls back to (the memory benchmark's
			// /api/v1/memory/stats) are refused to a companion key.
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		var env struct {
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		require.NoError(t, json.Unmarshal(body, &env))
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := httptest.NewRecorder()
		srv.CompanionMCPHandler(rec, withCompanionBearer(r, raw))
		if env.Method == "tools/call" {
			text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
			c.mu.Lock()
			c.calls = append(c.calls, contractCall{Tool: env.Params.Name, Args: env.Params.Arguments, Text: text, IsError: isErr})
			c.mu.Unlock()
		}
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

// assertDeclared fails on any call the handler refused for an argument, and
// on any call carrying an argument the tool does not declare (the second
// catches a name the daemon tolerates for an old client: an in-repo client
// must not depend on a tolerance).
func (c *contractRecorder) assertDeclared(t *testing.T) map[string]int {
	t.Helper()
	perTool := map[string]int{}
	for _, call := range c.calls {
		perTool[call.Tool]++
		assert.NotContainsf(t, call.Text, "unknown argument", "%s was refused an argument: %s", call.Tool, call.Text)
		assert.NoErrorf(t, CheckCompanionToolArgs(call.Tool, call.Args), "%s sent %s", call.Tool, call.Args)
	}
	return perTool
}

func logDenominator(t *testing.T, client string, perTool map[string]int) {
	t.Helper()
	tools := make([]string, 0, len(perTool))
	total := 0
	for tool, n := range perTool {
		tools = append(tools, tool)
		total += n
	}
	sort.Strings(tools)
	t.Logf("%s: examined %d calls across tools %v", client, total, tools)
}

func TestCompanionClientContract_MemoryBenchmarkAdapter(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	srv.memoryCompanion = &fakeMemoryCompanion{
		recallReturn: []MemorySearchResult{{ChunkID: "c1", ProjectID: "alpha", SourceName: "q1_s1", Content: "hit", Score: 0.9}},
		rememberOut:  RememberResult{},
	}
	raw, _ := seedCompanionKeyWithCaps(t, keyRepo, "alpha", nil, true, true)

	var rec contractRecorder
	hs := httptest.NewServer(rec.wrap(t, srv, raw))
	t.Cleanup(hs.Close)

	ctx := context.Background()
	for _, noRerank := range []bool{false, true} {
		sys := membench.NewVornikSystem(membench.VornikConfig{BaseURL: hs.URL, Token: raw, Client: hs.Client(), NoRerank: noRerank})
		_, err := sys.Ingest(ctx, "lme/q1", []membench.Item{{
			DocumentID: "q1_s1", Context: "Session 1", Content: "Alice moved to Berlin",
			EventTime: time.Date(2023, 5, 14, 9, 30, 0, 0, time.UTC),
		}})
		require.NoError(t, err)
		// Every Query field set: the budget (the incident), the date window.
		_, err = sys.Recall(ctx, "lme/q1", membench.Query{
			Text: "where does Alice live?", MaxTokens: 4096,
			From: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC), To: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC),
		})
		require.NoError(t, err, "the 2026.10.6 release-benchmark failure")
		// The whoami-backed probes (their errors are about fields this
		// fixture does not populate, not about arguments).
		_, _ = sys.WriteTargetDatabase(ctx)
		_, _ = sys.WriteTargetProject(ctx)
		_, _ = sys.EmbeddingReadiness(ctx)
		_, _ = sys.ObservedEmbedder(ctx)
		_, _ = sys.PendingIngest(ctx)
		_ = sys.DaemonRevision(ctx)
	}
	logDenominator(t, "membench", rec.assertDeclared(t))
}

func TestCompanionClientContract_AgentBenchmarkRunner(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	taskRepo.GetFunc = func(_ context.Context, id string) (*persistence.Task, error) {
		return &persistence.Task{ID: id, ProjectID: "alpha", Status: persistence.TaskStatusCompleted,
			CreationSource: persistence.TaskCreationSourceCompanion}, nil
	}

	var rec contractRecorder
	hs := httptest.NewServer(rec.wrap(t, srv, raw))
	t.Cleanup(hs.Close)

	attachment := filepath.Join(t.TempDir(), "diff.patch")
	require.NoError(t, os.WriteFile(attachment, []byte("--- a\n+++ b\n"), 0o600))

	runner := agentbench.NewDaemonTaskRunner(agentbench.DaemonConfig{
		BaseURL: hs.URL, Token: raw, Project: "alpha",
		PollInterval: time.Millisecond, Timeout: 5 * time.Second, HTTPClient: hs.Client(),
	})
	ctx := context.Background()
	_, _ = runner.WriteTargetDatabase(ctx)
	for _, spec := range []agentbench.TaskSpec{
		{ID: "t1", Workflow: "wf-alpha", Prompt: "p"},
		{ID: "t2", Workflow: "wf-alpha", Prompt: "p", Attachments: []string{attachment}},
	} {
		out, err := runner.Run(ctx, spec)
		require.NoError(t, err, "submit %s: the agent benchmark's delegate must be accepted", spec.ID)
		assert.Truef(t, out.Succeeded, "%s: %+v", spec.ID, out)
	}
	perTool := rec.assertDeclared(t)
	assert.Positive(t, perTool["delegate"])
	assert.Positive(t, perTool["status"])
	logDenominator(t, "agentbench", perTool)
}

// GitHub #378 (2026.10.6 delta audit, #334): the agent benchmark cleared
// --project's store while its companion key ran every task in the key's own
// project. Against the REAL whoami handler, a key for "alpha" must be refused
// for --project "beta" (naming both) and accepted for "alpha", whether or not
// WriteTargetDatabase already fetched the reply.
func TestCompanionClientContract_AgentBenchmarkKeyProject(t *testing.T) {
	srv, keyRepo, _ := newCompanionMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-alpha"})
	var rec contractRecorder
	hs := httptest.NewServer(rec.wrap(t, srv, raw))
	t.Cleanup(hs.Close)

	for _, tc := range []struct {
		project     string
		whoamiFirst bool
		wantRefusal bool
	}{
		{"beta", true, true},
		{"beta", false, true},
		{"alpha", true, false},
		{"alpha", false, false},
	} {
		runner := agentbench.NewDaemonTaskRunner(agentbench.DaemonConfig{
			BaseURL: hs.URL, Token: raw, Project: tc.project, HTTPClient: hs.Client(),
		})
		ctx := context.Background()
		if tc.whoamiFirst {
			_, _ = runner.WriteTargetDatabase(ctx)
		}
		err := runner.VerifyKeyProject(ctx)
		if !tc.wantRefusal {
			require.NoErrorf(t, err, "project %s (whoamiFirst=%v)", tc.project, tc.whoamiFirst)
			continue
		}
		require.Errorf(t, err, "a key for alpha must not run --project %s", tc.project)
		assert.Contains(t, err.Error(), `"alpha"`)
		assert.Contains(t, err.Error(), `"beta"`)
	}
	rec.assertDeclared(t)
}
