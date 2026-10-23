//go:build integration

package cli

// GitHub #414: the transcript rescan suite on Postgres, where metadata is jsonb
// (re-normalised on write) and task_messages has a foreign key to tasks. The
// SQLite half is secrets_scan_transcripts_sqlite_test.go.

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence/postgres"
)

func TestScanTranscripts414_Postgres(t *testing.T) {
	db := dbcovSetup(t)
	project := dbcovUniqueProject("scan414")
	tasks := postgres.NewTaskRepository(db)
	runTranscriptRescanSuite(t, transcriptRig{
		env: postgresScanEnv(db),
		createTask: func(t *testing.T, id, project string) {
			t.Helper()
			if err := tasks.Create(context.Background(), newTask414(id, project)); err != nil {
				t.Fatalf("create task: %v", err)
			}
			t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, id) })
		},
	}, project)
}

// The operator's view: `vornikctl secrets scan-history --project P` (a dry
// run) names task_messages and prints the per-column denominator. Before #414
// the store was absent from the report, which read the same as clean.
func TestScanHistoryCommand414_ReportsTaskMessagesAndColumns(t *testing.T) {
	db := dbcovOpen(t)
	dbcovWriteConfigWith(t, dbcovDBConfig(t), "secrets:\n  enabled: true\n")
	project := dbcovUniqueProject("scan414cmd")
	taskID := project + "-task"
	if err := postgres.NewTaskRepository(db).Create(context.Background(), newTask414(taskID, project)); err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM tasks WHERE id = $1`, taskID) })
	if _, err := db.Exec(`INSERT INTO task_messages (id, task_id, author_kind, message_kind, content, created_at)
		VALUES ($1, $2, 'operator', 'message', $3, $4)`, project+"-m", taskID, "key "+aws414, time.Now().UTC()); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// GitHub #425: the step-outcome signals are a sixth target, printed with
	// their column's denominator.
	if _, err := db.Exec(`INSERT INTO execution_step_outcomes
		(id, project_id, task_id, execution_id, step_id, role, model, outcome, recorded_at, hallucination_signals)
		VALUES ($1, $2, $3, $4, 'write', 'writer', 'm', 'ok', $5, $6)`,
		project+"-o", project, taskID, project+"-e", time.Now().UTC(),
		`[{"detector":"url_not_fetched","claim_value":"https://x.test/?k=`+aws414+`"}]`); err != nil {
		t.Fatalf("seed outcome: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM execution_step_outcomes WHERE project_id = $1`, project) })

	prevProject, prevApply, prevJSON, prevRules := secretsScanProject, secretsScanApply, secretsScanJSON, secretsScanRules
	secretsScanProject, secretsScanApply, secretsScanJSON, secretsScanRules = project, false, false, ruleSpecStrong
	t.Cleanup(func() {
		secretsScanProject, secretsScanApply, secretsScanJSON, secretsScanRules = prevProject, prevApply, prevJSON, prevRules
	})
	out, err := dbcovCapture(t, func() error { return runSecretsScanHistory(nil, nil) })
	if err != nil {
		t.Fatalf("scan-history: %v\n%s", err, out)
	}
	for _, want := range []string{
		"task_messages: scanned 1 row(s), 1 contained secret-shaped value(s)",
		"content 1/1, metadata 0/0, author_id 0/0",
		"hallucination_signals_json 0/0, chat_id 0/0, user_id 0/0",
		"execution_step_outcomes: scanned 1 row(s), 1 contained secret-shaped value(s)",
		"per column (rows holding a value / rows matching): hallucination_signals 1/1",
		"DRY-RUN",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, aws414) {
		t.Error("the report must never print a matched value")
	}
}
