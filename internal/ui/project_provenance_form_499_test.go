package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// GitHub #499: memory.provenance_clone and memory.provenance_branch (the
// trust root of companion-delegated design chunks, design 22 part 8g) are
// YAML-only (ProjectDeferredPaths). Saving unrelated form fields must keep
// both (ui-asset-management design, amendment 2026-10-22).
func TestProjectSchemaSavePreservesProvenanceClone(t *testing.T) {
	root := writeFormFixture(t)
	clone := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(clone, ".git"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "config"),
		[]byte("[remote \"origin\"]\n\turl = https://github.com/acme/widgets.git\n"), 0o600))
	path := filepath.Join(root, "projects", "form-demo.yaml")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	block := []byte("\nrepo_scope: github.com/acme/widgets\nmemory:\n  provenance_clone: " + clone + "\n  provenance_branch: main\n")
	require.NoError(t, os.WriteFile(path, append(original, block...), 0o600))
	s, _ := formServer(t, root)
	values := baselineSchemaValues()
	values.Set("displayName", "Provenance Preserved")
	// The form owns repo_scope; the strict loader refuses a clone whose
	// origin is not the scope, so the save carries it.
	values.Set("repo_scope", "github.com/acme/widgets")
	w := httptest.NewRecorder()
	s.ProjectSchemaConfigSave(w, postSchema(values), "form-demo")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	p := s.projectReg.GetProject("form-demo")
	require.Equal(t, "Provenance Preserved", p.DisplayName)
	require.Equal(t, clone, p.Memory.ProvenanceClone)
	require.Equal(t, "main", p.Memory.ProvenanceBranch)
}
