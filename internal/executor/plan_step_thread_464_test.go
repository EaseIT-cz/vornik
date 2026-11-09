package executor

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #464 (task-lifecycle-conversational design §18). Found at review,
// observable at executor-e2e: the plan step listed the OLDEST 50 messages and
// rendered the newest 30 of those, so on a 60-message thread the operator's
// latest directive never reached the lead. Driven with a migrated SQLite
// repository through the plan step's own load and its renderer. Lower than
// executor-e2e because the lead's prompt is built from exactly these two
// calls; the e2e lane adds a container and a model call around them.
func TestRunLeadPlanning_464_NewestOperatorMessageReachesTheLead(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	tasks := sqlite.NewTaskRepository(db.DB)
	msgs := sqlite.NewTaskMessageRepository(db.DB)
	ctx := context.Background()
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	wf := "wf"
	task := &persistence.Task{ID: "t464", ProjectID: "p", WorkflowID: &wf, Status: persistence.TaskStatusRunning,
		CreationSource: persistence.TaskCreationSourceUser, Priority: 50, Payload: []byte(`{}`),
		CreatedAt: base, UpdatedAt: base, MaxAttempts: 1, Attempt: 1}
	if err := tasks.Create(ctx, task); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		kind, content := persistence.TaskMessageKindMessage, fmt.Sprintf("progress note %02d", i)
		if i == 0 {
			content = "OLDEST-original-ask"
		}
		if i == 59 {
			kind, content = persistence.TaskMessageKindDirective, "NEWEST-operator-directive: switch to plan B"
		}
		if err := msgs.Insert(ctx, &persistence.TaskMessage{ID: fmt.Sprintf("m%02d", i), TaskID: task.ID,
			AuthorKind: persistence.TaskMessageAuthorOperator, MessageKind: kind, Content: content,
			CreatedAt: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	e := &Executor{logger: zerolog.Nop(), taskMessageRepo: msgs}
	prompt := buildPlanningPromptWithContext("plan it", nil, nil, e.loadLeadThread(ctx, task.ID), nil)
	if !strings.Contains(prompt, "NEWEST-operator-directive: switch to plan B") {
		t.Fatalf("the newest operator directive must reach the lead's prompt:\n%s", prompt)
	}
	if strings.Contains(prompt, "OLDEST-original-ask") {
		t.Fatalf("the window is the newest messages; the oldest falls outside it")
	}
}
