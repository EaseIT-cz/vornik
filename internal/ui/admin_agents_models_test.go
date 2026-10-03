package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
)

// Agent-administered design §18.6 item 2 in detail (round 2 F7, round 3 F3):
// the operator console is the only place a model destination approval is
// withdrawn (removed_at). The assistant's page lists the namespace's model
// destinations, and a POST withdraws one; the agent key cannot reach either.

type withdrawRecorder struct {
	stubAgents
	ns, dest string
	none     bool // answer "nothing withdrawn"
}

func (w *withdrawRecorder) WithdrawModelDestination(_ context.Context, ns, dest string) (bool, error) {
	w.ns, w.dest = ns, dest
	return !w.none, nil
}

func TestAdminAgents_ModelDestinationsListedAndWithdrawn(t *testing.T) {
	page := &AgentPage{
		Row: AgentRow{Namespace: "hermes", ClientKind: "hermes", KeyStatus: "active"},
		Models: []AgentItem{{Name: "vertex@aiplatform.googleapis.com", Status: "approved"},
			{Name: "http@api.old.example", Status: "withdrawn"}},
	}
	rec := &withdrawRecorder{stubAgents: stubAgents{rows: []AgentRow{page.Row}, page: page}}
	srv := NewServer(WithAgents(rec))

	got := getAgents(t, srv, "/admin/agents/hermes", true)
	body := got.Body.String()
	if got.Code != http.StatusOK || !strings.Contains(body, "vertex@aiplatform.googleapis.com") ||
		!strings.Contains(body, "/ui/admin/agents/hermes/models/withdraw") {
		t.Fatalf("page %d:\n%s", got.Code, body)
	}

	post := func(asAdmin bool) *httptest.ResponseRecorder {
		form := url.Values{"destination": {"vertex@aiplatform.googleapis.com"}}
		req := httptest.NewRequest(http.MethodPost, "/admin/agents/hermes/models/withdraw", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req = req.WithContext(api.ContextWithAuthEnabled(req.Context(), true))
		if asAdmin {
			req = withAdminUI(req)
		}
		w := httptest.NewRecorder()
		srv.adminRouter(w, req)
		return w
	}
	if w := post(false); w.Code < 300 || w.Code == http.StatusSeeOther || rec.dest != "" {
		t.Fatalf("without the admin session: %d, withdrew %q", w.Code, rec.dest)
	}
	if w := post(true); w.Code != http.StatusSeeOther || rec.ns != "hermes" || rec.dest != "vertex@aiplatform.googleapis.com" {
		t.Fatalf("withdraw: %d, %q %q", w.Code, rec.ns, rec.dest)
	}
	// Review 20261003-a525 A4: an absent or already-withdrawn destination
	// says so instead of pretending.
	rec.none = true
	if w := post(true); w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "nothing to withdraw") {
		t.Fatalf("withdrawing nothing: %d %s", w.Code, w.Body.String())
	}
}
