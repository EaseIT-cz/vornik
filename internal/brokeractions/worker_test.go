package brokeractions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/mcp"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// Broker write-actions design §5.4: the daemon executes an approved action
// exactly once, with the stored bytes, after re-checking the gates, and
// records what happened. Nothing retries a write.

type fakeRepo struct {
	persistence.BrokerActionRepository
	mu        sync.Mutex
	rows      map[string]*persistence.BrokerAction
	finished  []string
	sweeps    []string
	getErr    error
	finishErr error
	expired   int64
}

func (f *fakeRepo) ClaimForExecution(_ context.Context, id string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.rows[id]
	if a == nil || a.Status != persistence.BrokerActionApproved {
		return false, nil
	}
	a.Status = persistence.BrokerActionExecuting
	return true, nil
}

func (f *fakeRepo) Get(_ context.Context, id string) (*persistence.BrokerAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	a := f.rows[id]
	if a == nil {
		return nil, persistence.ErrNotFound
	}
	cp := *a
	return &cp, nil
}

func (f *fakeRepo) Finish(_ context.Context, id, status, class string, outcome []byte, _ time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.finishErr != nil {
		return f.finishErr
	}
	a := f.rows[id]
	if a == nil || a.Status != persistence.BrokerActionExecuting {
		return persistence.ErrBrokerActionNoTransition
	}
	a.Status, a.OutcomeClass, a.OutcomeJSON = status, class, outcome
	f.finished = append(f.finished, id)
	return nil
}

