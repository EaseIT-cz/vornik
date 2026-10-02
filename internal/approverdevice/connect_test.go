package approverdevice

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

type fakeConnect struct {
	started []string
	shas    []string
}

func (f *fakeConnect) Applies(r persistence.AgentApprovalRequestRow) bool { return r.ID != "apr_value" }
func (f *fakeConnect) Start(_ context.Context, d *Device, r persistence.AgentApprovalRequestRow, shownSHA string) (string, *http.Cookie, error) {
	f.started = append(f.started, d.ID+"/"+r.ID)
	f.shas = append(f.shas, shownSHA)
	return "https://vendor.example/authorize?state=s1", &http.Cookie{Name: "vornik_oauth_flow", Value: "nonce", Path: "/auth/mcp/callback", HttpOnly: true}, nil
}

// Plan P4.4: an OAuth slot's page offers Connect instead of a value field;
// the device's POST starts the sign-in (it decides nothing), sets the flow
// cookie and sends the phone to the provider. Control: the connect route.
func TestPages_OAuthSlotConnects(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	fc := &fakeConnect{}
	f.svc.RegisterEffect(persistence.ApprovalKindCredentialSlot, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	f.svc.RegisterValueEntry(persistence.ApprovalKindCredentialSlot, func(context.Context, *Device, persistence.AgentApprovalRequestRow, string, []byte) error { return nil })
	f.svc.RegisterConnect(persistence.ApprovalKindCredentialSlot, fc)
	for _, id := range []string{"apr_oauth", "apr_value"} {
		if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindCredentialSlot,
			Sentence: "Tap Connect.", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh",
			Status: persistence.ApprovalPending, CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
			t.Fatal(err)
		}
	}
	_, page := phone.do(http.MethodGet, "/ui/approve/apr_oauth", nil, nil)
	if !strings.Contains(page, `action="/ui/approve/apr_oauth/connect"`) || strings.Contains(page, `name="value"`) {
		t.Fatalf("the OAuth slot page: %s", page)
	}
	if _, other := phone.do(http.MethodGet, "/ui/approve/apr_value", nil, nil); !strings.Contains(other, `name="value"`) {
		t.Fatal("a value slot lost its field")
	}
	res, _ := phone.do(http.MethodPost, "/ui/approve/apr_oauth/connect", url.Values{"rendered_sha256": {"hhh"}}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "https://vendor.example/authorize?state=s1" {
		t.Fatalf("connect: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if !strings.Contains(res.Header.Get("Set-Cookie"), "vornik_oauth_flow=nonce") {
		t.Fatalf("no flow cookie: %v", res.Header["Set-Cookie"])
	}
	if len(fc.started) != 1 || fc.shas[0] != "hhh" {
		t.Fatalf("started %v %v", fc.started, fc.shas)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_oauth"); r.Status != persistence.ApprovalPending {
		t.Fatalf("starting the sign-in decided the request: %s", r.Status)
	}
	// An OAuth slot is never approved by a value or a plain Approve.
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_oauth", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}, "value": {"x"}}, nil); res.StatusCode != http.StatusConflict {
		t.Fatalf("a plain approve of an OAuth slot: %d", res.StatusCode)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_oauth"); r.Status != persistence.ApprovalPending {
		t.Fatalf("an OAuth slot was decided without its sign-in: %s", r.Status)
	}
	// Only a device reaches it, and only by POST from the same origin.
	stranger := newBrowser(t, srv)
	if res, _ := stranger.do(http.MethodPost, "/ui/approve/apr_oauth/connect", url.Values{"rendered_sha256": {"hhh"}}, nil); res.StatusCode == http.StatusSeeOther && strings.Contains(res.Header.Get("Location"), "vendor") {
		t.Fatal("a browser without a device started a sign-in")
	}
	if res, _ := phone.do(http.MethodGet, "/ui/approve/apr_oauth/connect", nil, nil); res.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET connect: %d", res.StatusCode)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_value/connect", url.Values{"rendered_sha256": {"hhh"}}, nil); res.StatusCode < 400 {
		t.Fatalf("a value slot started a sign-in: %d", res.StatusCode)
	}
}
