package registry

// Acceptance suite for dp-01-nilguard (benchmark LLD §12.24). Written from the
// task's specification, never shown to the agents. Pinned API:
//   type Registry; func New() *Registry
//   func (r *Registry) Add(name string, v any) error
//   func (r *Registry) Get(name string) (any, bool)
//   var ErrNilRegistry error — what Add returns on a nil receiver

import (
	"errors"
	"testing"
)

func TestAcceptance_NilReceiverIsSafe(t *testing.T) {
	var r *Registry
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("a method on a nil *Registry panicked: %v", p)
		}
	}()
	if v, ok := r.Get("x"); v != nil || ok {
		t.Errorf("nil Get = (%v, %v), want (nil, false)", v, ok)
	}
	if err := r.Add("x", 1); !errors.Is(err, ErrNilRegistry) {
		t.Errorf("nil Add = %v, want ErrNilRegistry", err)
	}
}

func TestAcceptance_AddThenGet(t *testing.T) {
	r := New()
	if err := r.Add("a", 42); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if v, ok := r.Get("a"); !ok || v != 42 {
		t.Errorf("Get(a) = (%v, %v), want (42, true)", v, ok)
	}
	if v, ok := r.Get("missing"); v != nil || ok {
		t.Errorf("Get(missing) = (%v, %v), want (nil, false)", v, ok)
	}
}
