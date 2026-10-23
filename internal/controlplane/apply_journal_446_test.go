package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #446 (2026-10-21, security audit): control-plane crash recovery
// false-positived the apply's OWN writes as read-set drift.
//
// Both producers of journaled proposals (the config assistant and the agent
// admin verbs) put the change's own targets in its read set, keyed to the
// pre-image hash or ABSENT. Recovery re-ran the forward path's strict check,
// so a target already at its intended content failed its own entry and the
// row parked in DRIFT, blocking the project, where §5 says it rolls forward
// (APPLYING, VERIFYING) or finishes the revert (REVERTING). Every earlier
// recovery test seeded an empty read set or one naming a file the bundle does
// not write; these seed the producers' shape (journal design §14).

// producerReadSet is the read set a producer files for bundleOps: the
// replace keyed to config.yaml's pre-image hash, each create ABSENT, plus
// any extra dependencies.
func producerReadSet(extra map[string]string) string {
	rs := map[string]string{
		"config.yaml":     hashBytes([]byte(oldContent)),
		"projects/a.yaml": persistence.JournalReadSetAbsent,
		"projects/b.yaml": persistence.JournalReadSetAbsent,
	}
	for k, v := range extra {
		rs[k] = v
	}
	b, _ := json.Marshal(map[string]any{"read_set": rs})
	return string(b)
}

// crashWithProducerReadSet seeds a producer-shaped proposal and crashes the
// apply at stage.
func crashWithProducerReadSet(t *testing.T, stage string, extra map[string]string) (*journalEnv, string) {
	t.Helper()
	env := newJournalEnv(t)
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\n---\n")
	id := env.seed(t, producerReadSet(extra))
	env.crashAt(stage, true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash at %s, got %v", stage, err)
	}
	env.e.crashAfter = nil
	return env, id
}

func assertNotBlocked(t *testing.T, env *journalEnv) {
	t.Helper()
	blocked, err := env.e.BlockedProjects(context.Background())
	if err != nil {
		t.Fatalf("BlockedProjects: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("projects %v blocked after a recovery that had nothing to decide", blocked)
	}
}

func assertIntended(t *testing.T, env *journalEnv) {
	t.Helper()
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Errorf("config.yaml = %q, want the intended content", got)
	}
	if got := readFile(t, env.path("projects/a.yaml")); got != jaContentA {
		t.Errorf("projects/a.yaml = %q", got)
	}
	if got := readFile(t, env.path("projects/b.yaml")); got != jaContentB {
		t.Errorf("projects/b.yaml = %q", got)
	}
}

// The forward cases: a crash after write #1 rolls forward to APPLIED.
func TestReconcile446_ProducerReadSetRollsForward(t *testing.T) {
	cases := []struct {
		name  string
		stage string
		// raced lands the first rename by hand after a crash at PREPARED (§5:
		// "a rename raced the crash").
		raced bool
	}{
		{name: "APPLYING after the first rename", stage: "write:projects/a.yaml"},
		{name: "APPLYING after the second rename", stage: "progress:projects/b.yaml"},
		{name: "VERIFYING, every rename landed", stage: "verifying"},
		{name: "PREPARED with a raced rename", stage: "prepared", raced: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, id := crashWithProducerReadSet(t, tc.stage, nil)
			if tc.raced {
				env.write(t, "projects/a.yaml", jaContentA)
			}
			if err := env.e.Reconcile(context.Background()); err != nil {
				t.Fatalf("recovery of the apply's own writes must roll forward, Reconcile = %v", err)
			}
			if j := env.latestJournal(t, id); j.State != persistence.JournalStateApplied {
				t.Fatalf("journal = %s (failure %q), want APPLIED", j.State, j.Failure)
			}
			if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApplied {
				t.Fatalf("proposal = %s, want APPLIED", st)
			}
			assertIntended(t, env)
			assertNotBlocked(t, env)
		})
	}
}

// The revert case: reload rejected, the revert crashed after restoring one
// target (reverse order: config.yaml first), so the others are still at
// intended content. Recovery finishes the revert; it does not park.
func TestReconcile446_ProducerReadSetFinishesTheRevert(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, producerReadSet(nil))
	env.e.Reload = func() error { return errors.New("bad yaml") }
	env.crashAt("reverting", true, func() {
		// restoreOne runs in reverse op order after this stage; land the
		// first restore by hand so the row is half-reverted.
		env.write(t, "config.yaml", oldContent)
	})
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash at reverting, got %v", err)
	}
	env.e.crashAfter = nil
	env.e.Reload = func() error { return nil }
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateReverting {
		t.Fatalf("fixture: journal = %s, want REVERTING", j.State)
	}
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("a half-done revert must finish, Reconcile = %v", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateReverted {
		t.Fatalf("journal = %s (failure %q), want REVERTED", j.State, j.Failure)
	}
	env.assertPreImages(t)
	assertNotBlocked(t, env)
}

