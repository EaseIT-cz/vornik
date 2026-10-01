package companionpush

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// fakeOutbox is an in-memory outbox: task and action rows with their
// current state and pushed_state.
type fakeOutbox struct {
	mu      sync.Mutex
	tasks   map[string]*persistence.TaskPushDue
	actions map[string]*persistence.ActionPushDue
	// onDue runs after each scan, before the loop sends: a transition landing
	// between scan and compare-and-set.
	onDue func()
}

func (f *fakeOutbox) DueTaskPushes(context.Context, int) ([]persistence.TaskPushDue, error) {
	f.mu.Lock()
	var out []persistence.TaskPushDue
	for _, d := range f.tasks {
		if d.PushedState == nil || *d.PushedState != d.State {
			cp := *d
			out = append(out, cp)
		}
	}
	hook := f.onDue
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	return out, nil
}

func (f *fakeOutbox) MarkTaskPushed(_ context.Context, id string, prev *string, state string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.tasks[id]
	if d == nil || !samePtr(d.PushedState, prev) {
		return false, nil
	}
	s := state
	d.PushedState = &s
	return true, nil
}

func (f *fakeOutbox) DueActionPushes(context.Context, int) ([]persistence.ActionPushDue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []persistence.ActionPushDue
	for _, d := range f.actions {
		if d.PushedState == nil || *d.PushedState != d.State {
			out = append(out, *d)
		}
	}
	return out, nil
}

func (f *fakeOutbox) MarkActionPushed(_ context.Context, id string, prev *string, state string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	d := f.actions[id]
	if d == nil || !samePtr(d.PushedState, prev) {
		return false, nil
	}
	s := state
	d.PushedState = &s
	return true, nil
}

func samePtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

type receiver struct {
	mu     sync.Mutex
	bodies []map[string]any
	auth   []string
	status int
}

func (r *receiver) handler(w http.ResponseWriter, req *http.Request) {
	b, _ := io.ReadAll(req.Body)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	r.mu.Lock()
	r.bodies = append(r.bodies, m)
	r.auth = append(r.auth, req.Header.Get("Authorization"))
	status := r.status
	r.mu.Unlock()
	if status == 0 {
		status = http.StatusNoContent
	}
	w.WriteHeader(status)
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.bodies)
}

func testPusher(t *testing.T, ob *fakeOutbox, allowed map[string][]netip.Prefix) *Pusher {
	t.Helper()
	p := New(Config{
		Outbox: ob,
		Allowed: func(projectID string) []netip.Prefix {
			return allowed[projectID]
		},
		Logger: zerolog.Nop(),
	})
	// httptest listens on loopback; stand it in for a LAN range the
	// project lists, so the loop's per-item allowlist is what decides.
	p.client.reach = func(a netip.Addr, al []netip.Prefix) bool {
		for _, pfx := range al {
			if pfx.Contains(a.Unmap()) {
				return true
			}
		}
		return false
	}
	return p
}

var loopAllowed = map[string][]netip.Prefix{"proj": {netip.MustParsePrefix("127.0.0.0/8")}}

func TestPass_TaskPushedOnceWithTokenAndClosedBody(t *testing.T) {
	rx := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{tasks: map[string]*persistence.TaskPushDue{
		"t1": {TaskID: "t1", ProjectID: "proj", State: "COMPLETED", URL: srv.URL, Token: "tok"},
	}}
	p := testPusher(t, ob, loopAllowed)
	p.Pass(context.Background())
	p.Pass(context.Background())
	if rx.count() != 1 {
		t.Fatalf("pushes = %d, want 1", rx.count())
	}
	if got := rx.bodies[0]; len(got) != 2 || got["task_id"] != "t1" || got["state"] != "COMPLETED" {
		t.Fatalf("body = %v, want exactly task_id and state", got)
	}
	if rx.auth[0] != "Bearer tok" {
		t.Fatalf("Authorization = %q", rx.auth[0])
	}
}

// review-20260930-5e5d F4: the allowlist is the task's project's, built per
// item; another project's task to the same address is refused.
func TestPass_AllowlistIsPerItemProject(t *testing.T) {
	rx := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{tasks: map[string]*persistence.TaskPushDue{
		"ok":    {TaskID: "ok", ProjectID: "proj", State: "FAILED", URL: srv.URL},
		"other": {TaskID: "other", ProjectID: "elsewhere", State: "FAILED", URL: srv.URL},
	}}
	p := testPusher(t, ob, loopAllowed)
	p.Pass(context.Background())
	if rx.count() != 1 || rx.bodies[0]["task_id"] != "ok" {
		t.Fatalf("delivered = %v, want only the allowed project's task", rx.bodies)
	}
	if ob.tasks["other"].PushedState != nil {
		t.Fatal("a refused push must not be marked delivered")
	}
}

