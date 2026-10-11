package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #300 (2026-10-08): companion cancel left the execution running. The
// audit of every cancel site (05-scheduler.md §4.11) found the execution
// cancel (site #4) tore the container down but skipped the parent unblock and
// the child cascade that cancelOne performs. Both now go through one
// primitive, so both owe all three.
func TestCancelExecutionOne_UnblocksParentAndCascades(t *testing.T) {
	repo, _ := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusRunning)
	spy := &teardownSpy{}
	execRepo := &mocks.MockExecutionRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Execution, error) {
			return &persistence.Execution{ID: id, TaskID: "t1", ProjectID: "p1", Status: persistence.ExecutionStatusRunning}, nil
		},
	}
	srv := NewServer(WithExecutionRepository(execRepo), WithTaskRepository(repo), WithExecutor(spy))
	if !srv.cancelExecutionOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "e1") {
		t.Fatal("a live execution cancel was refused")
	}
	if len(spy.teardowns) != 1 || len(spy.calls) != 1 || spy.calls[0] != "t1" ||
		len(spy.cascadeCalls) != 1 || spy.cascadeCalls[0] != "t1" {
		t.Fatalf("teardown=%v unblock=%v cascade=%v, want one each for t1", spy.teardowns, spy.calls, spy.cascadeCalls)
	}
}

// Site #3, the UI task cancel, goes through the same primitive: teardown,
// parent unblock and cascade, once each, after the write (review c4af F8).
func TestCancelOne_GoesThroughThePrimitive(t *testing.T) {
	repo, stored := uiStaleRepo(persistence.TaskStatusRunning, persistence.TaskStatusRunning)
	spy := &teardownSpy{}
	srv := NewServer(WithTaskRepository(repo), WithExecutor(spy))
	if !srv.cancelOne(context.Background(), httptest.NewRequest(http.MethodPost, "/", nil), "t1") {
		t.Fatal("a live cancel was refused")
	}
	if *stored != persistence.TaskStatusCancelled {
		t.Fatalf("stored %s, want CANCELLED", *stored)
	}
	if len(spy.teardowns) != 1 || len(spy.calls) != 1 || len(spy.cascadeCalls) != 1 {
		t.Fatalf("teardown=%v unblock=%v cascade=%v, want one each", spy.teardowns, spy.calls, spy.cascadeCalls)
	}
}
