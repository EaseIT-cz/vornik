package api

// GitHub #881, registry design §12.2 C2: a project created from a template
// with a model checks that model before any file is written.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/modelcheck"
	"vornik.io/vornik/internal/templates"
)

func modelTemplateServer(t *testing.T) (*Server, string) {
	t.Helper()
	tplDir := filepath.Join(t.TempDir(), "tpl")
	require.NoError(t, os.MkdirAll(filepath.Join(tplDir, "demo"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "demo", "template.yaml"), []byte(`
displayName: "Demo"
description: "Test template"
domain: "general"
parameters:
  - {name: projectId, type: string, label: "ID", required: true, pattern: "[a-z][a-z0-9-]{1,20}[a-z0-9]"}
  - {name: llmModel, type: string, label: "Model", default: ""}
files:
  - {source: swarm.md.tmpl, target: "swarms/{{.projectId}}-swarm.md"}
`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "demo", "swarm.md.tmpl"), []byte(
		"---\nswarmId: {{.projectId}}-swarm\ncoordinatorRole: lead\nroles:\n  - name: lead\n    count: 1\n    runtimePolicy: ephemeral\n    model: \"{{.llmModel}}\"\n    runtime:\n      image: img\n---\n"), 0o644))
	cat, err := templates.Load(tplDir)
	require.NoError(t, err)
	configsDir := t.TempDir()
	return &Server{
		projectTemplates: cat,
		configsDir:       configsDir,
		adminConfig:      config.AdminConfig{Enabled: true, AllowedKeys: []string{"sk-admin"}},
		modelChecker: &modelcheck.Checker{Provider: stubModelRouter{answers: map[string]error{
			"dead-model": &chat.GatewayError{Status: 410, Message: "retired"},
		}}},
	}, configsDir
}

func createFromTemplate(t *testing.T, srv *Server, model string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"slug": "demo", "parameters": map[string]string{"projectId": "my-project", "llmModel": model}})
	req := templateAdminReq(httptest.NewRequest(http.MethodPost, "/api/v1/projects/from-template", bytes.NewReader(body)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.CreateProjectFromTemplate(rec, req)
	return rec
}

func TestCreateProjectFromTemplate_881_ChecksTheModel(t *testing.T) {
	srv, configsDir := modelTemplateServer(t)
	rec := createFromTemplate(t, srv, "dead-model")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "MODEL_UNREACHABLE")
	_, err := os.Stat(filepath.Join(configsDir, "swarms", "my-project-swarm.md"))
	require.True(t, os.IsNotExist(err), "a refused create wrote files")

	rec = createFromTemplate(t, srv, "good-model")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestCreateProjectFromTemplate_881_NoModelNoCheck(t *testing.T) {
	srv, _ := modelTemplateServer(t)
	srv.modelChecker = &modelcheck.Checker{Provider: stubModelRouter{answers: map[string]error{"": &chat.GatewayError{Status: 500}}}}
	rec := createFromTemplate(t, srv, "")
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}
