package approverdevice

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// Hermes approval transport design §4.2: the page offers Allow once, Allow
// for this session (only when allowed, with the pattern identifier beneath
// it), and Deny; never Always and never a plain Approve. The command is in
// a <pre>, escaped, under its label; the surface and Vornik's own deadline
// are stated.
func TestHostActionPage_ChoicesCommandSurfaceDeadline(t *testing.T) {
	f, _ := hostFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	req := hermesRequest("aa01")
	req.Command = `rm -rf /tmp/build <script>alert(1)</script>`
	if _, err := f.svc.FileHostAction(context.Background(), "hermes", req); err != nil {
		t.Fatal(err)
	}
	id := HostActionID("hermes", "aa01")
	_, page := phone.do(http.MethodGet, "/ui/approve/"+id, nil, nil)
	for _, want := range []string{
		`value="once"`, "Allow once", `value="session"`, "Allow for this session", `value="deny"`, "Deny",
		"Hermes will not ask again for commands matching <code>rm_recursive</code> until this conversation ends.",
		"The command, as Hermes shows it (secrets already masked)",
		"&lt;script&gt;alert(1)&lt;/script&gt;", "in a terminal",
		"Hermes asked for an answer by 09:04",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, never := range []string{`value="always"`, `value="approve"`, "<script>alert(1)"} {
		if strings.Contains(page, never) {
			t.Errorf("page has %q", never)
		}
	}
	if !strings.Contains(page, "<pre") {
		t.Error("the command is not in a <pre>")
	}

	// Without session in allowed_choices, no session button and no note.
	req2 := hermesRequest("aa02")
	req2.AllowedChoices = []string{"once", "deny"}
	req2.Surface = "gateway"
	if _, err := f.svc.FileHostAction(context.Background(), "hermes", req2); err != nil {
		t.Fatal(err)
	}
	_, page2 := phone.do(http.MethodGet, "/ui/approve/"+HostActionID("hermes", "aa02"), nil, nil)
	if strings.Contains(page2, `value="session"`) || strings.Contains(page2, "will not ask again") || !strings.Contains(page2, "from a chat") {
		t.Errorf("a session-less request offers session, or the surface is wrong")
	}
}

// §4.1: the ceiling and expiry wordings state Vornik's own deadline.
func TestHostActionPage_CeilingAndExpiredWording(t *testing.T) {
	f, _ := hostFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	long := hermesRequest("aa01")
	long.TimeoutSeconds = 2 * 24 * 3600
	if _, err := f.svc.FileHostAction(context.Background(), "hermes", long); err != nil {
		t.Fatal(err)
	}
	_, page := phone.do(http.MethodGet, "/ui/approve/"+HostActionID("hermes", "aa01"), nil, nil)
	if !strings.Contains(page, "Vornik accepts an answer until 09:00 UTC on 3 Oct; Hermes may wait longer and will then deny.") {
		t.Errorf("ceiling wording missing:\n%s", page)
	}
	if _, err := f.svc.FileHostAction(context.Background(), "hermes", hermesRequest("aa02")); err != nil {
		t.Fatal(err)
	}
	f.advance(10 * time.Minute)
	_, page = phone.do(http.MethodGet, "/ui/approve/"+HostActionID("hermes", "aa02"), nil, nil)
	if !strings.Contains(page, "This request expired at 09:04") || !strings.Contains(page, "an answer now would not be used") || strings.Contains(page, `value="once"`) {
		t.Errorf("expired wording missing or buttons shown:\n%s", page)
	}
}

// §4.2: the page's choice is recorded with the device; a plain approve is
// refused on a host_action (it would lose the scope).
func TestHostActionPage_DecideOnce(t *testing.T) {
	f, _ := hostFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	if _, err := f.svc.FileHostAction(context.Background(), "hermes", hermesRequest("aa01")); err != nil {
		t.Fatal(err)
	}
	id := HostActionID("hermes", "aa01")
	_, page := phone.do(http.MethodGet, "/ui/approve/"+id, nil, nil)
	h := hashField.FindStringSubmatch(page)
	if h == nil {
		t.Fatal("no hash field")
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/"+id, url.Values{"decision": {"approve"}, "rendered_sha256": {h[1]}}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a plain approve on a host_action: %d, want 400", res.StatusCode)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/"+id, url.Values{"decision": {"always"}, "rendered_sha256": {h[1]}}, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("always from the phone: %d, want 400", res.StatusCode)
	}
	if r, _ := f.repo.GetRequest(context.Background(), id); r.Status != persistence.ApprovalPending {
		t.Fatalf("a refused post decided: %s", r.Status)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/"+id, url.Values{"decision": {"once"}, "rendered_sha256": {h[1]}}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("once: %d", res.StatusCode)
	}
	d, _ := f.svc.Authenticate(context.Background(), phone.deviceCookie())
	if r, _ := f.repo.GetRequest(context.Background(), id); r.Status != persistence.ApprovalApproved || r.DecidedChoice != "once" || d == nil || r.DecidedByDevice != d.ID {
		t.Fatalf("after once: %+v", r)
	}
}
