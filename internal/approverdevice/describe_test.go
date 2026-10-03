package approverdevice

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// Agent-administered design §18.7 (operator, 2026-10-02): the approval
// page leads with a plain summary and a Low/Medium/High level with reasons,
// supplied by the requester's describer from the approved document. The
// level is a word, never a colour alone. A kind with no describer shows the
// page as before.
func TestPages_RequestLeadsWithThePlainSummaryAndLevel(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.svc.RegisterDescriber(persistence.ApprovalKindWideningChange, func(persistence.AgentApprovalRequestRow) *Description {
		return &Description{Summary: "Your assistant wants to read from fio.", Level: "Medium", Reasons: []string{"it can read from fio (api.fio.example)"}}
	})
	for _, id := range []string{"apr_w", "apr_c"} {
		kind := persistence.ApprovalKindWideningChange
		if id == "apr_c" {
			kind = persistence.ApprovalKindCredentialSlot
		}
		if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: id, Kind: kind,
			Sentence: "Exact sentence " + id + ".", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh", Status: persistence.ApprovalPending,
			CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
			t.Fatal(err)
		}
	}
	_, page := phone.do(http.MethodGet, "/ui/approve/apr_w", nil, nil)
	for _, want := range []string{"Your assistant wants to read from fio.", "Risk: Medium", "it can read from fio (api.fio.example)", "Exact sentence apr_w."} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Index(page, "Your assistant wants to read from fio.") > strings.Index(page, "Exact sentence apr_w.") {
		t.Error("the plain summary does not come before the exact sentence")
	}
	_, plain := phone.do(http.MethodGet, "/ui/approve/apr_c", nil, nil)
	if strings.Contains(plain, "Risk:") || !strings.Contains(plain, "Exact sentence apr_c.") {
		t.Error("a kind with no describer changed its page")
	}
}
