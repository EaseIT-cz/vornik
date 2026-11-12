package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/secretstore"
	"vornik.io/vornik/internal/storage"
)

// Configuration placeholders resolve from the operator credential store
// (GitHub #626; agent-administered Vornik design, amendment 2026-11-12 A):
// the daemon loads the configuration twice, and between the two loads it
// reads the credentials the configuration names.

// storedCredentialsLookupTimeout bounds the boot's store read.
const storedCredentialsLookupTimeout = 10 * time.Second

// credentialReader reads one operator credential.
type credentialReader interface {
	GetOperatorCredential(ctx context.Context, name string) ([]byte, error)
}

// readStoredCredentials returns the stored value of each name the store
// holds. A name it does not hold is left out. On any other failure it
// returns no map and the error: a partly read set would load a partly
// filled configuration (review a3dc F1), so the caller loads as if the store
// could not be opened.
func readStoredCredentials(ctx context.Context, st credentialReader, names []string) (map[string]string, error) {
	found := map[string]string{}
	for _, name := range names {
		v, err := st.GetOperatorCredential(ctx, name)
		if errors.Is(err, secretstore.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("credential %s: %w", name, err)
		}
		if len(v) > 0 {
			found[name] = string(v)
		}
	}
	return found, nil
}

// bootCredentials is what the boot's store read found, and why it found
// nothing when it could not read. Names only; the values go to the load.
type bootCredentials struct {
	found map[string]string
	err   error    // the store could not be opened or read
	names []string // the names it was asked for
}

// storedCredentialsForBoot opens one database connection and the store with
// the bootstrap configuration, reads the operator credential of each name,
// and closes the connection. With no names it opens nothing. A store that
// cannot be opened (no key, a wrong key, an unmigrated database) yields no
// credentials and the reason; the full load then fails exactly as it did
// before, if a required field is still empty.
func storedCredentialsForBoot(ctx context.Context, boot *config.Config, configPath string, names []string) bootCredentials {
	out := bootCredentials{names: names}
	if len(names) == 0 || boot == nil {
		return out
	}
	key, err := secretstore.LoadKey(filepath.Join(onboardingSecretsDir(configPath), "store.key"))
	if err != nil {
		out.err = fmt.Errorf("the store key: %w", err)
		return out
	}
	ctx, cancel := context.WithTimeout(ctx, storedCredentialsLookupTimeout)
	defer cancel()
	backend, err := storage.OpenReadOnly(ctx, boot.Database)
	if err != nil {
		out.err = fmt.Errorf("the database: %w", err)
		return out
	}
	defer func() { _ = backend.Close() }()
	if backend.Repos == nil || backend.Repos.AgentSecrets == nil {
		out.err = errors.New("the credential store is not available on this database")
		return out
	}
	st, err := secretstore.New(backend.Repos.AgentSecrets, key)
	if err != nil {
		out.err = err
		return out
	}
	out.found, out.err = readStoredCredentials(ctx, st, names)
	return out
}

// logBootCredentials reports the boot's store read and every shadowed
// name, once, after the logger exists. Never a value.
func (c *Container) logBootCredentials(b bootCredentials, cfg *config.Config) {
	if b.err != nil {
		c.Logger.Warn().Err(b.err).Strs("names", b.names).
			Msg("config: the credential store could not be read; placeholders it would fill stay unresolved")
	} else if len(b.found) > 0 {
		c.Logger.Info().Int("count", len(b.found)).Msg("config: placeholders filled from the credential store")
	}
	c.logShadowedCredentials(cfg)
}

// logShadowedCredentials warns once per name whose environment value won
// over a different stored value (design B).
func (c *Container) logShadowedCredentials(cfg *config.Config) {
	if cfg == nil {
		return
	}
	for _, name := range cfg.ShadowedCredentials() {
		c.Logger.Warn().Str("credential", name).
			Msg("credential " + name + ": the environment value shadows the stored one")
	}
}

// stageConfigForReload is the reload's config.yaml pass, in two passes as
// at boot (GitHub #626): the names the file references, their stored values
// from the running store, then the full load. A failed first pass is logged
// and the full load runs with the environment only, which fails exactly as
// before if a required field is left empty.
func (c *Container) stageConfigForReload() (*config.Config, *config.Provenance, error) {
	var opts []config.LoadOption
	boot, err := config.LoadBootstrapFromPath(c.ConfigPath)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("config reload: the bootstrap pass failed; placeholders resolve from the environment only")
	} else {
		opts = append(opts, config.WithCredentials(c.storedCredentialsForReload(boot.CredentialCandidates())))
	}
	staged, prov, err := config.LoadFromPathWithProvenance(c.ConfigPath, opts...)
	if err != nil {
		return nil, nil, err
	}
	c.logShadowedCredentials(staged)
	return staged, prov, nil
}

// storedCredentialsForReload reads the names a reload's configuration
// references from the running daemon's store. The reload's loader callback
// carries no context, so the read is bounded by its own timeout.
func (c *Container) storedCredentialsForReload(names []string) map[string]string {
	if len(names) == 0 {
		return nil
	}
	src := c.currentSecretSource()
	if src.KeyErr != nil || src.Store == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), storedCredentialsLookupTimeout)
	defer cancel()
	found, err := readStoredCredentials(ctx, src.Store, names)
	if err != nil {
		c.Logger.Warn().Err(err).Msg("config reload: the credential store could not be read")
	}
	return found
}
