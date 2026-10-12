package ui

import (
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Issue #319: editing unrelated project form fields must not erase YAML-only
// Forge repo/label grants or accidentally toggle their default-off write gates.
func TestProjectSchemaSavePreservesForgeIssuePolicy(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	policy := []byte("\nforge:\n  issues:\n    repos: [acme/service]\n    labels_enabled: true\n    allowed_labels: [bug, P1]\n    close_enabled: false\n    writes_without_approval: true\n")
	require.NoError(t, os.WriteFile(path, append(original, policy...), 0600))
	s, _ := formServer(t, root)
	values := baselineSchemaValues()
	values.Set("displayName", "Policy Preserved")
	w := httptest.NewRecorder()
	s.ProjectSchemaConfigSave(w, postSchema(values), "form-demo")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	p := s.projectReg.GetProject("form-demo")
	require.Equal(t, []string{"acme/service"}, p.Forge.Issues.Repos)
	require.Equal(t, []string{"bug", "P1"}, p.Forge.Issues.AllowedLabels)
	require.True(t, p.Forge.Issues.LabelsEnabled)
	require.False(t, p.Forge.Issues.CloseEnabled)
	// audit 2026-10-12 finding C: the operator's acceptance of unapproved
	// writes is YAML-only and must survive a form save.
	require.True(t, p.Forge.Issues.WritesWithoutApproval)
}
