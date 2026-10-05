package approverdevice

import (
	"context"
	"errors"
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
// A stale tab is one whose value is neither current nor previous: after the
// successor has been presented (amendment 2026-10-05), the old value is sent
// to pairing on GET and refused on POST; it never decides.
func TestMiddleware_StaleTabIsRefused(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	old := phone.deviceCookie()
	d, _ := f.svc.Authenticate(context.Background(), old)
	succ, err := f.svc.Rotate(context.Background(), d, old)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(context.Background(), succ); err != nil {
		t.Fatal(err)
	}
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

// The operator's iPhone, 2026-10-03 (agent-administered §9.2a): a link
// tapped in Telegram opened an unpaired browser, and pairing landed on the
// list, never on the request the link named.
func TestPairing_ReturnsToTheRequestedPage(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	ctx := context.Background()

	phone := newBrowser(t, srv)
	if res, _ := phone.do(http.MethodGet, "/ui/approve/apr_x", nil, nil); res.Header.Get("Location") != "/ui/pair?next=%2Fui%2Fapprove%2Fapr_x" {
		t.Fatalf("unpaired GET went to %q", res.Header.Get("Location"))
	}
	if _, body := phone.do(http.MethodGet, "/ui/pair?next=%2Fui%2Fapprove%2Fapr_x", nil, nil); !strings.Contains(body, `name="next" value="/ui/approve/apr_x"`) {
		t.Fatalf("the form does not carry next: %s", body)
	}
	code, _, _ := f.svc.StartPairing(ctx, "Pixel")
	res, _ := phone.do(http.MethodPost, "/ui/pair", url.Values{"code": {code}, "next": {"/ui/approve/apr_x"}}, nil)
	if res.Header.Get("Location") != "/ui/approve/apr_x" {
		t.Fatalf("first pairing went to %q", res.Header.Get("Location"))
	}

	safari := newBrowser(t, srv)
	code2, _, _ := f.svc.StartPairing(ctx, "Safari")
	res, _ = safari.do(http.MethodPost, "/ui/pair", url.Values{"code": {code2}, "next": {"/ui/approve/apr_y"}}, nil)
	if res.Header.Get("Location") != "/ui/pair/wait" {
		t.Fatalf("second pairing went to %q", res.Header.Get("Location"))
	}
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	id := regexp.MustCompile(`/ui/approve/(apr_[0-9a-f]+)`).FindStringSubmatch(list)
	_, page := phone.do(http.MethodGet, "/ui/approve/"+id[1], nil, nil)
	h := hashField.FindStringSubmatch(page)
	phone.do(http.MethodPost, "/ui/approve/"+id[1], url.Values{"decision": {"approve"}, "rendered_sha256": {h[1]}}, nil)
	if res, _ := safari.do(http.MethodGet, "/ui/pair/wait", nil, nil); res.Header.Get("Location") != "/ui/approve/apr_y" {
		t.Fatalf("approved wait went to %q", res.Header.Get("Location"))
	}
}

func TestPairing_DropsAForeignNext(t *testing.T) {
	for _, next := range []string{
		"https://evil.example/ui/approve/x", "//evil.example/ui/approve/x", "/ui/approve/../pair",
		"/ui/approve//evil", "/ui/projects", "/ui/approve/x?y=1", "javascript:alert(1)",
	} {
		t.Run(next, func(t *testing.T) {
			f := newFixture(t)
			srv := serve(t, f)
			phone := newBrowser(t, srv)
			if _, body := phone.do(http.MethodGet, "/ui/pair?next="+url.QueryEscape(next), nil, nil); strings.Contains(body, `name="next"`) {
				t.Fatalf("the form carries a foreign next: %s", body)
			}
			code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
			res, _ := phone.do(http.MethodPost, "/ui/pair", url.Values{"code": {code}, "next": {next}}, nil)
			if res.Header.Get("Location") != "/ui/approve/" {
				t.Fatalf("pairing followed %q to %q", next, res.Header.Get("Location"))
			}
		})
	}
}

const iPhoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Mobile/15E148 Telegram-iOS"

func TestPairing_SaysWhichBrowserAndOffersToReopen(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	_, body := phone.do(http.MethodGet, "/ui/pair?next=%2Fui%2Fapprove%2Fapr_x", nil, map[string]string{"User-Agent": iPhoneUA})
	for _, want := range []string{
		"paired in one browser",
		`href="googlechromes://vornik.example/ui/approve/apr_x"`,
		`href="x-safari-https://vornik.example/ui/approve/apr_x"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("iOS pair page lacks %q", want)
		}
	}
	_, body = phone.do(http.MethodGet, "/ui/pair?next=%2Fui%2Fapprove%2Fapr_x", nil, map[string]string{"User-Agent": "Mozilla/5.0 (Linux; Android 15) Chrome/130"})
	if !strings.Contains(body, "paired in one browser") || strings.Contains(body, "googlechromes:") {
		t.Errorf("non-iOS pair page: explanation or buttons wrong")
	}
	// The buttons name vornik.example, the configured origin, although the
	// request reached 127.0.0.1: the host is never taken from the request.

	code, _, _ := f.svc.StartPairing(context.Background(), "First")
	newBrowser(t, srv).pairWith(code)
	code2, _, _ := f.svc.StartPairing(context.Background(), "Second")
	phone.do(http.MethodPost, "/ui/pair", url.Values{"code": {code2}, "next": {"/ui/approve/apr_x"}}, nil)
	_, body = phone.do(http.MethodGet, "/ui/pair/wait", nil, map[string]string{"User-Agent": iPhoneUA})
	for _, want := range []string{"On the browser you paired before", "paired in one browser", `href="googlechromes://vornik.example/ui/approve/apr_x"`} {
		if !strings.Contains(body, want) {
			t.Errorf("wait page lacks %q", want)
		}
	}
}

func TestPush_NamesThePairedBrowser(t *testing.T) {
	f := newFixture(t)
	if err := f.svc.FileRequest(context.Background(), persistence.AgentApprovalRequestRow{
		ID: "apr_0000000000000001", Namespace: "claudecode", Kind: "widening_change", Sentence: "Raise a budget.",
		Rendered: []byte(`{}`), RenderedSHA256: "x", Status: "pending",
		CreatedAt: f.now, ExpiresAt: f.now.Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if len(f.pushes) != 1 || !strings.Contains(f.pushes[0].body, "Open it in the browser you paired (in Telegram: ••• → Open in Chrome or Safari).") {
		t.Fatalf("push: %+v", f.pushes)
	}
}

// Review 20261003-601e F2: x-safari-https does not work on iOS 16, so every
// browser is also given the address to copy, from the configured origin.
func TestPairing_OffersTheAddressToCopy(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	_, body := newBrowser(t, srv).do(http.MethodGet, "/ui/pair?next=%2Fui%2Fapprove%2Fapr_x", nil, map[string]string{"User-Agent": "Mozilla/5.0 (Linux; Android 15) Chrome/130"})
	if !strings.Contains(body, "https://vornik.example/ui/approve/apr_x") {
		t.Fatalf("no address to copy: %s", body)
	}
}

// Review 20261003-601e F5, F7: the next cookie is cleared once used, and a
// tampered one is re-validated and dropped.
func TestPairing_NextCookieIsClearedAndRevalidated(t *testing.T) {
	for name, val := range map[string]string{
		"valid":  url.QueryEscape("/ui/approve/apr_y"),
		"scheme": url.QueryEscape("https://evil.example/ui/approve/x"),
		"double": url.QueryEscape("/ui/approve//evil"),
		"other":  url.QueryEscape("/ui/projects"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			srv := serve(t, f)
			ctx := context.Background()
			phone := newBrowser(t, srv)
			code, _, _ := f.svc.StartPairing(ctx, "First")
			phone.pairWith(code)
			tablet := newBrowser(t, srv)
			code2, _, _ := f.svc.StartPairing(ctx, "Second")
			tablet.pairWith(code2)
			u, _ := url.Parse(srv.URL + "/ui/pair")
			tablet.c.Jar.SetCookies(u, []*http.Cookie{{Name: NextCookieName, Value: val, Path: "/ui/pair"}})
			_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
			id := regexp.MustCompile(`/ui/approve/(apr_[0-9a-f]+)`).FindStringSubmatch(list)
			_, page := phone.do(http.MethodGet, "/ui/approve/"+id[1], nil, nil)
			h := hashField.FindStringSubmatch(page)
			phone.do(http.MethodPost, "/ui/approve/"+id[1], url.Values{"decision": {"approve"}, "rendered_sha256": {h[1]}}, nil)
			want := "/ui/approve/"
			if name == "valid" {
				want = "/ui/approve/apr_y"
			}
			res, _ := tablet.do(http.MethodGet, "/ui/pair/wait", nil, nil)
			if got := res.Header.Get("Location"); got != want {
				t.Fatalf("approved wait went to %q, want %q", got, want)
			}
			for _, c := range tablet.c.Jar.Cookies(u) {
				if c.Name == NextCookieName {
					t.Fatal("the next cookie survived its use")
				}
			}
		})
	}
}

// P1 2026-10-05, approver devices unpaired at random (design §9.2, amendment
// "Rotation must survive a lost response"): the decision POST commits the
// rotation, but its response — the only carrier of the new cookie — never
// reaches the phone (a double tap, an app switch, a dropped connection). The
// phone still holds the old value; it must not be sent to pairing while every
// other device lists it as active.
func TestRotation_LostResponseDoesNotUnpairTheBrowser(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	applied := 0
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { applied++; return nil })
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: "apr_w", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "Your assistant wants X.", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	old := phone.deviceCookie()

	// The POST reaches the server; its response never reaches the phone.
	lost := newBrowser(t, srv)
	u, _ := url.Parse(srv.URL + "/ui/")
	lost.c.Jar.SetCookies(u, []*http.Cookie{{Name: CookieName, Value: old, Path: "/ui/"}})
	if res, _ := lost.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	if r, _ := f.repo.GetRequest(context.Background(), "apr_w"); r.Status != persistence.ApprovalApproved || applied != 1 {
		t.Fatalf("decision: %s, applied %d", r.Status, applied)
	}
	if devs, _ := f.svc.ListDevices(context.Background()); len(devs) != 1 || devs[0].RevokedAt != nil {
		t.Fatalf("devices: %+v", devs)
	}

	// The phone, still holding the old value, is admitted — not paired again —
	// and the response re-sends the successor the lost response carried.
	res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("phone after a lost rotation response: %d %s — the device is listed active but its browser is unpaired",
			res.StatusCode, res.Header.Get("Location"))
	}
	succ := phone.deviceCookie()
	if succ == old {
		t.Fatal("the fallback GET did not re-send the successor")
	}
	// Presented once, the successor confirms the rotation; the old value dies.
	if res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK || phone.deviceCookie() != succ {
		t.Fatalf("the successor: %d, cookie changed %v", res.StatusCode, phone.deviceCookie() != succ)
	}
	if _, err := f.svc.Authenticate(context.Background(), old); !errors.Is(err, ErrNoDevice) {
		t.Fatalf("the old value after the successor was presented: %v", err)
	}
	// Within the quiet window: counted, no alert.
	f.assertNoPushContaining(t, "previous credential")
}

// P1 2026-10-05, the double tap and out-of-order completion: two decision
// POSTs leave with the same cookie, from different tabs, and complete in any
// order. Every response carries the SAME successor, so whichever the browser
// keeps works; the decision is made once.
func TestRotation_DoubleTapResponsesCarryOneCookie(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	applied := 0
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { applied++; return nil })
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: "apr_w", Kind: persistence.ApprovalKindWideningChange,
		Sentence: "Your assistant wants X.", Rendered: []byte(`{"a":1}`), RenderedSHA256: "hhh", Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
	old := phone.deviceCookie()
	tab := func() *browser {
		b := newBrowser(t, srv)
		u, _ := url.Parse(srv.URL + "/ui/")
		b.c.Jar.SetCookies(u, []*http.Cookie{{Name: CookieName, Value: old, Path: "/ui/"}})
		return b
	}
	one, two := tab(), tab()
	if res, _ := one.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("first tap: %d", res.StatusCode)
	}
	res, _ := two.do(http.MethodPost, "/ui/approve/apr_w", url.Values{"decision": {"approve"}, "rendered_sha256": {"hhh"}}, nil)
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("second tap on a decided request: %d, want 409", res.StatusCode)
	}
	if applied != 1 {
		t.Fatalf("applied %d times, want 1", applied)
	}
	if one.deviceCookie() == old || one.deviceCookie() != two.deviceCookie() {
		t.Fatalf("the two responses carry different cookies: %q vs %q", one.deviceCookie(), two.deviceCookie())
	}
	for name, b := range map[string]*browser{"first": one, "second": two} {
		if res, _ := b.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK {
			t.Fatalf("the %s response's cookie: %d", name, res.StatusCode)
		}
	}
}

// pairedPhone pairs the first device and registers a no-op widening effect;
// requests are filed with fileReq.
func pairedPhone(t *testing.T, f *fixture, srv *httptest.Server) *browser {
	t.Helper()
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.svc.RegisterEffect(persistence.ApprovalKindWideningChange, func(context.Context, persistence.AgentApprovalRequestRow) error { return nil })
	return phone
}

func fileReq(t *testing.T, f *fixture, id string) {
	t.Helper()
	if err := f.repo.CreateRequest(context.Background(), persistence.AgentApprovalRequestRow{ID: id, Kind: persistence.ApprovalKindWideningChange,
		Sentence: "Your assistant wants " + id + ".", Rendered: []byte(`{"id":"` + id + `"}`), RenderedSHA256: "sha-" + id, Status: persistence.ApprovalPending,
		CreatedAt: f.clock(), ExpiresAt: f.clock().Add(RequestTTL)}); err != nil {
		t.Fatal(err)
	}
}

func approve(b *browser, id string) *http.Response {
	res, _ := b.do(http.MethodPost, "/ui/approve/"+id, url.Values{"decision": {"approve"}, "rendered_sha256": {"sha-" + id}}, nil)
	return res
}

// holding returns a browser whose jar holds value v.
func holding(t *testing.T, srv *httptest.Server, v string) *browser {
	b := newBrowser(t, srv)
	u, _ := url.Parse(srv.URL + "/ui/")
	b.c.Jar.SetCookies(u, []*http.Cookie{{Name: CookieName, Value: v, Path: "/ui/"}})
	return b
}

// P1 2026-10-05: a response lost OUTSIDE the share still signs the phone out
// (strict), but the pairing page explains why and a terminal code restores
// the SAME device row, with one alert carrying no secret.
func TestRotation_LostResponseAfterTheShareResumesWithACode(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	fileReq(t, f, "apr_1")
	old := phone.deviceCookie()
	before, _ := f.svc.ListDevices(context.Background())
	if res := approve(holding(t, srv, old), "apr_1"); res.StatusCode != http.StatusSeeOther { // response lost
		t.Fatalf("approve: %d", res.StatusCode)
	}
	f.advance(ShareGrace + time.Second)
	res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(res.Header.Get("Location"), "/ui/pair") {
		t.Fatalf("after the share: %d %s, want the pairing page", res.StatusCode, res.Header.Get("Location"))
	}
	// The reason is never read from the query.
	_, body := phone.do(http.MethodGet, "/ui/pair?reason=confirmed&signed_out=x", nil, nil)
	if !strings.Contains(body, "did not reach it") || strings.Contains(body, "another browser used") {
		t.Fatalf("pairing page: %s", body)
	}
	code, _, _ := f.svc.StartPairing(context.Background(), "ignored")
	if res := phone.pairWith(code); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("resume: %d", res.StatusCode)
	}
	if res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("after resume: %d", res.StatusCode)
	}
	after, _ := f.svc.ListDevices(context.Background())
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("resume must restore the same device row: %+v", after)
	}
	if n := f.countPushesContaining("was restored"); n != 1 {
		t.Fatalf("restore alerts = %d, want 1", n)
	}
	f.assertNoSecretsPushed(t, old, phone.deviceCookie(), code)
}

// The thief approves first and follows its redirect: the phone's value dies
// as confirmed. The phone is told so, and a code does NOT restore it — it
// takes the further-device path (an enrollment request), as built.
func TestRotation_ConfirmedDeadValueIsNotResumable(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	fileReq(t, f, "apr_1")
	thief := holding(t, srv, phone.deviceCookie())
	if res := approve(thief, "apr_1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("thief approve: %d", res.StatusCode)
	}
	if res, _ := thief.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK { // the redirect
		t.Fatalf("thief redirect: %d", res.StatusCode)
	}
	_, body := phone.do(http.MethodGet, "/ui/pair", nil, nil)
	if !strings.Contains(body, "another browser used") {
		t.Fatalf("pairing page: %s", body)
	}
	code, _, _ := f.svc.StartPairing(context.Background(), "Phone again")
	if res := phone.pairWith(code); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/ui/pair/wait" {
		t.Fatalf("a confirmed dead value with a code: %d %s, want the enrollment path", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := thief.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatal("nothing should have signed the thief out yet")
	}
	f.assertNoPushContaining(t, "was restored")
}

// The successor delivered, the browser backgrounded past the share: its
// predecessor expires although nothing was lost. Presenting the successor
// relabels it confirmed, so a clone of the predecessor cannot resume.
func TestRotation_ExpiredThenSuccessorPresentedIsNotResumable(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	fileReq(t, f, "apr_1")
	old := phone.deviceCookie()
	if res := approve(phone, "apr_1"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("approve: %d", res.StatusCode)
	}
	f.advance(ShareGrace + time.Second)
	if err := f.svc.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK {
		t.Fatalf("the delivered successor: %d", res.StatusCode)
	}
	clone := holding(t, srv, old)
	code, _, _ := f.svc.StartPairing(context.Background(), "x")
	if res := clone.pairWith(code); res.Header.Get("Location") != "/ui/pair/wait" {
		t.Fatalf("clone of the predecessor with a code: %d %s, want the enrollment path", res.StatusCode, res.Header.Get("Location"))
	}
}

// A clone admitted in two consecutive shares — with the device's own
// confirming redirect between them — raises ONE alert with no secret. A
// single double tap raises none.
func TestRotation_CloneInConsecutiveSharesAlerts(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	clone := holding(t, srv, phone.deviceCookie())
	for i, id := range []string{"apr_1", "apr_2", "apr_3"} {
		fileReq(t, f, id)
		if res := approve(phone, id); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("approve %s: %d", id, res.StatusCode)
		}
		if i < 2 { // the clone polls inside the share, with the value it holds
			if res, _ := clone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK {
				t.Fatalf("clone in share %d: %d", i, res.StatusCode)
			}
		}
		if res, _ := phone.do(http.MethodGet, "/ui/approve/", nil, nil); res.StatusCode != http.StatusOK { // the redirect
			t.Fatalf("phone redirect %d: %d", i, res.StatusCode)
		}
		want := 0
		if i >= 1 {
			want = 1
		}
		if n := f.countPushesContaining("another browser has been using"); n != want {
			t.Fatalf("after share %d: clone alerts = %d, want %d", i, n, want)
		}
	}
	f.assertNoSecretsPushed(t, phone.deviceCookie(), clone.deviceCookie())
}

// A slow decision whose successor is superseded before its response is
// written sends no cookie (lazy write), so the jar keeps the newer value.
func TestRotation_SupersededCookieIsNotWritten(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	d, tok := f.firstDevice(t)
	slow, err := f.svc.rotate(ctx, d, tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Authenticate(ctx, slow.token); err != nil { // another tab got it and moved on
		t.Fatal(err)
	}
	if _, err := f.svc.Rotate(ctx, d, slow.token); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/ui/approve/x", nil)
	w := f.svc.withCookie(rec, req, slow.token)
	w.WriteHeader(http.StatusSeeOther)
	if got := rec.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Fatalf("a superseded successor was written: %v", got)
	}
	// Current: written, also on an implicit 200 (Write without WriteHeader).
	cur, _ := f.svc.Rotate(ctx, d, slow.token)
	rec = httptest.NewRecorder()
	w = f.svc.withCookie(rec, req, cur)
	_, _ = w.Write([]byte("ok"))
	if got := rec.Header().Get("Set-Cookie"); !strings.Contains(got, cur) {
		t.Fatalf("the current successor was not written on an implicit 200: %q", got)
	}
}

// A paired browser that reaches /ui/pair (a stale tab's redirect) is sent on,
// never shown the form.
func TestPair_ValidDeviceIsRedirected(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	if res, _ := phone.do(http.MethodGet, "/ui/pair?next=%2Fui%2Fapprove%2Fapr_9", nil, nil); res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/ui/approve/apr_9" {
		t.Fatalf("paired browser on /ui/pair: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// The share is closed in a defer: a decision that rotates and then renders an
// error (the request was already decided) still ends its share ShareGrace
// later, not at the ShareCap backstop.
func TestRotation_ErrorRenderClosesTheShare(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := pairedPhone(t, f, srv)
	fileReq(t, f, "apr_1")
	approve(phone, "apr_1")
	phone.do(http.MethodGet, "/ui/approve/", nil, nil) // confirm
	if res := approve(phone, "apr_1"); res.StatusCode != http.StatusConflict {
		t.Fatalf("second decision: %d, want 409", res.StatusCode)
	}
	d, err := f.repo.GetDeviceByTokenHash(context.Background(), HashToken(phone.deviceCookie()))
	if err != nil || d.ShareUntil == nil || !d.ShareUntil.Equal(f.clock().Add(ShareGrace)) {
		t.Fatalf("share after an error render: %+v, %v; want share_until = now+%v", d, err, ShareGrace)
	}
}
