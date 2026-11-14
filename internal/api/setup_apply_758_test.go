package api

// POST /api/v1/setup/apply restarts an idle, supervised daemon so the
// first-run page needs no shell (installation-onboarding-guide-design.md,
// "Amendment 2026-11-14 (#758)", W1). Before #758 every setup commit
// answered restart_required and the page told the user to run
// `systemctl --user restart vornik`.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/configlock"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// applyRig is a server whose restart hook counts calls.
type applyRig struct {
	srv      *Server
	restarts atomic.Int32
	called   chan struct{}
}

func supervisedEnv(k string) string {
	switch k {
	case "INVOCATION_ID":
		return "abc123"
	case "VORNIK_RESTART_ON_EXIT":
		return "1"
	}
	return ""
}

func newApplyRig(t *testing.T, env func(string) string, tasks persistence.TaskRepository) *applyRig {
	t.Helper()
	r := &applyRig{called: make(chan struct{}, 4)}
	opts := []ServerOption{WithRestartRequester(func() {
		r.restarts.Add(1)
		r.called <- struct{}{}
	})}
	if tasks != nil {
		opts = append(opts, WithTaskRepository(tasks))
	}
	r.srv = NewServer(opts...)
	r.srv.setupEnv = env
	return r
}

func (r *applyRig) apply() *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	r.srv.SetupApply(rec, httptest.NewRequest(http.MethodPost, "/api/v1/setup/apply", nil))
	return rec
}

// releaseApply undoes what a successful apply holds (the writer lock, the
// in-progress flag): in production the process exits instead.
func (r *applyRig) releaseApply() { r.srv.releaseSetupApply() }

func TestSetupApply758_SupervisedIdleDaemonRestartsAfterTheReply(t *testing.T) {
	r := newApplyRig(t, supervisedEnv, nil)
	rec := r.apply()
	if rec.Code != http.StatusAccepted {
		t.Fatalf("apply: %d %s, want 202", rec.Code, rec.Body.String())
	}
	select {
	case <-r.called:
	case <-time.After(2 * time.Second):
		t.Fatal("the restart hook was never called")
	}
	if n := r.restarts.Load(); n != 1 {
		t.Fatalf("restart hook called %d times, want 1", n)
	}
	r.releaseApply()
}

func TestSetupApply758_RefusesWithoutADeclaredSupervisor(t *testing.T) {
	for name, env := range map[string]func(string) string{
		"no INVOCATION_ID": func(k string) string {
			if k == "INVOCATION_ID" {
				return ""
			}
			return supervisedEnv(k)
		},
		"no VORNIK_RESTART_ON_EXIT": func(k string) string {
			if k == "VORNIK_RESTART_ON_EXIT" {
				return ""
			}
			return supervisedEnv(k)
		},
	} {
		r := newApplyRig(t, env, nil)
		if rec := r.apply(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "NO_SUPERVISOR") {
			t.Errorf("%s: %d %s, want 409 NO_SUPERVISOR", name, rec.Code, rec.Body.String())
		}
		if r.restarts.Load() != 0 {
			t.Errorf("%s: restarted", name)
		}
		assertApplyReleased(t, r)
	}
}

func TestSetupApply758_RefusesWhileAJobRuns(t *testing.T) {
	tasks := sqlite.NewTaskRepository(sqlitetest.Memory(t).DB)
	now := time.Now().UTC()
	if err := tasks.Create(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p", Status: persistence.TaskStatusRunning, Payload: []byte(`{}`), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	r := newApplyRig(t, supervisedEnv, tasks)
	if rec := r.apply(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "JOB_RUNNING") {
		t.Fatalf("a running job: %d %s, want 409 JOB_RUNNING", rec.Code, rec.Body.String())
	}
	if r.restarts.Load() != 0 {
		t.Fatal("restarted while a job runs")
	}
	assertApplyReleased(t, r)
}

// An in-flight config write finishes before the daemon exits.
func TestSetupApply758_WaitsForAConfigWrite(t *testing.T) {
	r := newApplyRig(t, supervisedEnv, nil)
	unlock, err := configlock.LockAsWriter(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- r.apply() }()
	select {
	case <-r.called:
		t.Fatal("the daemon restarted while a config write held the lock")
	case <-time.After(200 * time.Millisecond):
	}
	unlock()
	if rec := <-done; rec.Code != http.StatusAccepted {
		t.Fatalf("apply after the write: %d", rec.Code)
	}
	<-r.called
	r.releaseApply()
}

// A daemon that does not hold the config-writer lease refuses, and the
// refusal releases the flag and the lock (code review fba4 F1).
func TestSetupApply758_NotTheConfigWriterRefuses(t *testing.T) {
	configlock.SetWriterCheck(func(context.Context) (bool, string) { return false, "another daemon holds the lease" })
	t.Cleanup(func() { configlock.SetWriterCheck(nil) })
	r := newApplyRig(t, supervisedEnv, nil)
	if rec := r.apply(); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "NOT_CONFIG_WRITER") {
		t.Fatalf("not the config writer: %d %s, want 409 NOT_CONFIG_WRITER", rec.Code, rec.Body.String())
	}
	if r.restarts.Load() != 0 {
		t.Fatal("restarted without the config-writer lease")
	}
	configlock.SetWriterCheck(nil)
	assertApplyReleased(t, r)
}

func TestSetupApply758_ConcurrentAppliesRestartOnce(t *testing.T) {
	r := newApplyRig(t, supervisedEnv, nil)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- r.apply().Code }()
	}
	wg.Wait()
	close(codes)
	got := map[int]int{}
	for c := range codes {
		got[c]++
	}
	if got[http.StatusAccepted] != 1 || got[http.StatusConflict] != 1 {
		t.Fatalf("two concurrent applies: %v, want one 202 and one 409", got)
	}
	<-r.called
	if r.restarts.Load() != 1 {
		t.Fatalf("restarted %d times", r.restarts.Load())
	}
	r.releaseApply()
}

func TestSetupApply758_NoRestartHookIsUnavailable(t *testing.T) {
	srv := NewServer()
	srv.setupEnv = supervisedEnv
	rec := httptest.NewRecorder()
	srv.SetupApply(rec, httptest.NewRequest(http.MethodPost, "/api/v1/setup/apply", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no restart hook wired: %d, want 503", rec.Code)
	}
	rec = httptest.NewRecorder()
	srv.SetupApply(rec, httptest.NewRequest(http.MethodGet, "/api/v1/setup/apply", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", rec.Code)
	}
}

// assertApplyReleased: a refused apply leaves the flag clear and the
// writer lock free.
func assertApplyReleased(t *testing.T, r *applyRig) {
	t.Helper()
	if r.srv.setupApplying.Load() {
		t.Error("a refused apply left the in-progress flag set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got := make(chan func(), 1)
	go func() {
		if u, err := configlock.LockAsWriter(ctx); err == nil {
			got <- u
		}
	}()
	select {
	case u := <-got:
		u()
	case <-ctx.Done():
		t.Error("a refused apply left the config writer lock held")
	}
}
