package api

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Document paths on uploads (memory rollback x supersession design,
// amendment 2026-09-26). Incident: re-ingesting a document never superseded
// its earlier versions, and two different files named index.md went up in
// one batch, so a bare file name cannot be the document's identity.

func TestValidateDocumentPath(t *testing.T) {
	ok := []string{"docs/a.md", "a.md", "https://docs.vornik.io"}
	for _, p := range ok {
		require.NoError(t, validateDocumentPath(p, pathBase(p)), p)
	}
	bad := map[string]string{
		"/etc/a.md":                        "absolute",
		"../a.md":                          "escapes",
		"docs/../a.md":                     "not clean",
		"docs//a.md":                       "not clean",
		"docs/./a.md":                      "not clean",
		"docs/a.md/":                       "not clean",
		"docs/b.md":                        "name mismatch",
		strings.Repeat("d/", 256) + "a.md": "too long",
	}
	for p, why := range bad {
		assert.Error(t, validateDocumentPath(p, "a.md"), "%s must be refused (%s)", p, why)
	}
}

func pathBase(p string) string { return p[strings.LastIndex(p, "/")+1:] }

func TestMergeInputsIntoContext_DocumentPaths(t *testing.T) {
	out, err := mergeInputsIntoContext(nil, []inputArtifactResult{
		{StoragePath: "/s/a", ArtifactID: "art_a", DocumentPath: "docs/a.md"},
		{StoragePath: "/s/b", ArtifactID: "art_b"},
	})
	require.NoError(t, err)
	var ctx map[string]any
	require.NoError(t, json.Unmarshal(out, &ctx))
	assert.Equal(t, map[string]any{"art_a": "docs/a.md"}, ctx["inputDocumentPaths"], "only artifacts that carried a path")

	out, err = mergeInputsIntoContext(nil, []inputArtifactResult{{StoragePath: "/s/b", ArtifactID: "art_b"}})
	require.NoError(t, err)
	ctx = nil
	require.NoError(t, json.Unmarshal(out, &ctx))
	_, present := ctx["inputDocumentPaths"]
	assert.False(t, present, "no paths means no map, today's shape")
}

func delegateDocument(t *testing.T, artifacts []map[string]any) (map[string]any, string, bool, int) {
	t.Helper()
	srv, keyRepo, taskRepo := newCompanionMCPServer(t)
	srv.inputArtifactStore = &fakeInputArtifactStore{}
	raw, _ := seedCompanionKey(t, keyRepo, "alpha", []string{"wf-artifacts"})
	req := withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{
		"name": "delegate",
		"arguments": map[string]any{
			"workflow": "wf-artifacts", "prompt": "ingest", "repo_scope": "github.com/acme/widgets",
			"inputArtifacts": artifacts,
		},
	}), raw)
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, req)
	text, isErr := decodeToolText(t, decodeJSONRPC(t, rec.Body.Bytes()))
	if isErr || taskRepo.LastCall.Task == nil {
		return nil, text, isErr, taskRepo.CallCount.Create
	}
	var payload map[string]any
	require.NoError(t, json.Unmarshal(taskRepo.LastCall.Task.Payload, &payload))
	taskCtx, _ := payload["context"].(map[string]any)
	return taskCtx, text, isErr, taskRepo.CallCount.Create
}

func TestCompanionDelegate_RecordsDocumentPaths(t *testing.T) {
	taskCtx, text, isErr, _ := delegateDocument(t, []map[string]any{
		{"name": "index.md", "content": "aGVsbG8=", "path": "docs/public/index.md"},
		{"name": "notes.md", "content": "aGVsbG8="},
	})
	require.False(t, isErr, text)
	ids, _ := taskCtx["inputArtifactIDs"].([]any)
	require.Len(t, ids, 2)
	paths, _ := taskCtx["inputDocumentPaths"].(map[string]any)
	assert.Equal(t, map[string]any{ids[0].(string): "docs/public/index.md"}, paths)
}

func TestCompanionDelegate_RefusesABadDocumentPath(t *testing.T) {
	_, text, isErr, creates := delegateDocument(t, []map[string]any{
		{"name": "index.md", "content": "aGVsbG8=", "path": "../outside/index.md"},
	})
	require.True(t, isErr, "a path escaping the repository must be refused")
	assert.Contains(t, text, "inputArtifacts[0]")
	assert.Equal(t, 0, creates, "no task on a refused path")
}
