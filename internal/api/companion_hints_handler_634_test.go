package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// #634 H1, H6 (companion hardening design §13): GET /api/v1/companion/hints
// serves the catalogue to a companion key for its own project, without the
// hints the project muted, and nothing when the project turned hints off.

func hintsRegistry(t *testing.T, companionYAML string) *registry.Registry {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"projects", "swarms", "workflows"} {
		require.NoError(t, os.MkdirAll(filepath.Join(dir, d), 0o755))
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "swarms", "s1.md"), []byte("---\nswarmId: \"s1\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "workflows", "wf.md"), []byte("---\nworkflowId: \"wf\"\nentrypoint: \"run\"\nsteps:\n  run:\n    type: \"agent\"\n    prompt: \"do\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "projects", "proj.yaml"), []byte("projectId: \"proj\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"wf\"\n"+companionYAML), 0o644))
	reg := registry.New()
	require.NoError(t, reg.Load(dir))
	return reg
}

func hintsRequest(row *persistence.APIKey) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/companion/hints", nil)
	if row != nil {
		r = r.WithContext(context.WithValue(r.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}}))
	}
	return r
}

type hintsBody struct {
	Project string          `json:"project"`
	Enabled bool            `json:"enabled"`
	Hints   []CompanionHint `json:"hints"`
	Muted   []string        `json:"muted"`
}

func getHints(t *testing.T, s *Server, row *persistence.APIKey) (int, hintsBody) {
	t.Helper()
	w := httptest.NewRecorder()
	s.CompanionHintsGet(w, hintsRequest(row))
	var b hintsBody
	if w.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &b))
	}
	return w.Code, b
}

func companionRow() *persistence.APIKey {
	return &persistence.APIKey{ID: "k1", ProjectID: "proj", ClientKind: "claude-code"}
}

func TestCompanionHintsGet_ServesActiveUnmuted_634(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), projectRegistry: hintsRegistry(t, "companion:\n  mute: [\"commit.review\"]\n")}
	code, b := getHints(t, s, companionRow())
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "proj", b.Project)
	require.True(t, b.Enabled)
	ids := map[string]bool{}
	for _, h := range b.Hints {
		ids[h.ID] = true
		require.True(t, h.Active, "an inactive hint was served: %s", h.ID)
	}
	require.False(t, ids["commit.review"], "a muted hint was served")
	require.True(t, ids["test_failed.recall"])
	require.Contains(t, b.Muted, "commit.review", "the mask carries the muted ids for the hooks' offline copy")
}

func TestCompanionHintsGet_OffServesNothing_634(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), projectRegistry: hintsRegistry(t, "companion:\n  hints: \"off\"\n")}
	code, b := getHints(t, s, companionRow())
	require.Equal(t, http.StatusOK, code)
	require.False(t, b.Enabled)
	require.Empty(t, b.Hints)
	require.Empty(t, b.Muted, "with hints off no mask is served")
}

// The project comes from the key: a companion key of another project gets
// that project's answer, never this one's (review 857b F2).
func TestCompanionHintsGet_ProjectFromKeyOnly_634(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), projectRegistry: hintsRegistry(t, "companion:\n  mute: [\"commit.review\"]\n")}
	other := &persistence.APIKey{ID: "k3", ProjectID: "other", ClientKind: "codex"}
	code, b := getHints(t, s, other)
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, "other", b.Project)
	require.Empty(t, b.Muted, "another project's key must not see this project's mask")
}

func TestCompanionHintsGet_RefusesNonCompanionKeys_634(t *testing.T) {
	s := &Server{logger: zerolog.Nop(), projectRegistry: hintsRegistry(t, "")}
	code, _ := getHints(t, s, &persistence.APIKey{ID: "k2", ProjectID: "proj"})
	require.Equal(t, http.StatusForbidden, code, "an ordinary project key")
	code, _ = getHints(t, s, nil)
	require.Equal(t, http.StatusForbidden, code, "no key at all")
}

// The hint routes open to companion keys as exact paths only (§13.8,
// §13.10 S3); the operator report is not one of them (S6).
func TestCompanionAllowedPaths_Hints_634(t *testing.T) {
	require.True(t, isCompanionAllowedPath("/api/v1/companion/hints"))
	require.True(t, isCompanionAllowedPath("/api/v1/companion/hints/outcomes"))
	for _, p := range []string{"/api/v1/companion/hints/", "/api/v1/companion/hints/x", "/api/v1/companion/hints/outcomes/", "/api/v1/companion/hints/report", "/api/v1/companion/grant", "/api/v1/companion/keys", "/api/v1/companion/hintsx"} {
		require.False(t, isCompanionAllowedPath(p), p)
	}
}
