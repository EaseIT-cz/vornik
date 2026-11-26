package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/leaderelection"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #894: on a node whose profile has run_workers: false, every
// singleton elector was nil, and the workers read a nil elector as "the single
// process, always leader". A ui node with an autonomy-enabled project ran the
// autonomy loop and the retention sweeper beside the worker node that holds
// the lease: duplicate autonomy ticks and two sweepers. Found at review (the
// #586 deploy idle gate as-built, §12.4); a user observes it as extra
// autonomy_evaluations rows and a second sweeper, which these tests count on
// a real container over a real (SQLite) database.

const project894 = "projectId: \"auto894\"\ndisplayName: \"Auto\"\nswarmId: \"s1\"\ndefaultWorkflowId: \"w1\"\n" +
	"autonomy:\n  enabled: true\n  observe: true\n  mode: cron\n  goal: \"tick\"\n  pollInterval: \"1h\"\n"

// container894 boots a real container with one autonomy-enabled project and
// the retention sweeper switched on, under the given node profile.
func container894(t *testing.T, profile string, opts ...ContainerOption) *Container {
	t.Helper()
	cfgPath := isolatedConfigPath(t)
	dir := filepath.Join(filepath.Dir(cfgPath), "configs")
	files := map[string]string{
		"workflows/w1.md":       "---\nworkflowId: \"w1\"\ndisplayName: \"W\"\nentrypoint: \"go\"\nsteps:\n  go:\n    type: \"agent\"\n    role: \"coder\"\n    on_success: \"done\"\nterminals:\n  done:\n    status: \"COMPLETED\"\n---\n\n## Prompts\n\n### go\n\nDo the thing.\n",
		"swarms/s1.md":          "---\nswarmId: \"s1\"\ndisplayName: \"S\"\nroles:\n  - name: \"coder\"\n    runtime:\n      image: \"test:latest\"\n---\n",
		"projects/auto894.yaml": project894,
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := newComposerWiringTestConfig(t)
	cfg.Node.Profile = profile
	cfg.Retention.Enabled = true
	c, err := newUnitTestContainer(t, cfg, cfgPath, opts...)
	if err != nil {
		t.Fatalf("NewContainer(%s): %v", profile, err)
	}
	t.Cleanup(func() {
		if c.autonomyManager != nil {
			c.autonomyManager.Stop()
		}
		if c.retentionCancel != nil {
			c.retentionCancel()
		}
		_ = c.StopConfigWriterLease(context.Background())
	})
	if c.Registry == nil || c.Registry.GetProject("auto894") == nil {
		t.Fatal("the autonomy project did not load; the test proves nothing")
	}
	// Since #586 NewContainer constructs these loops and startWork starts
	// them; start them as startWork's first step does.
	c.startConstructedWork(context.Background())
	return c
}

// autonomyTicks894 counts the autonomy evaluations recorded for the project,
// waiting up to wait for the first one.
func autonomyTicks894(t *testing.T, c *Container, wait time.Duration) int {
	t.Helper()
	pid := "auto894"
	deadline := time.Now().Add(wait)
	for {
		rows, err := c.repos.AutonomyEvaluations.List(context.Background(), persistence.AutonomyEvaluationFilter{ProjectID: &pid, PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) > 0 || time.Now().After(deadline) {
			return len(rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The positive control: a single node (profile "all", the default) still runs
// the autonomy loop and the retention sweeper. Without it, the negative test
// below could pass on a wiring that ticks nowhere.
func TestNonWorker894_SingleNodeStillRunsAutonomyAndRetention(t *testing.T) {
	c := container894(t, "all")
	if c.autonomyManager == nil || c.autonomyManager.ActiveLoops() != 1 {
		t.Fatalf("single node: autonomy loops = %v, want 1", c.autonomyManager)
	}
	if n := autonomyTicks894(t, c, 5*time.Second); n == 0 {
		t.Fatal("single node: no autonomy tick recorded within 5s; the counting window is too short to prove the negative")
	}
	if c.retentionDone == nil {
		t.Fatal("single node: the retention sweeper did not start")
	}
}

// The regression: a ui node (run_workers: false) starts no autonomy loop,
// records no autonomy tick, and starts no retention sweeper.
func TestNonWorker894_UINodeRunsNoAutonomyOrRetention(t *testing.T) {
	c := container894(t, "ui")
	if c.capabilities().RunWorkers {
		t.Fatal("the ui profile resolved run_workers=true; the test proves nothing")
	}
	if c.autonomyManager != nil && c.autonomyManager.ActiveLoops() > 0 {
		t.Errorf("ui node runs %d autonomy loop(s), want 0", c.autonomyManager.ActiveLoops())
	}
	// Same window the positive control needed (it records within ms).
	if n := autonomyTicks894(t, c, time.Second); n != 0 {
		t.Errorf("ui node recorded %d autonomy tick(s), want 0", n)
	}
	if c.retentionDone != nil {
		t.Error("ui node started a retention sweeper, want none")
	}
}

// The central gate: on a run_workers: false node every singleton elector is a
// standby gate that never leads and fails the epoch fence, so a worker that
// still starts there skips every tick instead of reading "nil = leader".
func TestNonWorker894_ElectorOnUINodeNeverLeads(t *testing.T) {
	c := container894(t, "ui")
	e := c.initWorkerElector("memory_consolidate")
	if e == nil {
		t.Fatal("initWorkerElector returned nil on a run_workers=false node; a worker reads nil as leader")
	}
	ctx := context.Background()
	e.BootstrapAcquire(ctx)
	if e.IsLeader() {
		t.Error("a standby elector became leader")
	}
	if ok, _ := leaderelection.DangerousWriteAllowed(ctx, e); ok {
		t.Error("the epoch fence let a standby elector write")
	}
	if e.WorkerID() != "memory_consolidate" {
		t.Errorf("worker id = %q", e.WorkerID())
	}
	for _, id := range c.WiredWorkerIDs() {
		t.Errorf("ui node reports %q as wired; standby electors must not read as wired workers", id)
	}
	// A worker node keeps its real, contending elector.
	w := container894(t, "worker")
	if we := w.initWorkerElector("memory_consolidate"); we == nil || we.Standby() {
		t.Errorf("worker node elector = %v, want a real elector", we)
	}
}
