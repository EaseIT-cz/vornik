package standins

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

const (
	prmPath = "/.well-known/oauth-protected-resource"
	asPath  = "/.well-known/oauth-authorization-server"
	// TokenPrefix starts every token the OAuth stand-in issues, so a lane's
	// canary sweep can look for any of them.
	TokenPrefix = "standin-tok-"
)

// OAuthServer is an authorization server for an MCP stand-in on the same
// origin: RFC 9728 protected-resource metadata, RFC 8414 metadata, RFC 7591
// registration, and the authorization-code flow with PKCE S256 and RFC 8707
// resource indicators. /authorize signs in as User with no login page.
type OAuthServer struct {
	User  string
	Email string

	mu           sync.Mutex
	clients      map[string]string // client_id -> registered redirect_uri
	codes        map[string]grant
	access       map[string]string // access token -> user
	accessClient map[string]string // access token -> client_id
	refresh      map[string]grant
	issued       []string
	exchanges    int
}

type grant struct {
	clientID, redirectURI, challenge, resource, user string
}

// NewOAuthServer builds an authorization server that signs in as user.
func NewOAuthServer(user, email string) *OAuthServer {
	return &OAuthServer{User: user, Email: email, clients: map[string]string{}, codes: map[string]grant{},
		access: map[string]string{}, accessClient: map[string]string{}, refresh: map[string]grant{}}
}

// UserFor resolves a live access token.
func (o *OAuthServer) UserFor(token string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	u, ok := o.access[token]
	return u, ok
}

// Exchanges counts successful token responses (codes and refreshes).
func (o *OAuthServer) Exchanges() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.exchanges
}

// IssuedTokens lists every access and refresh token issued (live or not):
// the canaries a lane sweeps for.
func (o *OAuthServer) IssuedTokens() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.issued...)
}

func randomToken(prefix string) string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return prefix + hex.EncodeToString(b)
}

func oauthError(w http.ResponseWriter, code, desc string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest) // RFC 6749 §5.2
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code, "error_description": desc})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// Handler serves the OAuth endpoints and hands every other path (the MCP
// endpoint) to next.
func (o *OAuthServer) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := baseURL(r)
		switch {
		case strings.HasPrefix(r.URL.Path, prmPath):
			res := strings.TrimPrefix(r.URL.Path, prmPath)
			if res == "" {
				res = "/mcp"
			}
			writeJSON(w, http.StatusOK, map[string]any{"resource": base + res, "authorization_servers": []string{base},
				"scopes_supported": []string{"read", "write"}, "bearer_methods_supported": []string{"header"}})
		case r.URL.Path == asPath || r.URL.Path == "/.well-known/openid-configuration":
			writeJSON(w, http.StatusOK, map[string]any{"issuer": base, "authorization_endpoint": base + "/authorize",
				"token_endpoint": base + "/token", "registration_endpoint": base + "/register",
				"response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code", "refresh_token"},
				"code_challenge_methods_supported":      []string{"S256"},
				"token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"read", "write"}})
		case r.URL.Path == "/register":
			o.register(w, r)
		case r.URL.Path == "/authorize":
			o.authorize(w, r, base)
		case r.URL.Path == "/token":
			o.token(w, r, base)
		default:
			next.ServeHTTP(w, r)
		}
	})
}

func (o *OAuthServer) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var in struct {
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || len(in.RedirectURIs) != 1 {
		oauthError(w, "invalid_client_metadata", "exactly one redirect_uri")
		return
	}
	id := randomToken("client-")
	o.mu.Lock()
	o.clients[id] = in.RedirectURIs[0]
	o.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"client_id": id, "redirect_uris": in.RedirectURIs,
		"token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code", "refresh_token"}})
}

