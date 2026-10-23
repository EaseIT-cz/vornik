//go:build integration
// +build integration

package integration_test

// GitHub #368, design 22 "Every search path honours repo_scope and
// strict_scope". Found at: review (the #142 census, wave 8 lane N,
// 2026-10-14; re-read 2026-10-16). Regression test at: the companion MCP
// tools/call handler, through the production adapter, Searcher and API-key
// repository on the real pgvector database: the level a client observes.
//
//   - W2: a strict recall that finds nothing in scope is a low verdict, so the
//     routing widen ran with StrictScope=false and returned the NULL-scoped
//     chunk the caller had excluded.
//   - E4: memory_correct with wrong_claim searched the whole project, so a
//     claim scoped to repository A refuted repository B's chunk.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/service"
)

const (
	gh368ScopeA = "github.com/example/repo-a"
	gh368ScopeB = "github.com/example/repo-b"
)

type gh368Harness struct {
	db      *sql.DB
	srv     *api.Server
	raw     string
	project string
}

func newGH368Harness(t *testing.T) *gh368Harness {
	t.Helper()
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	project := fmt.Sprintf("itest-gh368-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM artifacts WHERE project_id = $1`, project)
	})

	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""                     // keyword-only recall: deterministic
	searcher := memory.NewSearcher(cfg, repo, nil) // routing on by default
	// The production companion wiring (container_scheduler.go): a secret
	// detector on the indexer and the synthetic artifacts row a companion
	// deposit needs (GitHub #436: a companion correction is a companion
	// deposit and goes through the same pipeline).
	indexer := memory.NewIndexer(cfg, repo, nil, zerolog.Nop())
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	indexer.SetSecrets(det, nil)
	pipeline := memory.NewPipeline(indexer, memory.PipelineConfig{
		Logger: zerolog.Nop(),
		CreateCompanionArtifact: func(ctx context.Context, projectID, artifactID, sourceName string, _ int64) error {
			_, err := db.ExecContext(ctx, `
				INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, size_bytes)
				VALUES ($1, $2, $3, 'METADATA', 'companion://inline', 0)`, artifactID, projectID, sourceName)
			return err
		},
	})
	adapter := service.NewMemoryCompanionAdapterForTesting(searcher, pipeline, repo, 0)
	require.NotNil(t, adapter)

	keys := postgres.NewAPIKeyRepository(db)
	raw, err := apikey.Generate(project)
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID:           persistence.GenerateID("akey-gh368"),
		ProjectID:    project,
		Name:         "gh368",
		KeyHash:      apikey.Hash(raw),
		KeyPrefix:    apikey.DisplayPrefix(raw),
		ClientKind:   "claude-code",
		SessionLabel: "itest/gh368",
		MemoryRead:   true,
		MemoryWrite:  true,
		CreatedAt:    time.Now().UTC(),
	}))
	srv := api.NewServer(api.WithAPIKeyRepository(keys), api.WithMemoryCompanionAdapter(adapter))
	return &gh368Harness{db: db, srv: srv, raw: raw, project: project}
}

func (h *gh368Harness) chunk(t *testing.T, id, scope, content string) string {
	t.Helper()
	id = h.project + "-" + id
	var s any
	if scope != "" {
		s = scope
	}
	_, err := h.db.Exec(`
		INSERT INTO project_memory_chunks (id, project_id, source_name, chunk_index, content, content_hash, repo_scope, created_at)
		VALUES ($1, $2, $3, 0, $4, $5, $6, NOW())`, id, h.project, id+".md", content, "hash-"+id, s)
	require.NoError(t, err, "insert chunk %s", id)
	return id
}

func (h *gh368Harness) call(t *testing.T, tool string, args map[string]any) map[string]any {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/companion", strings.NewReader(string(body)))
	req = req.WithContext(api.ContextWithAPIKeyForTesting(req.Context(), h.raw))
	rec := httptest.NewRecorder()
	h.srv.CompanionMCPHandler(rec, req)
	var resp struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	require.Nil(t, resp.Error, rec.Body.String())
	require.NotEmpty(t, resp.Result.Content, rec.Body.String())
	require.False(t, resp.Result.IsError, resp.Result.Content[0].Text)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(resp.Result.Content[0].Text), &out), resp.Result.Content[0].Text)
	return out
}

func recallHitIDs(out map[string]any) []string {
	hits, _ := out["hits"].([]any)
	ids := make([]string, 0, len(hits))
	for _, h := range hits {
		m, _ := h.(map[string]any)
		id, _ := m["chunk_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestIntegration_CompanionStrictRecallSurvivesWiden(t *testing.T) {
	h := newGH368Harness(t)
	legacy := h.chunk(t, "legacy", "", "The kelpwarden relay legacy note without a scope says port 7104 on every node.")
	h.chunk(t, "a", gh368ScopeA, "The zorbquill relay for repository alpha listens on port 7101.")

	strict := h.call(t, "recall", map[string]any{"query": "kelpwarden", "repo_scope": gh368ScopeA, "strict_scope": true})
	rounds, _ := strict["widen_rounds"].(float64)
	require.Greater(t, rounds, float64(0), "the widen did not run; the test does not exercise W2: %v", strict)
	require.Empty(t, recallHitIDs(strict), "a strict recall returned a NULL-scoped chunk")

	// The chunk is there to find: the lenient recall returns it.
	lenient := h.call(t, "recall", map[string]any{"query": "kelpwarden", "repo_scope": gh368ScopeA})
	require.Equal(t, []string{legacy}, recallHitIDs(lenient))
}

func TestIntegration_CompanionMemoryCorrectStaysInScope(t *testing.T) {
	h := newGH368Harness(t)
	other := h.chunk(t, "b", gh368ScopeB, "The quillgate relay for repository bravo listens on port 7102 on every node.")
	// Shares no content word with the claim, so a scoped claim search has
	// nothing in A to refute.
	h.chunk(t, "a", gh368ScopeA, "Zorbquill gateway alpha uses socket 9000 behind the cache tier.")

	out := h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "quillgate relay listens on port 7102",
		"repo_scope":  gh368ScopeA,
	})
	refuted, _ := out["refuted"].([]any)
	require.Empty(t, refuted, "a claim scoped to repository A refuted another repository's chunk: %v", out)

	var status string
	require.NoError(t, h.db.QueryRow(`SELECT validation_status FROM project_memory_chunks WHERE id = $1`, other).Scan(&status))
	require.NotEqual(t, "refuted", status)

	// Scoped to B, the same claim does refute it: the search is scoped, not broken.
	out = h.call(t, "memory_correct", map[string]any{
		"wrong_claim": "quillgate relay listens on port 7102",
		"repo_scope":  gh368ScopeB,
	})
	refuted, _ = out["refuted"].([]any)
	require.Len(t, refuted, 1, "%v", out)
}
