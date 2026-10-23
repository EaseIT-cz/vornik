package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// These run through the PRODUCTION constructor (newProposalApplier), the
// production lease lifecycle (StartConfigWriterLease), the real SQLite journal
// and the production startup recovery (ReconcileConfigApplyJournal): the level
// at which an operator observes both defects (journal design §14.4).

func leasedWorkspaceContainer(t *testing.T) (*Container, string) {
	t.Helper()
	c := newProposalApplierContainer(t)
	ws := t.TempDir()
	c.Config = &config.Config{}
	c.Config.Runtime.ProjectWorkspacePath = ws
	if err := c.StartConfigWriterLease(context.Background()); err != nil {
		t.Fatalf("StartConfigWriterLease: %v", err)
	}
	t.Cleanup(func() { _ = c.StopConfigWriterLease(context.Background()) })
	return c, filepath.Join(ws, wsLeaseTestProject, ".autonomy", "PROJECT_CONTEXT.md")
}

// GitHub #446 (2026-10-21): crash recovery false-positived the apply's own
// writes as read-set drift. The proposal here has the agent-admin shape — its
// create target is in its read set as ABSENT — and the daemon dies inside
// Reload (runtime.Goexit: the apply goroutine stops there, deferred unlocks
// run, nothing else does), leaving the row VERIFYING with the file written:
// the shape of the two production rows in journal design §11.1. Before the
// fix the restart parked the row in DRIFT and paused the project.
func TestReconcile446_AgentShapedCrashDuringReloadRollsForward(t *testing.T) {
	c, target := leasedWorkspaceContainer(t)
	ctx := context.Background()
	p := approvedWorkspaceProposal(t, c)

	engine := c.newProposalApplier()
	if engine == nil {
		t.Fatal("newProposalApplier returned nil")
	}
	engine.Reload = func() error { runtime.Goexit(); return nil }
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = engine.Apply(ctx, p.ID, "operator", false)
	}()
	<-done

	row, err := c.repos.ApplyJournal.OpenForProposal(ctx, p.ID)
	if err != nil {
		t.Fatalf("fixture: no open journal row after the crash: %v", err)
	}
	if row.State != persistence.JournalStateVerifying {
		t.Fatalf("fixture: journal = %s, want VERIFYING", row.State)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("fixture: the write did not land: %v", err)
	}

	// The restart.
	if err := c.ReconcileConfigApplyJournal(ctx); err != nil {
		t.Fatalf("startup recovery of the apply's own write must roll forward: %v", err)
	}
	after, err := c.repos.ApplyJournal.Get(ctx, row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.State != persistence.JournalStateApplied {
		t.Fatalf("journal = %s (failure %q), want APPLIED", after.State, after.Failure)
	}
	got, err := c.repos.Proposals.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal = %s, want APPLIED", got.Status)
	}
	if blocked, reason := c.configApplyBlockedGate()(wsLeaseTestProject); blocked {
		t.Fatalf("project paused after a recovery that had nothing to decide: %s", reason)
	}
}

// applyUnderLease applies the workspace proposal normally and returns it.
func applyUnderLease(t *testing.T, c *Container) *persistence.ControlPlaneProposal {
	t.Helper()
	p := approvedWorkspaceProposal(t, c)
	if err := c.newProposalApplier().Apply(context.Background(), p.ID, "operator", false); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return p
}

// GitHub #447 (2026-10-21): legacy Rollback was not fenced by the
// config-writer lease. Another daemon takes config_writer; this daemon then
// rolls back through engine.Rollback — the very function the canary guard
// is wired with (startCanaryGuardWorker), under its own different lease, and
// the one the operator API and UI call. Before the fix the file was removed
// and the proposal recorded ROLLED_BACK over the writer's tree.
func TestRollback447_DaemonWithoutTheWriterLeaseCannotRollBack(t *testing.T) {
	c, target := leasedWorkspaceContainer(t)
	ctx := context.Background()
	p := applyUnderLease(t, c)

	// Daemon A takes config_writer over after this daemon's lease lapsed
	// (a takeover of an expired row, which bumps the epoch; the SQLite
	// Release is a no-op, so expiry is the takeover path on this driver).
	later := time.Now().Add(2 * leaderelection.DefaultTTL)
	ok, _, aerr := c.repos.LeaderLocks.Acquire(ctx, configWriterLease, "daemon-a", later, leaderelection.DefaultTTL)
	if aerr != nil || !ok {
		t.Fatalf("fixture: daemon-a did not take config_writer: ok=%v err=%v", ok, aerr)
	}

	err := c.newProposalApplier().Rollback(ctx, p.ID)
	if !errors.Is(err, controlplane.ErrWriterFenced) {
		t.Fatalf("Rollback = %v, want ErrWriterFenced", err)
	}
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "# context\n" {
		t.Fatalf("a daemon without the writer lease changed the writer's tree: %q %v", got, rerr)
	}
	got, gerr := c.repos.Proposals.GetByID(ctx, p.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != persistence.ProposalStatusApplied {
		t.Fatalf("proposal = %s, want APPLIED", got.Status)
	}
}

// The counterpart: the config writer itself still rolls back.
func TestRollback447_WriterStillRollsBack(t *testing.T) {
	c, target := leasedWorkspaceContainer(t)
	ctx := context.Background()
	p := applyUnderLease(t, c)
	if err := c.newProposalApplier().Rollback(ctx, p.ID); err != nil {
		t.Fatalf("the config writer must still roll back: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("the created file should be gone after rollback, stat err = %v", err)
	}
	got, err := c.repos.Proposals.GetByID(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != persistence.ProposalStatusRolledBack {
		t.Fatalf("proposal = %s, want ROLLED_BACK", got.Status)
	}
}
