package approverdevice

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/chatauth"
	"vornik.io/vornik/internal/persistence"
)

// browser is a phone: a cookie jar and same-origin POSTs.
type browser struct {
	t   *testing.T
	srv *httptest.Server
	c   *http.Client
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, srv: srv, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) do(method, path string, form url.Values, hdr map[string]string) (*http.Response, string) {
	b.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, b.srv.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == http.MethodPost {
		req.Header.Set("Origin", b.srv.URL)
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	raw, _ := io.ReadAll(res.Body)
	return res, string(raw)
}

// deviceCookie is the device cookie the jar holds, or "".
func (b *browser) deviceCookie() string {
	u, _ := url.Parse(b.srv.URL + "/ui/")
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == CookieName {
			return c.Value
		}
	}
	return ""
}

func serve(t *testing.T, f *fixture) *httptest.Server {
	srv := httptest.NewServer(f.svc.Handler(nil))
	t.Cleanup(srv.Close)
	return srv
}

var hashField = regexp.MustCompile(`name="rendered_sha256" value="([0-9a-f]+)"`)

func (b *browser) pairWith(code string) *http.Response {
	b.t.Helper()
	res, _ := b.do(http.MethodPost, "/ui/pair", url.Values{"code": {code}}, nil)
	return res
}

func TestPages_FirstAndSecondDevice(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	ctx := context.Background()

	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(ctx, "Pixel")
	if res := phone.pairWith(code); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/ui/approve/" {
		t.Fatalf("first pairing: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, body := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != 200 || !strings.Contains(body, "Nothing is waiting") {
		t.Fatalf("list: %d", res.StatusCode)
	}

	tablet := newBrowser(t, srv)
	code2, _, _ := f.svc.StartPairing(ctx, "Tablet")
	if res := tablet.pairWith(code2); res.Header.Get("Location") != "/ui/pair/wait" {
		t.Fatalf("second pairing went to %q", res.Header.Get("Location"))
	}
	if res, body := tablet.do(http.MethodGet, "/ui/pair/wait", nil, nil); res.StatusCode != 200 || !strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("wait page: %d", res.StatusCode)
	}
	if res, _ := tablet.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("a waiting tablet reached the approvals: %d", res.StatusCode)
	}

	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	id := regexp.MustCompile(`/ui/approve/(apr_[0-9a-f]+)`).FindStringSubmatch(list)
	if id == nil || !strings.Contains(list, "Tablet") {
		t.Fatalf("enrollment not listed: %s", list)
	}
	_, page := phone.do(http.MethodGet, "/ui/approve/"+id[1], nil, nil)
	h := hashField.FindStringSubmatch(page)
	if h == nil {
		t.Fatalf("no hash field: %s", page)
	}
	before := phone.deviceCookie()
	res, _ := phone.do(http.MethodPost, "/ui/approve/"+id[1], url.Values{"decision": {"approve"}, "rendered_sha256": {h[1]}}, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	if after := phone.deviceCookie(); after == "" || after == before {
		t.Fatal("the device cookie was not rotated on a decision")
	}
	if res, _ := tablet.do(http.MethodGet, "/ui/pair/wait", nil, nil); res.Header.Get("Location") != "/ui/approve/" {
		t.Fatalf("approved tablet went to %q", res.Header.Get("Location"))
	}
	if tablet.deviceCookie() == "" {
		t.Fatal("the approved tablet holds no device cookie")
	}
	if res, _ := tablet.do(http.MethodGet, "/ui/approve/devices", nil, nil); res.StatusCode != 200 {
		t.Fatalf("the new device cannot see devices: %d", res.StatusCode)
	}
	// The second tab, after the first completed (amendment 9).
	if res, _ := tablet.do(http.MethodGet, "/ui/pair/wait", nil, nil); res.Header.Get("Location") != "/ui/approve/" {
		t.Fatalf("second tab went to %q", res.Header.Get("Location"))
	}
}

func TestPages_HashMismatchAndDoublePost(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: "apr_w", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "Your assistant wants X.", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	res, body := phone.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"approve"}, "rendered_sha256": {"stale"}}, nil)
	if res.StatusCode != http.StatusConflict || !strings.Contains(body, "changed after you opened it") || !strings.Contains(body, `value="hhh"`) {
		t.Fatalf("mismatch: %d %s", res.StatusCode, body)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_w"); r.Status != persistence.ApprovalPending {
		t.Fatalf("a mismatched hash decided the request: %s", r.Status)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	res, body = phone.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"reject"}, "rendered_sha256": {"hhh"}}, nil)
	if res.StatusCode != http.StatusConflict || strings.Contains(body, `name="decision"`) {
		t.Fatalf("double post: %d", res.StatusCode)
	}
}

