package controlplane

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #102 (2026-10-08): reconcile logged "journal →DRIFT: invalid
// transition" at every start. §10.3 (CA-21) parked a row whose read set moved
// with markDrift, which is a REVERTING → DRIFT compare-and-set, but the row
// was PREPARED, APPLYING or VERIFYING (production: two VERIFYING rows), so
// the transition failed, the row stayed open, and the next start repeated
// it. The CA-21 test only asserted "an error and not APPLIED", which the
// invalid-transition error satisfied; these assert the state reached.

// readSetCase seeds a bundle whose read set names swarms/dev.md, crashes at
// stage, then edits the dependency, as an operator would while the daemon
// was down.
func readSetCase(t *testing.T, stage string) (*journalEnv, string) {
	t.Helper()
	env := newJournalEnv(t)
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\n---\n")
	dep := hashBytes([]byte("---\nswarmId: dev\n---\n"))
	id := env.seed(t, `{"read_set":{"swarms/dev.md":"`+dep+`"}}`)
	env.crashAt(stage, true, nil)
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, errCrashInjected) {
		t.Fatalf("expected the injected crash at %s, got %v", stage, err)
	}
	env.e.crashAfter = nil
	env.write(t, "swarms/dev.md", "---\nswarmId: dev\nleadRole: reviewer\n---\n")
	return env, id
}

func logTo(env *journalEnv) *bytes.Buffer {
	var buf bytes.Buffer
	env.e.Logger = zerolog.New(&buf)
	return &buf
}

func TestReconcile_ReadSetDriftOnPreparedRowFails(t *testing.T) {
	env, id := readSetCase(t, "prepared")
	logs := logTo(env)
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("a PREPARED row with nothing written needs no person; Reconcile = %v", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateFailed || j.TerminalAt == nil {
		t.Fatalf("journal = %s (terminal %v), want FAILED", j.State, j.TerminalAt)
	}
	if !strings.Contains(j.Failure, "swarms/dev.md") {
		t.Errorf("failure %q does not name the drifted dependency", j.Failure)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusRejected {
		t.Errorf("proposal = %s, want REJECTED (auto-retired as stale)", st)
	}
	env.assertPreImages(t)
	if strings.Contains(logs.String(), `"level":"error"`) {
		t.Errorf("an ERROR was logged for a row that needed nobody: %s", logs.String())
	}
}

// A PREPARED row whose target already holds the intended content (a rename
// raced the crash) has written something, so it is not FAILED: it parks.
func TestReconcile_ReadSetDriftOnPreparedRowWithAWriteParksInDrift(t *testing.T) {
	env, id := readSetCase(t, "prepared")
	env.write(t, "projects/a.yaml", jaContentA)
	err := env.e.Reconcile(context.Background())
	if !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateDrift {
		t.Fatalf("journal = %s, want DRIFT", j.State)
	}
}

func assertParkedInDrift(t *testing.T, env *journalEnv, id string, err error) {
	t.Helper()
	if !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	j := env.latestJournal(t, id)
	if j.State != persistence.JournalStateDrift {
		t.Fatalf("journal = %s, want DRIFT (failure %q)", j.State, j.Failure)
	}
	if !strings.Contains(j.Failure, "swarms/dev.md") {
		t.Errorf("failure %q does not name the drifted dependency", j.Failure)
	}
	if got := readFile(t, env.path("swarms/dev.md")); !strings.Contains(got, "leadRole: reviewer") {
		t.Errorf("the hand-edited dependency was touched: %q", got)
	}
	blocked, berr := env.e.BlockedProjects(context.Background())
	if berr != nil || len(blocked) == 0 {
		t.Fatalf("BlockedProjects = %v, %v; the DRIFT row must block its projects", blocked, berr)
	}
	if st := env.proposalStatus(t, id); st == persistence.ProposalStatusApplied {
		t.Error("the ledger recorded APPLIED against a changed dependency")
	}
}

func TestReconcile_ReadSetDriftOnApplyingRowParksInDrift(t *testing.T) {
	env, id := readSetCase(t, "write:projects/a.yaml")
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateApplying {
		t.Fatalf("precondition: journal = %s, want APPLYING", j.State)
	}
	assertParkedInDrift(t, env, id, env.e.Reconcile(context.Background()))
	// Nothing rolled forward or back: the first write is still there.
	if got := readFile(t, env.path("projects/a.yaml")); got != jaContentA {
		t.Errorf("projects/a.yaml = %q; recovery must not touch targets on DRIFT", got)
	}
}

// The production shape: VERIFYING, every target at its intended content.
func TestReconcile_ReadSetDriftOnVerifyingRowParksInDrift(t *testing.T) {
	env, id := readSetCase(t, "verifying")
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateVerifying {
		t.Fatalf("precondition: journal = %s, want VERIFYING", j.State)
	}
	assertParkedInDrift(t, env, id, env.e.Reconcile(context.Background()))
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Errorf("config.yaml = %q; recovery must not revert on DRIFT", got)
	}
}

func TestReconcile_SecondStartIsQuiet(t *testing.T) {
	env, id := readSetCase(t, "verifying")
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("first start: %v, want ErrJournalDrift", err)
	}
	first := env.latestJournal(t, id)
	logs := logTo(env)
	if err := env.e.Reconcile(context.Background()); err != nil {
		t.Fatalf("second start: %v, want nil (a DRIFT row is the operator's)", err)
	}
	second := env.latestJournal(t, id)
	if second.State != first.State || !second.UpdatedAt.Equal(first.UpdatedAt) {
		t.Errorf("the second start changed the row: %s %v -> %s %v", first.State, first.UpdatedAt, second.State, second.UpdatedAt)
	}
	if logs.Len() != 0 {
		t.Errorf("the second start logged: %s", logs.String())
	}
}

