package github

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"
)

// MintFromKeyFile mints an installation token with the App key read from
// keyPath: the one mint the executor's push token and the provenance clones
// share (GitHub #508 F3).
func MintFromKeyFile(ctx context.Context, apiBase string, appID, installID int64, keyPath string) (string, time.Time, error) {
	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("read github private key: %w", err)
	}
	key, err := LoadPrivateKeyPEM(keyBytes)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("parse github private key: %w", err)
	}
	return MintInstallationToken(ctx, &http.Client{Timeout: 15 * time.Second}, apiBase, appID, installID, key)
}

// InstallationTokenSource mints and caches installation tokens by
// installation id until five minutes before they expire.
type InstallationTokenSource struct {
	// Mint defaults to MintFromKeyFile.
	Mint func(ctx context.Context, apiBase string, appID, installID int64, keyPath string) (string, time.Time, error)
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
}

type cachedInstallationToken struct {
	token   string
	expires time.Time
}

const installationTokenRenewBuffer = 5 * time.Minute

// Token returns a cached or freshly minted token for the installation.
func (s *InstallationTokenSource) Token(ctx context.Context, apiBase string, appID, installID int64, keyPath string) (string, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := AppIdentity{APIBase: apiBase, AppID: appID, InstallationID: installID, KeyPath: keyPath}
	if c, ok := s.cache[id]; ok && c.expires.Sub(now()) > installationTokenRenewBuffer {
		return c.token, nil
	}
	mint := s.Mint
	if mint == nil {
		mint = MintFromKeyFile
	}
	tok, exp, err := mint(ctx, apiBase, appID, installID, keyPath)
	if err != nil {
		return "", err
	}
	if s.cache == nil {
		s.cache = map[AppIdentity]cachedInstallationToken{}
	}
	s.cache[id] = cachedInstallationToken{token: tok, expires: exp}
	return tok, nil
}
