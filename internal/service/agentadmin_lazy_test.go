package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentadmin"
)

// Regression (local deployment, 2026-10-02): the daemon started before
// make install-config-assets put the agent templates in place, and the
// agent admin service cached that failure until the next restart, so
// companion-admin stayed false and vornikctl agent connect blamed
// agent_admin.enabled. The service now retries until the templates load,
// and the API reaches it through a proxy, so installing the assets is
// enough. Control: the retrying agentAdmin and agentAdminProxy.
func TestAgentAdmin_TemplatesInstalledAfterStart(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = ""
	path := isolatedConfigPath(t)
	cfg.Server.Address = "127.0.0.1:0"
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewContainer(cfg, path)
	if err != nil {
		t.Fatal(err)
	}
	if c.agentAdmin() != nil {
		t.Fatal("the service was built without templates")
	}
	capable := func() bool {
		rec := httptest.NewRecorder()
		c.apiServer.GetCapabilities(rec, httptest.NewRequest(http.MethodGet, "/api/v1/capabilities", nil).WithContext(context.Background()))
		return strings.Contains(rec.Body.String(), `"companion-admin":true`)
	}
	if capable() {
		t.Fatal("companion-admin advertised with no templates")
	}
	// Until then the wired surfaces say why, rather than failing obscurely.
	if _, err := (agentAdminProxy{c: c}).EnsureHome(context.Background(), "probe", "hermes"); !errors.Is(err, agentadmin.ErrUnavailable) {
		t.Fatalf("proxy before templates: %v", err)
	}
	view := c.agentsView()
	if view == nil {
		t.Fatal("the agents pages were not wired")
	}
	if _, err := view.ListAgents(context.Background()); !errors.Is(err, agentadmin.ErrUnavailable) {
		t.Fatalf("agents page before templates: %v", err)
	}

	copyDir(t, "../../configs/agent-templates", filepath.Join(filepath.Dir(path), "configs", "agent-templates"))
	if c.agentAdmin() == nil {
		t.Fatal("the service did not load the templates installed after start")
	}
	if !capable() {
		t.Fatal("companion-admin stayed false after the templates were installed")
	}
	if _, err := view.ListAgents(context.Background()); err != nil {
		t.Fatalf("agents page after templates: %v", err)
	}
}
