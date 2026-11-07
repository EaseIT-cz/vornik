package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// #639 (design 22, "Amendment 2026-11-04 (#639)"): memory.mirror_paths is a
// deferred path, YAML only. A form save of an unrelated field must keep it,
// as it keeps the provenance clone (#499).
func TestProjectSchemaSavePreservesMirrorPaths_639(t *testing.T) {
	root := writeFormFixture(t)
	path := filepath.Join(root, "projects", "form-demo.yaml")
	original, err := os.ReadFile(path)
	require.NoError(t, err)
	block := []byte("\nrepo_scope: github.com/acme/widgets\nmemory:\n  mirror_paths: [docs/public/, docs/guides/]\n")
	require.NoError(t, os.WriteFile(path, append(original, block...), 0o600))
	s, _ := formServer(t, root)
	values := baselineSchemaValues()
	values.Set("displayName", "Mirror Preserved")
	values.Set("repo_scope", "github.com/acme/widgets")
	w := httptest.NewRecorder()
	s.ProjectSchemaConfigSave(w, postSchema(values), "form-demo")
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	p := s.projectReg.GetProject("form-demo")
	require.Equal(t, "Mirror Preserved", p.DisplayName)
	require.Equal(t, []string{"docs/public/", "docs/guides/"}, p.Memory.MirrorPaths)
}
