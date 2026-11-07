package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #640 (federation design §6, corrected 2026-11-05): a consult made
// from a task an inbound A2A consult created sends that task's hop count plus
// one. The count is read from the task's payload, context.a2a.metadata, or
// from the nearest ancestor's when the consult comes from a subtask. Before
// the fix the lookup was nil, so every consult sent hop 1.
func TestTaskHopLookup_ReadsPayloadAndAncestors_640(t *testing.T) {
	parent := "t-inbound"
	tasks := map[string]*persistence.Task{
		"t-inbound": {ID: "t-inbound", Payload: []byte(`{"context":{"a2a":{"metadata":{"x-vornik-consult-hops":2}}}}`)},
		"t-child":   {ID: "t-child", ParentTaskID: &parent, Payload: []byte(`{"context":{"prompt":"sub"}}`)},
		"t-local":   {ID: "t-local", Payload: []byte(`{"context":{"prompt":"from chat"}}`)},
		"t-bad":     {ID: "t-bad", Payload: []byte(`{"context":{"a2a":{"metadata":{"x-vornik-consult-hops":"7"}}}}`)},
	}
	repo := &mocks.MockTaskRepository{GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
		if tk, ok := tasks[id]; ok {
			return tk, nil
		}
		return nil, persistence.ErrNotFound
	}}
	l := taskHopLookup{tasks: repo}
	ctx := context.Background()
	for id, want := range map[string]int{"t-inbound": 2, "t-child": 2, "t-local": 0, "t-bad": 0, "t-missing": 0} {
		if got := l.InboundConsultHops(ctx, id); got != want {
			t.Errorf("%s: hops %d, want %d", id, got, want)
		}
	}
}
