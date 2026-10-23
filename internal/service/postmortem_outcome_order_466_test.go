package service

// GitHub #466 (second part): the post-mortem evidence heads its step-outcome
// list "most recent first", but the listing comes back newest first
// (recorded_at DESC) and the loop walked it from the end, so the model was
// shown the OLDEST 20 of up to 50 outcomes, oldest first, and never the step
// that actually failed last. Found at: review (recorded in the #439/#440
// amendment's as-built A3); an operator observes it in the evidence the
// "Explain this failure" button sends, so the test drives the UI route over
// the explainer the container builds and real SQLite, and reads what the
// provider stub received (process-spawn-law design, "Amendment 2026-10-21
// (#466)").

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/ui"
)

func TestPostMortem466_StepOutcomesNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	stub := &recordingChat439{}
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}, ChatClient: stub}
	c.Config.Chat.Model = "stub-model"
	c.livePub = livepubsub.New(10)
	c.repos = &storage.Repositories{
		Tasks:        sqlite.NewTaskRepository(db.DB),
		Executions:   sqlite.NewExecutionRepository(db.DB),
		StepOutcomes: sqlite.NewExecutionStepOutcomeRepository(db.DB),
		ToolAudit:    sqlite.NewToolAuditRepository(db.DB),
		PostMortems:  sqlite.NewTaskPostMortemRepository(db.DB),
	}
	require.NoError(t, c.repos.Tasks.Create(ctx, &persistence.Task{
		ID: "t466", ProjectID: "alpha", Status: persistence.TaskStatusFailed, MaxAttempts: 1, Payload: []byte(`{}`),
	}))
	require.NoError(t, c.repos.Executions.Create(ctx, &persistence.Execution{
		ID: "e466", TaskID: "t466", ProjectID: "alpha", WorkflowID: "wf", Status: persistence.ExecutionStatusFailed,
	}))
	// 30 outcomes, step-00 oldest, step-29 newest.
	base := time.Now().UTC().Add(-time.Hour)
	for i := 0; i < 30; i++ {
		require.NoError(t, c.repos.StepOutcomes.Record(ctx, &persistence.ExecutionStepOutcome{
			ID: fmt.Sprintf("o%02d", i), ProjectID: "alpha", TaskID: "t466", ExecutionID: "e466",
			StepID: fmt.Sprintf("step-%02d", i), Role: "r", Outcome: "tool_error",
			ErrorDetail: fmt.Sprintf("detail %02d", i), RecordedAt: base.Add(time.Duration(i) * time.Second),
		}))
	}

	exp := c.buildPostMortemExplainer()
	require.NotNil(t, exp)
	srv := ui.NewServer(ui.WithPostMortemExplainer(exp), ui.WithTaskRepository(c.repos.Tasks))
	req := httptest.NewRequest(http.MethodPost, "/ui/tasks/t466/post-mortem", nil)
	// The explainer's egress is scrubbed whoever presses the button; the
	// caller is the operator (auth off), stated since GitHub #491.
	req = req.WithContext(api.ContextWithAuthEnabled(req.Context(), false))
	rec := httptest.NewRecorder()
	srv.TaskPostMortemGenerate(rec, req, "t466")
	require.Equal(t, http.StatusSeeOther, rec.Code)

	var user string
	for _, m := range stub.msgs {
		if m.Role == "user" {
			user = m.Content
		}
	}
	require.NotEmpty(t, user, "the provider was not called")
	at := strings.Index(user, "Step outcomes (most recent first):")
	require.GreaterOrEqual(t, at, 0, "the evidence has no step-outcome list")
	section := user[at:]
	var steps []string
	for _, line := range strings.Split(section, "\n") {
		if i := strings.Index(line, "step=step-"); i >= 0 {
			steps = append(steps, line[i+len("step="):i+len("step=")+len("step-00")])
		}
	}
	want := make([]string, 0, 20)
	for i := 29; i >= 10; i-- {
		want = append(want, fmt.Sprintf("step-%02d", i))
	}
	assert.Equal(t, want, steps, "the newest 20 outcomes, newest first")
}
