package cli

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Found-at-level regression for GitHub #360 and #361 (ENGINEERING-TENETS §4;
// docs/audits/2026-10-14-found-at-level-retro.md).
//
// #360 (2026-10-13): `vornikctl retention` refused SQLite, and the sweeper's
// statements were Postgres-shaped. #361 (2026-10-13): SQLite runs with
// foreign_keys OFF, so a pruned task's executions, artifacts and messages
// outlived it. Both fixes were tested below the operator's command:
// TestRetentionDBOpensBothDrivers checks only the handle selection, and
// repotest.RunCascadeDeleteSuite calls retention.Sweeper.Sweep with a
// hand-built Policy. An operator observes both through `vornikctl retention
// --apply` on a SQLite install: this drives that command (config.yaml with
// driver sqlite, config.Load, storage.Open, the registry, the sweeper) on a
// migrated SQLite file and checks a 100-day-old completed task is pruned with
// its children while a 1-day-old one and its children stay.
func TestRetentionCLI_SQLiteApplyPrunesTaskWithItsChildren(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "vornik.db")
	db := sqlitetest.Open(t, dbPath)

	project := "lite-retention"
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	arts := sqlite.NewArtifactRepository(db.DB)
	now := time.Now().UTC()
	seed := func(name string, age time.Duration) (task, exec, art, msg string) {
		t.Helper()
		task, exec, art, msg = "t-"+name, "e-"+name, "a-"+name, "m-"+name
		wf := "wf"
		at := now.Add(-age)
		if err := tasks.Create(ctx, &persistence.Task{ID: task, ProjectID: project, WorkflowID: &wf,
			Status: persistence.TaskStatusCompleted, Payload: []byte(`{}`), CreatedAt: at, UpdatedAt: at}); err != nil {
			t.Fatalf("create task: %v", err)
		}
		if err := execs.Create(ctx, &persistence.Execution{ID: exec, TaskID: task, ProjectID: project,
			WorkflowID: "wf", WorkflowRevision: "r1", Status: persistence.ExecutionStatusRunning}); err != nil {
			t.Fatalf("create execution: %v", err)
		}
		// created_at now: the artifact step's own horizon cannot select it,
		// so only the cascade from its execution can remove it.
		if err := arts.Create(ctx, &persistence.Artifact{ID: art, ProjectID: project, TaskID: &task, ExecutionID: &exec,
			Name: "a.txt", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: filepath.Join(dir, "nonexistent", art),
			CreatedAt: now}); err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_messages (id, task_id, execution_id, author_kind, message_kind, content, created_at)
			VALUES ($1, $2, $3, 'system', 'note', 'c', $4)`, msg, task, exec, persistence.SQLiteTime(now)); err != nil {
			t.Fatalf("seed task message: %v", err)
		}
		return task, exec, art, msg
	}
	oldT, oldE, oldA, oldM := seed("old", 100*24*time.Hour)
	newT, newE, newA, newM := seed("new", 24*time.Hour)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte("server:\n  address: \":8080\"\ndatabase:\n  driver: sqlite\n  path: "+dbPath+
		"\nstorage:\n  artifacts_path: "+filepath.Join(dir, "artifacts")+"\napi:\n  auth_enabled: false\nretention:\n  tasks_days: 60\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_CONFIG", cfgPath)
	configs := filepath.Join(dir, "configs")
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configs, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for path, body := range map[string]string{
		"projects/" + project + ".yaml": "projectId: " + project + "\ndisplayName: " + project + "\nswarmId: s\ndefaultWorkflowId: w\n",
		"swarms/s.md":                   "---\nswarmId: s\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n",
		"workflows/w.md":                "---\nworkflowId: w\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n",
	} {
		if err := os.WriteFile(filepath.Join(configs, path), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("VORNIK_CONFIGS_DIR", configs)

	retentionProject, retentionApply, retentionJSON = project, true, true
	t.Cleanup(func() { retentionProject, retentionApply, retentionJSON = "", false, false })
	if err := runRetention(retentionCmd, nil); err != nil {
		t.Fatalf("vornikctl retention --apply on SQLite: %v", err)
	}

	check := sqlitetest.Connect(t, dbPath)
	exists := func(table, id string) bool {
		t.Helper()
		var n int
		if err := check.DB.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n == 1
	}
	for table, id := range map[string]string{"tasks": oldT, "executions": oldE, "artifacts": oldA, "task_messages": oldM} {
		if exists(table, id) {
			t.Errorf("%s %s of the 100-day-old task survived `vornikctl retention --apply` (tasks_days 60)", table, id)
		}
	}
	for table, id := range map[string]string{"tasks": newT, "executions": newE, "artifacts": newA, "task_messages": newM} {
		if !exists(table, id) {
			t.Errorf("%s %s of the 1-day-old task was deleted", table, id)
		}
	}
}
