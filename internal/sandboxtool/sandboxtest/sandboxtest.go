// Package sandboxtest is a fake sandbox for the unit tests of the features
// that run through internal/sandboxtool: it records every Spec and plays the
// tool by writing into a fresh /out directory, so no test starts a container
// or a host program.
package sandboxtest

import (
	"context"
	"os"
	"sync"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
)

// Handler plays one tool run: in holds each input's bytes by name, and the
// handler writes what the tool would into out. A returned error is the run's
// error.
type Handler func(spec sandboxtool.Spec, in map[string][]byte, out string) error

// OutputHandler is a Handler that also returns what the tool printed: the
// run's Result.Output, which the real runner fills with the container's
// combined stdout and stderr even when the tool exits 0.
type OutputHandler func(spec sandboxtool.Spec, in map[string][]byte, out string) (output []byte, err error)

// Fake implements sandboxtool.Sandbox.
type Fake struct {
	t      *testing.T
	handle OutputHandler

	mu    sync.Mutex
	specs []sandboxtool.Spec
}

// New returns a Fake whose runs are played by handle.
func New(t *testing.T, handle Handler) *Fake {
	t.Helper()
	return &Fake{t: t, handle: func(spec sandboxtool.Spec, in map[string][]byte, out string) ([]byte, error) {
		return nil, handle(spec, in, out)
	}}
}

// NewWithOutput returns a Fake whose runs are played by handle, including
// what each tool printed.
func NewWithOutput(t *testing.T, handle OutputHandler) *Fake {
	t.Helper()
	return &Fake{t: t, handle: handle}
}

// Run records spec, reads its inputs, and lets the handler play the tool.
func (f *Fake) Run(_ context.Context, spec sandboxtool.Spec) (*sandboxtool.Result, error) {
	f.mu.Lock()
	f.specs = append(f.specs, spec)
	f.mu.Unlock()
	in := map[string][]byte{}
	for _, i := range spec.Inputs {
		if i.Path == "" {
			in[i.Name] = i.Data
			continue
		}
		b, err := os.ReadFile(i.Path)
		if err != nil {
			return nil, err
		}
		in[i.Name] = b
	}
	out := f.t.TempDir()
	printed, err := f.handle(spec, in, out)
	if err != nil {
		return nil, err
	}
	return &sandboxtool.Result{OutDir: out, Output: printed}, nil
}

// Specs returns every run so far, in order.
func (f *Fake) Specs() []sandboxtool.Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sandboxtool.Spec(nil), f.specs...)
}

// NotAvailable is the error a runner returns when the image lacks the tool.
func NotAvailable(feature sandboxtool.Feature) error {
	return &sandboxtool.RunError{Outcome: sandboxtool.OutcomeNotAvailable, Feature: feature,
		Detail: "the image does not declare the tool"}
}

// Failed is a tool failure with the tool's own message.
func Failed(feature sandboxtool.Feature, detail string) error {
	return &sandboxtool.RunError{Outcome: sandboxtool.OutcomeFailed, Feature: feature, Detail: detail}
}