// A REVERTING row with a moved dependency: today's legal REVERTING → DRIFT,
// pinned so the markDrift change cannot regress it (it passed before the
// fix too; it is a guard, not the reproduction).
func TestReconcile_ReadSetDriftOnRevertingRowParksInDrift(t *testing.T) {
	env, id := readSetCase(t, "write:projects/a.yaml")
	j := env.latestJournal(t, id)
	if err := env.journal.Transition(context.Background(), j.ID, persistence.JournalStateApplying,
		persistence.JournalStateReverting, persistence.JournalPatch{Failure: "test: a revert was under way"}); err != nil {
		t.Fatal(err)
	}
	assertParkedInDrift(t, env, id, env.e.Reconcile(context.Background()))
}

// movingJournal moves a row out of PREPARED just before the engine's
// PREPARED → FAILED compare-and-set: another writer won the race.
type movingJournal struct {
	persistence.ApplyJournalRepository
}

func (m movingJournal) Transition(ctx context.Context, id, from, to string, patch persistence.JournalPatch) error {
	if from == persistence.JournalStatePrepared && to == persistence.JournalStateFailed {
		if err := m.ApplyJournalRepository.Transition(ctx, id, from, persistence.JournalStateApplying, persistence.JournalPatch{}); err != nil {
			return err
		}
	}
	return m.ApplyJournalRepository.Transition(ctx, id, from, to, patch)
}

// Review b240 finding 8 (2026-10-08): parkReadSetDrift discarded the
// PREPARED → FAILED error, reported success, and retired the proposal of a
// row that had moved on. The failed transition is returned and the
// proposal is left alone.
func TestReconcile_ReadSetDriftPreparedConcurrentMoveIsReported(t *testing.T) {
	env, id := readSetCase(t, "prepared")
	env.e.Journal = movingJournal{ApplyJournalRepository: env.journal}
	if err := env.e.Reconcile(context.Background()); err == nil || !errors.Is(err, persistence.ErrInvalidTransition) {
		t.Fatalf("Reconcile = %v, want the lost compare-and-set reported", err)
	}
	if st := env.proposalStatus(t, id); st != persistence.ProposalStatusApproved {
		t.Fatalf("proposal = %s, want APPROVED (not retired on a lost race)", st)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateApplying {
		t.Fatalf("journal = %s, want the other writer's APPLYING", j.State)
	}
}

// Review b240 finding 9: a crash between "from → REVERTING" and
// "REVERTING → DRIFT" leaves the row REVERTING; the next start re-parks it
// (the read-set check runs before the REVERTING branch would restore), and
// no file is written.
func TestReconcile_CrashBetweenParkingStepsReParksWithoutWriting(t *testing.T) {
	env, id := readSetCase(t, "verifying")
	env.crashAt("parking", true, nil)
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, errCrashInjected) {
		t.Fatalf("first start = %v, want the injected crash", err)
	}
	if j := env.latestJournal(t, id); j.State != persistence.JournalStateReverting {
		t.Fatalf("after the crash: %s, want REVERTING", j.State)
	}
	env.e.crashAfter = nil
	assertParkedInDrift(t, env, id, env.e.Reconcile(context.Background()))
	if got := readFile(t, env.path("config.yaml")); got != newContent {
		t.Fatalf("the re-park restored config.yaml: %q", got)
	}
}
