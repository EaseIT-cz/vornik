package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/api"
)

type stubAgents struct {
	rows []AgentRow
	page *AgentPage
	err  error
}

func (s stubAgents) ListAgents(context.Context) ([]AgentRow, error) { return s.rows, s.err }
func (s stubAgents) DescribeAgent(_ context.Context, ns string) (*AgentPage, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.page != nil && s.page.Row.Namespace == ns {
		return s.page, nil
	}
	return nil, nil
}

func getAgents(t *testing.T, srv *Server, path string, asAdmin bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req = req.WithContext(api.ContextWithAuthEnabled(req.Context(), true))
	if asAdmin {
		req = withAdminUI(req)
	}
	rec := httptest.NewRecorder()
	srv.adminRouter(rec, req)
	return rec
}

// Plan P6.5: /ui/admin/agents lists each connected assistant, and
// /ui/admin/agents/<ns> shows its setup in a person's words (projects,
// workflows and their approval, integrations, credentials by name and status
// only, budgets, and requests). Both sit behind the admin session gate, so an
// agent admin key cannot reach them. Control: AdminAgents and the router
// case.
func TestAdminAgents(t *testing.T) {
	page := &AgentPage{
		Row:    AgentRow{Namespace: "hermes", ClientKind: "hermes", Class: "mcp_only", KeyStatus: "active", KeyPrefix: "sk-vornik-hermes--h.ab", Projects: 1, Pending: 1},
		Claims: []string{"T1 held: you never receive a credential value, only handles."},
		Projects: []AgentProject{{ID: "hermes--finance", Purpose: "Money", BudgetUSD: 2,
			Workflows:    []AgentItem{{Name: "hermes--finance--report", Status: "approved"}},
			Integrations: []AgentItem{{Name: "fio", Status: "approved", Detail: "https://api.fio.example/v1"}},
			Credentials:  []AgentItem{{Name: "FIO", Status: "set"}}}},
		Pending: []AgentRequest{{Sentence: "Connect the bank API fio.", When: "2 minutes ago"}},
	}
	srv := NewServer(WithAgents(stubAgents{rows: []AgentRow{page.Row}, page: page}))

	rec := getAgents(t, srv, "/admin/agents", true)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "/ui/admin/agents/hermes") {
		t.Fatalf("list: %d\n%s", rec.Code, rec.Body.String())
	}
	rec = getAgents(t, srv, "/admin/agents/hermes", true)
	body := rec.Body.String()
	for _, want := range []string{"hermes--finance", "Money", "hermes--finance--report", "fio", "FIO", "Connect the bank API fio.", "T1 held"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not show %q", want)
		}
	}
	if rec := getAgents(t, srv, "/admin/agents/nobody", true); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown namespace: %d", rec.Code)
	}
	for _, p := range []string{"/admin/agents", "/admin/agents/hermes"} {
		if rec := getAgents(t, srv, p, false); rec.Code != http.StatusForbidden {
			t.Fatalf("%s without an admin session: %d", p, rec.Code)
		}
	}

	// Local deployment 2026-10-02: before the agent templates are installed
	// the pages say so and what to run, not "could not be read" and not "no
	// assistant is connected" (review 20261002-a45d F2).
	unbuilt := NewServer(WithAgents(stubAgents{err: agentadmin.ErrUnavailable}))
	for _, p := range []string{"/admin/agents", "/admin/agents/hermes"} {
		body := getAgents(t, unbuilt, p, true).Body.String()
		if !strings.Contains(body, "make install-config-assets") || strings.Contains(body, "No assistant is connected") {
			t.Fatalf("%s before the templates:\n%s", p, body)
		}
	}

	// No assistant connected says so (plan P6 amendment F10).
	empty := NewServer(WithAgents(stubAgents{}))
	if body := getAgents(t, empty, "/admin/agents", true).Body.String(); !strings.Contains(body, "No assistant is connected") {
		t.Fatalf("empty list:\n%s", body)
	}
}
