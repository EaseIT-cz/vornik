package api

// GitHub #591 (operations-api-contract-design.md §6.7): setup/managed can
// carry the public origin and the connector router's redirect URI, so the
// control plane can run provision in the warm pool, before a customer and
// its handle exist. The origin binds like the owner. Found at: design
// review (vornik-cloud D52), observed at integration (the control plane's
// HTTP call), so these run through NewRouter on a real config file.

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/config"
)

func originBody591(origin, router string) map[string]any {
	b := managedSetupBody582("owner-sub-1")
	if origin != "" {
		b["public_base_url"] = origin
	}
	if router != "" {
		b["oauth_redirect_uri"] = router
	}
	return b
}

func TestSetupManaged591_OriginInTheBody(t *testing.T) {
	r := newSetupRigOn582(t, nil, managedYAML582())

	// Without an origin anywhere, setup still cannot bind a login provider.
	code, out := r.post(t, originBody591("", ""))
	require.Equal(t, http.StatusConflict, code, "%v", out)
	require.Equal(t, "NO_PUBLIC_ORIGIN", out["error"].(map[string]any)["code"])

	code, out = r.post(t, originBody591("https://acme.vornik.cloud/", "https://oauth.vornik.cloud/callback"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, true, out["changed"])
	require.Equal(t, true, out["restart_required"])
	cfg, err := config.LoadFromPath(r.path)
	require.NoError(t, err, "the written config must load")
	require.Equal(t, "https://acme.vornik.cloud", cfg.Server.PublicBaseURL)
	require.Equal(t, "https://acme.vornik.cloud", cfg.Auth.ExternalBaseURL)
	require.Equal(t, "https://oauth.vornik.cloud/callback", cfg.Hosting.OAuthRedirectURI)

	// The control plane's retry changes nothing.
	before, _ := os.ReadFile(r.path)
	code, out = r.post(t, originBody591("https://acme.vornik.cloud", "https://oauth.vornik.cloud/callback"))
	require.Equal(t, http.StatusOK, code, "%v", out)
	require.Equal(t, false, out["changed"])
	after, _ := os.ReadFile(r.path)
	require.Equal(t, string(before), string(after))
}

func TestSetupManaged591_OriginBound(t *testing.T) {
	body := managedYAML582() + "  oauth_redirect_uri: https://oauth.vornik.cloud/callback\n"
	body = strings.Replace(body, "server:\n", "server:\n  public_base_url: https://acme.vornik.cloud\n", 1)
	cases := []struct{ name, origin, router string }{
		{"another origin", "https://other.vornik.cloud", ""},
		{"another router URI", "", "https://oauth.vornik.cloud/elsewhere"},
	}
	for _, c := range cases {
		r := newSetupRigOn582(t, nil, body)
		before, _ := os.ReadFile(r.path)
		code, out := r.post(t, originBody591(c.origin, c.router))
		require.Equal(t, http.StatusConflict, code, "%s: %v", c.name, out)
		require.Equal(t, "ORIGIN_BOUND", out["error"].(map[string]any)["code"], c.name)
		after, _ := os.ReadFile(r.path)
		require.Equal(t, string(before), string(after), "%s: ORIGIN_BOUND must write nothing", c.name)
	}

	// The same values, or none, keep what the file has.
	for _, b := range []map[string]any{originBody591("https://acme.vornik.cloud", "https://oauth.vornik.cloud/callback"), originBody591("", "")} {
		r := newSetupRigOn582(t, nil, body)
		code, out := r.post(t, b)
		require.Equal(t, http.StatusOK, code, "%v", out)
		cfg, err := config.LoadFromPath(r.path)
		require.NoError(t, err)
		require.Equal(t, "https://acme.vornik.cloud", cfg.PublicOrigin())
		require.Equal(t, "https://oauth.vornik.cloud/callback", cfg.Hosting.OAuthRedirectURI)
	}
}

func TestSetupManaged591_Refusals(t *testing.T) {
	cases := []struct{ name, origin, router string }{
		{"a plain-http origin", "http://acme.vornik.cloud", ""},
		{"an origin with a path", "https://acme.vornik.cloud/x", ""},
		{"an origin with a query", "https://acme.vornik.cloud?a=1", ""},
		{"an origin with userinfo", "https://u@acme.vornik.cloud", ""},
		{"an apex origin (its parent is a public suffix)", "https://vornik.cloud", "https://oauth.vornik.cloud/callback"},
		{"a router URI on another parent domain", "https://acme.vornik.cloud", "https://evil.example/callback"},
		{"a router URI without the origin", "", "https://oauth.vornik.cloud/callback"},
	}
	for _, c := range cases {
		r := newSetupRigOn582(t, nil, managedYAML582())
		before, _ := os.ReadFile(r.path)
		code, out := r.post(t, originBody591(c.origin, c.router))
		require.Equal(t, http.StatusBadRequest, code, "%s: %v", c.name, out)
		require.Equal(t, "BAD_BODY", out["error"].(map[string]any)["code"], c.name)
		after, _ := os.ReadFile(r.path)
		require.Equal(t, string(before), string(after), "%s: a refusal must write nothing", c.name)
		_, err := os.Stat(r.secrets + "/chat.env")
		require.True(t, os.IsNotExist(err), "%s: a refusal must write no secret", c.name)
	}
}
