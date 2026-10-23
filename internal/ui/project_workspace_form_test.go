package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/registry"
)

// GitHub #400: workspace.refresh_before_work and workspace.refresh_branch are
// YAML-only (ProjectDeferredPaths). Saving unrelated form fields must neither
// erase the opt-in nor its branch (ui-asset-management design, amendment
// 2026-10-17).
func TestProjectSchemaSavePreservesWorkspaceRefresh(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	block := []byte("\nworkspace:\n  refresh_before_work: true\n  refresh_branch: main\n")
	require.NoError(t, os.WriteFile(path, append(original, block...), 0600))
	s, _ := formServer(t, root)
	values := baselineSchemaValues()
	values.Set("displayName", "Refresh Preserved")
	w := httptest.NewRecorder()
	s.ProjectSchemaConfigSave(w, postSchema(values), "form-demo")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	p := s.projectReg.GetProject("form-demo")
	require.Equal(t, "Refresh Preserved", p.DisplayName)
	require.Equal(t, registry.ProjectWorkspace{RefreshBeforeWork: true, RefreshBranch: "main"}, p.Workspace)
}
