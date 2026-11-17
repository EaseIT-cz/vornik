// Package registry is a deliberately BROKEN variant (Get is not nil-safe) for dp-01-nilguard's acceptance suite.
package registry

import (
	"errors"
	"sync"
)

var ErrNilRegistry = errors.New("registry: nil registry")

type Registry struct {
	mu sync.RWMutex
	m  map[string]any
}

func New() *Registry { return &Registry{m: map[string]any{}} }

func (r *Registry) Add(name string, v any) error {
	if r == nil {
		return ErrNilRegistry
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[name] = v
	return nil
}

func (r *Registry) Get(name string) (any, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[name]
	return v, ok
}
