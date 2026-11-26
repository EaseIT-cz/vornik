package agentapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"vornik.io/vornik/internal/apiaccess"
	"vornik.io/vornik/internal/apigateway"
)

// GitHub #910 (found in production, 2026-11-24): Brave answered an agent
// project's query_api with HTTP 301 and an empty body. The status never
// reached the role: it saw {"provenance":"third_party"} and reported zero
// venues, blaming the socket. The role must be told the status, and for a
// redirect that query_api does not follow it, and that its path is relative
// to the base URL. Control: apiaccess.Service.Query over a real agentapi
// client and a real HTTP upstream.
func TestQueryAPI_910_UpstreamStatusReachesTheRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   []string
	}{
		{"redirect with an empty body", http.StatusMovedPermanently, "", []string{"HTTP 301", `"fio"`, "does not follow redirects", "relative to the base URL"}},
		{"not found keeps the body", http.StatusNotFound, `{"error":"no such route"}`, []string{"HTTP 404", `{"error":"no such route"}`}},
		{"server error with no body", http.StatusBadGateway, "", []string{"HTTP 502", "empty body"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.status/100 == 3 {
					w.Header().Set("Location", "/v1/elsewhere")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			c, _, _ := newClient(t, srv.URL, false, "GET", "HEAD")
			svc := &apiaccess.Service{Client: c}
			out := svc.Query(context.Background(), "hermes--fin", "scout", apigateway.Request{Provider: "fio", Method: "GET", Path: "web/search"})
			if out.Refusal != "" {
				t.Fatalf("unexpected refusal: %s", out.Refusal)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.Body, w) {
					t.Fatalf("body %q lacks %q", out.Body, w)
				}
			}
		})
	}
}

// A 2xx body reaches the role unchanged (no status line added).
func TestQueryAPI_910_SuccessBodyUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()
	c, _, _ := newClient(t, srv.URL, false, "GET", "HEAD")
	out := (&apiaccess.Service{Client: c}).Query(context.Background(), "hermes--fin", "scout", apigateway.Request{Provider: "fio", Path: "web/search"})
	if out.Body != `{"web":{"results":[]}}` {
		t.Fatalf("2xx body changed: %q", out.Body)
	}
}

// #910: list_apis showed only the host, so the role could not know the base
// URL already carries /v1 and doubled it. It now shows the base URL.
func TestListProviders_910_ShowsTheBaseURLPath(t *testing.T) {
	c, _, _ := newClient(t, "https://api.example.com", false, "GET", "HEAD")
	got := c.ListProviders()
	if len(got) != 1 || !strings.Contains(got[0].Description, "https://api.example.com/v1") || !strings.Contains(got[0].Description, "relative to") {
		t.Fatalf("description: %+v", got)
	}
}
