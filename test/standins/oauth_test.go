package standins

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/mcpauth"
)

// GitHub #337 (2026-10-12): the OAuth stand-in is driven by Vornik's own
// discovery and flow code (internal/mcpauth), and it refuses what a strict
// provider refuses: a wrong PKCE verifier, a redirect_uri other than the
// registered one, a reused code and a token for another resource.

const redirect = "http://127.0.0.1:1/auth/mcp/callback"

type oauthRig struct {
	srv  *httptest.Server
	as   *OAuthServer
	mcp  *MCPServer
	md   mcpauth.Metadata
	cred mcpauth.ClientCredentials
}

func newOAuthRig(t *testing.T) *oauthRig {
	t.Helper()
	r := &oauthRig{as: NewOAuthServer("alice", "alice@home.example")}
	r.mcp = echoServer()
	r.mcp.Protect(r.as)
	r.srv = httptest.NewServer(r.as.Handler(r.mcp))
	t.Cleanup(r.srv.Close)
	ctx := context.Background()
	md, err := mcpauth.Discover(ctx, nil, r.srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if md.Resource != r.srv.URL+"/mcp" || !md.SupportsDCR() {
		t.Fatalf("metadata: %+v", md)
	}
	r.md = md
	if r.cred, err = mcpauth.Register(ctx, nil, md, redirect, nil); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

// authorize follows the authorization URL without following the redirect
// back, and returns the redirect's query (code and state, or error).
func (r *oauthRig) authorize(t *testing.T, authURL string) url.Values {
	t.Helper()
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Get(authURL)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		return url.Values{"status": {resp.Status}}
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if !strings.HasPrefix(loc.String(), redirect) {
		t.Fatalf("redirected to %s", loc)
	}
	return loc.Query()
}

func (r *oauthRig) start(t *testing.T) (mcpauth.PKCE, url.Values) {
	t.Helper()
	pkce, _ := mcpauth.NewPKCE()
	authURL, err := mcpauth.AuthorizationURL(r.md, r.cred, redirect, nil, "st-1", pkce)
	if err != nil {
		t.Fatal(err)
	}
	q := r.authorize(t, authURL)
	if q.Get("state") != "st-1" || q.Get("code") == "" {
		t.Fatalf("authorize: %v", q)
	}
	return pkce, q
}

func TestOAuth_TheFullFlowSignsInAsTheUser(t *testing.T) {
	r := newOAuthRig(t)
	ctx := context.Background()
	pkce, q := r.start(t)
	tok, err := mcpauth.ExchangeCode(ctx, nil, r.md, r.cred, redirect, q.Get("code"), pkce)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if email, ok := agentadmin.EmailFromIDToken(tok.IDToken); !ok || email != "alice@home.example" {
		t.Fatalf("id_token email %q %v", email, ok)
	}
	var got struct{ User string }
	call(t, dial(t, r.srv.URL+"/mcp", tok.AccessToken), "echo_read", map[string]any{}, &got)
	if got.User != "alice" {
		t.Fatalf("user %q", got.User)
	}

	// Refresh rotates both tokens; the old access token stops working.
	next, err := mcpauth.Refresh(ctx, nil, r.md, r.cred, tok.RefreshToken)
	if err != nil || next.AccessToken == tok.AccessToken || next.RefreshToken == tok.RefreshToken {
		t.Fatalf("refresh: %+v %v", next, err)
	}
	if _, ok := r.as.UserFor(tok.AccessToken); ok {
		t.Fatal("the replaced access token still works")
	}
	if _, err := mcpauth.Refresh(ctx, nil, r.md, r.cred, tok.RefreshToken); err == nil {
		t.Fatal("a used refresh token was accepted again")
	}
	if !strings.Contains(strings.Join(r.as.IssuedTokens(), " "), next.AccessToken) {
		t.Fatal("IssuedTokens does not list the live access token (the lane's canary list)")
	}
}

func TestOAuth_RefusesWhatAStrictProviderRefuses(t *testing.T) {
	r := newOAuthRig(t)
	ctx := context.Background()

	// A wrong verifier.
	_, q := r.start(t)
	other, _ := mcpauth.NewPKCE()
	if _, err := mcpauth.ExchangeCode(ctx, nil, r.md, r.cred, redirect, q.Get("code"), other); err == nil {
		t.Fatal("a wrong PKCE verifier was accepted")
	}
	// A code is single use, even after a failed attempt.
	pkce, q := r.start(t)
	if _, err := mcpauth.ExchangeCode(ctx, nil, r.md, r.cred, redirect, q.Get("code"), pkce); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpauth.ExchangeCode(ctx, nil, r.md, r.cred, redirect, q.Get("code"), pkce); err == nil {
		t.Fatal("a code was accepted twice")
	}
	// The token request's redirect_uri must be the authorize request's.
	pkce, q = r.start(t)
	if _, err := mcpauth.ExchangeCode(ctx, nil, r.md, r.cred, redirect+"x", q.Get("code"), pkce); err == nil {
		t.Fatal("a different redirect_uri at the token endpoint was accepted")
	}
	// The token request's resource must be the authorize request's.
	pkce, q = r.start(t)
	wrong := r.md
	wrong.Resource = r.srv.URL + "/other"
	if _, err := mcpauth.ExchangeCode(ctx, nil, wrong, r.cred, redirect, q.Get("code"), pkce); err == nil {
		t.Fatal("a token for another resource was issued")
	}

	// /authorize refuses before signing in: an unregistered redirect_uri, an
	// unknown client, a plain challenge, another resource.
	pkce, _ = mcpauth.NewPKCE()
	good, _ := mcpauth.AuthorizationURL(r.md, r.cred, redirect, nil, "s", pkce)
	for name, edit := range map[string]func(url.Values){
		"redirect_uri":          func(v url.Values) { v.Set("redirect_uri", "http://127.0.0.1:2/evil") },
		"client_id":             func(v url.Values) { v.Set("client_id", "nobody") },
		"code_challenge_method": func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"resource":              func(v url.Values) { v.Set("resource", "http://elsewhere.example/mcp") },
		"response_type":         func(v url.Values) { v.Set("response_type", "token") },
	} {
		u, _ := url.Parse(good)
		v := u.Query()
		edit(v)
		u.RawQuery = v.Encode()
		c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := c.Get(u.String())
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: HTTP %d, want 400 (%s)", name, resp.StatusCode, b)
		}
	}
}

func TestOAuth_TheChallengeNamesTheMetadata(t *testing.T) {
	r := newOAuthRig(t)
	resp, err := http.Post(r.srv.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized ||
		resp.Header.Get("WWW-Authenticate") != `Bearer resource_metadata="`+r.srv.URL+`/.well-known/oauth-protected-resource/mcp"` {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	// A static token still works beside OAuth when one is configured; none
	// is here, so an arbitrary bearer is refused.
	if _, err := mcpauth.Discover(context.Background(), nil, r.srv.URL+"/mcp"); errors.Is(err, mcpauth.ErrNotProtected) {
		t.Fatal("the protected server reads as unprotected")
	}
}
