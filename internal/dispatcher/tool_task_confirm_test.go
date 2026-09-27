package dispatcher

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// Chat memory-write design §12 (2026-09-25; backlog P1 "Regulatory audit
// surface"): cancel_task / retry_task copied `confirm` from the MODEL's own
// tool arguments, so a model could cancel or retry a task on the first turn
// without asking — `{"task_id":"t1","confirm":true}` executed immediately
// (TestCancelTask_Happy pinned exactly that). The confirmation now comes from
// the §5.3 two-step: the tool proposes, the SAME human acknowledges in their
// own next turn with a closed phrase, and only then does the next call act.

// mutationRepo is a task repository that records whether anything tried to
// change a task.
func mutationRepo(status persistence.TaskStatus, mutated *bool) *mocks.MockTaskRepository {
	return &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "snake", Status: status, Attempt: 1, MaxAttempts: 3}, nil
		},
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			*mutated = true
			return true, nil
		},
		UpdateStatusFunc: func(context.Context, string, persistence.TaskStatus) error { *mutated = true; return nil },
		UpdateFunc:       func(context.Context, *persistence.Task) error { *mutated = true; return nil },
	}
}

type taskToolCase struct {
	name   string
	scope  string
	status persistence.TaskStatus
	call   func(te *ToolExecutor, ctx context.Context, args string) ToolResult
}

var taskToolCases = []taskToolCase{
	{"cancel", scopeCancelTask, persistence.TaskStatusRunning, func(te *ToolExecutor, ctx context.Context, a string) ToolResult {
		return te.cancelTask(ctx, a, nil)
	}},
	{"retry", scopeRetryTask, persistence.TaskStatusFailed, func(te *ToolExecutor, ctx context.Context, a string) ToolResult {
		return te.retryTask(ctx, a, nil)
	}},
}

func taskExecutor(confirms *fakeConfirmRepo, repo persistence.TaskRepository) *ToolExecutor {
	return newExecutor(withTaskRepo(repo), withExecRepo(&mocks.MockExecutionRepository{}), func(te *ToolExecutor) {
		if confirms != nil {
			te.memoryConfirms = confirms
		}
	})
}

// seedTaskAck seeds an acknowledged confirmation for one task action.
func seedTaskAck(repo *fakeConfirmRepo, scope, taskID, operator string, expires time.Time) {
	now := time.Now()
	repo.seedAcknowledged(persistence.ChatMemoryWriteConfirmation{
		Channel: testMemChannel, SessionID: testMemSession,
		ContentFingerprint: taskActionFingerprint(scope, taskID),
		Scope:              scope, OperatorID: operator,
		ProposedAt: now.Add(-time.Minute), ExpiresAt: expires,
	}, now)
}

// The defect: a model's confirm:true on the first call must NOT act. It
// proposes, and the reply lists the phrases the human must type.
func TestTaskTools_ModelConfirmDoesNotAct(t *testing.T) {
	for _, tc := range taskToolCases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := false
			confirms := newFakeConfirmRepo(nil)
			te := taskExecutor(confirms, mutationRepo(tc.status, &mutated))
			res := tc.call(te, sharedCtx(), `{"task_id":"t1","confirm":true}`)
			if mutated {
				t.Fatal("the task changed on the model's own confirm:true — the bypass is back")
			}
			row, ok := confirms.get(testMemChannel)
			if !ok || row.Scope != tc.scope || row.Acknowledged() || row.OperatorID != testMemOperator {
				t.Fatalf("no pending %s proposal: ok=%v row=%+v", tc.scope, ok, row)
			}
			for _, p := range acknowledgementPhrasesFor(tc.scope) {
				if !strings.Contains(res.Content, p) {
					t.Errorf("the reply must list the phrase %q: %q", p, res.Content)
				}
			}
		})
	}
}

