package a2a

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// allowLoopbackForTest relaxes the connect-time SSRF guard so a test can reach
// an httptest server (which binds 127.0.0.1). Restored after the test. The
// guard itself is exercised by TestPushNotifier_BlocksInternalTarget, which
// does NOT call this.
func allowLoopbackForTest(t *testing.T) {
	t.Helper()
	prev := blockPushIP
	blockPushIP = func(net.IP) bool { return false }
	t.Cleanup(func() { blockPushIP = prev })
}

type fakePushRepo struct {
	cfg *persistence.A2APushConfig
}

func (f fakePushRepo) Get(_ context.Context, _ string) (*persistence.A2APushConfig, error) {
	if f.cfg == nil {
		return nil, persistence.ErrNotFound
	}
	return f.cfg, nil
}

type capturedPush struct {
	count int
	auth  string
	body  pushEnvelope
}

func pushServer(t *testing.T, cap *capturedPush) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.count++
		cap.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &cap.body)
		w.WriteHeader(http.StatusOK)
	}))
}

func TestPushNotifier_Completed(t *testing.T) {
	allowLoopbackForTest(t)
	cap := &capturedPush{}
	srv := pushServer(t, cap)
	defer srv.Close()

	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL, Token: "secret"}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")

	if cap.count != 1 {
		t.Fatalf("want 1 webhook POST, got %d", cap.count)
	}
	if cap.body.Status.State != "completed" || !cap.body.Status.Final {
		t.Errorf("state=%q final=%v, want completed/true", cap.body.Status.State, cap.body.Status.Final)
	}
	if cap.auth != "Bearer secret" {
		t.Errorf("auth header = %q, want Bearer secret", cap.auth)
	}
}

func TestPushNotifier_FailedAndCanceled(t *testing.T) {
	allowLoopbackForTest(t)
	for _, tc := range []struct {
		msg  string
		want string
	}{
		{"boom error", "failed"},
		{"Task cancelled", "canceled"},
	} {
		cap := &capturedPush{}
		srv := pushServer(t, cap)
		n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
		n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, false, tc.msg)
		if cap.body.Status.State != tc.want {
			t.Errorf("msg %q → state %q, want %q", tc.msg, cap.body.Status.State, tc.want)
		}
		srv.Close()
	}
}

func TestPushNotifier_SteeringInputRequired(t *testing.T) {
	allowLoopbackForTest(t)
	cap := &capturedPush{}
	srv := pushServer(t, cap)
	defer srv.Close()
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
	n.NotifySteeringRequired(context.Background(), &persistence.Task{ID: "t1"}, "AWAITING_INPUT")
	if cap.count != 1 || cap.body.Status.State != "input-required" || cap.body.Status.Final {
		t.Fatalf("want 1 input-required non-final POST, got count=%d state=%q final=%v", cap.count, cap.body.Status.State, cap.body.Status.Final)
	}
}

func TestPushNotifier_NoConfig_NoPost(t *testing.T) {
	cap := &capturedPush{}
	srv := pushServer(t, cap)
	defer srv.Close()
	n := NewPushNotifier(fakePushRepo{cfg: nil}, zerolog.Nop()) // Get → ErrNotFound
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	if cap.count != 0 {
		t.Fatalf("task with no push config must not POST; got %d", cap.count)
	}
}

// TestPushNotifier_BlocksInternalTarget proves the connect-time SSRF guard
// fires: with the real blockPushIP predicate, a webhook pointed at a loopback
// httptest server (which IS listening) must be refused at dial — the server
// is never hit. This is the layer that defeats DNS rebinding and alternate
// IPv4 encodings, since it inspects the resolved IP, not the URL string.
func TestPushNotifier_BlocksInternalTarget(t *testing.T) {
	cap := &capturedPush{}
	srv := pushServer(t, cap) // binds 127.0.0.1
	defer srv.Close()
	// NO allowLoopbackForTest — the real guard must block the loopback dial.
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
	n.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t1"}, true, "done")
	if cap.count != 0 {
		t.Fatalf("connect-time SSRF guard must block the loopback dial; server was hit %d times", cap.count)
	}
}

func TestValidateWebhookURL(t *testing.T) {
	ok := []string{"https://caller.example.com/hook", "http://caller.example.com:8443/x"}
	for _, u := range ok {
		if err := ValidateWebhookURL(u); err != nil {
			t.Errorf("ValidateWebhookURL(%q) = %v, want nil", u, err)
		}
	}
	bad := []string{
		"",                     // empty
		"ftp://x/y",            // scheme
		"https://localhost/x",  // localhost
		"http://127.0.0.1/x",   // loopback
		"http://10.0.0.5/x",    // private
		"http://100.64.0.1/x",  // RFC 6598 carrier-grade NAT (non-public)
		"http://169.254.1.1/x", // link-local
		"http://[::1]/x",       // ipv6 loopback
		"not a url at all %%%", // unparseable
		"http://LOCALHOST./x",  // normalized: uppercase + trailing dot still = localhost
		"http://0.0.0.0/x",     // unspecified
	}
	for _, u := range bad {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("ValidateWebhookURL(%q) = nil, want error", u)
		}
	}
}

// review-20260930-5e5d F3: the companion push allowlist has its own
// validator (companionpush.ValidateNotifyURL); this one, used by the A2A
// submit path, still refuses every private literal.
func TestValidateWebhookURL_StillRefusesPrivateAddresses(t *testing.T) {
	for _, u := range []string{"http://192.168.1.20:8080/push", "http://10.0.0.5/", "http://[fd00::1]/", "http://100.64.1.1/"} {
		if err := ValidateWebhookURL(u); err == nil {
			t.Errorf("%s accepted; the A2A path must keep refusing private addresses", u)
		}
	}
}

// Broker write-actions design §7a: a companion-created task's push config is
// served by the companion outbox, in its own envelope. The A2A pusher skips
// it, so one config is served by one pusher.
func TestPushNotifier_SkipsCompanionTasks(t *testing.T) {
	allowLoopbackForTest(t)
	got := &capturedPush{}
	srv := pushServer(t, got)
	defer srv.Close()
	n := NewPushNotifier(fakePushRepo{cfg: &persistence.A2APushConfig{TaskID: "t1", URL: srv.URL}}, zerolog.Nop())
	task := &persistence.Task{ID: "t1", CreationSource: persistence.TaskCreationSourceCompanion}
	n.NotifyTaskCompleted(context.Background(), task, true, "done")
	n.NotifySteeringRequired(context.Background(), task, "AWAITING_INPUT")
	if got.count != 0 {
		t.Fatalf("the A2A pusher sent %d pushes for a companion task", got.count)
	}
}
