package approverdevice

// GitHub #163 (agent-administered design part 09 §9.3a): Web Push to
// approver devices. Every push that goes to the operator channel also goes,
// encrypted and content-free, to each device that turned notifications on.

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/webpush"
)

type sentPush struct {
	endpoint string
	payload  map[string]string
}

type fakeSender struct {
	mu      sync.Mutex
	sent    []sentPush
	answer  func(endpoint string) webpush.Result
	panics  bool
	nCalled int
}

func (f *fakeSender) PublicKey() string { return "BPUBLICKEY" }

func (f *fakeSender) Send(_ context.Context, endpoint, _, _ string, payload []byte) webpush.Result {
	f.mu.Lock()
	f.nCalled++
	panics := f.panics
	f.mu.Unlock()
	if panics {
		panic("sender broke")
	}
	var p map[string]string
	_ = json.Unmarshal(payload, &p)
	f.mu.Lock()
	f.sent = append(f.sent, sentPush{endpoint, p})
	answer := f.answer
	f.mu.Unlock()
	if answer != nil {
		return answer(endpoint)
	}
	return webpush.Result{Outcome: webpush.OutcomeSent, Status: http.StatusCreated}
}

func (f *fakeSender) list() []sentPush {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sentPush(nil), f.sent...)
}

type pushOutcomes struct {
	mu sync.Mutex
	n  map[string]int
}

func (o *pushOutcomes) record(outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n == nil {
		o.n = map[string]int{}
	}
	o.n[outcome]++
}

func (o *pushOutcomes) get(outcome string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.n[outcome]
}

func pushKeys(t *testing.T) (string, string) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(k.PublicKey().Bytes()), base64.RawURLEncoding.EncodeToString(make([]byte, 16))
}

func newPushFixture(t *testing.T) (*fixture, *fakeSender, *pushOutcomes) {
	t.Helper()
	s := &fakeSender{}
	o := &pushOutcomes{}
	f := newFixture(t, func(svc *Service) {
		svc.pushRepo = svc.repo.(persistence.ApproverPushRepository)
		svc.pushSender = s
		svc.pushOutcome = o.record
	})
	return f, s, o
}

func (f *fixture) subscribe(t *testing.T, deviceID, endpoint string) {
	t.Helper()
	p, a := pushKeys(t)
	if err := f.svc.SubscribePush(context.Background(), deviceID, endpoint, p, a); err != nil {
		t.Fatalf("subscribe %s: %v", deviceID, err)
	}
}