// After the same speaker's acknowledgement the next call acts, ONCE: the row
// is deleted, and a further call proposes again.
func TestTaskTools_AcknowledgedActsOnce(t *testing.T) {
	for _, tc := range taskToolCases {
		t.Run(tc.name, func(t *testing.T) {
			mutated := false
			confirms := newFakeConfirmRepo(nil)
			seedTaskAck(confirms, tc.scope, "t1", testMemOperator, time.Now().Add(10*time.Minute))
			te := taskExecutor(confirms, mutationRepo(tc.status, &mutated))
			tc.call(te, sharedCtx(), `{"task_id":"t1"}`)
			if !mutated {
				t.Fatal("an acknowledged action did not execute")
			}
			if _, ok := confirms.get(testMemChannel); ok {
				t.Fatal("the confirmation must be one-shot (deleted after acting)")
			}
			mutated = false
			tc.call(te, sharedCtx(), `{"task_id":"t1"}`)
			if mutated {
				t.Fatal("a second call acted without a new acknowledgement")
			}
		})
	}
}

// Every way an acknowledged-looking row must still refuse.
func TestTaskTools_RowsThatDoNotAuthorize(t *testing.T) {
	for _, tc := range taskToolCases {
		other := scopeRetryTask
		if tc.scope == scopeRetryTask {
			other = scopeCancelTask
		}
		for name, seed := range map[string]func(r *fakeConfirmRepo){
			"another speaker": func(r *fakeConfirmRepo) { seedTaskAck(r, tc.scope, "t1", "slack:UBOB", time.Now().Add(time.Hour)) },
			"another task":    func(r *fakeConfirmRepo) { seedTaskAck(r, tc.scope, "t2", testMemOperator, time.Now().Add(time.Hour)) },
			"expired": func(r *fakeConfirmRepo) {
				seedTaskAck(r, tc.scope, "t1", testMemOperator, time.Now().Add(-time.Second))
			},
			"the other action": func(r *fakeConfirmRepo) { seedTaskAck(r, other, "t1", testMemOperator, time.Now().Add(time.Hour)) },
			"wrong scope, same fingerprint": func(r *fakeConfirmRepo) {
				now := time.Now()
				r.seedAcknowledged(persistence.ChatMemoryWriteConfirmation{
					Channel: testMemChannel, SessionID: testMemSession,
					ContentFingerprint: taskActionFingerprint(tc.scope, "t1"),
					Scope:              string(memoryScopeShared), OperatorID: testMemOperator,
					ProposedAt: now, ExpiresAt: now.Add(time.Hour),
				}, now)
			},
		} {
			t.Run(tc.name+"/"+name, func(t *testing.T) {
				mutated := false
				confirms := newFakeConfirmRepo(nil)
				seed(confirms)
				te := taskExecutor(confirms, mutationRepo(tc.status, &mutated))
				tc.call(te, sharedCtx(), `{"task_id":"t1"}`)
				if mutated {
					t.Fatalf("%s authorized the action", name)
				}
			})
		}
	}
}

// Where the gate cannot run, the tools refuse at the tool layer and change
// nothing — never fall back to trusting the model.
func TestTaskTools_FailClosed(t *testing.T) {
	for _, tc := range taskToolCases {
		t.Run(tc.name+"/no store", func(t *testing.T) {
			mutated := false
			te := taskExecutor(nil, mutationRepo(tc.status, &mutated))
			res := tc.call(te, sharedCtx(), `{"task_id":"t1","confirm":true}`)
			if mutated || res.Content == "" {
				t.Fatalf("no confirmation store: must refuse and change nothing (mutated=%v) %q", mutated, res.Content)
			}
		})
		t.Run(tc.name+"/no speaker", func(t *testing.T) {
			mutated := false
			confirms := newFakeConfirmRepo(nil)
			te := taskExecutor(confirms, mutationRepo(tc.status, &mutated))
			ctx := WithCallSiteForTest(context.Background(), testMemChannel, testMemSession)
			tc.call(te, ctx, `{"task_id":"t1","confirm":true}`)
			if _, ok := confirms.get(testMemChannel); mutated || ok {
				t.Fatalf("no speaker: must refuse, propose nothing and change nothing (mutated=%v proposed=%v)", mutated, ok)
			}
		})
	}
}

