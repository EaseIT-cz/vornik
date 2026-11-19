//go:build e2e_http
// +build e2e_http

package e2e_test

// GitHub #816 (follow-up to #815): six Community features gated on the
// Enterprise admin gate, which answers 501 EDITION_UNSUPPORTED on Community
// Edition with auth on (18-security-and-tenancy.md, Amendment 2026-11-18
// (#816)). Found at: review (the #815 audit); a user observes it on a
// Community daemon over HTTP, the e2e lane, where this test runs.

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCommunityOperatorRoutesAnswerOnCommunityWithAuth816(t *testing.T) {
	base, _ := bootCommunityAuthDaemon(t)
	client := &http.Client{Timeout: 30 * time.Second}
	call := func(method, path, body, key string) (int, string) {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	routes := []struct{ method, path, body string }{
		{http.MethodPost, "/api/v1/doctor/features/no-such-feature/enable", `{}`},
		{http.MethodPost, "/api/v1/doctor/ack", `{"check":"no-such-check","file":"no-such-file"}`},
		{http.MethodGet, "/api/v1/mcp/servers", ``},
		{http.MethodGet, "/api/v1/memory/cache-stats", ``},
		{http.MethodPost, "/api/v1/memory/backfill-titles?count=true", ``},
		{http.MethodGet, fmt.Sprintf("/api/v1/projects/%s/doctor", e2eStaticProject), ``},
	}
	for _, r := range routes {
		code, body := call(r.method, r.path, r.body, ce815AdminKey)
		if code == http.StatusNotImplemented || code == http.StatusUnauthorized || code == http.StatusForbidden || strings.Contains(body, "EDITION_UNSUPPORTED") {
			t.Errorf("%s %s with an admin-class key on Community: %d %s", r.method, r.path, code, body)
		}
		if code, body := call(r.method, r.path, r.body, ce815PlainKey); code != http.StatusForbidden {
			t.Errorf("%s %s with a plain key: %d %s, want 403", r.method, r.path, code, body)
		}
	}
}
