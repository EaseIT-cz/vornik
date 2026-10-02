package service

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/mcpauth"
	"vornik.io/vornik/internal/secretstore"
)

// Agent-administered Vornik design §8.1: agent credentials resolve through
// the namespaced secret store; operator secrets keep resolving from the
// process environment.

// storeKeyPath is the master key's location: the secrets/ dir beside config.yaml.
func (c *Container) storeKeyPath() string {
	return filepath.Join(onboardingSecretsDir(c.ConfigPath), "store.key")
}

// mcpGrantsFor builds a project-scoped server's grants. The namespace comes
// from the project ID alone (agentns), so no config field can claim one.
func mcpGrantsFor(projectID string, allowed []string) mcpauth.Grants {
	ns, _ := agentns.FromID(projectID)
	return mcpauth.Grants{Allowed: allowed, Namespace: ns}
}

// ensureSecretStore opens the agent secret store once, when a key exists. It
// never creates a key: creation belongs to the first Put (credential entry),
// so a daemon whose key went missing reports it instead of minting a key that
// opens nothing. When the key is unusable while sealed rows exist, the error
// is kept so every namespaced lookup reports it (plan review f329 finding 8).
func (c *Container) ensureSecretStore() {
	c.secretStoreMu.Lock()
	defer c.secretStoreMu.Unlock()
	if c.secretStoreLoaded || c.repos == nil || c.repos.AgentSecrets == nil {
		return
	}
	c.secretStoreLoaded = true
	key, err := secretstore.LoadKey(c.storeKeyPath())
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if n, cerr := c.repos.AgentSecrets.CountAll(ctx); cerr == nil && n > 0 {
			c.secretStoreErr = fmt.Errorf("%d agent credential(s) are stored but %w", n, err)
		}
		return
	}
	st, err := secretstore.New(c.repos.AgentSecrets, key)
	if err != nil {
		c.secretStoreErr = err
		return
	}
	c.secretStore = st
}

// setSecretStore installs a store created after boot (the first credential
// entry creates the key) and clears any recorded key error.
func (c *Container) setSecretStore(st *secretstore.Store) {
	c.secretStoreMu.Lock()
	defer c.secretStoreMu.Unlock()
	c.secretStore, c.secretStoreErr, c.secretStoreLoaded = st, nil, true
}

// secretStoreForWrite returns the store, creating the master key on the
// first credential entry (plan P4.1). It is the only key-creating path.
// LoadOrCreateKey is atomic across concurrent creators (O_EXCL temp file,
// hard-linked into place), and the mutex makes one store of it. It refuses
// when sealed rows exist but the key is unusable: a new key would open none
// of them (the doctor reports secret_store_key).
func (c *Container) secretStoreForWrite() (*secretstore.Store, error) {
	c.ensureSecretStore()
	c.secretStoreMu.Lock()
	defer c.secretStoreMu.Unlock()
	if c.secretStore != nil {
		return c.secretStore, nil
	}
	if c.secretStoreErr != nil {
		return nil, c.secretStoreErr
	}
	if c.repos == nil || c.repos.AgentSecrets == nil {
		return nil, fmt.Errorf("the agent credential store is not available")
	}
	key, err := secretstore.LoadOrCreateKey(c.storeKeyPath())
	if err != nil {
		return nil, err
	}
	st, err := secretstore.New(c.repos.AgentSecrets, key)
	if err != nil {
		return nil, err
	}
	c.secretStore, c.secretStoreErr, c.secretStoreLoaded = st, nil, true
	return st, nil
}

// currentSecretSource snapshots the store state for one lookup.
func (c *Container) currentSecretSource() secretstore.Source {
	c.ensureSecretStore()
	c.secretStoreMu.Lock()
	st, kerr := c.secretStore, c.secretStoreErr
	c.secretStoreMu.Unlock()
	return secretstore.Source{
		Store:  st,
		Env:    mcpauth.EnvSecretSource{},
		KeyErr: kerr,
		OnError: func(name string, err error) {
			c.Logger.Error().Err(err).Str("secret", name).Msg("agent secret could not be resolved")
		},
	}
}

// containerSecretSource asks the container on every lookup, so a connector
// built at boot sees a store created later.
type containerSecretSource struct{ c *Container }

func (s containerSecretSource) Get(name string) (string, bool) {
	return s.c.currentSecretSource().Get(name)
}

// secretSource is the resolver every mcpauth call uses.
func (c *Container) secretSource() mcpauth.SecretSource { return containerSecretSource{c: c} }
