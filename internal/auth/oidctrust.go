package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/rs/zerolog/log"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #792 (design 27 §C2): a GitHub Actions ID token authenticates as
// the companion key a trust binds to that workflow. Every claim the trust
// names must match exactly; any failure after the issuer gate is
// ErrUnauthorized, never a fall-through.

// GitHubActionsIssuer is the only issuer a trust may name.
const GitHubActionsIssuer = "https://token.actions.githubusercontent.com"

// oidcClockSkew bounds nbf and iat, as OpenIDProvider does for iat.
const oidcClockSkew = time.Minute

// OIDCTrustStore is the slice of persistence.CompanionOIDCTrustRepository the
// backend reads.
type OIDCTrustStore interface {
	OIDCTrustsForRepository(ctx context.Context, issuer string, repositoryID int64) ([]persistence.CompanionOIDCTrust, error)
	TouchOIDCTrust(ctx context.Context, id string, at time.Time) error
}

// APIKeyByID loads a key row by id, revoked or not.
type APIKeyByID interface {
	GetByID(ctx context.Context, keyID string) (*persistence.APIKey, error)
}

// OIDCTrustConfig wires the backend.
type OIDCTrustConfig struct {
	Issuer     string // default GitHubActionsIssuer; tests inject a local one
	JWKSURL    string // default Issuer + "/.well-known/jwks"
	Trusts     OIDCTrustStore
	Keys       APIKeyByID
	Admit      *DBKeysBackend // admitRow: the DB-keys backend's own checks
	HTTPClient *http.Client   // guarded, time-bounded, size-capped
	Now        func() time.Time
}

// OIDCTrustBackend implements Backend for GitHub Actions ID tokens.
type OIDCTrustBackend struct {
	cfg      OIDCTrustConfig
	verifier *gooidc.IDTokenVerifier
}

// NewOIDCTrustBackend builds the backend. go-oidc's key set caches keys and
// refetches on an unknown kid.
func NewOIDCTrustBackend(cfg OIDCTrustConfig) *OIDCTrustBackend {
	if cfg.Issuer == "" {
		cfg.Issuer = GitHubActionsIssuer
	}
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = strings.TrimSuffix(cfg.Issuer, "/") + "/.well-known/jwks"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	ctx := context.Background()
	if cfg.HTTPClient != nil {
		ctx = gooidc.ClientContext(ctx, cfg.HTTPClient)
	}
	keys := gooidc.NewRemoteKeySet(ctx, cfg.JWKSURL)
	v := gooidc.NewVerifier(cfg.Issuer, keys, &gooidc.Config{
		SkipClientIDCheck:    true, // the audience is checked below, exactly
		SupportedSigningAlgs: []string{gooidc.RS256},
		Now:                  cfg.Now,
	})
	return &OIDCTrustBackend{cfg: cfg, verifier: v}
}

// Name returns the audit-trail identifier.
func (b *OIDCTrustBackend) Name() string { return "oidc-trust" }

// githubClaims are the claims a trust matches (design 27 §1).
type githubClaims struct {
	Repository     string `json:"repository"`
	RepositoryID   string `json:"repository_id"`
	Ref            string `json:"ref"`
	JobWorkflowRef string `json:"job_workflow_ref"`
	EventName      string `json:"event_name"`
	NotBefore      int64  `json:"nbf"`
}

// Authenticate implements Backend.
func (b *OIDCTrustBackend) Authenticate(ctx context.Context, cred Credential) (*Identity, error) {
	if b == nil || b.cfg.Trusts == nil || b.cfg.Keys == nil || b.cfg.Admit == nil {
		return nil, ErrNoCredential
	}
	if unverifiedIssuer(cred.BearerToken) != b.cfg.Issuer {
		return nil, ErrNoCredential
	}
	idt, err := b.verifier.Verify(ctx, cred.BearerToken)
	if err != nil {
		return nil, b.refuse("bad_token", err)
	}
	now := b.cfg.Now()
	if idt.IssuedAt.After(now.Add(oidcClockSkew)) {
		return nil, b.refuse("bad_token", errors.New("iat in the future"))
	}
	var c githubClaims
	if err := idt.Claims(&c); err != nil {
		return nil, b.refuse("bad_token", err)
	}
	// GitHub always sets nbf; a token without one is checked by exp and iat
	// only.
	if c.NotBefore != 0 && time.Unix(c.NotBefore, 0).After(now.Add(oidcClockSkew)) {
		return nil, b.refuse("bad_token", errors.New("nbf in the future"))
	}
	if len(idt.Audience) != 1 {
		return nil, b.refuse("claim_mismatch", fmt.Errorf("%d audiences, want exactly one", len(idt.Audience)))
	}
	repoID, err := strconv.ParseInt(c.RepositoryID, 10, 64)
	if err != nil {
		return nil, b.refuse("claim_mismatch", errors.New("repository_id is not a number"))
	}
	trusts, err := b.cfg.Trusts.OIDCTrustsForRepository(ctx, b.cfg.Issuer, repoID)
	if err != nil {
		return nil, b.refuse("trust_store_error", err)
	}
	// Exactly one trust may match: the token never names a key, so two
	// matches would let row order pick it (design 27 §C1, review 3e53 F1).
	// The create path refuses overlapping trusts; this refuses what two
	// concurrent creates could still leave.
	var match *persistence.CompanionOIDCTrust
	for i := range trusts {
		if trustMatches(trusts[i], idt.Audience[0], c) {
			if match != nil {
				return nil, b.refuse("ambiguous_trust", fmt.Errorf("trusts %s and %s both match", match.ID, trusts[i].ID))
			}
			match = &trusts[i]
		}
	}
	if match == nil {
		return nil, b.refuse("no_matching_trust", fmt.Errorf("repository_id %d: no trust matches", repoID))
	}
	row, err := b.cfg.Keys.GetByID(ctx, match.KeyID)
	if err != nil {
		return nil, b.refuse("key_refused", err)
	}
	if row.ClientKind == "" {
		return nil, b.refuse("key_refused", errors.New("the bound key is not a companion key"))
	}
	id, err := b.cfg.Admit.admitRow(ctx, row)
	if err != nil {
		return nil, b.refuse("key_refused", err)
	}
	go func(trustID string, at time.Time) {
		_ = b.cfg.Trusts.TouchOIDCTrust(context.Background(), trustID, at)
	}(match.ID, now)
	return id, nil
}

// trustMatches compares every claim the trust names, exactly.
func trustMatches(t persistence.CompanionOIDCTrust, aud string, c githubClaims) bool {
	if t.Audience != aud || t.Repository != c.Repository || t.Ref != c.Ref || t.JobWorkflowRef != c.JobWorkflowRef {
		return false
	}
	for _, e := range t.EventNames {
		if e == c.EventName {
			return true
		}
	}
	return false
}

// refuse logs the class and cause (the chain returns a bare sentinel) and
// returns ErrUnauthorized wrapped, so errors.Is still matches.
func (b *OIDCTrustBackend) refuse(class string, cause error) error {
	log.Warn().Str("component", "auth-oidc-trust").Str("class", class).Err(cause).
		Msg("GitHub Actions token refused")
	return fmt.Errorf("%w: oidc trust %s: %v", ErrUnauthorized, class, cause)
}

// unverifiedIssuer reads iss from a compact JWT without verifying it, only to
// decide whether this backend has an opinion. "" for anything else.
func unverifiedIssuer(tok string) string {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil {
		return ""
	}
	return c.Iss
}
