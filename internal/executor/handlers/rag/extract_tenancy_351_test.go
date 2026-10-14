package rag

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// TestExtractHandler_RefusesAnotherProjectsArtifact — audit 2026-10-12:
// GitHub #351 (sibling read, 15-artifact-store.md §13.2 C4). A project-A
// caller writes context.inputArtifactIDs itself; rag.extract loaded each row
// by ID with no project check and extracted project B's artifact into A's
// extracted documents. A row of another project must fail the step before any
// extractor runs.
func TestExtractHandler_RefusesAnotherProjectsArtifact(t *testing.T) {
	fake := &fakeExtractor{name: "fake-text", version: "1.0.0"}
	reg := extractor.NewRegistry()
	require.NoError(t, reg.Register(fake, "text/markdown"))
	docRepo := &fakeExtractedDocRepo{}
	runner := &extractor.Runner{Repo: docRepo, BasePath: t.TempDir()}
	artRepo := &fakeArtifactRepo{byID: map[string]*persistence.Artifact{
		"art_b": newTestArtifact("art_b", "projB", "secret.md", "text/markdown", "/store/projB/exec/secret.md"),
	}}
	h := NewExtractHandler(reg, runner, artRepo)

	payload, _ := json.Marshal(map[string]any{
		"context": map[string]any{"inputArtifactIDs": []string{"art_b"}},
	})
	in := executor.SystemStepInput{
		Task:      &persistence.Task{ID: "t1", ProjectID: "projA", Payload: payload},
		Execution: &persistence.Execution{ID: "e1"},
		Step:      &registry.WorkflowStep{Type: "system", Handler: "rag.extract"},
	}

	_, err := h.Execute(context.Background(), in)
	require.Error(t, err, "project A's extract must refuse project B's artifact")
	assert.Contains(t, err.Error(), "another project")
	assert.Equal(t, 0, fake.calls, "no extractor may run on another project's artifact")
	assert.Empty(t, docRepo.upserts)
}
