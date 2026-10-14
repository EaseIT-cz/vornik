package controlplane

import (
	"context"
	"errors"
	"testing"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #302 (2026-10-13): a DRIFT row paused its projects and no agent
// surface said why (config-apply journal design §13.1). PauseReasons is the
// one reader the scheduler gate and the agent view share (§13.2); these pin
// that each producer's text classifies as its own drift class, and that
// BlockedProjects is the union of the same reasons.

func TestPauseReasons_EditedFileFromTheRevertPath(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.e.Reload = func() error { return errors.New("bad yaml") }
	env.crashAt("reverting", false, func() { env.write(t, "projects/a.yaml", "operator: edited\n") })
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("expected ErrJournalDrift, got %v", err)
	}
	row := env.latestJournal(t, id)

	reasons, err := env.e.PauseReasons(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 {
		t.Fatalf("PauseReasons = %+v, want one reason for the one DRIFT row", reasons)
	}
	r := reasons[0]
	if r.JournalID != row.ID || r.Drift != DriftEditedFile || r.Files != len(row.Ops) || r.Files == 0 {
		t.Errorf("reason = %+v, want journal %s, drift %s, files %d", r, row.ID, DriftEditedFile, len(row.Ops))
	}
	if len(r.Projects) != 1 || r.Projects[0] != "digest" {
		t.Errorf("projects = %v, want [digest]", r.Projects)
	}
	if !r.Since.Equal(row.UpdatedAt) {
		t.Errorf("since = %v, want the row's updated_at %v", r.Since, row.UpdatedAt)
	}
}

func TestPauseReasons_ChangedDependencyFromRecovery(t *testing.T) {
	env, id := readSetCase(t, "write:projects/a.yaml")
	if err := env.e.Reconcile(context.Background()); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("Reconcile = %v, want ErrJournalDrift", err)
	}
	row := env.latestJournal(t, id)
	reasons, err := env.e.PauseReasons(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(reasons) != 1 || reasons[0].JournalID != row.ID || reasons[0].Drift != DriftChangedDependency {
		t.Fatalf("PauseReasons = %+v, want one changed_dependency reason for %s", reasons, row.ID)
	}
}

func TestPauseReasons_OnlyDriftRowsAndUnknownText(t *testing.T) {
	if got := driftClass("something else entirely"); got != DriftUnknown {
		t.Errorf("driftClass(other) = %q, want %q", got, DriftUnknown)
	}
	env := newJournalEnv(t)
	// A seeded, applied proposal leaves only terminal rows.
	id := env.seed(t, "")
	if err := env.e.Apply(context.Background(), id, "vadim", false); err != nil {
		t.Fatal(err)
	}
	reasons, err := env.e.PauseReasons(context.Background())
	if err != nil || len(reasons) != 0 {
		t.Fatalf("PauseReasons = %+v, %v; a clean journal pauses nothing", reasons, err)
	}
}

// countingJournal counts ListOpen calls, to pin that BlockedProjects reads
// through PauseReasons once (review 0a10 F3: structural, not two reads that
// happen to agree).
type countingJournal struct {
	persistence.ApplyJournalRepository
	listOpen int
}

func (c *countingJournal) ListOpen(ctx context.Context) ([]*persistence.ConfigApplyJournal, error) {
	c.listOpen++
	return c.ApplyJournalRepository.ListOpen(ctx)
}

func TestBlockedProjects_IsTheUnionOfPauseReasons(t *testing.T) {
	env := newJournalEnv(t)
	id := env.seed(t, "")
	env.e.Reload = func() error { return errors.New("bad yaml") }
	env.crashAt("reverting", false, func() { env.write(t, "projects/a.yaml", "operator: edited\n") })
	if err := env.e.Apply(context.Background(), id, "vadim", false); !errors.Is(err, ErrJournalDrift) {
		t.Fatalf("expected ErrJournalDrift, got %v", err)
	}
	counter := &countingJournal{ApplyJournalRepository: env.e.Journal}
	env.e.Journal = counter
	blocked, err := env.e.BlockedProjects(context.Background())
	if err != nil || len(blocked) != 1 || blocked[0] != "digest" {
		t.Fatalf("BlockedProjects = %v, %v; want [digest]", blocked, err)
	}
	if counter.listOpen != 1 {
		t.Errorf("BlockedProjects read the journal %d times, want once (through PauseReasons)", counter.listOpen)
	}
}