func TestMiddleware_RefusesWithoutADevice(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	anon := newBrowser(t, srv)
	if res, _ := anon.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/ui/pair" {
		t.Fatalf("GET without a device: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := anon.do(http.MethodPost, "/ui/approve/apr_x", url.Values{"decision": {"approve"}}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without a device: %d", res.StatusCode)
	}
	// Credentials of every other kind are simply not read.
	for _, hdr := range []map[string]string{
		{"X-API-Key": "sk-admin"}, {"Authorization": "Bearer sk-admin"}, {"Cookie": "vornik_session=abc"},
	} {
		if res, _ := anon.do(http.MethodGet, "/ui/approve/", nil, hdr); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("%v admitted: %d", hdr, res.StatusCode)
		}
	}
}

// Plan amendment 1: SameSite=Strict plus approval.CheckRequest is the CSRF
// control of record, so a cross-site POST carrying a valid cookie is refused.
func TestMiddleware_CrossSitePostRefused(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	for name, hdr := range map[string]map[string]string{
		"sec-fetch-site cross-site": {"Sec-Fetch-Site": "cross-site", "Origin": ""},
		"foreign origin":            {"Origin": "https://evil.example"},
	} {
		res, _ := phone.do(http.MethodPost, "/ui/approve/devices/dev_x/revoke", url.Values{}, hdr)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: %d, want 403", name, res.StatusCode)
		}
	}
	res, _ := phone.do(http.MethodPost, "/ui/pair", url.Values{"code": {"X"}}, map[string]string{"Origin": "https://evil.example"})
	if res.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site pairing POST: %d", res.StatusCode)
	}
}

func TestCookie_Attributes(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/ui/pair", nil)
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	SetCookie(w, r, "tok")
	c := w.Result().Cookies()[0]
	if c.Name != CookieName || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/ui/" || !c.Secure || c.MaxAge != int(IdleExpiry.Seconds()) {
		t.Fatalf("cookie = %+v", c)
	}
	w = httptest.NewRecorder()
	SetCookie(w, httptest.NewRequest(http.MethodGet, "/ui/pair", nil), "tok")
	if w.Result().Cookies()[0].Secure {
		t.Fatal("Secure on plain HTTP would break the LAN preview")
	}
}

// Two tabs: one decides (rotating the cookie in the shared jar is what a real
// browser does, so the stale tab is simulated by replaying the old value).
func TestMiddleware_StaleTabIsConflict(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	old := phone.deviceCookie()
	d, _ := f.svc.Authenticate(context.Background(), old)
	if _, err := f.svc.Rotate(context.Background(), d, old); err != nil {
		t.Fatal(err)
	}
	// The old token no longer authenticates at all, so the stale tab is sent
	// to pairing on GET and refused on POST; it never decides.
	stale := newBrowser(t, srv)
	u, _ := url.Parse(srv.URL + "/ui/")
	stale.c.Jar.SetCookies(u, []*http.Cookie{{Name: CookieName, Value: old, Path: "/ui/"}})
	if res, _ := stale.do(http.MethodPost, "/ui/approve/apr_x", url.Values{"decision": {"approve"}}, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("stale tab POST: %d", res.StatusCode)
	}
}

func TestPages_PairLimiterIs429(t *testing.T) {
	f := newFixture(t, WithLimiters(chatauth.NewRedemptionLimiterWith(1, time.Hour), chatauth.NewRedemptionLimiterWith(100, time.Hour)))
	srv := serve(t, f)
	b := newBrowser(t, srv)
	b.pairWith("WRONG")
	if res := b.pairWith("WRONG"); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second attempt: %d, want 429", res.StatusCode)
	}
}

func TestPages_RevokeSelfClearsCookie(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	d, _ := f.svc.Authenticate(context.Background(), phone.deviceCookie())
	res, _ := phone.do(http.MethodPost, "/ui/approve/devices/"+d.ID+"/revoke", url.Values{}, nil)
	if res.StatusCode != http.StatusOK || phone.deviceCookie() != "" {
		t.Fatalf("self-revoke: %d, cookie %q", res.StatusCode, phone.deviceCookie())
	}
	if res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatal("a revoked device still reaches approvals")
	}
}

func TestRoutes_AllServed(t *testing.T) {
	f := newFixture(t)
	h := f.svc.Handler(nil)
	n := 0
	for _, rt := range Routes() {
		req := httptest.NewRequest(rt.Method, rt.Path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code == http.StatusNotFound {
			t.Errorf("%s %s is listed in Routes but not served", rt.Method, rt.Path)
		}
		n++
	}
	t.Logf("examined %d routes", n)
}
