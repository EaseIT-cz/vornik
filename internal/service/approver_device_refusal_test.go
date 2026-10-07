package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/approverdevice"
)

// Agent-administered Vornik design §9.2, plan P2.7: on every device route,
// every credential that is not a device cookie is refused, on the daemon's
// REAL mux (NewContainer, auth on), including an admin key that also holds
// operator rights. The route list comes from approverdevice.Routes(), so a
// route added there is covered here without editing this test.
func TestApproverDeviceRoutes_RefuseEveryNonDeviceCredential(t *testing.T) {
	const key = "sk-vornik-approver-refusal-admin"
	cfg := newComposerWiringTestConfig(t)
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{key}
	cfg.Admin.Enabled = true
	cfg.Admin.AllowedKeys = []string{key}
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := c.approverDeviceService()
	if svc == nil {
		t.Fatal("precondition: the approver device service is not wired")
	}
	ctx := context.Background()

	// A revoked device: a cookie that was once valid.
	code, _, _ := svc.StartPairing(ctx, "Old phone")
	red, err := svc.Redeem(ctx, code, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, red.Device.ID); err != nil {
		t.Fatal(err)
	}
	revoked := red.DeviceToken

	creds := nonDeviceCredentials(key, revoked)
	examined := 0
	for _, rt := range approverdevice.Routes() {
		if !rt.DeviceOnly {
			continue
		}
		for name, apply := range creds {
			var body *strings.Reader
			if rt.Method == http.MethodPost {
				body = strings.NewReader(url.Values{"decision": {"approve"}, "rendered_sha256": {"x"}}.Encode())
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(rt.Method, rt.Path, body)
			req.Host = "localhost" // the approver pages refuse plain http to a non-loopback host (T12); this test is about the device gate
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			apply(req)
			rec := httptest.NewRecorder()
			c.HTTPServer.Handler.ServeHTTP(rec, req)
			examined++
			switch {
			// The pairing page, carrying the page asked for (agent-administered
			// §9.2a): nothing but pairing is reachable without a device.
			case rt.Method == http.MethodGet && rec.Code == http.StatusSeeOther && pairingRedirect(rec.Header().Get("Location"), rt.Path):
			case rt.Method == http.MethodPost && rec.Code == http.StatusForbidden:
			default:
				t.Errorf("%s %s with %s: %d (Location %q), want the device refusal",
					rt.Method, rt.Path, name, rec.Code, rec.Header().Get("Location"))
			}
		}
	}
	if examined == 0 {
		t.Fatal("no device route was examined")
	}
	t.Logf("examined %d route x credential pairs", examined)

	// Positive control: the same mux admits a live device, so the refusals
	// above are the device gate, not an unreachable mount.
	code2, _, _ := svc.StartPairing(ctx, "Phone")
	live, err := svc.Redeem(ctx, code2, "10.0.0.2")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/ui/approve/", nil)
	req.Host = "localhost" // the approver pages refuse plain http to a non-loopback host (T12); this test is about the device gate
	req.AddCookie(&http.Cookie{Name: approverdevice.CookieName, Value: live.DeviceToken})
	rec := httptest.NewRecorder()
	c.HTTPServer.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Waiting for you") {
		t.Fatalf("a live device was not admitted on the real mux: %d", rec.Code)
	}
}

// nonDeviceCredentials is every credential that is not a live device cookie:
// the admin key three ways (it holds operator rights), a web session, a
// companion key, a revoked and a forged device cookie, and none.
func nonDeviceCredentials(key, revoked string) map[string]func(*http.Request) {
	return map[string]func(*http.Request){
		"no credential":         func(*http.Request) {},
		"admin key, X-API-Key":  func(r *http.Request) { r.Header.Set("X-API-Key", key) },
		"admin key, Bearer":     func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key) },
		"admin key, Basic":      func(r *http.Request) { r.SetBasicAuth("operator", key) },
		"web session cookie":    func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "vornik_session", Value: "sess-admin"}) },
		"companion-style key":   func(r *http.Request) { r.Header.Set("X-API-Key", "sk-vornik-companion-hermes") },
		"revoked device cookie": func(r *http.Request) { r.AddCookie(&http.Cookie{Name: approverdevice.CookieName, Value: revoked}) },
		"forged device cookie":  func(r *http.Request) { r.AddCookie(&http.Cookie{Name: approverdevice.CookieName, Value: "forged"}) },
	}
}

