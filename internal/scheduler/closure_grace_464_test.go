package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// GitHub #464 (task-lifecycle-conversational design §18), the second place:
// the closure-grace sweep listed the OLDEST 5 closure_request messages and
// took the last as the most recent. With six, the newest still inside the
// grace window, it read an old one and closed the task. Real SQLite message
// repository; the task repository is a mock that records the transition.
func TestClosureGrace_464_ReadsTheLatestRequest(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	msgs := sqlite.NewTaskMessageRepository(db.DB)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * closureGracePeriod)
	for i := 0; i < 6; i++ {
		at := old.Add(time.Duration(i) * time.Minute)
		if i == 5 {
			at = time.Now().UTC() // the latest request is fresh
		}
		if err := msgs.Insert(ctx, &persistence.TaskMessage{ID: fmt.Sprintf("cr%d", i), TaskID: "t464",
			AuthorKind: persistence.TaskMessageAuthorLead, MessageKind: persistence.TaskMessageKindClosureRequest,
			Content: "close it", CreatedAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	closed := false
	repo := &mocks.MockTaskRepository{
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) {
			return []*persistence.Task{{ID: "t464", Status: persistence.TaskStatusCompleted, UpdatedAt: old}}, nil
		},
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			closed = true
			return true, nil
		},
	}
	NewExternalWaitMonitor(repo, repo, msgs, nil, time.Hour, zerolog.Nop()).scanClosureGrace(ctx)
	if closed {
		t.Fatal("the newest closure request is inside the grace window; the task must stay COMPLETED")
	}
}