// authorize validates the request, then signs in as o.User and redirects
// with a one-time code. A request it cannot trust is answered 400 here, not
// redirected, as a strict provider does for a bad client or redirect_uri.
func (o *OAuthServer) authorize(w http.ResponseWriter, r *http.Request, base string) {
	q := r.URL.Query()
	o.mu.Lock()
	registered, known := o.clients[q.Get("client_id")]
	o.mu.Unlock()
	switch {
	case !known:
		oauthError(w, "invalid_client", "unknown client_id")
		return
	case q.Get("redirect_uri") != registered:
		oauthError(w, "invalid_request", "redirect_uri is not the registered one")
		return
	case q.Get("response_type") != "code":
		oauthError(w, "unsupported_response_type", "code only")
		return
	case q.Get("code_challenge_method") != "S256" || len(q.Get("code_challenge")) < 43:
		oauthError(w, "invalid_request", "PKCE S256 is required")
		return
	case q.Get("resource") != base+"/mcp":
		oauthError(w, "invalid_target", "unknown resource")
		return
	}
	code := randomToken("code-")
	o.mu.Lock()
	o.codes[code] = grant{clientID: q.Get("client_id"), redirectURI: registered, challenge: q.Get("code_challenge"),
		resource: q.Get("resource"), user: o.User}
	o.mu.Unlock()
	back, _ := url.Parse(registered)
	bq := back.Query()
	bq.Set("code", code)
	bq.Set("state", q.Get("state"))
	back.RawQuery = bq.Encode()
	http.Redirect(w, r, back.String(), http.StatusFound)
}

func (o *OAuthServer) token(w http.ResponseWriter, r *http.Request, base string) {
	if r.Method != http.MethodPost || r.ParseForm() != nil {
		oauthError(w, "invalid_request", "POST a form")
		return
	}
	f := r.PostForm
	o.mu.Lock()
	defer o.mu.Unlock()
	var g grant
	switch f.Get("grant_type") {
	case "authorization_code":
		var ok bool
		g, ok = o.codes[f.Get("code")]
		delete(o.codes, f.Get("code")) // single use, even when this attempt fails
		sum := sha256.Sum256([]byte(f.Get("code_verifier")))
		challenge := base64.RawURLEncoding.EncodeToString(sum[:])
		switch {
		case !ok:
			oauthError(w, "invalid_grant", "unknown or used code")
			return
		case f.Get("client_id") != g.clientID:
			oauthError(w, "invalid_grant", "code issued to another client")
			return
		case f.Get("redirect_uri") != g.redirectURI:
			oauthError(w, "invalid_grant", "redirect_uri differs from the authorization request")
			return
		case subtle.ConstantTimeCompare([]byte(challenge), []byte(g.challenge)) != 1:
			oauthError(w, "invalid_grant", "PKCE verification failed")
			return
		case f.Get("resource") != g.resource:
			oauthError(w, "invalid_target", "resource differs from the authorization request")
			return
		}
	case "refresh_token":
		var ok bool
		g, ok = o.refresh[f.Get("refresh_token")]
		delete(o.refresh, f.Get("refresh_token")) // rotated: single use
		if !ok || f.Get("client_id") != g.clientID || f.Get("resource") != g.resource {
			oauthError(w, "invalid_grant", "unknown or used refresh token")
			return
		}
		for tok, u := range o.access { // the replaced access token stops working
			if u == g.user && o.accessClient[tok] == g.clientID {
				delete(o.access, tok)
				delete(o.accessClient, tok)
			}
		}
	default:
		oauthError(w, "unsupported_grant_type", "authorization_code or refresh_token")
		return
	}
	at, rt := randomToken(TokenPrefix+"at-"), randomToken(TokenPrefix+"rt-")
	o.access[at] = g.user
	o.accessClient[at] = g.clientID
	o.refresh[rt] = g
	o.issued = append(o.issued, at, rt)
	o.exchanges++
	writeJSON(w, http.StatusOK, map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600,
		"refresh_token": rt, "scope": "read write", "id_token": o.idToken(base, g.clientID)})
}

// idToken is an unsigned-shape JWT (alg none is refused by some parsers, so
// HS256 with a throwaway key): Vornik reads the email claim without a
// signature check (agent-administered design §22.2.7), and nothing here
// publishes a JWKS.
func (o *OAuthServer) idToken(issuer, aud string) string {
	enc := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	head := enc(map[string]string{"alg": "HS256", "typ": "JWT"})
	body := enc(map[string]any{"iss": issuer, "aud": aud, "sub": o.User, "email": o.Email, "email_verified": true})
	sig := sha256.Sum256([]byte(head + "." + body))
	return head + "." + body + "." + base64.RawURLEncoding.EncodeToString(sig[:])
}
