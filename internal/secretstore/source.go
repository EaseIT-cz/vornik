package secretstore

import (
	"context"
	"errors"
	"time"

	"vornik.io/vornik/internal/mcpauth"
)

// Source resolves secret names for mcpauth: "<ns>/<NAME>" from the store,
// anything else from Env. A namespaced name NEVER falls back to the
// environment: that is the flat-namespace hole this package exists to close.
type Source struct {
	Store *Store
	Env   mcpauth.SecretSource
	// KeyErr is set when sealed rows exist but the master key could not be
	// loaded at boot. A namespaced lookup then reports it through OnError,
	// so the cause is a key problem, never a plain "missing" (plan review
	// f329 finding 8; Review Focus 2).
	KeyErr  error
	Timeout time.Duration
	// OnError observes failures other than "not set" (a key problem, a DB
	// error) so the caller can log the cause. The value is never passed.
	OnError func(name string, err error)
}

// Get implements mcpauth.SecretSource.
func (s Source) Get(name string) (string, bool) {
	ns, bare, namespaced := mcpauth.SplitSecretName(name)
	if !namespaced {
		if s.Env == nil {
			return "", false
		}
		return s.Env.Get(name)
	}
	if s.KeyErr != nil {
		s.report(name, s.KeyErr)
		return "", false
	}
	if s.Store == nil {
		return "", false
	}
	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	v, err := s.Store.Get(ctx, ns, bare)
	if err != nil {
		if !errors.Is(err, ErrNotFound) {
			s.report(name, err)
		}
		return "", false
	}
	return string(v), len(v) > 0
}

func (s Source) report(name string, err error) {
	if s.OnError != nil {
		s.OnError(name, err)
	}
}
