//go:build integration
// +build integration

package integration_test

// GitHub #435, design 22 "A companion deposit never certifies itself".
// Found at: review (security audit 2026-10-17). Regression test at: the
// companion MCP tools/call handler, through the production adapter, pipeline,
// indexer and repository on the real pgvector database: the level a companion
// client reaches. Before the fix remember(class="decision") landed every chunk
// validation_status='verified', validator_role='companion:claude-code',
// confidence 0.9 and expires_at NULL (818 such production chunks, measured
// 2026-10-21).

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
	"vornik.io/vornik/internal/service"
)

type gh435Harness struct {
	db       *sql.DB
	srv      *api.Server
	pipeline *memory.Pipeline
	raw      string
	project  string
}

func gh435SeedArtifact(db *sql.DB, projectID, artifactID, name string) error {
	_, err := db.Exec(`INSERT INTO artifacts (id, project_id, name, artifact_class, storage_path, size_bytes)
VALUES ($1, $2, $3, 'METADATA', 'companion://inline', 0) ON CONFLICT (id) DO NOTHING`, artifactID, projectID, name)
	return err
}

func newGH435Harness(t *testing.T) *gh435Harness {
	t.Helper()
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	project := fmt.Sprintf("itest-gh435-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		_, _ = db.Exec(`DELETE FROM project_memory_chunks WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM artifacts WHERE project_id = $1`, project)
		_, _ = db.Exec(`DELETE FROM api_keys WHERE project_id = $1`, project)
	})

	repo := memory.NewRepository(db)
	cfg := memory.DefaultConfig()
	cfg.Enabled = true
	cfg.EmbeddingEndpoint = ""
	pipeline := memory.NewPipeline(memory.NewIndexer(cfg, repo, nil, zerolog.Nop()), memory.PipelineConfig{
		Logger: zerolog.Nop(),
		// The reviewer control is an operator's task (#482: an unwired
		// origin hook counts as companion-delegated).
		CompanionDelegated: func(context.Context, string) (bool, error) { return false, nil },
		CreateCompanionArtifact: func(_ context.Context, projectID, artifactID, sourceName string, _ int64) error {
			return gh435SeedArtifact(db, projectID, artifactID, sourceName)
		},
	})
	adapter := service.NewMemoryCompanionAdapterForTesting(memory.NewSearcher(cfg, repo, nil), pipeline, repo, 0)

	keys := postgres.NewAPIKeyRepository(db)
	raw, err := apikey.Generate(project)
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID:           persistence.GenerateID("akey-gh435"),
		ProjectID:    project,
		Name:         "gh435",
		KeyHash:      apikey.Hash(raw),
		KeyPrefix:    apikey.DisplayPrefix(raw),
		ClientKind:   "claude-code",
		SessionLabel: "itest/gh435",
		MemoryRead:   true,
		MemoryWrite:  true,
		CreatedAt:    time.Now().UTC(),
	}))
	srv := api.NewServer(api.WithAPIKeyRepository(keys), api.WithMemoryCompanionAdapter(adapter))
	return &gh435Harness{db: db, srv: srv, pipeline: pipeline, raw: raw, project: project}
}

func (h *gh435Harness) remember(t *testing.T, args map[string]any) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "remember", "arguments": args},
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
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), rec.Body.String())
	require.NotEmpty(t, resp.Result.Content, rec.Body.String())
	require.False(t, resp.Result.IsError, resp.Result.Content[0].Text)
	var out struct {
		Decision   string `json:"decision"`
		ArtifactID string `json:"artifact_id"`
	}
	require.NoError(t, json.Unmarshal([]byte(resp.Result.Content[0].Text), &out), resp.Result.Content[0].Text)
	require.Equal(t, "ALLOW", out.Decision, resp.Result.Content[0].Text)
	require.NotEmpty(t, out.ArtifactID, resp.Result.Content[0].Text)
	return out.ArtifactID
}

type gh435Row struct {
	status     string
	validator  sql.NullString
	confidence float64
	expires    sql.NullTime
	class      string
}

func (h *gh435Harness) rows(t *testing.T, artifactID string) []gh435Row {
	t.Helper()
	rs, err := h.db.Query(`SELECT validation_status, validator_role, confidence, expires_at, content_class
FROM project_memory_chunks WHERE project_id = $1 AND artifact_id = $2`, h.project, artifactID)
	require.NoError(t, err)
	defer func() { _ = rs.Close() }()
	var out []gh435Row
	for rs.Next() {
		var r gh435Row
		require.NoError(t, rs.Scan(&r.status, &r.validator, &r.confidence, &r.expires, &r.class))
		out = append(out, r)
	}
	require.NoError(t, rs.Err())
	require.NotEmpty(t, out, "no chunk landed for artifact %s", artifactID)
	return out
}

func TestIntegration_CompanionRememberNeverSelfCertifies(t *testing.T) {
	h := newGH435Harness(t)
	content := "We decided to drop the pgvector lane from the release gates; the sqlite lane is enough from now on."

	for _, tc := range []struct {
		name    string
		args    map[string]any
		class   string
		expires time.Duration
	}{
		{"decision, no ttl", map[string]any{"content": content, "class": "decision"}, "decision", 30 * 24 * time.Hour},
		{"spec, ttl 400", map[string]any{"content": content + " (spec form)", "class": "spec", "ttl_days": 400}, "spec", 400 * 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			art := h.remember(t, tc.args)
			for _, r := range h.rows(t, art) {
				require.Equal(t, tc.class, r.class, "the class label is kept")
				require.Equal(t, "unverified", r.status, "a companion deposit certified itself")
				require.False(t, r.validator.Valid, "validator_role = %q; a companion is never its own validator", r.validator.String)
				require.LessOrEqual(t, r.confidence, 0.3+1e-6, "a companion deposit carries the class's confidence")
				require.True(t, r.expires.Valid, "a companion deposit never expires")
				d := time.Until(r.expires.Time)
				require.InDelta(t, tc.expires.Hours(), d.Hours(), 24, "expires in %v", d)
			}
		})
	}

	// Control: an agent role's decision artifact through the same pipeline is
	// still verified by the role-of-record shortcut, so the assertions above
	// distinguish a companion from an agent rather than a broken shortcut.
	art := persistence.GenerateID("itest-gh435-review")
	require.NoError(t, gh435SeedArtifact(h.db, h.project, art, "review-gh435.md"))
	stats, err := h.pipeline.IngestArtifactWithOptions(context.Background(), h.project, "", art, "review-gh435.md",
		"reviewer", "exec-gh435", content+" Reviewer ruling: approved after the full gate run.", 0, "",
		memory.IngestArtifactOptions{ClassOverride: memory.ClassDecision})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Admitted, "%+v", stats)
	for _, r := range h.rows(t, art) {
		require.Equal(t, "verified", r.status)
		require.Equal(t, "reviewer", r.validator.String)
	}
}
