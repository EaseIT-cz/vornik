package brokergrants_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/testutil/testwait"
)

// noAudit discards the sweeper's audit rows.
type noAudit struct{}

func (noAudit) Insert(context.Context, *persistence.AdminAuditEntry) error { return nil }
func (noAudit) List(context.Context, persistence.AdminAuditFilter) ([]*persistence.AdminAuditEntry, error) {
	return nil, nil
}

// waitRecorder is the real sweeper with the deadline of the context the
// lifecycle waits on recorded (GitHub #423).
type waitRecorder struct {
	*projectarchive.Sweeper
	deadline time.Time
	bounded  bool
}

func (w *waitRecorder) WaitOutcome(ctx context.Context, projectID string, afterSeq uint64) (projectarchive.DeleteOutcome, bool) {
	w.deadline, w.bounded = ctx.Deadline()
	return w.Sweeper.WaitOutcome(ctx, projectID, afterSeq)
}

// GitHub #370 carry-over, delta audit 2026-10-14 (lane W8-W, archival design
// section I). Found at: code review; observed when an operator re-creates a
// project with a deleted project's ID. broker_standing_grants was left behind
// by the project wipe, keyed by (project_id, workflow_id, action), so the
// re-created project's matching broker writes were approved by the deleted
// project's standing grant with no device approval. Deletes through
// LifecycleService.DeleteNowAndWait with the real Sweeper and the real SQLite
// deleter, then asks Service.Cover — the path that approves a write under a
// grant — about a new matching action of the re-created project.
func TestCover_ReCreatedProjectInheritsNoStandingGrant_370(t *testing.T) {
	e := newEnv(t)
	e.seed(`{"to":"jana@example.com","body":"seed"}`, 7, 20)
	if covered, err := e.svc.Cover(e.ctx, e.action("ns1--mail", `{"to":"jana@example.com","body":"before"}`)); err != nil || !covered {
		t.Fatalf("precondition: the grant does not cover a matching action (%v %v)", covered, err)
	}

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	yaml := filepath.Join(dir, "projects", "ns1--p.yaml")
	if err := os.WriteFile(yaml, []byte("projectId: ns1--p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"ns1--p": {ID: "ns1--p", Lifecycle: registry.ProjectLifecycle{
		Status: "archived", ScheduledDeleteAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}})
	sw := projectarchive.NewSweeper(projectarchive.Config{
		Registry: reg, DataDeleter: sqlite.NewProjectDataCleanupRepository(e.db.DB),
		ConfigDir: dir, ArtifactBasePath: t.TempDir(), AuditRepo: noAudit{}, Logger: zerolog.Nop(),
	})
	runCtx, stop := context.WithCancel(e.ctx)
	t.Cleanup(stop)
	rec := &waitRecorder{Sweeper: sw}
	lc := &projectarchive.LifecycleService{
		ConfigDir: dir,
		Patcher:   func(c []byte, _ []projectarchive.PatchOp) ([]byte, error) { return c, nil },
		Sweeper:   rec,
		Reload:    func(context.Context) error { go sw.Run(runCtx); return nil },
	}
	// GitHub #423 (found at: unit, a loaded make test lane): with a fixed 10 s
	// bound this answered "pending" for a wipe that takes 4 ms unloaded. The
	// wait ends on the sweeper's outcome; only the test's own deadline may
	// cut it short, which the recorder checks.
	minDeadline := time.Now().Add(testwait.ForOutcome(t) - 5*time.Second)
	res, err := lc.DeleteNowAndWait(e.ctx, "ns1--p", true, testwait.ForOutcome(t))
	if _, testBounded := t.Deadline(); testBounded && rec.bounded && rec.deadline.Before(minDeadline) {
		t.Errorf("the wait for the wipe's outcome ends %s from now, before the test's own budget (%s): a fixed bound answers pending under load (#423)",
			time.Until(rec.deadline).Round(time.Second), time.Until(minDeadline).Round(time.Second))
	}
	if err != nil || res.State != projectarchive.DeleteNowDeleted {
		t.Fatalf("delete-now = %q (%v, %v), want deleted", res.State, res.Err, err)
	}

	// The operator re-creates ns1--p with the same workflow; its first
	// matching write must wait for a device, not ride the old grant.
	if err := os.WriteFile(yaml, []byte("projectId: ns1--p\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := e.action("ns1--mail", `{"to":"jana@example.com","body":"after re-create"}`)
	if covered, err := e.svc.Cover(e.ctx, a); err != nil || covered {
		t.Fatalf("the re-created project's write was covered by the deleted project's standing grant (covered=%v err=%v)", covered, err)
	}
	if st := e.state(a.ActionID); st.Status != persistence.BrokerActionPending {
		t.Errorf("the re-created project's action is %s, want pending", st.Status)
	}
}
