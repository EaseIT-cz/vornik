package mcpconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
)

// GitHub #566, SaaS MVP M7 (mcp-server-authentication-design.md §16): sign-ins
// through the EaseIT OAuth router. Found at: design review; regression tests
// at: unit, the connector against a stand-in authorization server.

const routerURI566 = "https://connect.vornik.cloud/oauth/callback"

// routedAuth566 is a managed connector's auth block: EaseIT's client and the
// router provider's name (design §16.5 C1).
var routedAuth566 = mcpauth.Auth{Mode: mcpauth.ModeOAuth, ClientID: "easeit-client", RouterProvider: "google"}

// vendorURL566 is the vendor authorization URL inside a router start URL.
func vendorURL566(t *testing.T, start string) *url.URL {
	t.Helper()
	s, err := url.Parse(start)
	require.NoError(t, err)
	require.Equal(t, "/oauth/start", s.Path, "a routed flow starts at the router: %s", start)
	u, err := url.Parse(s.Query().Get("authorize"))
	require.NoError(t, err)
	return u
}

func routedConnector(t *testing.T, logs *bytes.Buffer) *Connector {
	t.Helper()
	c := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	c.Router = func() (string, string) { return routerURI566, "acme" }
	if logs != nil {
		c.Logger = zerolog.New(logs)
	}
	return c
}

func TestConnectorRouter566_RedirectAndState(t *testing.T) {
	vendor := newOAuthServer(t, true, "read:x")
	c := routedConnector(t, nil)
	got, err := c.RedirectURI()
	require.NoError(t, err)
	assert.Equal(t, routerURI566, got)

	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "cal", URL: vendor.URL + "/mcp", Auth: routedAuth566,
	}, "op")
	require.NoError(t, err)
	u := vendorURL566(t, begun.AuthorizationURL)
	assert.Equal(t, routerURI566, u.Query().Get("redirect_uri"))
	id, rnd, ok := strings.Cut(begun.State, ".")
	assert.True(t, ok && id == "acme" && len(rnd) == 32, "state %q, want acme.<32>", begun.State)

	// A state naming another instance matches nothing here.
	_, err = c.Complete(context.Background(), "other."+rnd, "code")
	require.ErrorIs(t, err, ErrUnknownState)

	_, err = c.Complete(context.Background(), begun.State, "the-code")
	require.NoError(t, err)
	assert.Equal(t, routerURI566, vendor.lastToken.Get("redirect_uri"))
	assert.NotEmpty(t, vendor.lastToken.Get("code_verifier"), "the exchange must send the PKCE verifier")

	// Without the router, the redirect URI and the state are as before.
	plain := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	begun, err = plain.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "cal", URL: vendor.URL + "/mcp", Auth: routedAuth566,
	}, "op")
	require.NoError(t, err)
	u, _ = url.Parse(begun.AuthorizationURL)
	assert.Equal(t, "https://acme.vornik.cloud/auth/mcp/callback", u.Query().Get("redirect_uri"))
	assert.NotContains(t, begun.State, ".")
}

// echoingVendor is an authorization server whose token endpoint fails and
// echoes the whole request in its error object.
func echoingVendor(t *testing.T) *httptest.Server {
	t.Helper()
	var v *httptest.Server
	v = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": v.URL + "/mcp", "authorization_servers": []string{v.URL}})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{"issuer": v.URL, "authorization_endpoint": v.URL + "/authorize",
				"token_endpoint": v.URL + "/token", "registration_endpoint": v.URL + "/register"})
		case r.URL.Path == "/register":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "cid"})
		case r.URL.Path == "/token":
			_ = r.ParseForm()
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": r.PostForm.Get("code_verifier"), "error_description": r.PostForm.Encode()})
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="x"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func TestConnectorRouter566_VerifierNeverLeaves(t *testing.T) {
	var logs bytes.Buffer
	c := routedConnector(t, &logs)
	vendor := echoingVendor(t)
	begun, err := c.Begin(context.Background(), ServerRef{
		ProjectID: "p1", ServerName: "cal", URL: vendor.URL + "/mcp", Auth: routedAuth566,
	}, "op")
	require.NoError(t, err)
	c.mu.Lock()
	verifier := c.pending[begun.State].PKCE.Verifier
	c.mu.Unlock()
	require.NotEmpty(t, verifier)
	assert.NotContains(t, begun.AuthorizationURL, verifier)
	assert.NotContains(t, fmt.Sprintf("%+v", begun), verifier)

	w := callbackGet(t, c.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"the-secret-code"}})
	assert.Equal(t, http.StatusBadGateway, w.Code)
	assert.NotContains(t, w.Body.String(), verifier)
	assert.NotContains(t, logs.String(), verifier, "the verifier reached a log line")
	assert.NotContains(t, logs.String(), "the-secret-code", "the code reached a log line")
}

func TestOAuthCallback566_LogsNeitherCodeNorState(t *testing.T) {
	var logs bytes.Buffer
	c := routedConnector(t, &logs)
	good := newOAuthServer(t, true, "read:x")
	bad := echoingVendor(t)
	var states []string
	drive := func(vendorURL string, q func(state string) url.Values) {
		begun, err := c.Begin(context.Background(), ServerRef{
			ProjectID: "p1", ServerName: "cal", URL: vendorURL + "/mcp", Auth: routedAuth566,
		}, "op")
		require.NoError(t, err)
		states = append(states, begun.State)
		_ = callbackGet(t, c.CallbackHandler(), q(begun.State))
	}
	const code = "a-code-value-for-566"
	drive(good.URL, func(s string) url.Values { return url.Values{"state": {s}, "error": {"access_denied"}} }) // a vendor error
	drive(good.URL, func(s string) url.Values { return url.Values{"state": {"acme.unknown" + s[5:]}, "code": {code}} })
	drive(bad.URL, func(s string) url.Values { return url.Values{"state": {s}, "code": {code}} })  // an exchange failure
	drive(good.URL, func(s string) url.Values { return url.Values{"state": {s}, "code": {code}} }) // a success
	out := logs.String()
	assert.NotContains(t, out, code, "a code reached a log line")
	for _, s := range states {
		assert.NotContains(t, out, s, "a state reached a log line")
	}
}
