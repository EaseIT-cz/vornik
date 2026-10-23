package hostguard

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// GitHub #465 (2026-10-21, cloudflare-real-ip-design "Amendment 2026-10-21
// (#465)"): with api.auth_enabled false nothing checked the Host header, so a
// DNS-rebinding page in the operator's browser could read any daemon answer.

func serve(t *testing.T, a Allowlist, host string) (int, string, bool) {
	t.Helper()
	reached := false
	var refusedName string
	h := Middleware(a, func(name string) { refusedName = name })(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body, _ := io.ReadAll(rec.Body)
	_ = refusedName
	return rec.Code, string(body), reached
}

func TestHostguard_AcceptsOwnNamesAndRefusesOthers_Issue465(t *testing.T) {
	a, err := NewAllowlist([]string{"vornik.lan", "Daemon.Example.Test"})
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{
		"", "127.0.0.1", "127.0.0.1:8080", "192.0.2.10:8080", "[::1]:8080", "[::1]", "::1",
		"localhost", "LOCALHOST:8080", "localhost.:8080", "app.localhost", "x.y.localhost:1",
		"host.containers.internal:8080", "host.docker.internal",
		"vornik.lan", "VORNIK.LAN.:8080", "daemon.example.test",
	} {
		code, _, reached := serve(t, a, host)
		if code != http.StatusOK || !reached {
			t.Errorf("Host %q: got %d reached=%v, want served", host, code, reached)
		}
	}
	for _, host := range []string{
		"attacker.example", "attacker.example:8080", "vornik.lan.attacker.example",
		"localhost.attacker.example", "evil-localhost", "vornik.lan..", "127.0.0.1.nip.io",
		"host.containers.internal.attacker.example", "[fe80::1%25eth0]:8080", "[::1",
	} {
		code, body, reached := serve(t, a, host)
		if code != http.StatusForbidden || reached {
			t.Errorf("Host %q: got %d reached=%v, want 403 and not reached", host, code, reached)
		}
		if strings.Contains(body, "attacker") {
			t.Errorf("Host %q: the refusal must not echo the name: %q", host, body)
		}
		if !strings.Contains(body, "server.accepted_hosts") {
			t.Errorf("Host %q: the refusal must name the config key: %q", host, body)
		}
	}
}

func TestHostguard_RefusalCallbackGetsTheNormalisedName_Issue465(t *testing.T) {
	a, _ := NewAllowlist(nil)
	var got string
	h := Middleware(a, func(name string) { got = name })(http.NotFoundHandler())
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Host = "Attacker.Example.:9"
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got != "attacker.example" {
		t.Errorf("onRefuse name = %q", got)
	}
}

func TestHostguard_BadEntriesAreRefusedByName_Issue465(t *testing.T) {
	for _, bad := range []string{"", " ", "http://vornik.lan", "vornik.lan:8080", "*.lan", "a b", "a/b", "user@vornik.lan", `a\b`, "a\tb", "a..b", ".lan", "under_score.lan"} {
		_, err := NewAllowlist([]string{"ok.lan", bad})
		if err == nil {
			t.Errorf("entry %q: want an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), strconv.Quote(bad)) {
			t.Errorf("entry %q: error must name it: %v", bad, err)
		}
	}
	if _, err := NewAllowlist([]string{"Vornik.LAN.", "a-b.c"}); err != nil {
		t.Errorf("valid entries refused: %v", err)
	}
}

func TestHostguard_NamesListsTheAcceptedSet_Issue465(t *testing.T) {
	a, _ := NewAllowlist([]string{"b.lan", "A.lan"})
	got := strings.Join(a.Names(), ",")
	if got != "a.lan,b.lan,host.containers.internal,host.docker.internal" {
		t.Errorf("Names() = %q", got)
	}
}
