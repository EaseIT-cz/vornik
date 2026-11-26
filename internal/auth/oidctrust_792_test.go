package auth

// GitHub #792 (design 27 §3): a GitHub Actions ID token authenticates as the
// companion key its trust binds, when every claim matches exactly. Before
// this change a JWT in the bearer matched no backend and was a 401 for every
// claim set, so a CI job could only use a stored secret.
//
// Found at: review (#792, 2026-11-16, vornik-cloud's secret-scan review),
// observed at unit (no backend accepts a token).

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"

	"vornik.io/vornik/internal/persistence"
)

type issuer792 struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	other *rsa.PrivateKey
}

func newIssuer792(t *testing.T) *issuer792 {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	o, _ := rsa.GenerateKey(rand.Reader, 2048)
	is := &issuer792{key: k, other: o}
	is.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &k.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	}))
	t.Cleanup(is.srv.Close)
	return is
}

func (is *issuer792) sign(t *testing.T, key *rsa.PrivateKey, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(claims)
	jws, err := signer.Sign(b)
	if err != nil {
		t.Fatal(err)
	}
	out, _ := jws.CompactSerialize()
	return out
}

const (
	aud792 = "https://vornik.example.org"
	wf792  = "acme/app/.github/workflows/review.yml@refs/heads/main"
)

func (is *issuer792) claims(now time.Time) map[string]any {
	return map[string]any{
		"iss": is.srv.URL, "aud": aud792, "sub": "repo:acme/app:ref:refs/heads/main",
		"repository": "acme/app", "repository_id": "4242", "ref": "refs/heads/main",
		"job_workflow_ref": wf792, "event_name": "push",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

type trustStore792 struct {
	mu      sync.Mutex
	trusts  []persistence.CompanionOIDCTrust
	touched []string
}

func (s *trustStore792) OIDCTrustsForRepository(_ context.Context, iss string, repoID int64) ([]persistence.CompanionOIDCTrust, error) {
	var out []persistence.CompanionOIDCTrust
	for _, t := range s.trusts {
		if t.Issuer == iss && t.RepositoryID == repoID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (s *trustStore792) TouchOIDCTrust(_ context.Context, id string, _ time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.touched = append(s.touched, id)
	return nil
}

type keysByID792 map[string]*persistence.APIKey

func (k keysByID792) GetByID(_ context.Context, id string) (*persistence.APIKey, error) {
	if r, ok := k[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, persistence.ErrAPIKeyNotFound
}

type fixture792 struct {
	is    *issuer792
	store *trustStore792
	keys  keysByID792
	owner *OwnerAccess
	b     *OIDCTrustBackend
	now   time.Time
}

func newFixture792(t *testing.T) *fixture792 {
	t.Helper()
	is := newIssuer792(t)
	f := &fixture792{is: is, now: time.Now(),
		store: &trustStore792{trusts: []persistence.CompanionOIDCTrust{{
			ID: "ktrust_1", KeyID: "akey-ci", Issuer: is.srv.URL, Audience: aud792, RepositoryID: 4242,
			Repository: "acme/app", Ref: "refs/heads/main", JobWorkflowRef: wf792, EventNames: []string{"push", "workflow_dispatch"},
		}}},
		keys: keysByID792{"akey-ci": {ID: "akey-ci", ProjectID: "ci-proj", Name: "ci", ClientKind: "github-actions", CreatedAt: time.Now()}},
	}
	db := NewDBKeysBackend(nil, nil)
	db.OwnerAccess = func(context.Context, string) (OwnerAccess, error) {
		if f.owner != nil {
			return *f.owner, nil
		}
		return OwnerAccess{}, nil
	}
	f.b = NewOIDCTrustBackend(OIDCTrustConfig{
		Issuer: is.srv.URL, JWKSURL: is.srv.URL, Trusts: f.store, Keys: f.keys, Admit: db,
		HTTPClient: is.srv.Client(), Now: func() time.Time { return f.now },
	})
	return f
}

func (f *fixture792) auth(t *testing.T, token string) (*Identity, error) {
	t.Helper()
	return f.b.Authenticate(context.Background(), Credential{BearerToken: token, Path: "/api/v1/mcp/companion"})
}

func TestOIDCTrust792_AMatchingTokenIsTheBoundKey(t *testing.T) {
	f := newFixture792(t)
	id, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now)))
	if err != nil {
		t.Fatalf("matching token refused: %v", err)
	}
	row, _ := id.Extra[ExtraDBKeyRow].(*persistence.APIKey)
	if row == nil || row.ID != "akey-ci" || id.BoundProjectID != "ci-proj" {
		t.Fatalf("identity %+v row %+v", id, row)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f.store.mu.Lock()
		n := len(f.store.touched)
		f.store.mu.Unlock()
		if n == 1 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the trust's last_used_at was not touched")
}

// Every one of these is a 401 (ErrUnauthorized), never a fall-through.
func TestOIDCTrust792_EachMismatchIsRefused(t *testing.T) {
	for name, mut := range map[string]func(c map[string]any){
		"another repository_id":       func(c map[string]any) { c["repository_id"] = "9999" },
		"same id, another repository": func(c map[string]any) { c["repository"] = "acme/renamed" },
		"another ref":                 func(c map[string]any) { c["ref"] = "refs/heads/feature" },
		"a pull request merge ref":    func(c map[string]any) { c["ref"] = "refs/pull/7/merge" },
		"another workflow file":       func(c map[string]any) { c["job_workflow_ref"] = "acme/app/.github/workflows/other.yml@refs/heads/main" },
		"a reusable workflow it called": func(c map[string]any) {
			c["job_workflow_ref"] = "acme/shared/.github/workflows/lib.yml@refs/heads/main"
		},
		"an event not listed": func(c map[string]any) { c["event_name"] = "pull_request_target" },
		"another audience":    func(c map[string]any) { c["aud"] = "https://elsewhere.example" },
		"two audiences":       func(c map[string]any) { c["aud"] = []string{aud792, "https://elsewhere.example"} },
		"expired":             func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() },
		"nbf in the future":   func(c map[string]any) { c["nbf"] = time.Now().Add(10 * time.Minute).Unix() },
		"iat in the future":   func(c map[string]any) { c["iat"] = time.Now().Add(10 * time.Minute).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture792(t)
			c := f.is.claims(f.now)
			mut(c)
			if _, err := f.auth(t, f.is.sign(t, f.is.key, c)); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("got %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestOIDCTrust792_ForgedTokensAreRefused(t *testing.T) {
	f := newFixture792(t)
	c := f.is.claims(f.now)
	if _, err := f.auth(t, f.is.sign(t, f.is.other, c)); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("signed by another key: %v", err)
	}
	payload, _ := json.Marshal(c)
	enc := base64.RawURLEncoding.EncodeToString
	none := enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc(payload) + "."
	if _, err := f.auth(t, none); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("alg none: %v", err)
	}
	hs := enc([]byte(`{"alg":"HS256","typ":"JWT","kid":"k1"}`)) + "." + enc(payload) + "." + enc([]byte("sig"))
	if _, err := f.auth(t, hs); !errors.Is(err, ErrUnauthorized) {
		t.Errorf("HS256: %v", err)
	}
}

func TestOIDCTrust792_TheKeyMustBeLiveCompanionAndItsOwnerEnabled(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		f := newFixture792(t)
		at := time.Now()
		f.keys["akey-ci"].RevokedAt = &at
		if _, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now))); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("revoked key: %v", err)
		}
	})
	t.Run("not a companion key", func(t *testing.T) {
		f := newFixture792(t)
		f.keys["akey-ci"].ClientKind = ""
		if _, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now))); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("non-companion key: %v", err)
		}
	})
	t.Run("owner disabled", func(t *testing.T) {
		f := newFixture792(t)
		f.owner = &OwnerAccess{Mapped: true, Disabled: true}
		if _, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now))); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("disabled owner: %v", err)
		}
	})
}

