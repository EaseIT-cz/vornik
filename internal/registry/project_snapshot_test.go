package registry

import (
	"errors"
	"testing"
)

// Issue #70: a reload cannot replace retention policy during approved deletion.
func TestWithProjectSnapshot_PinsPolicyUntilActionCompletes(t *testing.T) {
	p := &Project{ID: "p"}
	r := &Registry{projects: map[string]*Project{"p": p}}
	want := errors.New("action failed")
	err := r.WithProjectSnapshot("p", func(got *Project) error {
		if got != p {
			t.Fatal("wrong snapshot")
		}
		if r.mu.TryLock() {
			r.mu.Unlock()
			t.Fatal("policy activation could race action")
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatal(err)
	}
	if !r.mu.TryLock() {
		t.Fatal("registry lock leaked")
	}
	r.mu.Unlock()
	if err = r.WithProjectSnapshot("missing", func(got *Project) error {
		if got != nil {
			t.Fatal("missing project returned")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIdleRetention_ValidatesProjectPolicy(t *testing.T) {
	for _, days := range []int{-1, 0, 1, 365, 36500, 36501} {
		p := &Project{ID: "p", SwarmID: "s", DefaultWorkflowID: "w", Retention: ProjectRetention{MemoryRequireApproval: true, MemoryIdleDays: days}}
		err := p.Validate("p.yaml")
		if (err != nil) != (days < 0 || days > 36500) {
			t.Fatalf("days %d: %v", days, err)
		}
	}
}
