// Package registry is the reference solution for dp-01-nilguard's acceptance suite.
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
	if r == nil {
		return nil, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	v, ok := r.m[name]
	return v, ok
}