// The allowance is for the row's own intended bytes only: a target
// hand-edited to anything else still parks, and the file is left alone.
func TestReconcile446_HandEditedTargetStillParks(t *testing.T) {
	env, id := crashWithProducerReadSet(t, "verifying", nil)
	edited := "server:\n  address: :7070\n"
	env.write(t, "config.yaml", edited)
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateDrift || !strings.Contains(j.Failure, "config.yaml") {
		t.Fatalf("journal = %s %q, want DRIFT naming config.yaml", j.State, j.Failure)
	}
	if got := readFile(t, env.path("config.yaml")); got != edited {
		t.Fatalf("the hand edit was overwritten: %q", got)
	}
	if st := env.proposalStatus(t, id); st == persistence.ProposalStatusApplied {
		t.Fatal("ledger APPLIED over a hand-edited target")
	}
}

// A target at intended content does not excuse a non-target dependency that
// moved: the row still parks, naming the dependency.
func TestReconcile446_MovedDependencyBesideOwnWritesStillParks(t *testing.T) {
	dep := hashBytes([]byte("---\nswarmId: dev\n---\n"))
	env, id := crashWithProducerReadSet(t, "verifying", map[string]string{"swarms/dev.md": dep})
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\nleadRole: reviewer\n---\n")
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateDrift || !strings.Contains(j.Failure, "swarms/dev.md") {
		t.Fatalf("journal = %s %q, want DRIFT naming swarms/dev.md", j.State, j.Failure)
	}
}

// A delete op's intended state is absence: a VERIFYING row whose delete
// landed rolls forward, with the deleted file's pre-image in the read set.
func TestReconcile446_LandedDeleteRollsForward(t *testing.T) {
	env := newJournalEnv(t)
	env.write(t, "projects/old.yaml", "id: old\n")
	ops := []applyFileOp{{Op: applyOpDelete, Path: "projects/old.yaml"}}
	raw, _ := json.Marshal(ops)
	ev, _ := json.Marshal(map[string]any{"read_set": map[string]string{"projects/old.yaml": hashBytes([]byte("id: old\n"))}})
	p := &persistence.ControlPlaneProposal{
		ID: persistence.GenerateID("cpp"), ProjectID: "digest",
		Kind: persistence.ProposalKindScaffold, BlastRadius: persistence.ProposalScopeProject,
		Title: "delete old", ApplyOps: string(raw), Evidence: string(ev),
		Status: persistence.ProposalStatusDraft, ProposedBy: "agent:test",
	}
	ctx := context.Background()
	if err := env.repo.Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := env.repo.SetStatus(ctx, p.ID, persistence.ProposalStatusApproved, "vadim"); err != nil {
		t.Fatal(err)
	}
	env.crashAt("verifying", true, nil)
	if err := env.e.Apply(ctx, p.ID, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	env.e.crashAfter = nil
	if err := env.e.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if j := env.latestJournal(t, p.ID); j.State != persistence.JournalStateApplied {
		t.Fatalf("journal = %s (failure %q), want APPLIED", j.State, j.Failure)
	}
	if env.exists("projects/old.yaml") {
		t.Fatal("the deleted file came back")
	}
}

// Matching is on the RESOLVED path (§14.2): a read-set key spelled
// differently from the op's path but naming the same file is that op's
// target, and gets the same allowance.
func TestReconcile446_DifferentlySpelledTargetKeyIsTheSameFile(t *testing.T) {
	env := newJournalEnv(t)
	rs := map[string]string{
		"./config.yaml":   hashBytes([]byte(oldContent)),
		"projects/a.yaml": persistence.JournalReadSetAbsent,
		"projects/b.yaml": persistence.JournalReadSetAbsent,
	}
	b, _ := json.Marshal(map[string]any{"read_set": rs})
	id := env.seed(t, string(b))
	env.crashAt("verifying", true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash, got %v", err)
	}
	env.e.crashAfter = nil
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateApplied {
		t.Fatalf("journal = %s (failure %q), want APPLIED", j.State, j.Failure)
	}
}

// The forward pass keeps the strict check: before write #1 nothing has been
// written, so a target already holding the intended bytes is not this
// proposal's base and the apply is refused as stale.
func TestApply446_ForwardPassStaysStrict(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, producerReadSet(nil))
	env.write(t, "config.yaml", newContent)
	err := env.e.Apply(context.Background(), id, "vadim", false)
	if !errors.Is(err, ErrStaleBase) {
		t.Fatalf("Apply = %v, want ErrStaleBase", err)
	}
}