// The tool schemas no longer offer the model a confirm argument.
func TestTaskTools_SchemaOffersNoConfirm(t *testing.T) {
	seen := 0
	for _, tool := range DispatcherTools() {
		f := tool.Function
		if f.Name != "cancel_task" && f.Name != "retry_task" {
			continue
		}
		seen++
		if strings.Contains(string(f.Parameters), `"confirm"`) || strings.Contains(f.Description, "confirm=true") {
			t.Errorf("%s still offers the model a confirm argument: %s / %s", f.Name, f.Parameters, f.Description)
		}
	}
	if seen != 2 {
		t.Fatalf("expected both cancel_task and retry_task in DispatcherTools, saw %d", seen)
	}
}

// A shared-memory write must not be authorized by a row of another scope, even
// with a matching fingerprint (design §12, review F9b).
func TestAuthorizeSharedWrite_RefusesAnotherScope(t *testing.T) {
	now := time.Now()
	ack := now
	rec := &persistence.ChatMemoryWriteConfirmation{
		ContentFingerprint: sharedWriteFingerprint("the fact"), Scope: scopeCancelTask,
		OperatorID: testMemOperator, ExpiresAt: now.Add(time.Hour), AcknowledgedAt: &ack,
	}
	if authorizeSharedWrite(rec, "the fact", testMemOperator, now).permits() {
		t.Fatal("a cancel_task row authorized a shared-memory write")
	}
}

// The authorization is consumed BEFORE the action runs (implementation review
// F2): when the action itself fails, the confirmation is already gone and a
// retry of the call does not act on it.
func TestTaskTools_AuthorizationConsumedEvenWhenTheActionFails(t *testing.T) {
	calls := 0
	repo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
			return &persistence.Task{ID: id, ProjectID: "snake", Status: persistence.TaskStatusRunning}, nil
		},
		TransitionConditionalFunc: func(context.Context, string, []persistence.TaskStatus, persistence.TaskStatus, persistence.TransitionOpts) (bool, error) {
			calls++
			return false, errors.New("db down")
		},
	}
	confirms := newFakeConfirmRepo(nil)
	seedTaskAck(confirms, scopeCancelTask, "t1", testMemOperator, time.Now().Add(time.Hour))
	te := taskExecutor(confirms, repo)
	if res := te.cancelTask(sharedCtx(), `{"task_id":"t1"}`, nil); !strings.Contains(res.Content, "Error") {
		t.Fatalf("the failing action must surface its error: %q", res.Content)
	}
	if _, ok := confirms.get(testMemChannel); ok {
		t.Fatal("a failed action left its authorization in place")
	}
	te.cancelTask(sharedCtx(), `{"task_id":"t1"}`, nil)
	if calls != 1 {
		t.Fatalf("the retried call acted again without a new acknowledgement (calls=%d)", calls)
	}
}

// One pending confirmation per conversation, whatever its kind (design §12,
// implementation review F3): proposing a cancel replaces a pending shared-memory
// proposal, and the reverse. An acknowledgement only ever discharges the most
// recent proposal.
func TestTaskTools_ProposalReplacesAPendingProposalOfAnotherKind(t *testing.T) {
	mutated := false
	confirms := newFakeConfirmRepo(nil)
	seedPendingScope(confirms, string(memoryScopeShared))
	te := taskExecutor(confirms, mutationRepo(persistence.TaskStatusRunning, &mutated))
	te.cancelTask(sharedCtx(), `{"task_id":"t1"}`, nil)
	row, ok := confirms.get(testMemChannel)
	if !ok || row.Scope != scopeCancelTask {
		t.Fatalf("the cancel proposal must replace the pending shared proposal: %+v", row)
	}

	confirms2 := newFakeConfirmRepo(nil)
	seedPendingScope(confirms2, scopeCancelTask)
	mem := sharedExecutor(confirms2, newFakeAuditRepo(nil))
	mem.rememberShared(sharedCtx(), testMemChannel, testMemSession, "proj", "a shared fact")
	row, ok = confirms2.get(testMemChannel)
	if !ok || row.Scope != string(memoryScopeShared) {
		t.Fatalf("the shared proposal must replace the pending cancel: %+v", row)
	}
}
