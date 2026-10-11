package mocks

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #303 (review-20261009-7123 F6): the mock keeps the drivers'
// contract for UpdateIfStatus, so a test cannot pass against it with a call
// the real repository refuses.
func TestMockTaskRepository_UpdateIfStatus(t *testing.T) {
	m := &MockTaskRepository{}
	if ok, err := m.UpdateIfStatus(context.Background(), &persistence.Task{ID: "t"}, nil); err == nil || ok {
		t.Fatalf("empty allowed = %v, %v; want false and an error", ok, err)
	}
	updated := false
	m.UpdateFunc = func(context.Context, *persistence.Task) error { updated = true; return nil }
	ok, err := m.UpdateIfStatus(context.Background(), &persistence.Task{ID: "t"}, []persistence.TaskStatus{persistence.TaskStatusRunning})
	if err != nil || !ok || !updated {
		t.Fatalf("default = %v, %v (update called %v); want it to behave as Update", ok, err, updated)
	}
	if m.CallCount.UpdateIfStatus != 2 {
		t.Fatalf("call count = %d", m.CallCount.UpdateIfStatus)
	}
}
