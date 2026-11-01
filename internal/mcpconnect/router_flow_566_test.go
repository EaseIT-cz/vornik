package mcpconnect

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/mcpauth"
)

// GitHub #566 follow-up (mcp-server-authentication-design.md §16.5, reviews
// c31d and 5e87 GREEN): the daemon's part of the EaseIT OAuth router. Every
// flow starts at the router; the callback's iss is checked against the
// flow's issuer (RFC 9207) and a widened grant is refused and revoked, all
// before or instead of storing anything. Found at: review (the router
// design's requirements on #566), observed at e2e-lane (a managed sign-in);
// regression tests at: unit, the connector against stand-in authorization
// servers.

// vendor566 is a configurable authorization server.
type vendor566 struct {
	*httptest.Server
	mu sync.Mutex
	// issuer overrides the metadata's issuer ("" means the server's URL).
	issuer string
	// issSupported is RFC 9207's metadata flag.
	issSupported bool
	// grant is the scope the token response grants ("" means requested).
	grant string
	// revocation serves an RFC 7009 endpoint when true.
	revocation   bool
	tokenCalls   int
	revokedToken string
}

func newVendor566(t *testing.T, configure func(*vendor566)) *vendor566 {
	t.Helper()
	v := &vendor566{}
	if configure != nil {
		configure(v)
	}
	v.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		switch {
		case strings.HasPrefix(r.URL.Path, "/.well-known/oauth-protected-resource"):
			_ = json.NewEncoder(w).Encode(map[string]any{"resource": v.URL + "/mcp", "authorization_servers": []string{v.URL}, "scopes_supported": []string{"read:x"}})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			iss := v.issuer
			switch iss {
			case "":
				iss = v.URL
			case "-":
				iss = ""
			}
			doc := map[string]any{"issuer": iss, "authorization_endpoint": v.URL + "/authorize", "token_endpoint": v.URL + "/token",
				"token_endpoint_auth_methods_supported": []string{"none"}, "authorization_response_iss_parameter_supported": v.issSupported}
			if v.revocation {
				doc["revocation_endpoint"] = v.URL + "/revoke"
			}
			_ = json.NewEncoder(w).Encode(doc)
		case r.URL.Path == "/token":
			v.tokenCalls++
			grant := v.grant
			if grant == "" {
				grant = "read:x"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at-566", "refresh_token": "rt-566", "expires_in": 3600, "scope": grant})
		case r.URL.Path == "/revoke":
			_ = r.ParseForm()
			v.revokedToken = r.PostForm.Get("token")
			w.WriteHeader(http.StatusOK)
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	t.Cleanup(v.Close)
	return v
}

func managedRef566(v *vendor566) ServerRef {
	return ServerRef{ProjectID: "p1", ServerName: "cal", URL: v.URL + "/mcp",
		Auth: mcpauth.Auth{Mode: mcpauth.ModeOAuth, ClientID: "easeit-client", Scopes: []string{"read:x"}, RouterProvider: "google"}}
}

func TestConnectorRouter566_StartsThroughTheRouter(t *testing.T) {
	v := newVendor566(t, nil)
	c := routedConnector(t, nil)
	begun, err := c.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	start, err := url.Parse(begun.AuthorizationURL)
	require.NoError(t, err)
	assert.Equal(t, "https://connect.vornik.cloud/oauth/start", start.Scheme+"://"+start.Host+start.Path)
	assert.Equal(t, "google", start.Query().Get("provider"))
	authorize, err := url.Parse(start.Query().Get("authorize"))
	require.NoError(t, err)
	assert.Equal(t, v.URL+"/authorize", authorize.Scheme+"://"+authorize.Host+authorize.Path)
	assert.Equal(t, begun.State, authorize.Query().Get("state"))
	assert.Equal(t, "S256", authorize.Query().Get("code_challenge_method"))
	assert.Equal(t, routerURI566, authorize.Query().Get("redirect_uri"))

	noProvider := managedRef566(v)
	noProvider.Auth.RouterProvider = ""
	_, err = c.Begin(context.Background(), noProvider, "op")
	require.ErrorIs(t, err, ErrNoRouterProvider)

	dcr := managedRef566(v)
	dcr.Auth.ClientID = ""
	_, err = c.Begin(context.Background(), dcr, "op")
	require.ErrorIs(t, err, ErrRouterNeedsClient, "a registered client is not EaseIT's; the router's allowlist refuses it")

	plain := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	begun, err = plain.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(begun.AuthorizationURL, v.URL+"/authorize?"), "without a router the vendor URL is handed out: %s", begun.AuthorizationURL)
}

func TestDiscover566_IssuerMustMatch(t *testing.T) {
	v := newVendor566(t, func(v *vendor566) { v.issuer = "https://elsewhere.example" })
	// With a router (managed): refused at begin.
	_, err := routedConnector(t, nil).Begin(context.Background(), managedRef566(v), "op")
	require.ErrorIs(t, err, mcpauth.ErrIssuerMismatch)
	// Without one (a self host): it begins, keeping both issuers it could
	// expect (commit security review of ac63c9bae: clearing them failed open).
	plain := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	begun, err := plain.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	plain.mu.Lock()
	md := plain.pending[begun.State].Metadata
	plain.mu.Unlock()
	assert.Equal(t, "https://elsewhere.example", md.Issuer)
	assert.Equal(t, v.URL, md.IssuerFetchedFrom)

	good := newVendor566(t, func(v *vendor566) { v.issSupported = true; v.revocation = true })
	md, err = mcpauth.Discover(context.Background(), &http.Client{}, good.URL+"/mcp")
	require.NoError(t, err)
	assert.Equal(t, good.URL, md.Issuer)
	assert.True(t, md.IssParameterSupported)
	assert.Equal(t, good.URL+"/revoke", md.RevocationEndpoint)
	assert.False(t, md.IssuerMismatch)
}

func TestComplete566_IssuerRules(t *testing.T) {
	type tc struct {
		name       string
		supported  bool
		iss        func(v *vendor566) string
		wantErr    error
		wantTokens int
	}
	for _, c := range []tc{
		{"a matching iss", true, func(v *vendor566) string { return v.URL }, nil, 1},
		{"a different iss is refused before the token request", true, func(*vendor566) string { return "https://evil.example" }, mcpauth.ErrIssuerMismatch, 0},
		{"an absent iss where the server sends one", true, func(*vendor566) string { return "" }, mcpauth.ErrIssuerMismatch, 0},
		{"an absent iss where the server sends none", false, func(*vendor566) string { return "" }, nil, 1},
		{"a different iss where the server sends none", false, func(*vendor566) string { return "https://evil.example" }, mcpauth.ErrIssuerMismatch, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newVendor566(t, func(v *vendor566) { v.issSupported = c.supported })
			conn := routedConnector(t, nil)
			begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
			require.NoError(t, err)
			_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: c.iss(v)}, nil)
			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, c.wantTokens, v.tokenCalls)
			// One-shot (c31d F4): the state is gone after any outcome.
			_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: c.iss(v)}, nil)
			require.ErrorIs(t, err, ErrUnknownState)
		})
	}

	// A manual endpoint (c31d F1, F6): auth.issuer and iss_parameter_supported.
	v := newVendor566(t, nil)
	for _, m := range []struct {
		name      string
		supported bool
		iss       string
		wantErr   error
	}{
		{"manual, sends none, absent", false, "", nil},
		{"manual, sends none, different", false, "https://evil.example", mcpauth.ErrIssuerMismatch},
		{"manual, sends one, absent", true, "", mcpauth.ErrIssuerMismatch},
		{"manual, sends one, matching", true, "https://as.example", nil},
	} {
		t.Run(m.name, func(t *testing.T) {
			conn := routedConnector(t, nil)
			ref := managedRef566(v)
			ref.Auth.AuthorizationEndpoint, ref.Auth.TokenEndpoint = v.URL+"/authorize", v.URL+"/token"
			ref.Auth.Issuer, ref.Auth.IssParameterSupported = "https://as.example", m.supported
			begun, err := conn.Begin(context.Background(), ref, "op")
			require.NoError(t, err)
			_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: m.iss}, nil)
			if m.wantErr != nil {
				require.ErrorIs(t, err, m.wantErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestComplete566_WidenedScopeIsRefused(t *testing.T) {
	for _, revocation := range []bool{true, false} {
		v := newVendor566(t, func(v *vendor566) { v.grant = "read:x write:x"; v.revocation = revocation })
		tokens := newFakeTokens()
		conn := newConnector(t, tokens, &fakeAudit{}, "https://acme.vornik.cloud")
		conn.Router = routedConnector(t, nil).Router
		begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
		require.NoError(t, err)
		_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code"}, nil)
		require.ErrorIs(t, err, ErrScopeWidened)
		var widened *ScopeWidenedError
		require.ErrorAs(t, err, &widened)
		assert.Equal(t, []string{"write:x"}, widened.Extra)
		assert.Equal(t, revocation, widened.Revoked)
		if revocation {
			assert.Equal(t, "rt-566", v.revokedToken, "the refresh token is revoked")
		}
		stored, _ := tokens.Get(context.Background(), "p1", "cal", "")
		assert.Nil(t, stored, "a widened grant is never stored")
	}

	// A narrower grant is stored, as before.
	v := newVendor566(t, func(v *vendor566) { v.grant = "read:x" })
	conn := routedConnector(t, nil)
	ref := managedRef566(v)
	ref.Auth.Scopes = []string{"read:x", "read:y"}
	begun, err := conn.Begin(context.Background(), ref, "op")
	require.NoError(t, err)
	tok, err := conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "read:x", tok.Scopes)
}

func TestOAuthCallback566_PassesIssAndScope(t *testing.T) {
	v := newVendor566(t, func(v *vendor566) { v.issSupported = true })
	conn := routedConnector(t, nil)
	begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	w := callbackGet(t, conn.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"code"}, "iss": {"https://evil.example/<b>"}})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, 0, v.tokenCalls, "a mismatched iss must stop before the token request")
	assert.NotContains(t, w.Body.String(), "<b>", "the page escapes what the callback carried")
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))

	w2 := newVendor566(t, func(v *vendor566) { v.grant = "read:x admin" })
	conn2 := routedConnector(t, nil)
	begun, err = conn2.Begin(context.Background(), managedRef566(w2), "op")
	require.NoError(t, err)
	rec := callbackGet(t, conn2.CallbackHandler(), url.Values{"state": {begun.State}, "code": {"code"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "admin", "the page names the scope it was not asked for")
}

// As shipped (design §16.5 C3): a self host keeps a wider grant, as before,
// with a warning. Providers add scopes such as offline_access on their own,
// and refusing them would break working connectors; the router confines a
// managed flow's scope, so only a routed flow refuses.
func TestComplete566_ASelfHostKeepsAWiderGrant(t *testing.T) {
	v := newVendor566(t, func(v *vendor566) { v.grant = "read:x offline_access"; v.revocation = true })
	var logs bytes.Buffer
	conn := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	conn.Logger = zerolog.New(&logs)
	begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	tok, err := conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code"}, nil)
	require.NoError(t, err)
	assert.Equal(t, "read:x offline_access", tok.Scopes)
	assert.Empty(t, v.revokedToken, "a self host revokes nothing")
	assert.Contains(t, logs.String(), "wider than requested")
}

// A routed manual-endpoint server must state its issuer: without one the
// daemon has nothing to compare iss with (found reviewing the shipped code,
// 2026-10-29; the router checks iss too, but the daemon's own check must not
// depend on it).
func TestConnectorRouter566_AManualEndpointNeedsAnIssuer(t *testing.T) {
	v := newVendor566(t, nil)
	ref := managedRef566(v)
	ref.Auth.AuthorizationEndpoint, ref.Auth.TokenEndpoint = v.URL+"/authorize", v.URL+"/token"
	_, err := routedConnector(t, nil).Begin(context.Background(), ref, "op")
	require.ErrorIs(t, err, ErrRouterNeedsIssuer)
	ref.Auth.Issuer = "https://as.example"
	_, err = routedConnector(t, nil).Begin(context.Background(), ref, "op")
	require.NoError(t, err)
}

// The operator's decision (2026-10-29, design §16.5 C4): EaseIT's apps are
// public PKCE clients wherever the provider offers one. A routed server with
// a client secret for a provider that accepts public clients is refused; a
// provider that refuses them, or does not say, may carry one.
func TestConnectorRouter566_APublicClientWhereOffered(t *testing.T) {
	v := newVendor566(t, nil) // advertises token_endpoint_auth_methods_supported: [none]
	ref := managedRef566(v)
	ref.Auth.ClientSecretFrom = "secret://easeit/google"
	_, err := routedConnector(t, nil).Begin(context.Background(), ref, "op")
	require.ErrorIs(t, err, ErrRouterPublicClientOffered)

	// The same block on a self host is the operator's own choice.
	plain := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	plain.Secrets = mapSecrets{"easeit/google": "s3cr3t"}
	_, err = plain.Begin(context.Background(), ref, "op")
	require.NotErrorIs(t, err, ErrRouterPublicClientOffered)
}

// Commit security review of ac63c9bae: on a self host an issuer mismatch
// cleared the issuer, so any iss passed (fail-open). The flow now accepts
// only the URL the metadata was fetched from, never the issuer the metadata
// claims, and keeps the advertised RFC 9207 flag.
func TestComplete566_ASelfHostMismatchStillRefusesAForeignIss(t *testing.T) {
	for _, c := range []struct {
		name    string
		iss     func(v *vendor566) string
		wantErr bool
	}{
		{"the fetched-from URL", func(v *vendor566) string { return v.URL }, false},
		// Not the metadata's claimed issuer: a server whose metadata is not
		// its own URL is not trusted to name one, since a malicious server
		// could claim an honest one's (the background review of the first
		// fix).
		{"the metadata's claimed issuer", func(*vendor566) string { return "https://elsewhere.example" }, true},
		{"a third issuer (a mix-up)", func(*vendor566) string { return "https://attacker.example" }, true},
		{"none, where the server sends one", func(*vendor566) string { return "" }, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := newVendor566(t, func(v *vendor566) { v.issuer = "https://elsewhere.example"; v.issSupported = true })
			conn := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
			begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
			require.NoError(t, err)
			_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: c.iss(v)}, nil)
			if c.wantErr {
				require.ErrorIs(t, err, mcpauth.ErrIssuerMismatch)
				assert.Equal(t, 0, v.tokenCalls)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// Commit security review of 573ff4f36: metadata with no issuer at all set the
// mismatch flag, but the comparison also required a non-empty issuer, so a
// present iss went unchecked. The check now compares a present iss with one
// expected issuer whatever the metadata claimed.
func TestComplete566_MetadataWithNoIssuerStillChecksIss(t *testing.T) {
	v := newVendor566(t, func(v *vendor566) { v.issuer = "-" }) // "-" serves an empty issuer
	conn := newConnector(t, newFakeTokens(), &fakeAudit{}, "https://acme.vornik.cloud")
	begun, err := conn.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: "https://attacker.example"}, nil)
	require.ErrorIs(t, err, mcpauth.ErrIssuerMismatch)
	assert.Equal(t, 0, v.tokenCalls)

	begun, err = conn.Begin(context.Background(), managedRef566(v), "op")
	require.NoError(t, err)
	_, err = conn.CompleteWith(context.Background(), CompleteRequest{State: begun.State, Code: "code", Iss: v.URL}, nil)
	require.NoError(t, err, "the fetched-from URL is the expected issuer")

	// A managed instance refuses such metadata at begin.
	_, err = routedConnector(t, nil).Begin(context.Background(), managedRef566(v), "op")
	require.ErrorIs(t, err, mcpauth.ErrIssuerMismatch)
}
