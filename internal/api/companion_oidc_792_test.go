package api

// GitHub #792 (design 27 §3): over the real AuthMiddleware and companion
// handler, a GitHub Actions ID token whose claims match a trust is served as
// the trust's key; one whose claims do not is a 401 that counts toward the
// lockout; a plain companion key is unaffected.
//
// Found at: review (#792, 2026-11-16), observed at integration (the
// middleware answers every JWT bearer with 401).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
)

type trusts792 struct {
	rows []persistence.CompanionOIDCTrust
}

func (s *trusts792) OIDCTrustsForRepository(_ context.Context, iss string, id int64) ([]persistence.CompanionOIDCTrust, error) {
	var out []persistence.CompanionOIDCTrust
	for _, t := range s.rows {
		if t.Issuer == iss && t.RepositoryID == id {
			out = append(out, t)
		}
	}
	return out, nil
}
func (s *trusts792) TouchOIDCTrust(context.Context, string, time.Time) error { return nil }

const (
	apiAud792    = "https://vornik.example.org"
	apiWF792     = "acme/app/.github/workflows/review.yml@refs/heads/main"
	whoami792    = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`
	toolsList792 = `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`
)

// oidc792 is a local issuer (an httptest JWKS and a generated RSA key; never
// GitHub) and the "alpha" companion key bound to one workflow by one trust.
type oidc792 struct {
	f   *diagFixture
	iss *httptest.Server
	key *rsa.PrivateKey
	cfg AuthConfig
	now time.Time
}

func newOIDC792(t *testing.T) *oidc792 {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	iss := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(iss.Close)
	f := newDiagFixture(t)
	row := f.keyRepo.rows[len(f.keyRepo.rows)-1] // the "alpha" companion key
	tr := &trusts792{rows: []persistence.CompanionOIDCTrust{{ID: "ktrust_1", KeyID: row.ID, Issuer: iss.URL,
		Audience: apiAud792, RepositoryID: 4242, Repository: "acme/app", Ref: "refs/heads/main",
		JobWorkflowRef: apiWF792, EventNames: []string{"push"}}}}
	return &oidc792{f: f, iss: iss, key: key, now: time.Now(),
		cfg: AuthConfig{Enabled: true, APIKeyLookup: f.keyRepo, OIDCTrusts: tr, APIKeyByID: f.keyRepo,
			OIDCIssuer: iss.URL, OIDCJWKSURL: iss.URL, OIDCHTTPClient: iss.Client()}}
}

// claims match the trust exactly.
func (o *oidc792) claims() map[string]any {
	return map[string]any{"iss": o.iss.URL, "aud": apiAud792, "repository": "acme/app",
		"repository_id": "4242", "ref": "refs/heads/main", "job_workflow_ref": apiWF792, "event_name": "push",
		"iat": o.now.Unix(), "nbf": o.now.Unix(), "exp": o.now.Add(5 * time.Minute).Unix()}
}

func (o *oidc792) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: o.key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	require.NoError(t, err)
	b, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := s.Sign(b)
	require.NoError(t, err)
	out, err := jws.CompactSerialize()
	require.NoError(t, err)
	return out
}

// call sends one MCP request through the real AuthMiddleware and companion
// handler, as a CI job would.
func (o *oidc792) call(h http.Handler, bearer, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp/companion", strings.NewReader(body))
	req.RemoteAddr = "198.51.100.7:5000"
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestCompanionOIDC792_ATokenIsServedAsItsTrustsKey(t *testing.T) {
	o := newOIDC792(t)
	h := AuthMiddleware(o.cfg)(http.HandlerFunc(o.f.srv.CompanionMCPHandler))

	rec := o.call(h, o.sign(t, o.claims()), whoami792)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "alpha", "whoami names the bound key's project")

	assert.Equal(t, http.StatusOK, o.call(h, o.f.plainRaw, whoami792).Code, "the key's own secret still works")
}

// The issue's acceptance list, observed where a CI job observes it: the HTTP
// status of the companion MCP endpoint. Each token differs from the served
// control token in one respect, and each is a 401.
func TestCompanionOIDC792_AcceptanceNegativesAreA401AtTheEndpoint(t *testing.T) {
	o := newOIDC792(t)
	h := AuthMiddleware(o.cfg)(http.HandlerFunc(o.f.srv.CompanionMCPHandler))
	require.Equal(t, http.StatusOK, o.call(h, o.sign(t, o.claims()), whoami792).Code, "the control token is served")

	tokens := map[string]string{}
	for name, mut := range map[string]func(c map[string]any){
		"wrong repository":    func(c map[string]any) { c["repository"] = "acme/other" },
		"wrong repository_id": func(c map[string]any) { c["repository_id"] = "9999" },
		"wrong ref":           func(c map[string]any) { c["ref"] = "refs/heads/feature" },
		"wrong workflow file": func(c map[string]any) { c["job_workflow_ref"] = "acme/app/.github/workflows/other.yml@refs/heads/main" },
		"wrong audience":      func(c map[string]any) { c["aud"] = "https://elsewhere.example" },
		"expired":             func(c map[string]any) { c["exp"] = o.now.Add(-time.Minute).Unix() },
	} {
		c := o.claims()
		mut(c)
		tokens[name] = o.sign(t, c)
	}
	enc := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(o.claims())
	require.NoError(t, err)
	tokens["unsigned (alg none)"] = enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + "."
	tokens["HS256 with a made-up signature"] = enc([]byte(`{"alg":"HS256","typ":"JWT","kid":"k1"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))

	for name, tok := range tokens {
		t.Run(name, func(t *testing.T) {
			rec := o.call(h, tok, whoami792)
			assert.Equal(t, http.StatusUnauthorized, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), "alpha", "a refused token learns nothing of the key")
		})
	}
}

// A refused token counts toward the per-address lockout, and the per-key
// limit applies to token requests under the bound key's id (design 27 §3).
func TestCompanionOIDC792_LockoutAndPerKeyLimit(t *testing.T) {
	o := newOIDC792(t)
	o.f.srv.apiKeyLimiter = ratelimit.NewAPIKeyLimiter()
	lock := newAuthFailureLimiter(3, time.Minute, time.Minute)
	o.cfg.AuthFailures = lock
	o.cfg.PerIPLimiter = ratelimit.NewPerIPLimiter()
	h := AuthMiddleware(o.cfg)(http.HandlerFunc(o.f.srv.CompanionMCPHandler))

	// The per-key limit: 50 token requests pass, the 51st is 429.
	tok := o.sign(t, o.claims())
	for i := 0; i < 50; i++ {
		require.Equal(t, http.StatusOK, o.call(h, tok, toolsList792).Code, "token request %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, o.call(h, tok, toolsList792).Code, "the bound key's limit applies to its tokens")

	// The lockout: three refused tokens lock the address out.
	bad := o.claims()
	bad["ref"] = "refs/heads/feature"
	badTok := o.sign(t, bad)
	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusUnauthorized, o.call(h, badTok, toolsList792).Code)
	}
	allowed, _ := lock.Allowed("198.51.100.7")
	assert.False(t, allowed, "refused tokens count toward the lockout")
}