// One push reaches the operator notifier and each subscribed device, with
// the subject as the title and the approvals list as the link, never the body.
func TestWebPush_FanOutToOperatorAndDevices(t *testing.T) {
	f, s, o := newPushFixture(t)
	ctx := context.Background()
	first, _ := f.firstDevice(t)
	f.svc.waitPushes() // the enrollment alert, before any subscription
	if len(s.list()) != 0 {
		t.Fatalf("pushed before any subscription: %+v", s.list())
	}
	f.subscribe(t, first.ID, "https://fcm.googleapis.com/fcm/send/one")

	code := f.pair(t, "Tablet")
	if _, err := f.svc.Redeem(ctx, code, "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	f.svc.waitPushes()

	got := s.list()
	if len(got) != 1 || got[0].endpoint != "https://fcm.googleapis.com/fcm/send/one" {
		t.Fatalf("device pushes = %+v", got)
	}
	if got[0].payload["title"] != "Vornik: a new device wants to approve for you" || got[0].payload["url"] != "/ui/approve/" || len(got[0].payload) != 2 {
		t.Fatalf("payload = %+v", got[0].payload)
	}
	if f.countPushesContaining("a new device wants to approve for you") != 1 {
		t.Fatalf("operator channel did not get the push: %+v", f.pushes)
	}
	row, err := f.svc.PushStatus(ctx, first.ID)
	if err != nil || row.LastStatus != "sent 201" || row.LastSentAt == nil {
		t.Fatalf("recorded = %+v, %v", row, err)
	}
	if o.get(webpush.OutcomeSent) != 1 {
		t.Fatalf("sent pushOutcomes = %d", o.get(webpush.OutcomeSent))
	}
}

func TestWebPush_GoneDeletesFailedRecords(t *testing.T) {
	f, s, o := newPushFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	f.svc.waitPushes()
	f.subscribe(t, d.ID, "https://fcm.googleapis.com/fcm/send/x")

	s.answer = func(string) webpush.Result {
		return webpush.Result{Outcome: webpush.OutcomeFailed, Status: http.StatusForbidden}
	}
	f.svc.push(ctx, "Vornik: test subject", "body")
	f.svc.waitPushes()
	row, err := f.svc.PushStatus(ctx, d.ID)
	if err != nil || row.LastStatus != "failed 403" {
		t.Fatalf("failed push recorded = %+v, %v", row, err)
	}

	s.answer = func(string) webpush.Result {
		return webpush.Result{Outcome: webpush.OutcomeGone, Status: http.StatusGone}
	}
	f.svc.push(ctx, "Vornik: test subject", "body")
	f.svc.waitPushes()
	if _, err := f.svc.PushStatus(ctx, d.ID); err == nil {
		t.Fatal("a 410 must delete the subscription")
	}
	if o.get(webpush.OutcomeFailed) != 1 || o.get(webpush.OutcomeGone) != 1 {
		t.Fatalf("pushOutcomes = %+v", o.n)
	}
}

func TestWebPush_SenderPanicIsRecovered(t *testing.T) {
	f, s, o := newPushFixture(t)
	d, _ := f.firstDevice(t)
	f.svc.waitPushes()
	f.subscribe(t, d.ID, "https://fcm.googleapis.com/fcm/send/x")
	s.panics = true
	f.svc.push(context.Background(), "Vornik: test subject", "body")
	f.svc.waitPushes()
	if o.get(webpush.OutcomeFailed) != 1 {
		t.Fatalf("a panicking send counts failed: %+v", o.n)
	}
}

func TestWebPush_RevokedDeviceIsNeverSentTo(t *testing.T) {
	f, s, _ := newPushFixture(t)
	ctx := context.Background()
	d, _ := f.firstDevice(t)
	f.svc.waitPushes()
	f.subscribe(t, d.ID, "https://fcm.googleapis.com/fcm/send/x")
	if err := f.svc.Revoke(ctx, d.ID); err != nil {
		t.Fatal(err)
	}
	f.svc.push(ctx, "Vornik: test subject", "body")
	f.svc.waitPushes()
	if len(s.list()) != 0 {
		t.Fatalf("sent to a revoked device: %+v", s.list())
	}
	if _, err := f.svc.PushStatus(ctx, d.ID); err == nil {
		t.Fatal("revoke must delete the subscription")
	}
}

func TestWebPush_SubscribeRefusesBadInput(t *testing.T) {
	f, _, _ := newPushFixture(t)
	d, _ := f.firstDevice(t)
	p, a := pushKeys(t)
	ctx := context.Background()
	for name, in := range map[string][3]string{
		"another host":   {"https://evil.example/x", p, a},
		"plain http":     {"http://fcm.googleapis.com/x", p, a},
		"bad p256dh":     {"https://fcm.googleapis.com/x", "AAAA", a},
		"short auth":     {"https://fcm.googleapis.com/x", p, "AAAA"},
		"empty endpoint": {"", p, a},
	} {
		if err := f.svc.SubscribePush(ctx, d.ID, in[0], in[1], in[2]); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := f.svc.PushStatus(ctx, d.ID); err == nil {
		t.Fatal("a refused subscription was stored")
	}
}

// The pages: the push page shows the key and the state; subscribe, test and
// off need the device session and same origin; the push page alone allows
// a same-origin script.
func TestWebPush_Pages(t *testing.T) {
	f, s, _ := newPushFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	f.svc.waitPushes()

	res, page := phone.do(http.MethodGet, "/ui/approve/push", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, `data-key="BPUBLICKEY"`) || !strings.Contains(page, "Notifications are off for this device") {
		t.Fatalf("push page %d:\n%s", res.StatusCode, page)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "script-src 'none'") {
		t.Fatalf("push page CSP %q", csp)
	}
	// Every other approval page keeps script-src 'none' with Web Push on
	// (§9.3a B.6; extends TestLook_CSPOnEveryResponse).
	for _, path := range []string{"/ui/approve/", "/ui/approve/devices", "/ui/approve/standing", "/ui/approve/memory", "/ui/approve/connectors", "/ui/approve/push/", "/ui/approve/pushx"} {
		res, _ := phone.do(http.MethodGet, path, nil, nil)
		if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
			t.Errorf("%s (%d): CSP %q", path, res.StatusCode, csp)
		}
	}

	p, a := pushKeys(t)
	form := url.Values{"endpoint": {"https://web.push.apple.com/abc"}, "p256dh": {p}, "auth": {a}}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/push", form, map[string]string{"Origin": "https://evil.example"}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin subscribe: %d", res.StatusCode)
	}
	bad := url.Values{"endpoint": {"https://evil.example/abc"}, "p256dh": {p}, "auth": {a}}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/push", bad, nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad endpoint: %d", res.StatusCode)
	}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/push", form, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("subscribe: %d", res.StatusCode)
	}
	_, page = phone.do(http.MethodGet, "/ui/approve/push", nil, nil)
	if !strings.Contains(page, "Notifications are on for this device") {
		t.Fatalf("after subscribe:\n%s", page)
	}
	_, list := phone.do(http.MethodGet, "/ui/approve/", nil, nil)
	if !strings.Contains(list, "This device: notifications on") {
		t.Fatalf("list line:\n%s", list)
	}

	if res, _ := phone.do(http.MethodPost, "/ui/approve/push/test", url.Values{}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("test push: %d", res.StatusCode)
	}
	got := s.list()
	if len(got) != 1 || got[0].endpoint != "https://web.push.apple.com/abc" || got[0].payload["title"] != "Vornik: test notification" {
		t.Fatalf("test push sent = %+v", got)
	}

	if res, _ := phone.do(http.MethodPost, "/ui/approve/push/off", url.Values{}, nil); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("off: %d", res.StatusCode)
	}
	_, page = phone.do(http.MethodGet, "/ui/approve/push", nil, nil)
	if !strings.Contains(page, "Notifications are off for this device") {
		t.Fatalf("after off:\n%s", page)
	}

	// Without a device session nothing is stored.
	stranger := newBrowser(t, srv)
	if res, _ := stranger.do(http.MethodPost, "/ui/approve/push", form, nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("no device: %d", res.StatusCode)
	}
}

// Without a VAPID key (no secret store) the page says so and nothing is
// accepted.
func TestWebPush_UnavailableWithoutSender(t *testing.T) {
	f := newFixture(t)
	srv := serve(t, f)
	phone := newBrowser(t, srv)
	code, _, _ := f.svc.StartPairing(context.Background(), "Pixel")
	phone.pairWith(code)
	res, page := phone.do(http.MethodGet, "/ui/approve/push", nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "not available on this installation") || strings.Contains(page, "<script") {
		t.Fatalf("unavailable page %d:\n%s", res.StatusCode, page)
	}
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'none'") {
		t.Fatalf("no script without push: %q", csp)
	}
	p, a := pushKeys(t)
	form := url.Values{"endpoint": {"https://web.push.apple.com/abc"}, "p256dh": {p}, "auth": {a}}
	if res, _ := phone.do(http.MethodPost, "/ui/approve/push", form, nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("subscribe without push: %d", res.StatusCode)
	}
}
