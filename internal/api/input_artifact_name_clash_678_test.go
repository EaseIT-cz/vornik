package api

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/taskcreate"
)

// GitHub #678 (memory ingest paths, §9, 2026-11-12): two inline inputs with
// the same file name were both stored, then staged into the agent's flat
// artifacts/in/<name>, where the second overwrote the first. A review was
// sent the Postgres and the SQLite chunk_graph_extraction_repository.go and
// reported that the Postgres file had the SQLite content. A name clash is now
// refused at intake, naming both entries.
//
// Found at: e2e-lane (companion-doc-review, task_20261108074733_0b05fdb32b49a670).
// Regression test at: integration TestInputArtifactNameClash678_DelegateRefuses
// (the delegate's MCP handler).
// Lower because: the clash is decided at intake, before any container runs;
// the handler test exercises the same code an e2e lane would reach.

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestInputArtifactNameClash678_IntakeRefuses(t *testing.T) {
	srv, _, _ := fixtureServerWithExtractor(t)
	_, err := srv.processInputArtifacts(context.Background(), "p", []InputArtifact{
		{Name: "repo.go", Content: b64("postgres"), Path: "internal/persistence/postgres/repo.go"},
		{Name: "repo.go", Content: b64("sqlite"), Path: "internal/persistence/sqlite/repo.go"},
	})
	require.Error(t, err)
	for _, want := range []string{"inputArtifacts[1]", "inputArtifacts[0]", `"repo.go"`} {
		assert.Contains(t, err.Error(), want)
	}
	// Distinct names are unaffected.
	_, err = srv.processInputArtifacts(context.Background(), "p", []InputArtifact{
		{Name: "postgres__repo.go", Content: b64("postgres")},
		{Name: "sqlite__repo.go", Content: b64("sqlite")},
	})
	require.NoError(t, err)
}

func TestInputArtifactNameClash678_DelegateRefuses(t *testing.T) {
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	reg := ingestGuardRegistry(t)
	srv.projectRegistry = reg
	srv.taskCreator = taskcreate.New(taskcreate.WithTaskRepository(taskRepo), taskcreate.WithProjectRegistry(reg))
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	srv.config = &config.Config{Runtime: config.RuntimeConfig{AgentLLM: config.AgentLLMConfig{ContextSize: 100000, MaxTokens: 16384}}}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-review"})
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-review",
			"prompt":   "review both repositories",
			"inputArtifacts": []map[string]any{
				{"name": "repo.go", "content": b64("postgres")},
				{"name": "repo.go", "content": b64("sqlite")},
			},
		},
	}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	require.True(t, isErr, "a delegation whose two inputs share a name must be refused; got: %s", text)
	assert.True(t, strings.Contains(text, "repo.go"), "the refusal names the clashing file: %s", text)
}
