package service

import (
	"context"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
)

// GitHub #903: initAutonomy started the per-project loops inside NewContainer,
// and each loop ran its due-at-restart evaluation at once, before
// Container.Run bootstrapped the autonomy elector. Every SQLite and Postgres
// install wires a lock store, so the gate was a real elector that did not lead
// yet: the evaluation reached createAutonomousTask and the epoch fence refused
// it (LEADER_FENCED, "superseded by a newer leader epoch"). In llm mode the
// LLM call is made before the fence, so the evaluation was paid for and
// created nothing. On a replica that does not hold the lease the initial
// evaluation ran at all, because it never read IsLeader. Found at:
// integration (a real container on real SQLite, while fixing #894); these
// tests count autonomy_evaluations rows on the same kind of container.

// firstEvaluation903 waits up to wait for the project's first evaluation row
// and returns it, or nil. It picks the earliest row by created_at rather than
// trusting List's order, so a later tick never stands in for the first one.
func firstEvaluation903(t *testing.T, c *Container, wait time.Duration) *persistence.AutonomyEvaluation {
	t.Helper()
	pid := "auto894"
	deadline := time.Now().Add(wait)
	for {
		rows, err := c.repos.AutonomyEvaluations.List(context.Background(), persistence.AutonomyEvaluationFilter{ProjectID: &pid, PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 {
			first := rows[0]
			for _, r := range rows[1:] {
				if r.CreatedAt.Before(first.CreatedAt) {
					first = r
				}
			}
			return first
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The regression and the positive control in one: a node that runs workers
// over a lock store (the default single-node install, profile "all", and a
// cluster "worker") still evaluates at start, and that evaluation passes the
// leader fence. Before the fix the row was LEADER_FENCED.
func TestAutonomy903_InitialEvaluationPassesTheLeaderFence(t *testing.T) {
	for _, profile := range []string{"all", "worker"} {
		t.Run(profile, func(t *testing.T) {
			c := container894(t, profile)
			if c.repos.LeaderLocks == nil || c.autonomyElector == nil || c.autonomyElector.Standby() {
				t.Fatal("no real autonomy elector over a lock store; the test proves nothing")
			}
			ev := firstEvaluation903(t, c, 5*time.Second)
			if ev == nil {
				t.Fatal("no initial autonomy evaluation recorded within 5s")
			}
			if ev.Outcome == persistence.AutonomyOutcomeLeaderFenced {
				t.Fatalf("initial evaluation was fenced (%s); it ran before the elector bootstrapped", ev.Reason)
			}
			// The project is observe-mode cron: a passed tick is WOULD_START.
			if ev.Outcome != persistence.AutonomyOutcomeWouldStart {
				t.Errorf("initial evaluation outcome = %s (%s), want %s", ev.Outcome, ev.Reason, persistence.AutonomyOutcomeWouldStart)
			}
		})
	}
}

// A replica that does not hold the autonomy lease (another node does) runs no
// initial evaluation: no LLM call, no row. Before the fix it evaluated and was
// fenced.
func TestAutonomy903_NonLeaderReplicaSkipsInitialEvaluation(t *testing.T) {
	db := sqlitetest.Memory(t)
	if ok, _, err := sqlite.NewLeaderLockRepository(db.DB).Acquire(context.Background(), "autonomy_manager", "other-node:1:x", time.Now(), time.Hour); err != nil || !ok {
		t.Fatalf("seed the other node's lease: ok=%v err=%v", ok, err)
	}
	opener := WithDatabaseOpener(func(_ context.Context, _ config.DatabaseConfig) (*storage.Backend, error) {
		be := storage.NewSQLiteBackend(db)
		be.MigratedAtOpen = true
		return be, nil
	})
	c := container894(t, "worker", opener)
	if c.autonomyManager == nil || c.autonomyManager.ActiveLoops() != 1 {
		t.Fatal("the autonomy loop did not start; the test proves nothing")
	}
	if c.autonomyElector.IsLeader() {
		t.Fatal("this replica took a lease another node holds; the test proves nothing")
	}
	// The positive control above records within milliseconds.
	if ev := firstEvaluation903(t, c, time.Second); ev != nil {
		t.Errorf("non-leader replica recorded an initial evaluation (%s: %s), want none", ev.Outcome, ev.Reason)
	}
}
