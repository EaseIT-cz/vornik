package autonomy

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/registry"
)

// GitHub #903: the loop's first evaluation, both the immediate one and the
// delayed one that waits out the rest of the interval after a restart, never
// read the leader gate; only the ticker did. A replica that did not hold the
// autonomy lease evaluated (paying the LLM call in llm mode) and was then
// refused by the epoch fence. The integration tests in internal/service
// (autonomy_initial_eval_903_test.go) cover the immediate evaluation on a real
// container; this one covers the delayed path, which the container tests
// cannot reach without waiting out an interval. Lower because: the delayed
// first evaluation fires only after a recent row and a partial interval, which
// a real container test would have to sleep through.

type leaderBit bool

func (b leaderBit) IsLeader() bool { return bool(b) }

// runDelayedInitial903 runs one project loop whose last evaluation is just
// under an interval old, so the delayed first evaluation fires after ~50ms,
// and returns the evaluations it recorded (RATE_LIMITED when it ran).
func runDelayedInitial903(t *testing.T, opts ...Option) []*persistence.AutonomyEvaluation {
	t.Helper()
	evals := &listMockEvalRepo{recent: []*persistence.AutonomyEvaluation{{
		ProjectID: "p903",
		CreatedAt: time.Now().Add(-time.Hour + 50*time.Millisecond),
		Outcome:   persistence.AutonomyOutcomeNoAction,
	}}}
	m := New(nil, &registry.Registry{}, &mockTaskRepo{}, nil, append(opts, WithEvaluationRepository(evals))...)
	project := &registry.Project{ID: "p903", Autonomy: registry.ProjectAutonomy{
		Enabled: true, Goal: "test", PollInterval: "1h", MaxTasksPerHour: 1,
	}}
	// Fill the hour's cap, so an evaluation that runs records RATE_LIMITED
	// at its first gate and never reaches the (nil) chat client.
	m.checkRateLimit(project)
	m.recordTaskCreated(project.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	m.wg.Add(1)
	done := make(chan struct{})
	go func() { m.projectLoop(ctx, project); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("projectLoop did not exit after ctx cancel")
	}
	return evals.snapshot()
}

// Positive control: with no gate (a single process) the delayed first
// evaluation runs and records a row.
func TestProjectLoop903_DelayedInitialEvaluatesWithoutAGate(t *testing.T) {
	if got := runDelayedInitial903(t); len(got) == 0 {
		t.Fatal("no gate: the delayed first evaluation recorded nothing; the test cannot prove the negative")
	}
}

// The leader runs it too.
func TestProjectLoop903_DelayedInitialEvaluatesOnTheLeader(t *testing.T) {
	if got := runDelayedInitial903(t, WithLeaderGate(leaderBit(true))); len(got) == 0 {
		t.Fatal("leader: the delayed first evaluation recorded nothing")
	}
}

// The regression: a non-leader skips it.
func TestProjectLoop903_DelayedInitialSkippedOffTheLeader(t *testing.T) {
	if got := runDelayedInitial903(t, WithLeaderGate(leaderBit(false))); len(got) != 0 {
		t.Errorf("non-leader recorded %d first evaluation(s) (%s), want none", len(got), got[0].Outcome)
	}
}

// Review 6dd3 (#903): skipping the first evaluation off the leader must not
// strand a replica. A replica that does not lead at start (another holder's
// lease is live) takes the lease once it expires, through the elector's renew
// loop over a real SQLite lock store, and then evaluates on the ticker path.
func TestProjectLoop903_NonLeaderTakesExpiredLeaseThenEvaluates(t *testing.T) {
	db := sqlitetest.Memory(t)
	locks := sqlite.NewLeaderLockRepository(db.DB)
	ttl := leaderelection.MinTTL
	// The other holder's lease expires about 500ms from now.
	if ok, _, err := locks.Acquire(context.Background(), "autonomy_manager", "other-node:1:x", time.Now().Add(-ttl+500*time.Millisecond), ttl); err != nil || !ok {
		t.Fatalf("seed the other node's lease: ok=%v err=%v", ok, err)
	}
	elector := leaderelection.New(locks, "autonomy_manager", "this-node:1:y", ttl, zerolog.Nop())
	elector.BootstrapAcquire(context.Background())
	if elector.IsLeader() {
		t.Fatal("this replica took a live lease; the test proves nothing")
	}

	reg := registryWithProject(t, "p903", "autonomy:\n  enabled: true\n  goal: \"test\"\n  pollInterval: \"200ms\"\n  maxTasksPerHour: 1\n")
	project := reg.GetProject("p903")
	if project == nil {
		t.Fatal("the project did not load")
	}
	evals := &listMockEvalRepo{} // no prior row: the immediate first evaluation is due
	m := New(nil, reg, &mockTaskRepo{}, nil, WithLeaderGate(elector), WithEvaluationRepository(evals))
	// Fill the hour's cap, so an evaluation that runs records RATE_LIMITED
	// at its first gate and never reaches the (nil) chat client.
	m.checkRateLimit(project)
	m.recordTaskCreated(project.ID)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.wg.Add(1)
	go m.projectLoop(ctx, project)

	// While the other lease is live, nothing is evaluated: the first
	// evaluation is skipped and the ticks are gated.
	time.Sleep(300 * time.Millisecond)
	if got := evals.snapshot(); len(got) != 0 {
		t.Fatalf("evaluated %d time(s) while another node held the lease", len(got))
	}

	go elector.Run(ctx)
	deadline := time.Now().Add(8 * time.Second)
	for len(evals.snapshot()) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("no evaluation within 8s after the other lease expired (leader=%v)", elector.IsLeader())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !elector.IsLeader() {
		t.Error("evaluated without holding the lease")
	}
	cancel()
	m.wg.Wait()
}
