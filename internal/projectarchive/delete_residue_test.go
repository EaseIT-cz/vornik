package projectarchive

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/registry"
)

// Delta audit 2026-10-14 (lane W8-W, archival design section H). Found at:
// code review; a user observes it through delete-now. When the artifact wipe
// failed AFTER the DB wipe, the sweeper still removed the YAML, reloaded the
// registry and wrote project.deleted — then returned the artifact error, so
// delete-now answered "failed" ("was NOT deleted ... the sweeper retries")
// for a project that was gone and that nothing would retry, and the audit log
// held both project.deleted and project.delete.failed. The regression runs
// the operator's call, LifecycleService.DeleteNowAndWait, over the real
// Sweeper with an ArtifactWiper that fails.
func TestDeleteNowAndWait_ArtifactWipeFailureIsDeletedWithErrors(t *testing.T) {
	ctx := context.Background()
	svc, yamlPath := newServiceFixture(t, "doomed", &passthroughPatcher{})
	audit := &stubAudit{}
	wiper := &stubWiper{count: 2, err: errors.New("s3: access denied on doomed/out/report.pdf")}
	reg := newTestRegistry(map[string]*registry.Project{"doomed": testDeletableProjectID("doomed")})
	sw := NewSweeper(Config{
		Registry: reg,
		// The daemon's reload re-reads the config dir, where the YAML is gone,
		// so the project leaves the registry. Without this the test registry
		// kept listing it and the delete-now kick swept it a second time,
		// doubling the audit rows (gate-stamp on 4ad362294, 2026-10-14).
		Reload: func(context.Context) error {
			registry.SeedForTest(reg, map[string]*registry.Project{})
			return nil
		},
		DataDeleter:   &stubDeleter{},
		ConfigDir:     svc.ConfigDir,
		ArtifactWiper: wiper,
		AuditRepo:     audit,
		Logger:        zerolog.Nop(),
	})
	svc.Sweeper = sw
	runCtx, stop := context.WithCancel(ctx)
	t.Cleanup(stop)
	svc.Reload = func(context.Context) error { go sw.Run(runCtx); return nil }

	res, err := svc.DeleteNowAndWait(ctx, "doomed", true, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.State != DeleteNowDeletedWithErrors {
		t.Fatalf("delete-now = %q (%v), want %q", res.State, res.Err, DeleteNowDeletedWithErrors)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "access denied") {
		t.Errorf("residue = %v, want the wiper's error", res.Err)
	}
	if _, err := os.Stat(yamlPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("project YAML still present (stat err=%v)", err)
	}

	audit.mu.Lock()
	defer audit.mu.Unlock()
	actions := map[string]int{}
	for _, r := range audit.rows {
		actions[r.Action]++
		if r.Action == "project.delete.partial" {
			p := auditPayload(t, r)
			if p["artifact_prefix"] != "doomed/" {
				t.Errorf("partial row artifact_prefix = %v, want doomed/", p["artifact_prefix"])
			}
			if !strings.Contains(p["error"].(string), "access denied") {
				t.Errorf("partial row error = %v", p["error"])
			}
			if p["artifacts_deleted_before_error"] != float64(2) {
				t.Errorf("partial row artifacts_deleted_before_error = %v, want 2", p["artifacts_deleted_before_error"])
			}
		}
	}
	if actions["project.deleted"] != 1 || actions["project.delete.partial"] != 1 || actions["project.delete.failed"] != 0 {
		t.Errorf("audit actions = %v, want one project.deleted, one project.delete.partial, no project.delete.failed", actions)
	}
	if o, ok := sw.LastOutcome("doomed"); !ok || o.Err != nil || o.Residue == nil {
		t.Errorf("LastOutcome = %+v, %v; want Err nil and a Residue", o, ok)
	}
}

// Review H/I round 2, finding 2: a reload failure after the YAML removal is a
// residue, and the stale registry entry makes the next tick run the sequence
// again. That retry must tolerate the already-removed YAML and answer
// deleted — replacing the residue outcome — not failed; the first
// project.delete.partial row stays as history.
func TestSweeper_ReloadResidueThenRetryIsDeleted(t *testing.T) {
	svc, yamlPath := newServiceFixture(t, "doomed", &passthroughPatcher{})
	audit := &stubAudit{}
	reloads := 0
	sw := NewSweeper(Config{
		Registry:    newTestRegistry(map[string]*registry.Project{"doomed": testDeletableProjectID("doomed")}),
		DataDeleter: &stubDeleter{},
		ConfigDir:   svc.ConfigDir,
		AuditRepo:   audit,
		Logger:      zerolog.Nop(),
		Reload: func(context.Context) error {
			reloads++
			if reloads == 1 {
				return errors.New("registry: parse error in another file")
			}
			return nil
		},
	})
	sw.sweepOnce(context.Background())
	o, _ := sw.LastOutcome("doomed")
	if o.Err != nil || o.Residue == nil || !strings.Contains(o.Residue.Error(), "reload") {
		t.Fatalf("first pass outcome = %+v, want a reload residue", o)
	}
	if _, err := os.Stat(yamlPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("YAML still present after the first pass (err=%v)", err)
	}
	sw.sweepOnce(context.Background()) // the stale entry is still archived and due
	o, _ = sw.LastOutcome("doomed")
	if o.Err != nil || o.Residue != nil {
		t.Fatalf("retry outcome = %+v, want deleted (no error, no residue)", o)
	}
	actions := map[string]int{}
	for _, r := range audit.rows {
		actions[r.Action]++
		if r.Action == "project.delete.partial" {
			if p := auditPayload(t, r); !strings.Contains(fmt.Sprint(p["reload_error"]), "parse error") {
				t.Errorf("partial row reload_error = %v, want the reload's error", p["reload_error"])
			}
		}
	}
	if actions["project.deleted"] != 2 || actions["project.delete.partial"] != 1 || actions["project.delete.failed"] != 0 {
		t.Errorf("audit actions = %v, want 2 project.deleted, 1 project.delete.partial, 0 project.delete.failed", actions)
	}
}

// The early-return failures stay "failed": a YAML that could not be removed
// means the project still exists and the next tick retries. The project ID
// "doomed/../x" makes removeProjectFiles refuse (a path separator in the id),
// which is the YAML-removal failure this needs.
func TestSweeper_YAMLRemovalFailureStaysFailed(t *testing.T) {
	svc, _ := newServiceFixture(t, "doomed", &passthroughPatcher{})
	sw := NewSweeper(Config{
		Registry:      newTestRegistry(map[string]*registry.Project{"doomed/../x": testDeletableProjectID("doomed/../x")}),
		DataDeleter:   &stubDeleter{},
		ConfigDir:     svc.ConfigDir,
		ArtifactWiper: &stubWiper{err: errors.New("boom")},
		AuditRepo:     &stubAudit{},
		Logger:        zerolog.Nop(),
	})
	sw.sweepOnce(context.Background())
	o, ok := sw.LastOutcome("doomed/../x")
	if !ok || o.Err == nil || o.Residue != nil {
		t.Errorf("LastOutcome = %+v, %v; want Err set and no Residue", o, ok)
	}
}