// Hermes approval transport design §8 (self-approval enumeration): a
// host_action joins the refusal matrix. A real host_action request is posted
// every one of its answers with every non-device credential on the daemon's
// real mux; none decides it.
func TestApproverDeviceRoutes_HostActionRefusesEveryNonDeviceCredential(t *testing.T) {
	const key = "sk-vornik-approver-refusal-admin"
	cfg := newComposerWiringTestConfig(t)
	cfg.API.AuthEnabled = true
	cfg.API.APIKeys = []string{key}
	cfg.Admin.Enabled = true
	cfg.Admin.AllowedKeys = []string{key}
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	svc := c.approverDeviceService()
	if svc == nil {
		t.Fatal("precondition: the approver device service is not wired")
	}
	ctx := context.Background()
	code, _, _ := svc.StartPairing(ctx, "Old phone")
	red, err := svc.Redeem(ctx, code, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(ctx, red.Device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.FileHostAction(ctx, "hermes", approverdevice.HostActionRequest{
		RequestID: "cafe01", Digest: strings.Repeat("c", 64), Command: "rm -rf /tmp/x", Description: "recursive delete",
		PatternKey: "rm", Surface: "cli", TimeoutSeconds: 300, AllowedChoices: []string{"once", "session", "always", "deny"}}); err != nil {
		t.Fatal(err)
	}
	id := approverdevice.HostActionID("hermes", "cafe01")
	row, _ := svc.Request(ctx, id)
	examined := 0
	for name, apply := range nonDeviceCredentials(key, red.DeviceToken) {
		for _, choice := range []string{"once", "session", "always", "deny", "approve"} {
			body := strings.NewReader(url.Values{"decision": {choice}, "rendered_sha256": {row.RenderedSHA256}}.Encode())
			req := httptest.NewRequest(http.MethodPost, "/ui/approve/"+id, body)
			req.Host = "localhost" // the approver pages refuse plain http to a non-loopback host (T12); this test is about the device gate
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			apply(req)
			rec := httptest.NewRecorder()
			c.HTTPServer.Handler.ServeHTTP(rec, req)
			examined++
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s answering %s: %d, want 403", name, choice, rec.Code)
			}
		}
	}
	if r, _ := svc.Request(ctx, id); r.Status != "pending" || r.DecidedChoice != "" {
		t.Fatalf("a non-device credential decided the host action: %+v", r)
	}
	t.Logf("examined %d credential x answer pairs", examined)
}

// Plan amendment 2: the per-IP backstop wraps the device routes even though
// they are mounted outside AuthMiddleware. The config is a ui-profile node,
// where until 2026-10-02 the limiter was never allocated at all (it sat below
// initScheduler's worker gate), so this also pins that fix.
func TestApproverDeviceRoutes_PerIPBackstop(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	if cfg.Node.Profile != "ui" {
		t.Fatalf("precondition: a ui-profile node, got %q", cfg.Node.Profile)
	}
	cfg.API.RateLimit.PerIP.RPS = 1
	cfg.API.RateLimit.PerIP.Burst = 2
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	limited := false
	for i := 0; i < 20 && !limited; i++ {
		req := httptest.NewRequest(http.MethodGet, "/ui/pair", nil)
		req.Host = "localhost" // the approver pages refuse plain http to a non-loopback host (T12); this test is about the device gate
		req.RemoteAddr = "203.0.113.7:1234"
		rec := httptest.NewRecorder()
		c.HTTPServer.Handler.ServeHTTP(rec, req)
		limited = rec.Code == http.StatusTooManyRequests
	}
	if !limited {
		t.Fatal("20 rapid requests to /ui/pair from one IP were never throttled")
	}
}

// Design §9.3: the push uses the operator alert channel when one is
// configured, and the service says so; without one, pairing still works and
// the doctor reports it.
func TestApproverDeviceService_PushFollowsTheAlertChannel(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if svc := c.approverDeviceService(); svc == nil || svc.PushConfigured() {
		t.Fatal("no alert channel configured, yet the service claims to push")
	}

	cfg2 := newComposerWiringTestConfig(t)
	cfg2.SteeringNotificationsEnabled = true
	cfg2.SteeringOperatorAlert.Channel = "email"
	cfg2.SteeringOperatorAlert.Address = "operator@example.test"
	c2, err := NewContainer(cfg2, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if svc := c2.approverDeviceService(); svc == nil || !svc.PushConfigured() {
		t.Fatal("an alert channel is configured, but the service does not push")
	}
}

// Review 20261002-ea5a F9: a channel with steering notifications turned off
// sends nothing, so the service must not claim to push (the CLI uses the same
// predicate, config.OperatorAlertActive).
func TestApproverDeviceService_ChannelWithNotificationsOffDoesNotPush(t *testing.T) {
	cfg := newComposerWiringTestConfig(t)
	cfg.SteeringNotificationsEnabled = false
	cfg.SteeringOperatorAlert.Channel = "email"
	cfg.SteeringOperatorAlert.Address = "operator@example.test"
	c, err := NewContainer(cfg, isolatedConfigPath(t))
	if err != nil {
		t.Fatal(err)
	}
	if svc := c.approverDeviceService(); svc == nil || svc.PushConfigured() {
		t.Fatal("notifications are off, yet the service claims to push")
	}
	if cfg.OperatorAlertActive() {
		t.Fatal("OperatorAlertActive true with notifications off")
	}
}

// pairingRedirect: the refusal for a GET is the pairing page, optionally
// with next naming exactly the page asked for, and nowhere else.
func pairingRedirect(loc, path string) bool {
	return loc == "/ui/pair" || loc == "/ui/pair?next="+url.QueryEscape(path)
}
