package github

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// KeyFileMint names one mint from a key file: the App, the installation, the
// key, the token's scope (#820) and whether the block is App-backed
// (use_registration), which decides whether the instance key may be used.
type KeyFileMint struct {
	APIBase        string
	AppID          int64
	InstallationID int64
	KeyPath        string
	Repositories   []string
	AppBacked      bool
}

// MintFromKeyFile mints an installation token with the App key read from
// m.KeyPath: the one mint the executor's push token and the provenance clones
// share (GitHub #508 F3). The key is checked against the instance App key at
// every mint (#820) and the token is scoped to m.Repositories.
func MintFromKeyFile(ctx context.Context, m KeyFileMint) (string, time.Time, error) {
	keyBytes, err := os.ReadFile(m.KeyPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read github private key: %w", err)
	}
	if err := GuardKey(KeyHash(keyBytes), m.AppBacked); err != nil {
		return "", time.Time{}, err
	}
	key, err := LoadPrivateKeyPEM(keyBytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse github private key: %w", err)
	}
	return MintInstallationToken(ctx, &http.Client{Timeout: 15 * time.Second}, m.APIBase, m.AppID, m.InstallationID, key, m.Repositories)
}

// InstallationTokenSource mints and caches installation tokens by
// installation id until five minutes before they expire.
type InstallationTokenSource struct {
	// Mint defaults to MintFromKeyFile.
	Mint func(ctx context.Context, m KeyFileMint) (string, time.Time, error)
	Now  func() time.Time

	mu    sync.Mutex
	cache map[AppIdentity]cachedInstallationToken
}

// AppIdentity is what a cached token belongs to: the whole App identity, not
// the installation id alone, which is configuration any project can name
// (security review of 180ed1da9). A project naming another's installation
// with its own App id or key mints with its own key, and fails if that key
// cannot.
type AppIdentity struct {
	APIBase        string
	AppID          int64
	InstallationID int64
	KeyPath        string
	// Scope is ScopeKey of the token's repositories (#820), so a scoped and
	// an unscoped token, or two scopes, never share a cache entry.
	Scope string
}

type cachedInstallationToken struct {
	token   string
	expires time.Time
}

const installationTokenRenewBuffer = 5 * time.Minute

// Token returns a cached or freshly minted token for the mint m, keyed by
// the App identity and the token's scope (#820).
func (s *InstallationTokenSource) Token(ctx context.Context, m KeyFileMint) (string, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := AppIdentity{APIBase: m.APIBase, AppID: m.AppID, InstallationID: m.InstallationID, KeyPath: m.KeyPath, Scope: ScopeKey(m.Repositories)}
	if c, ok := s.cache[id]; ok && c.expires.Sub(now()) > installationTokenRenewBuffer {
		return c.token, nil
	}
	mint := s.Mint
	if mint == nil {
		mint = MintFromKeyFile
	}
	if len(m.Repositories) == 0 {
		log.Printf("github-app: installation %d: minting an unscoped token (the project names no repo; #820)", m.InstallationID)
	}
	tok, exp, err := mint(ctx, m)
	if err != nil {
		return "", err
	}
	if s.cache == nil {
		s.cache = map[AppIdentity]cachedInstallationToken{}
	}
	s.cache[id] = cachedInstallationToken{token: tok, expires: exp}
	return tok, nil
}