func (f *fakeRepo) ListByStatus(_ context.Context, _, status string, _ int) ([]*persistence.BrokerAction, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*persistence.BrokerAction
	for _, a := range f.rows {
		if a.Status == status {
			cp := *a
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (f *fakeRepo) PromoteStagedOfCompletedTasks(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, "promote")
	return 0, nil
}
func (f *fakeRepo) ExpireDue(context.Context, time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, "expire")
	return f.expired, nil
}
func (f *fakeRepo) DiscardStagedOrphans(context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sweeps = append(f.sweeps, "discard")
	return 0, nil
}
func (f *fakeRepo) CountStuck(context.Context, time.Time, time.Duration, time.Duration) ([]persistence.BrokerActionStuck, error) {
	return []persistence.BrokerActionStuck{{ProjectID: "p", Status: "unknown", Count: 2}}, nil
}

type fakeCaller struct {
	mu    sync.Mutex
	calls []string
	text  string
	isErr bool
	err   error
	block bool
	panic bool
}

func (c *fakeCaller) CallToolOnce(ctx context.Context, project, tool, args string) (string, bool, error) {
	c.mu.Lock()
	c.calls = append(c.calls, project+"|"+tool+"|"+args)
	c.mu.Unlock()
	if c.panic {
		panic("server library bug")
	}
	if c.block {
		<-ctx.Done()
		return "", false, ctx.Err()
	}
	return c.text, c.isErr, c.err
}

func approvedRow(t *testing.T, args string) *persistence.BrokerAction {
	t.Helper()
	sum, err := approval.CanonicalSHA256([]byte(args))
	if err != nil {
		t.Fatal(err)
	}
	return &persistence.BrokerAction{ActionID: "act-1", ProjectID: "broker-p", Tool: "mcp__w__send",
		ArgsJSON: []byte(args), ArgsSHA256: sum, Status: persistence.BrokerActionApproved}
}

func newWorker(repo *fakeRepo, caller *fakeCaller) *Worker {
	return New(Config{
		Repo: repo, Caller: caller,
		WritesOn:     func() bool { return true },
		ToolDeclared: func(string, string) error { return nil },
		Timeout:      time.Second,
		Redact:       func(b []byte) []byte { return []byte(strings.ReplaceAll(string(b), "secret-token", "[REDACTED]")) },
		Logger:       zerolog.Nop(),
	})
}

func TestExecute_CallsOnceWithTheStoredBytes(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{"to":"jana@example.com"}`)}}
	caller := &fakeCaller{text: "queued id=1 secret-token"}
	w := newWorker(repo, caller)
	w.Execute(context.Background(), "act-1")
	w.Execute(context.Background(), "act-1") // already executed: no second call

	if len(caller.calls) != 1 || caller.calls[0] != `broker-p|mcp__w__send|{"to":"jana@example.com"}` {
		t.Fatalf("calls = %v", caller.calls)
	}
	got := repo.rows["act-1"]
	if got.Status != persistence.BrokerActionExecuted || got.OutcomeClass != persistence.BrokerOutcomeOK {
		t.Fatalf("row = %+v", got)
	}
	if strings.Contains(string(got.OutcomeJSON), "secret-token") {
		t.Fatal("the outcome must be redacted before it is stored")
	}
}

func TestExecute_Outcomes(t *testing.T) {
	cases := []struct {
		name   string
		caller *fakeCaller
		status string
		class  string
	}{
		{"tool error", &fakeCaller{text: "550 rejected", isErr: true}, persistence.BrokerActionFailed, persistence.BrokerOutcomeToolError},
		{"not sent", &fakeCaller{err: fmt.Errorf("%w: server not connected", mcp.ErrNotSent)}, persistence.BrokerActionFailed, persistence.BrokerOutcomePreSendError},
		{"timeout", &fakeCaller{block: true}, persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout},
		{"transport", &fakeCaller{err: errors.New("connection reset")}, persistence.BrokerActionUnknown, persistence.BrokerOutcomeTransportError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
			w := newWorker(repo, tc.caller)
			w.cfg.Timeout = 20 * time.Millisecond
			w.Execute(context.Background(), "act-1")
			got := repo.rows["act-1"]
			if got.Status != tc.status || got.OutcomeClass != tc.class {
				t.Fatalf("got %s/%s, want %s/%s", got.Status, got.OutcomeClass, tc.status, tc.class)
			}
		})
	}
}

// §5.4 step 2: the gates are re-checked after the claim, immediately before
// the call. A failed re-check sends nothing and records pre_send_error.
func TestExecute_RecheckRefusesWithoutCalling(t *testing.T) {
	cases := map[string]func(w *Worker, a *persistence.BrokerAction){
		"writes turned off": func(w *Worker, _ *persistence.BrokerAction) { w.cfg.WritesOn = func() bool { return false } },
		"server no longer write": func(w *Worker, _ *persistence.BrokerAction) {
			w.cfg.ToolDeclared = func(string, string) error { return errors.New("not broker_write") }
		},
		"args changed after hash": func(_ *Worker, a *persistence.BrokerAction) { a.ArgsJSON = []byte(`{"to":"attacker@evil.test"}`) },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			row := approvedRow(t, `{"to":"jana@example.com"}`)
			repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": row}}
			caller := &fakeCaller{}
			w := newWorker(repo, caller)
			mutate(w, row)
			w.Execute(context.Background(), "act-1")
			if len(caller.calls) != 0 {
				t.Fatalf("nothing may be sent when a re-check fails; calls = %v", caller.calls)
			}
			if got := repo.rows["act-1"]; got.Status != persistence.BrokerActionFailed || got.OutcomeClass != persistence.BrokerOutcomePreSendError {
				t.Fatalf("row = %s/%s", got.Status, got.OutcomeClass)
			}
		})
	}
}

func TestSweep_RunsEveryMaintenanceStepAndFeedsTheGauge(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	caller := &fakeCaller{}
	w := newWorker(repo, caller)
	var gauged []persistence.BrokerActionStuck
	w.cfg.Gauge = func(s []persistence.BrokerActionStuck) { gauged = s }
	w.Sweep(context.Background())
	if strings.Join(repo.sweeps, ",") != "expire,discard" {
		t.Fatalf("sweeps = %v", repo.sweeps)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("an approved row left behind (worker restarted) must be executed by the sweep; calls = %v", caller.calls)
	}
	if len(gauged) != 1 || gauged[0].Count != 2 {
		t.Fatalf("gauge = %+v", gauged)
	}
}

func TestRun_PromotesOnStartAndExecutesKicks(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	caller := &fakeCaller{}
	w := newWorker(repo, caller)
	w.cfg.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	w.Kick("act-1")
	deadline := time.After(2 * time.Second)
	for {
		caller.mu.Lock()
		n := len(caller.calls)
		caller.mu.Unlock()
		if n == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("a kicked action was not executed")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
	if len(repo.sweeps) == 0 || repo.sweeps[0] != "promote" {
		t.Fatalf("Run must promote staged rows of completed tasks first; sweeps = %v", repo.sweeps)
	}
	var nilWorker *Worker
	nilWorker.Kick("x") // nil-safe
}

// TestFakeRepo_HonoursTheMissContract keeps the double honest: the worker
// relies on Get's miss being ErrNotFound, as on both real drivers.
func TestFakeRepo_HonoursTheMissContract(t *testing.T) {
	f := &fakeRepo{rows: map[string]*persistence.BrokerAction{}}
	repotest.AssertMissRepo(t, "BrokerActionRepository.Get", f.Get)
}

// A zero Timeout would expire every call before it is sent and record each
// action unknown; New defaults it.
func TestNew_DefaultsANonPositiveTimeout(t *testing.T) {
	if w := New(Config{}); w.cfg.Timeout != DefaultActionTimeout {
		t.Fatalf("Timeout = %v, want %v", w.cfg.Timeout, DefaultActionTimeout)
	}
}

// review-20260930-bd00 F3: a panic in one action's call must not end the
// worker. The claimed row stays executing (nothing is known about the call),
// which the stuck gauge and doctor surface, and the next action still runs.
func TestExecute_APanicLeavesTheRowExecutingAndTheWorkerAlive(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	w := newWorker(repo, &fakeCaller{panic: true})
	w.Execute(context.Background(), "act-1")
	if got := repo.rows["act-1"].Status; got != persistence.BrokerActionExecuting {
		t.Fatalf("status after a panicking call = %s, want executing", got)
	}
	next := approvedRow(t, `{"n":2}`)
	next.ActionID = "act-2"
	repo.rows["act-2"] = next
	w.cfg.Caller = &fakeCaller{text: "ok"}
	w.Execute(context.Background(), "act-2")
	if got := repo.rows["act-2"].Status; got != persistence.BrokerActionExecuted {
		t.Fatalf("the worker did not survive the panic: act-2 = %s", got)
	}
}

// review-20260930-bd00 minor: a claimed row that cannot be read is never
// called and never finished; it stays executing for the stuck gauge.
func TestExecute_ClaimedButUnreadableIsNotCalled(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}, getErr: errors.New("db gone")}
	caller := &fakeCaller{}
	newWorker(repo, caller).Execute(context.Background(), "act-1")
	if len(caller.calls) != 0 || len(repo.finished) != 0 || repo.rows["act-1"].Status != persistence.BrokerActionExecuting {
		t.Fatalf("calls=%v finished=%v status=%s", caller.calls, repo.finished, repo.rows["act-1"].Status)
	}
}

// review-20260930-bd00 F4: an outcome that could not be recorded is reported,
// so "finished" counts are not silently short.
func TestExecute_FinishFailureIsReported(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}, finishErr: errors.New("db gone")}
	w := newWorker(repo, &fakeCaller{text: "ok"})
	var reported []string
	w.cfg.OnFinishError = func(a *persistence.BrokerAction, status string) { reported = append(reported, a.ActionID+"|"+status) }
	w.Execute(context.Background(), "act-1")
	if len(reported) != 1 || reported[0] != "act-1|executed" {
		t.Fatalf("finish failures reported = %v", reported)
	}
}

// review-20260930-bd00 F5: with no redactor, every stored tool response is
// counted as unscanned, so the degraded state is visible, not only logged.
func TestExecute_UnscannedOutcomeIsCounted(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	w := New(Config{
		Repo: repo, Caller: &fakeCaller{text: "ok"},
		WritesOn: func() bool { return true }, Timeout: time.Second, Logger: zerolog.Nop(),
	})
	n := 0
	w.cfg.OnUnscanned = func() { n++ }
	w.Execute(context.Background(), "act-1")
	if n != 1 {
		t.Fatalf("unscanned outcomes counted = %d, want 1", n)
	}
}

// review-20260930-bd00 F10: the cap never splits a character.
func TestCapString_KeepsValidUTF8(t *testing.T) {
	s := strings.Repeat("é", 10) // 2 bytes each
	got := capString(s, 5)
	if !utf8.ValidString(got) || got != "éé" {
		t.Fatalf("capString = %q", got)
	}
	if capString("abc", 5) != "abc" {
		t.Fatal("short string altered")
	}
}

// review-20260930-bd00 minor: Run sweeps once at start, so approved rows left
// by a restart do not wait a full interval.
func TestRun_SweepsOnStart(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	caller := &fakeCaller{text: "ok"}
	w := newWorker(repo, caller)
	w.cfg.Interval = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		caller.mu.Lock()
		n := len(caller.calls)
		caller.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the approved row was not executed at start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// Design §7a: the worker tells the push outbox when it changed an action —
// after each terminal write and after an expiry sweep that expired rows —
// so those transitions are pushed at once rather than on the next pass.
func TestWorker_OnChangeAfterFinishAndExpiry(t *testing.T) {
	repo := &fakeRepo{rows: map[string]*persistence.BrokerAction{"act-1": approvedRow(t, `{}`)}}
	w := newWorker(repo, &fakeCaller{text: "ok"})
	changes := 0
	w.cfg.OnChange = func() { changes++ }
	w.Execute(context.Background(), "act-1")
	if changes != 1 {
		t.Fatalf("changes after a terminal write = %d, want 1", changes)
	}
	repo.expired = 2
	w.Sweep(context.Background())
	if changes != 2 {
		t.Fatalf("changes after an expiry sweep = %d, want 2", changes)
	}
	repo.expired = 0
	w.Sweep(context.Background())
	if changes != 2 {
		t.Fatalf("a sweep that expired nothing must not signal, changes = %d", changes)
	}
}