// Anything that is not a JWT from this issuer is left to the other backends.
func TestOIDCTrust792_OtherCredentialsGetNoOpinion(t *testing.T) {
	f := newFixture792(t)
	c := f.is.claims(f.now)
	c["iss"] = "https://another-issuer.example"
	for name, tok := range map[string]string{
		"a sk-vornik key":  "sk-vornik-abc_def",
		"not a JWT":        "abc.def",
		"a foreign issuer": f.is.sign(t, f.is.key, c),
		"an empty bearer":  "",
	} {
		if _, err := f.auth(t, tok); !errors.Is(err, ErrNoCredential) {
			t.Errorf("%s: got %v, want ErrNoCredential", name, err)
		}
	}
}

// A JWKS that cannot be fetched is a 401, never a fall-through.
func TestOIDCTrust792_UnreachableKeysAreA401(t *testing.T) {
	f := newFixture792(t)
	tok := f.is.sign(t, f.is.key, f.is.claims(f.now))
	f.is.srv.Close()
	if _, err := f.auth(t, tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("JWKS down: %v", err)
	}
}

// Two trusts matching one token: refused, never row order (review 3e53 F1).
func TestOIDCTrust792_AmbiguousTrustsAreRefused(t *testing.T) {
	f := newFixture792(t)
	second := f.store.trusts[0]
	second.ID, second.KeyID, second.EventNames = "ktrust_2", "akey-ci2", []string{"push"}
	f.store.trusts = append(f.store.trusts, second)
	f.keys["akey-ci2"] = &persistence.APIKey{ID: "akey-ci2", ProjectID: "other-proj", ClientKind: "github-actions", CreatedAt: time.Now()}
	if _, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now))); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("a token two trusts match: %v", err)
	}
}

type toucher792 struct {
	mu  sync.Mutex
	ids []string
}

func (t *toucher792) TouchLastUsed(_ context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ids = append(t.ids, id)
	return nil
}

// The bound key's last_used_at is touched once per request, by admitRow.
func TestOIDCTrust792_TheKeyIsTouchedOnce(t *testing.T) {
	f := newFixture792(t)
	tc := &toucher792{}
	f.b.cfg.Admit.Toucher = tc
	if _, err := f.auth(t, f.is.sign(t, f.is.key, f.is.claims(f.now))); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		tc.mu.Lock()
		n := len(tc.ids)
		tc.mu.Unlock()
		if n >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	tc.mu.Lock()
	defer tc.mu.Unlock()
	if len(tc.ids) != 1 || tc.ids[0] != "akey-ci" {
		t.Fatalf("key touches %v, want exactly [akey-ci]", tc.ids)
	}
}
