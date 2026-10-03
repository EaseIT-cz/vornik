package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Hermes approval transport design §4.3 and §6 (CLI): `vornikctl agent
// host-approval` files Hermes's request with the namespace's key,
// long-polls until a decision or the deadline, and prints
// {"choice","reason"}. An answer from the phone exits 0; every failure
// prints deny with its reason and exits non-zero. The key is never on
// stdout or stderr.

const hostKey = "sk-vornik-hermes-HOSTCANARY42"

const hermesStdin = `{"schema_version":1,"request_id":"ab01","digest":"` +
	"dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd" +
	`","command":"rm -rf /tmp/x","description":"recursive delete","pattern_key":"rm","pattern_keys":["rm"],` +
	`"surface":"cli","timeout_seconds":300,"allowed_choices":["once","session","always","deny"]}`

type hostResult struct {
	Choice string `json:"choice"`
	Reason string `json:"reason"`
}

func runHost(t *testing.T, h http.HandlerFunc, deadline time.Time) (hostResult, int) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	var out, errw bytes.Buffer
	code := runHostApproval(context.Background(), strings.NewReader(hermesStdin), &out, &errw,
		hostApprovalConfig{Base: srv.URL, Key: hostKey, Deadline: deadline, Poll: 50 * time.Millisecond})
	if strings.Contains(out.String()+errw.String(), hostKey) {
		t.Fatalf("the key reached stdout or stderr: %q %q", out.String(), errw.String())
	}
	var r hostResult
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &r); err != nil {
		t.Fatalf("stdout is not one JSON object: %q (%v)", out.String(), err)
	}
	return r, code
}

func TestHostApprovalCLI_AnswerFromThePhone(t *testing.T) {
	var polls atomic.Int32
	var auth, body string
	r, code := runHost(t, func(w http.ResponseWriter, req *http.Request) {
		auth = req.Header.Get("Authorization")
		if req.Method == http.MethodPost {
			b, _ := io.ReadAll(req.Body)
			body = string(b)
			_, _ = w.Write([]byte(`{"status":"pending","deadline":"2026-10-03T10:00:00Z"}`))
			return
		}
		if req.URL.Path != "/api/v1/agent/host-approvals/ab01" || req.URL.Query().Get("wait") == "" {
			t.Errorf("poll %s", req.URL)
		}
		if polls.Add(1) < 2 {
			_, _ = w.Write([]byte(`{"status":"pending","deadline":"2026-10-03T10:00:00Z"}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"approved","choice":"session","deadline":"2026-10-03T10:00:00Z"}`))
	}, time.Now().Add(30*time.Second))
	if code != 0 || r.Choice != "session" {
		t.Fatalf("exit %d, %+v", code, r)
	}
	if auth != "Bearer "+hostKey || !strings.Contains(body, `"request_id":"ab01"`) {
		t.Fatalf("auth %q body %q", auth, body)
	}

	r, code = runHost(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"rejected","choice":"deny","deadline":"2026-10-03T10:00:00Z"}`))
	}, time.Now().Add(30*time.Second))
	if code != 0 || r.Choice != "deny" {
		t.Fatalf("a phone deny: exit %d, %+v", code, r)
	}
}

func TestHostApprovalCLI_DeadlineIsDenyTimeout(t *testing.T) {
	start := time.Now()
	r, code := runHost(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"pending","deadline":"2026-10-03T10:00:00Z"}`))
	}, time.Now().Add(500*time.Millisecond))
	if code == 0 || r.Choice != "deny" || r.Reason != "timeout" {
		t.Fatalf("exit %d, %+v", code, r)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("the CLI outlived its deadline")
	}
}

func TestHostApprovalCLI_FailuresAreDeny(t *testing.T) {
	for name, tc := range map[string]struct {
		status int
		body   string
		reason string
	}{
		"429":     {http.StatusTooManyRequests, `{"error":"busy","reason":"3 waiting"}`, "busy"},
		"409":     {http.StatusConflict, `{"error":"conflict"}`, "conflict"},
		"403":     {http.StatusForbidden, `{"error":"forbidden"}`, "refused"},
		"expired": {http.StatusOK, `{"status":"expired","deadline":"2026-10-03T10:00:00Z"}`, "expired"},
		"always":  {http.StatusOK, `{"status":"approved","choice":"always","deadline":"2026-10-03T10:00:00Z"}`, "invalid answer"},
		"garbage": {http.StatusOK, `not json`, "invalid answer"},
	} {
		r, code := runHost(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}, time.Now().Add(5*time.Second))
		if code == 0 || r.Choice != "deny" || !strings.Contains(r.Reason, tc.reason) {
			t.Errorf("%s: exit %d, %+v", name, code, r)
		}
	}
	// Vornik down.
	var out, errw bytes.Buffer
	code := runHostApproval(context.Background(), strings.NewReader(hermesStdin), &out, &errw,
		hostApprovalConfig{Base: "http://127.0.0.1:1", Key: hostKey, Deadline: time.Now().Add(2 * time.Second), Poll: 50 * time.Millisecond})
	if code == 0 || !strings.Contains(out.String(), `"deny"`) || strings.Contains(out.String()+errw.String(), hostKey) {
		t.Fatalf("Vornik down: exit %d, %q %q", code, out.String(), errw.String())
	}
	// Unreadable stdin.
	out.Reset()
	code = runHostApproval(context.Background(), strings.NewReader("{"), &out, &errw,
		hostApprovalConfig{Base: "http://127.0.0.1:1", Key: hostKey, Deadline: time.Now().Add(2 * time.Second)})
	if code == 0 || !strings.Contains(out.String(), `"deny"`) {
		t.Fatalf("bad stdin: exit %d, %q", code, out.String())
	}
}

// The command's own wiring: the key comes from the namespace's key file;
// without one it denies and exits non-zero.
func TestHostApprovalCLI_NoKeyFileIsDeny(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("VORNIK_API_URL", "")
	var out, errw bytes.Buffer
	cmd := agentHostApprovalCmd
	cmd.SetIn(strings.NewReader(hermesStdin))
	cmd.SetOut(&out)
	cmd.SetErr(&errw)
	hostApprovalNamespace, hostApprovalDeadline, hostApprovalURL = "hermes", float64(time.Now().Add(5*time.Second).Unix()), "http://127.0.0.1:1"
	t.Cleanup(func() { hostApprovalNamespace, hostApprovalDeadline, hostApprovalURL = "", 0, "" })
	err := cmd.RunE(cmd, nil)
	if err == nil || !strings.Contains(out.String(), `"deny"`) || !strings.Contains(out.String(), "key") {
		t.Fatalf("no key file: %v, %q", err, out.String())
	}
}
