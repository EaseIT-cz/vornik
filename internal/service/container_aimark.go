package service

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/secretstore"
)

// buildAIMarker returns the daemon's EU AI Act Art 50(2) marker (GitHub
// #117; Art 50 design §14.7, §14.8 B): an Ed25519 signer derived from the
// secret store's master key, recording into ledger, with its public key
// registered so marks stay verifiable after the key is gone. Without a
// secret store there is no key and so no marker: output is sent unmarked,
// which the caller logs.
func buildAIMarker(ctx context.Context, st *secretstore.Store, ledger aimark.Ledger, release func() string, logger *zerolog.Logger) *aimark.Marker {
	if st == nil || ledger == nil {
		return nil
	}
	seed, err := st.AIMarkSigningSeed()
	if err != nil {
		if logger != nil {
			logger.Warn().Err(err).Msg("AI output marking off: the signing key could not be derived")
		}
		return nil
	}
	signer := aimark.NewSigner(seed)
	if err := aimark.RegisterKey(ctx, ledger, signer, time.Now()); err != nil && logger != nil {
		logger.Warn().Err(err).Str("kid", signer.KID()).Msg("AI output mark key not recorded; marks are still signed")
	}
	return aimark.NewMarker(signer, ledger, release, logger, prometheus.DefaultRegisterer)
}

// aiMarker builds the container's marker once, from the secret store loaded
// at start. A store created later (the first credential entry) takes effect
// at the next start.
func (c *Container) aiMarker() *aimark.Marker {
	c.aiMarkerOnce.Do(func() {
		c.ensureSecretStore()
		c.secretStoreMu.Lock()
		st := c.secretStore
		c.secretStoreMu.Unlock()
		var ledger aimark.Ledger
		if c.repos != nil && c.repos.AIOutputMarks != nil {
			ledger = c.repos.AIOutputMarks
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.aiMarkerVal = buildAIMarker(ctx, st, ledger, c.Version, &c.Logger)
		if c.aiMarkerVal == nil {
			c.Logger.Warn().Msg("AI output marking (EU AI Act Art 50(2)) is off: no secret store key; generated output is sent unmarked until one exists and the daemon restarts")
		}
	})
	return c.aiMarkerVal
}
