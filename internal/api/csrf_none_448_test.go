package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// GitHub #448 (2026-10-21, cloudflare-real-ip-design "Amendment 2026-10-21
// (#494, #448)", change item 3): approval.SameOrigin accepted
// Sec-Fetch-Site "none" as same-origin, so a state-changing Basic request
// whose browser reported no initiator passed the CSRF gate. "none" now falls
// through to the Origin rung. Driven through AuthMiddleware with auth on, the
// gate a browser request meets.
func TestCSRFGate_SecFetchSiteNoneNeedsOrigin_Issue448(t *testing.T) {
	mw := AuthMiddleware(AuthConfig{
		Enabled:       true,
		StaticAPIKeys: map[string][]string{"key-real": nil},
	})
	reached := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})
	post := func(origin string) *httptest.ResponseRecorder {
		req := newAuthRequest(t, "/api/v1/projects/proj-a/tasks")
		req.Method = http.MethodPost
		req.SetBasicAuth("api", "key-real")
		req.Header.Set("Sec-Fetch-Site", "none")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		rec := httptest.NewRecorder()
		mw(handler).ServeHTTP(rec, req)
		return rec
	}

	rec := post("")
	require.Equal(t, http.StatusForbidden, rec.Code, "none without an Origin must not pass the CSRF gate")
	require.Contains(t, rec.Body.String(), "CSRF_BLOCKED")
	require.Zero(t, reached)

	rec = post("http://evil.example")
	require.Equal(t, http.StatusForbidden, rec.Code, "none with a foreign Origin must not pass")
	require.Zero(t, reached)
}