func TestPass_ActionsCarryIdsAndFrontStateOnly(t *testing.T) {
	rx := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{actions: map[string]*persistence.ActionPushDue{
		"a1": {TaskID: "t1", ProjectID: "proj", ActionID: "a1", Action: "send_reply", State: "pending_approval", URL: srv.URL},
		"a2": {TaskID: "t2", ProjectID: "proj", ActionID: "a2", Action: "send_reply", State: "executed"}, // no config
	}}
	p := testPusher(t, ob, loopAllowed)
	p.Pass(context.Background())
	if rx.count() != 1 {
		t.Fatalf("pushes = %d, want 1", rx.count())
	}
	b := rx.bodies[0]
	if len(b) != 4 || b["task_id"] != "t1" || b["action_id"] != "a1" || b["action"] != "send_reply" || b["state"] != "pending_approval" {
		t.Fatalf("body = %v", b)
	}
	if ob.actions["a2"].PushedState == nil || *ob.actions["a2"].PushedState != "executed" {
		t.Fatal("an action without a config must be marked without sending")
	}
}

// review-20260930-5e5d F5: a transition landing between the scan and the
// compare-and-set is pushed on the next pass, and pushed_state ends at the
// latest state.
func TestPass_TransitionDuringAPassIsPushedNext(t *testing.T) {
	rx := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{tasks: map[string]*persistence.TaskPushDue{
		"t1": {TaskID: "t1", ProjectID: "proj", State: "FAILED", URL: srv.URL},
	}}
	once := sync.Once{}
	ob.onDue = func() {
		once.Do(func() {
			ob.mu.Lock()
			ob.tasks["t1"].State = "CANCELLED" // a retried task changed state after the scan
			ob.mu.Unlock()
		})
	}
	p := testPusher(t, ob, loopAllowed)
	p.Pass(context.Background())
	p.Pass(context.Background())
	if rx.count() != 2 || rx.bodies[0]["state"] != "FAILED" || rx.bodies[1]["state"] != "CANCELLED" {
		t.Fatalf("pushes = %v, want FAILED then CANCELLED", rx.bodies)
	}
	if got := *ob.tasks["t1"].PushedState; got != "CANCELLED" {
		t.Fatalf("pushed_state = %s", got)
	}
}

// review-20260930-5e5d F5: an item failing 10 passes is abandoned (counted,
// no more attempts); a new pusher (a restart) tries it again.
func TestPass_AbandonsAfterTenPassesAndARestartRetries(t *testing.T) {
	rx := &receiver{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{tasks: map[string]*persistence.TaskPushDue{
		"t1": {TaskID: "t1", ProjectID: "proj", State: "FAILED", URL: srv.URL},
	}}
	p := testPusher(t, ob, loopAllowed)
	var abandoned int
	p.cfg.OnResult = func(_, result string) {
		if result == "abandoned" {
			abandoned++
		}
	}
	for i := 0; i < 12; i++ {
		p.Pass(context.Background())
	}
	if abandoned != 1 {
		t.Fatalf("abandoned = %d, want 1", abandoned)
	}
	sent := rx.count()
	if sent != 20 { // 10 passes x 2 attempts
		t.Fatalf("POSTs = %d, want 20 (10 passes, two attempts each)", sent)
	}
	testPusher(t, ob, loopAllowed).Pass(context.Background())
	if rx.count() != sent+2 {
		t.Fatal("a restarted pusher must try an abandoned item again")
	}
}

// review-20260930-5e5d F5: the completion observer only kicks, and a kick
// never blocks, even when the loop is not draining.
func TestKickNeverBlocks(t *testing.T) {
	p := New(Config{Outbox: &fakeOutbox{}, Logger: zerolog.Nop()})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			p.NotifyTaskCompleted(context.Background(), &persistence.Task{ID: "t"}, true, "")
			p.Kick()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a kick blocked")
	}
	var nilP *Pusher
	nilP.Kick()
}

func TestRun_PassesOnKick(t *testing.T) {
	rx := &receiver{}
	srv := httptest.NewServer(http.HandlerFunc(rx.handler))
	defer srv.Close()
	ob := &fakeOutbox{tasks: map[string]*persistence.TaskPushDue{}}
	p := testPusher(t, ob, loopAllowed)
	p.cfg.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	ob.mu.Lock()
	ob.tasks["t1"] = &persistence.TaskPushDue{TaskID: "t1", ProjectID: "proj", State: "COMPLETED", URL: srv.URL}
	ob.mu.Unlock()
	p.Kick()
	deadline := time.Now().Add(3 * time.Second)
	for rx.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if rx.count() != 1 {
		t.Fatalf("pushes after kick = %d", rx.count())
	}
}

// review-20260930-dcfc: a pusher built without an outbox does nothing
// rather than crash on its first pass.
func TestPass_NoOutboxIsANoOp(_ *testing.T) {
	New(Config{Logger: zerolog.Nop()}).Pass(context.Background())
	var nilP *Pusher
	nilP.Pass(context.Background())
}
