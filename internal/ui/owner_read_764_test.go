package ui

// GitHub #764 (18-security-and-tenancy.md, Amendment 2026-11-18 (#764)):
// managed hosting's instance owner reads the swarm and workflow lists, the
// definitions and the workflow graph, GET only, and changes nothing. Found
// at: n/a (feature). Test at: unit, through the UI's real handler and its
// session wrapper.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/auth"
)

func TestOwnerRead764_TheRule(t *testing.T) {
	allowed := []string{"/swarms", "/workflows", "/swarms/dev-swarm/edit", "/workflows/dev-pipeline/edit", "/workflows/dev-pipeline/graph"}
	for _, p := range allowed {
		if !tenantAdminReadPath(http.MethodGet, p) {
			t.Errorf("GET %s refused, want allowed", p)
		}
		if tenantAdminReadPath(http.MethodPost, p) || tenantAdminReadPath(http.MethodHead, p) {
			t.Errorf("POST or HEAD %s allowed", p)
		}
	}
	for _, p := range []string{
		"/swarms/new", "/workflows/new", "/swarms/x/schema", "/workflows/x/schema/step", "/swarms/x/delete",
		"/workflows/x/clone", "/workflows/x/graph/node", "/swarms/x/edit/../delete", "/swarms/../edit",
		"/swarms/./edit", "/swarms//edit", "/swarms/x/edit/", "/swarms/", "/swarms/x/graph", "/swarms/a/b/edit",
	} {
		if tenantAdminReadPath(http.MethodGet, p) {
			t.Errorf("GET %s allowed, want refused", p)
		}
	}
}

func TestOwnerRead764_TheWrapperAdmitsReadsAndRefusesTheRest(t *testing.T) {
	h := NewServer().Handler()
	status := func(method, path, role string) int {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, setupAuthRequest(method, path, role))
		return rec.Code
	}
	for _, p := range []string{"/swarms", "/workflows"} {
		if code := status(http.MethodGet, p, auth.RoleTenantAdmin); code != http.StatusOK {
			t.Errorf("owner GET %s: %d, want 200", p, code)
		}
	}
	for _, p := range []string{"/swarms", "/workflows", "/swarms/x/edit", "/workflows/x/edit", "/workflows/x/graph"} {
		// A detail page for a missing id answers 404 after the gate; the
		// gate's own answer is 403.
		if code := status(http.MethodGet, p, auth.RoleTenantAdmin); code == http.StatusForbidden || code == http.StatusUnauthorized {
			t.Errorf("owner GET %s: %d, want admitted past the gate", p, code)
		}
		if code := status(http.MethodPost, p, auth.RoleTenantAdmin); code != http.StatusForbidden {
			t.Errorf("owner POST %s: %d, want 403", p, code)
		}
		if code := status(http.MethodGet, p, auth.RoleUser); code != http.StatusForbidden {
			t.Errorf("project user GET %s: %d, want 403", p, code)
		}
	}
	for _, p := range []string{"/swarms/new", "/swarms/x/schema", "/swarms/x/edit/../delete", "/swarms/x/edit/", "/workflows/x/clone"} {
		if code := status(http.MethodGet, p, auth.RoleTenantAdmin); code != http.StatusForbidden {
			t.Errorf("owner GET %s: %d, want 403", p, code)
		}
	}
	if code := status(http.MethodHead, "/swarms", auth.RoleTenantAdmin); code != http.StatusForbidden {
		t.Errorf("owner HEAD /swarms: %d, want 403", code)
	}
}

func TestOwnerRead764_PagesRenderReadOnly(t *testing.T) {
	s := NewServer()
	// The page's own content: the layout's sign-out button and scripts are
	// not the page's controls.
	render := func(tpl string, data any) string {
		rec := httptest.NewRecorder()
		s.render(rec, tpl, data)
		b := rec.Body.String()
		if i := strings.Index(b, "<main"); i >= 0 {
			b = b[i:]
			if j := strings.Index(b, "</main>"); j >= 0 {
				b = b[:j]
			}
		}
		return b
	}
	for name, body := range map[string]string{
		"swarm":    render("swarm_edit.html", SwarmEditData{SwarmID: "dev-swarm", ReadOnly: true}),
		"workflow": render("workflow_edit.html", WorkflowEditData{WorkflowID: "dev-pipeline", ReadOnly: true}),
		"graph":    render("workflow_graph.html", WorkflowGraphData{WorkflowID: "dev-pipeline", ReadOnly: true}),
	} {
		if strings.Contains(body, `type="submit"`) {
			t.Errorf("%s: a read-only page has a submit control", name)
		}
		if strings.Contains(body, "/delete") || strings.Contains(body, "/schema") || strings.Contains(body, "/graph/") || strings.Contains(body, "assist") {
			t.Errorf("%s: a read-only page names a mutating route", name)
		}
		if name != "graph" && !strings.Contains(body, "disabled") {
			t.Errorf("%s: the editor's fieldset is not disabled", name)
		}
	}
	if !strings.Contains(render("swarm_edit.html", SwarmEditData{SwarmID: "dev-swarm"}), `type="submit"`) {
		t.Error("the operator's editor lost its save control")
	}
}
