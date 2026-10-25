package github

// GitHub #508 F3: the provenance clones mint App installation tokens through
// a cache, so a fetch every two minutes does not mint a token each time.

import (
	"context"
	"testing"
	"time"
)

func TestInstallationTokenSource508_CachesUntilNearExpiry(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	mints := 0
	s := &InstallationTokenSource{Now: func() time.Time { return now }, Mint: func(context.Context, string, int64, int64, string) (string, time.Time, error) {
		mints++
		return "tok", now.Add(time.Hour), nil
	}}
	for i := 0; i < 3; i++ {
		if tok, err := s.Token(context.Background(), "https://api.github.com", 1, 7, "/k"); err != nil || tok != "tok" {
			t.Fatalf("Token = %q, %v", tok, err)
		}
	}
	if mints != 1 {
		t.Errorf("mints = %d, want 1", mints)
	}
	now = now.Add(56 * time.Minute)
	_, _ = s.Token(context.Background(), "https://api.github.com", 1, 7, "/k")
	if mints != 2 {
		t.Errorf("mints near expiry = %d, want 2", mints)
	}
}

// Security review of 180ed1da9: a project that names another project's
// installation id with its own App id or key must not receive the cached
// token; the cache is keyed by the whole App identity.
func TestInstallationTokenSource508_CacheKeyIsTheWholeAppIdentity(t *testing.T) {
	var minted []string
	s := &InstallationTokenSource{Mint: func(_ context.Context, _ string, _, _ int64, key string) (string, time.Time, error) {
		minted = append(minted, key)
		return "tok-" + key, time.Now().Add(time.Hour), nil
	}}
	ctx := context.Background()
	victim, _ := s.Token(ctx, "https://api.github.com", 1, 77, "/victim.pem")
	borrower, _ := s.Token(ctx, "https://api.github.com", 2, 77, "/attacker.pem")
	other, _ := s.Token(ctx, "https://api.github.com", 1, 77, "/attacker.pem")
	if borrower == victim || other == victim || len(minted) != 3 {
		t.Errorf("a different App identity reused the cached token: %q %q %q (mints %v)", victim, borrower, other, minted)
	}
}
