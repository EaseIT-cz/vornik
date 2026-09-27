package api

import (
	"bytes"
	"encoding/base64"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/taskcreate"
)

// The observed run: 248 KB of staged artifacts against a 100k context, which
// the container's own preflight measured at 93,346 prompt tokens on iteration 1
// and proceeded anyway — four iterations, ~$0.27, and a shape-contract failure
// instead of a review.
func TestStagedArtifactBudget_RefusesTheObservedRun(t *testing.T) {
	err := checkStagedArtifactBudget("companion-architectural-review", 248*1024, 100000, 16384)
	if err == nil {
		t.Fatal("248 KB against a 100k context was admitted")
	}
	for _, want := range []string{"ARTIFACTS_EXCEED_CONTEXT", "248 KB", "Split the upload"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal missing %q: %v", want, err)
		}
	}
}

// The workaround that actually worked on the day — three diff-only reviews of
// ~39 KB, ~51 KB and ~76 KB — must all pass, or the guard would have refused
// the remedy it recommends.
func TestStagedArtifactBudget_AdmitsTheSplitThatWorked(t *testing.T) {
	for _, kb := range []int{39, 51, 76} {
		if err := checkStagedArtifactBudget("companion-architectural-review", kb*1024, 100000, 16384); err != nil {
			t.Fatalf("%d KB was refused, but this split is what the guard tells callers to do: %v", kb, err)
		}
	}
}

// Unmeasurable is not refusable. A daemon that does not declare a context size
// must not have every artifact delegation fail.
func TestStagedArtifactBudget_UnknownContextAdmits(t *testing.T) {
	if err := checkStagedArtifactBudget("wf", 10*1024*1024, 0, 0); err != nil {
		t.Fatalf("an unknown context size became a refusal: %v", err)
	}
	if err := checkStagedArtifactBudget("wf", 0, 100000, 16384); err != nil {
		t.Fatalf("a delegation with no artifacts was refused: %v", err)
	}
}

// A large context is exactly what a big upload is for. The guard must scale
// with the window rather than enforcing a fixed byte ceiling.
func TestStagedArtifactBudget_ScalesWithTheWindow(t *testing.T) {
	if err := checkStagedArtifactBudget("wf", 248*1024, 1000000, 32768); err != nil {
		t.Fatalf("248 KB against a 1M context was refused: %v", err)
	}
}

// The numbers must be IN the message: a caller that is told only "too large"
// cannot tell a 10% overshoot from a 10x one, and the split differs.
func TestStagedArtifactBudget_RefusalCarriesItsArithmetic(t *testing.T) {
	err := checkStagedArtifactBudget("wf", 400*1024, 100000, 16384)
	if err == nil {
		t.Fatal("want a refusal")
	}
	msg := err.Error()
	// ~400 KB / 3 ≈ 136k tokens against a ceiling of (100000-16384)/2 ≈ 41808.
	if !strings.Contains(msg, "136") || !strings.Contains(msg, "41808") {
		t.Fatalf("the refusal does not carry its arithmetic: %s", msg)
	}
}

// ingestGuardRegistry has one agent-less ingest workflow and one agent workflow,
// both artifact-only.
func ingestGuardRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	root := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, d), 0o755))
	}
	files := map[string]string{
		"swarms/swarm.md": "---\nswarmId: swarm-1\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n",
		"workflows/wf-ingest.md": "---\nworkflowId: wf-ingest\nentrypoint: done\nrequire_input_artifacts: true\n" +
			"ingest_input_artifacts: true\nterminals:\n  done:\n    status: COMPLETED\n---\n",
		"workflows/wf-review.md": "---\nworkflowId: wf-review\nentrypoint: run\nrequire_input_artifacts: true\nsteps:\n  run:\n" +
			"    type: agent\n    prompt: \"review\"\n    role: worker\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n",
		"projects/alpha.yaml": "projectId: alpha\ndisplayName: Alpha\nswarmId: swarm-1\ndefaultWorkflowId: wf-review\ndefaultPriority: 50\n",
	}
	for name, body := range files {
		require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte(body), 0o644))
	}
	reg := registry.New()
	require.NoError(t, reg.Load(root))
	return reg
}

func delegateLargeUpload(t *testing.T, workflow string) (string, bool) {
	t.Helper()
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	reg := ingestGuardRegistry(t)
	srv.projectRegistry = reg
	srv.taskCreator = taskcreate.New(taskcreate.WithTaskRepository(taskRepo), taskcreate.WithProjectRegistry(reg))
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	srv.config = &config.Config{Runtime: config.RuntimeConfig{AgentLLM: config.AgentLLMConfig{ContextSize: 100000, MaxTokens: 16384}}}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{workflow})
	doc := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("x"), 280*1024)) // the benchmark LLD's size
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow":       workflow,
			"prompt":         "ingest the staged design",
			"inputArtifacts": []map[string]any{{"name": "design.md", "content": doc}},
		},
	}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	return decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
}

// The 2026-09-26 regression: the guard refused companion-rag-ingest, which
// has no agent step, so no document over about 125 KB could reach RAG.
func TestStagedArtifactBudget_AgentlessIngestIsExempt(t *testing.T) {
	text, isErr := delegateLargeUpload(t, "wf-ingest")
	require.False(t, isErr, "an agent-less ingest reads nothing into a context; got: %s", text)
}

func TestStagedArtifactBudget_AgentWorkflowIsStillRefused(t *testing.T) {
	text, isErr := delegateLargeUpload(t, "wf-review")
	require.True(t, isErr, "an agent that reads the upload must still be protected")
	assert.Contains(t, text, "ARTIFACTS_EXCEED_CONTEXT")
}

// An unknown workflow cannot be shown agent-less, so it stays measured
// (review-20260926-405f F3).
func TestStagedArtifactBudget_UnknownWorkflowIsStillRefused(t *testing.T) {
	text, isErr := delegateLargeUpload(t, "wf-not-in-the-registry")
	require.True(t, isErr, "an unknown workflow must not be exempt")
	assert.Contains(t, text, "ARTIFACTS_EXCEED_CONTEXT")
}
