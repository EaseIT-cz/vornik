package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/ui"
)

// GitHub #365 (2026-10-13), found-at-level regression (ENGINEERING-TENETS §4;
// docs/audits/2026-10-14-found-at-level-retro.md). On SQLite, "Delete now" on
// an archived project failed at `no such table: memory_embed_queue`, rolled
// back and deleted nothing, while every surface answered "scheduled". The fix
// shipped with the wipe tested on the repository (RunProjectDataWipeSuite),
// the driver choice tested as a function (TestProjectDataDeleterFor_*) and the
// API/UI/CLI answers tested against a fake lifecycle. Nothing ran the
// operator's path as the daemon wires it: this builds the container's archive
// sweeper and lifecycle (initArchiveSweeper, initArchiveLifecycle with the UI's
// YAML patcher) on a migrated SQLite file and calls DeleteNowAndWait, the call
// the REST, UI and CLI "delete now" make, then checks the rows are gone, the
// project's YAML is gone, another project's rows stay, and the answer is
// "deleted".
func TestArchiveDeleteNow_SQLiteWipesThroughTheDaemonWiring_365(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := sqlitetest.Open(t, filepath.Join(dir, "vornik.db"))

	configs := filepath.Join(dir, "configs")
	for _, sub := range []string{"projects", "swarms", "workflows"} {
		if err := os.MkdirAll(filepath.Join(configs, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().Add(30 * 24 * time.Hour).UTC().Format(time.RFC3339)
	files := map[string]string{
		"swarms/s.md":    "---\nswarmId: s\nroles:\n  - name: worker\n    runtime:\n      image: test-image\n---\n",
		"workflows/w.md": "---\nworkflowId: w\nentrypoint: run\nsteps:\n  run:\n    type: agent\n    role: worker\n    prompt: \"do work\"\n    on_success: done\nterminals:\n  done:\n    status: COMPLETED\n---\n",
		"projects/doomed.yaml": "projectId: doomed\ndisplayName: doomed\nswarmId: s\ndefaultWorkflowId: w\n" +
			"lifecycle:\n  status: archived\n  archivedAt: \"" + time.Now().UTC().Format(time.RFC3339) + "\"\n  scheduledDeleteAt: \"" + later + "\"\n",
		"projects/keeper.yaml": "projectId: keeper\ndisplayName: keeper\nswarmId: s\ndefaultWorkflowId: w\n",
	}
	for p, body := range files {
		if err := os.WriteFile(filepath.Join(configs, p), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg := registry.New()
	if err := reg.Load(configs); err != nil {
		t.Fatalf("load registry: %v", err)
	}

	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	arts := sqlite.NewArtifactRepository(db.DB)
	type seeded struct{ task, exec, art, msg string }
	seed := func(project string) seeded {
		t.Helper()
		s := seeded{"t-" + project, "e-" + project, "a-" + project, "m-" + project}
		wf := "w"
		if err := tasks.Create(ctx, &persistence.Task{ID: s.task, ProjectID: project, WorkflowID: &wf,
			Status: persistence.TaskStatusCompleted, Payload: []byte(`{}`)}); err != nil {
			t.Fatalf("create task: %v", err)
		}
		if err := execs.Create(ctx, &persistence.Execution{ID: s.exec, TaskID: s.task, ProjectID: project,
			WorkflowID: "w", WorkflowRevision: "r1", Status: persistence.ExecutionStatusCompleted}); err != nil {
			t.Fatalf("create execution: %v", err)
		}
		if err := arts.Create(ctx, &persistence.Artifact{ID: s.art, ProjectID: project, TaskID: &s.task, ExecutionID: &s.exec,
			Name: "a.txt", ArtifactClass: persistence.ArtifactClassOutput, StoragePath: filepath.Join(dir, "nonexistent", s.art)}); err != nil {
			t.Fatalf("create artifact: %v", err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO task_messages (id, task_id, execution_id, author_kind, message_kind, content, created_at)
			VALUES ($1, $2, $3, 'system', 'note', 'c', $4)`, s.msg, s.task, s.exec, persistence.SQLiteTime(time.Now())); err != nil {
			t.Fatalf("seed task message: %v", err)
		}
		return s
	}
	victim, control := seed("doomed"), seed("keeper")

	cfg := &config.Config{}
	cfg.Database.Driver = "sqlite"
	cfg.Storage.ArtifactsPath = filepath.Join(dir, "artifacts")
	c := &Container{Logger: zerolog.Nop(), Config: cfg, DB: db.DB, Registry: reg, repos: &storage.Repositories{}}
	c.archiveSweeper = c.initArchiveSweeper()
	if c.archiveSweeper == nil {
		t.Fatal("initArchiveSweeper returned nil on SQLite")
	}
	lc := c.initArchiveLifecycle(ui.LifecyclePatcher())
	if lc == nil {
		t.Fatal("initArchiveLifecycle returned nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); c.archiveSweeper.Run(runCtx) }()
	t.Cleanup(func() { cancel(); <-done })

	res, err := lc.DeleteNowAndWait(ctx, "doomed", true, 20*time.Second)
	if err != nil {
		t.Fatalf("DeleteNowAndWait: %v", err)
	}
	if res.State != projectarchive.DeleteNowDeleted {
		t.Fatalf("delete-now answered %q (err %v), want %q: the SQLite wipe through the daemon wiring failed",
			res.State, res.Err, projectarchive.DeleteNowDeleted)
	}

	count := func(table, id string) int {
		t.Helper()
		var n int
		if err := db.DB.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	for table, ids := range map[string][2]string{
		"tasks": {victim.task, control.task}, "executions": {victim.exec, control.exec},
		"artifacts": {victim.art, control.art}, "task_messages": {victim.msg, control.msg},
	} {
		if count(table, ids[0]) != 0 {
			t.Errorf("%s row %s of the deleted project survived the wipe", table, ids[0])
		}
		if count(table, ids[1]) != 1 {
			t.Errorf("%s row %s of another project was deleted", table, ids[1])
		}
	}
	if _, err := os.Stat(filepath.Join(configs, "projects", "doomed.yaml")); !os.IsNotExist(err) {
		t.Errorf("the deleted project's YAML is still on disk (stat err %v)", err)
	}
}
