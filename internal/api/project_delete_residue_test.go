package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
)

type failingWiper struct{ err error }

func (f failingWiper) DeleteProjectArtifacts(context.Context, string) (int, error) { return 1, f.err }

// deleteNowOverRealSweeper runs the delete-now handler over the real
// LifecycleService and a real Sweeper built from cfg (Registry, ConfigDir and
// Logger are filled in), for an archived project "proj" past its grace.
func deleteNowOverRealSweeper(t *testing.T, cfg projectarchive.Config) ArchiveResponse {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "proj.yaml"), []byte("projectId: proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"proj": {ID: "proj", Lifecycle: registry.ProjectLifecycle{
		Status: "archived", ScheduledDeleteAt: time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)}}})
	cfg.Registry, cfg.ConfigDir, cfg.Logger = reg, dir, zerolog.Nop()
	sw := projectarchive.NewSweeper(cfg)
	runCtx, stop := context.WithCancel(context.Background())
	t.Cleanup(stop)
	lc := &projectarchive.LifecycleService{
		ConfigDir: dir,
		Patcher:   func(c []byte, _ []projectarchive.PatchOp) ([]byte, error) { return c, nil },
		Sweeper:   sw,
		// Start the sweeper after DeleteNowAndWait captured its sequence, so
		// its first pass is the attempt the call waits for.
		Reload: func(context.Context) error { go sw.Run(runCtx); return nil },
	}
	s := &Server{logger: zerolog.Nop(), projectRegistry: reg, archiveService: lc, deleteNowWait: 10 * time.Second}

	rec := postDeleteNow(s)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var resp ArchiveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

// Delta audit 2026-10-14 (lane W8-W, archival design section H). Found at:
// code review; a user observes it through delete-now. With an artifact wipe
// that failed after the project's rows and YAML were removed, the API
// answered 500 DELETE_FAILED "was NOT deleted ... the sweeper retries" for a
// project that was gone and that nothing retried. Runs the handler over the
// real LifecycleService and Sweeper.
func TestProjectDeleteNow_ArtifactResidueIsDeletedWithWarning(t *testing.T) {
	resp := deleteNowOverRealSweeper(t, projectarchive.Config{
		ArtifactWiper: failingWiper{err: errors.New("s3: access denied on proj/out/a.bin")},
	})
	if resp.Status != "deleted_with_errors" || !strings.Contains(resp.Warning, "access denied") ||
		!strings.Contains(resp.Warning, "artifact files may remain") {
		t.Errorf("response = %+v, want status deleted_with_errors and a warning naming the error", resp)
	}
	if len(resp.ResidueSteps) != 1 || resp.ResidueSteps[0] != "artifacts" {
		t.Errorf("residue_steps = %v, want [artifacts]", resp.ResidueSteps)
	}
}

// 2026.10.6 delta audit (#334), follow-up to section H. Found at: code
// review; a user observes it through delete-now. A delete-now whose only
// residue was the registry reload was answered "artifact files may remain"
// (and the handler's comment said the registry was clean) although every
// artifact was removed and the archived project stays in the registry until
// the next tick completes the deletion. The warning must name the step.
func TestProjectDeleteNow_ReloadOnlyResidueNamesTheReload(t *testing.T) {
	resp := deleteNowOverRealSweeper(t, projectarchive.Config{
		Reload: func(context.Context) error { return errors.New("projects dir: permission denied") },
	})
	if resp.Status != "deleted_with_errors" {
		t.Fatalf("status = %q, want deleted_with_errors (%+v)", resp.Status, resp)
	}
	if strings.Contains(resp.Warning, "artifact") {
		t.Errorf("warning = %q: a reload-only residue must not mention artifact files", resp.Warning)
	}
	for _, want := range []string{"registry reload failed", "next tick", "permission denied"} {
		if !strings.Contains(resp.Warning, want) {
			t.Errorf("warning = %q, want it to contain %q", resp.Warning, want)
		}
	}
	if len(resp.ResidueSteps) != 1 || resp.ResidueSteps[0] != "reload" {
		t.Errorf("residue_steps = %v, want [reload]", resp.ResidueSteps)
	}
}
