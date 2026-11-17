// Package metrics is the reference solution for dp-03-metric's acceptance suite.
//
// Label cardinality is bounded because every distinct label set is a new map
// entry that is never freed: an unbounded label (a user id, a request id)
// grows memory without limit, a leak rather than a nuisance.
package metrics

import (
	"errors"
	"sort"
	"strings"
	"sync"
)

var ErrTooManyLabels = errors.New("metrics: more than 4 label keys")

type Registry struct {
	mu sync.Mutex
	m  map[string]int
}

func New() *Registry { return &Registry{m: map[string]int{}} }

func (r *Registry) Inc(name string, labels map[string]string) error {
	if len(labels) > 4 {
		return ErrTooManyLabels
	}
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(name)
	for _, k := range keys {
		b.WriteString("|" + k + "=" + labels[k])
	}
	r.mu.Lock()
	r.m[b.String()]++
	r.mu.Unlock()
	return nil
}

func (r *Registry) Snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]int, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
