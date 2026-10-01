package repotest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunBrokerActionSuite pins the broker-action store on both backends
// (broker write-actions design §5): staging and promotion, first visible
// proposal wins, write-once args, every guarded transition, the single
// execution winner, expiry and the sweeps. setTaskStatus writes a tasks row
// (the sweeps join on the task's status).
func RunBrokerActionSuite(t *testing.T, repo persistence.BrokerActionRepository, setTaskStatus func(t *testing.T, taskID, projectID, status string)) {
	t.Helper()
	h := &brokerActionHarness{repo: repo, ctx: context.Background(), now: time.Now().UTC().Truncate(time.Second), setTaskStatus: setTaskStatus}
	t.Run("MissContract", func(t *testing.T) { brokerActionMisscontract(t, h) })
	t.Run("Stage_replaces_only_unapprovable_rows", func(t *testing.T) { brokerActionStageReplacesOnlyUnapprovableRows(t, h) })
	t.Run("Stage_refuses_an_approvable_status", func(t *testing.T) { brokerActionStageRefusesAnApprovableStatus(t, h) })
	t.Run("Approve_is_bound_to_the_shown_hash_and_expiry", func(t *testing.T) { brokerActionApproveIsBoundToTheShownHashAndExpiry(t, h) })
	t.Run("Execution_has_one_winner_and_records_the_outcome", func(t *testing.T) { brokerActionExecutionHasOneWinnerAndRecordsTheOutcome(t, h) })
	t.Run("Resolve_only_from_executing_or_unknown", func(t *testing.T) { brokerActionResolveOnlyFromExecutingOrUnknown(t, h) })
	t.Run("Reject_and_Expire", func(t *testing.T) { brokerActionRejectAndExpire(t, h) })
	t.Run("Sweeps_follow_the_task_status", func(t *testing.T) { brokerActionSweepsFollowTheTaskStatus(t, h) })
	t.Run("CountStuck", func(t *testing.T) { brokerActionCountstuck(t, h) })
	t.Run("ListByStatus", func(t *testing.T) { brokerActionListbystatus(t, h) })
	t.Run("Expiry_is_exact_below_a_second", func(t *testing.T) { brokerActionExpiryIsExactBelowASecond(t, h) })
	t.Run("Discarded_row_is_replaced_when_the_task_resumes", func(t *testing.T) { brokerActionDiscardedRowIsReplaced(t, h) })
	t.Run("Stuck_ages_from_the_claim_not_the_approval", func(t *testing.T) { brokerActionStuckAgesFromTheClaim(t, h) })
}

type brokerActionHarness struct {
	repo          persistence.BrokerActionRepository
	ctx           context.Context
	now           time.Time
	setTaskStatus func(t *testing.T, taskID, projectID, status string)
}

func (h *brokerActionHarness) stage(t *testing.T, taskID, status, args string) (*persistence.BrokerAction, bool) {
	t.Helper()
	a := &persistence.BrokerAction{
		ActionID: uniqueID("bact"), ProjectID: "broker-p", TaskID: taskID, APIKeyID: "akey-1",
		WorkflowID: "mail-reply", ActionKind: "reply", Tool: "mcp__w__send",
		ArgsJSON: []byte(args), ArgsSHA256: "sha-" + args, Status: status,
		CreatedAt: h.now, ExpiresAt: h.now.Add(24 * time.Hour),
	}
	replaced, err := h.repo.Stage(h.ctx, a)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	return a, replaced
}

func (h *brokerActionHarness) only(t *testing.T, taskID string) *persistence.BrokerAction {
	t.Helper()
	rows, err := h.repo.ListByTask(h.ctx, taskID)
	if err != nil || len(rows) != 1 {
		t.Fatalf("ListByTask(%s) = %d rows, %v; want 1", taskID, len(rows), err)
	}
	return rows[0]
}

func brokerActionMisscontract(t *testing.T, h *brokerActionHarness) {
	AssertMissRepo(t, "BrokerActionRepository.Get", h.repo.Get)
}

func brokerActionStageReplacesOnlyUnapprovableRows(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	h.stage(t, task, persistence.BrokerActionProposalMissing, `{}`)
	_, replaced := h.stage(t, task, persistence.BrokerActionStaged, `{"v":1}`)
	if !replaced {
		t.Fatal("a proposal_missing row must be replaceable by a retry's staged proposal")
	}
	h.stage(t, task, persistence.BrokerActionStaged, `{"v":2}`)
	if got := h.only(t, task); string(got.ArgsJSON) != `{"v":2}` || got.Status != persistence.BrokerActionStaged {
		t.Fatalf("staged row not replaced: %s %s", got.Status, got.ArgsJSON)
	}
	if n, err := h.repo.PromoteStaged(h.ctx, task); err != nil || n != 1 {
		t.Fatalf("PromoteStaged = %d, %v", n, err)
	}
	// First VISIBLE proposal wins: a pending row is never replaced.
	_, replaced = h.stage(t, task, persistence.BrokerActionStaged, `{"v":3}`)
	if replaced {
		t.Fatal("a pending row must not be replaced")
	}
	got := h.only(t, task)
	if string(got.ArgsJSON) != `{"v":2}` || got.ArgsSHA256 != `sha-{"v":2}` || got.Status != persistence.BrokerActionPending {
		t.Fatalf("pending row changed: %s %s %s", got.Status, got.ArgsJSON, got.ArgsSHA256)
	}
}

func brokerActionStageRefusesAnApprovableStatus(t *testing.T, h *brokerActionHarness) {
	a := &persistence.BrokerAction{ActionID: uniqueID("bact"), ProjectID: "p", TaskID: uniqueID("task"),
		ActionKind: "k", Tool: "mcp__w__send", Status: persistence.BrokerActionPending, CreatedAt: h.now, ExpiresAt: h.now}
	if _, err := h.repo.Stage(h.ctx, a); err == nil {
		t.Fatal("Stage must refuse to write a pending row directly")
	}
}

func brokerActionApproveIsBoundToTheShownHashAndExpiry(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"a"}`)
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("a staged row must not be approvable, got %v", err)
	}
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	if err := h.repo.Approve(h.ctx, a.ActionID, "sha-something-else", "op", h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("a hash mismatch must refuse, got %v", err)
	}
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now.Add(25*time.Hour)); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("an expired row must refuse, got %v", err)
	}
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	got, _ := h.repo.Get(h.ctx, a.ActionID)
	if got.Status != persistence.BrokerActionApproved || got.Approver != "op" || got.DecidedAt == nil {
		t.Fatalf("approved row = %+v", got)
	}
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op2", h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("a second approve must refuse, got %v", err)
	}
}

func brokerActionExecutionHasOneWinnerAndRecordsTheOutcome(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"b"}`)
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now); err != nil {
		t.Fatal(err)
	}
	var wins int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			won, err := h.repo.ClaimForExecution(h.ctx, a.ActionID, h.now)
			if err != nil {
				t.Errorf("Claim: %v", err)
			}
			if won {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d claims won; exactly one may execute", wins)
	}
	if err := h.repo.Finish(h.ctx, a.ActionID, persistence.BrokerActionExecuted, persistence.BrokerOutcomeOK, []byte(`{"id":"m1"}`), h.now); err != nil {
		t.Fatalf("Finish: %v", err)
	}
	got, _ := h.repo.Get(h.ctx, a.ActionID)
	if got.Status != persistence.BrokerActionExecuted || got.OutcomeClass != persistence.BrokerOutcomeOK || got.ExecutedAt == nil || string(got.OutcomeJSON) != `{"id":"m1"}` {
		t.Fatalf("finished row = %+v", got)
	}
	if err := h.repo.Finish(h.ctx, a.ActionID, persistence.BrokerActionFailed, persistence.BrokerOutcomeToolError, nil, h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("a finished row must not finish again, got %v", err)
	}
	if err := h.repo.Finish(h.ctx, a.ActionID, persistence.BrokerActionApproved, persistence.BrokerOutcomeOK, nil, h.now); err == nil {
		t.Fatal("Finish must refuse a non-terminal target")
	}
}

func brokerActionResolveOnlyFromExecutingOrUnknown(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"c"}`)
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	if err := h.repo.Resolve(h.ctx, a.ActionID, persistence.BrokerActionExecuted, "op", []byte(`{"note":"x"}`), h.now); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("a pending row must not be resolvable, got %v", err)
	}
	_ = h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now)
	_, _ = h.repo.ClaimForExecution(h.ctx, a.ActionID, h.now)
	_ = h.repo.Finish(h.ctx, a.ActionID, persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout, nil, h.now)
	if err := h.repo.Resolve(h.ctx, a.ActionID, persistence.BrokerActionExecuted, "op", []byte(`{"note":"checked the mailbox"}`), h.now); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	got, _ := h.repo.Get(h.ctx, a.ActionID)
	if got.Status != persistence.BrokerActionExecuted || got.OutcomeClass != persistence.BrokerOutcomeOperatorResolved || len(got.OutcomeJSON) == 0 {
		t.Fatalf("resolved row = %+v", got)
	}
}

func brokerActionRejectAndExpire(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"d"}`)
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	if err := h.repo.Reject(h.ctx, a.ActionID, "op", h.now); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if won, _ := h.repo.ClaimForExecution(h.ctx, a.ActionID, h.now); won {
		t.Fatal("a rejected row must never be claimed")
	}
	task2 := uniqueID("task")
	b, _ := h.stage(t, task2, persistence.BrokerActionStaged, `{"to":"e"}`)
	_, _ = h.repo.PromoteStaged(h.ctx, task2)
	if n, err := h.repo.ExpireDue(h.ctx, h.now.Add(25*time.Hour)); err != nil || n < 1 {
		t.Fatalf("ExpireDue = %d, %v", n, err)
	}
	got, _ := h.repo.Get(h.ctx, b.ActionID)
	if got.Status != persistence.BrokerActionExpired {
		t.Fatalf("status = %s, want expired", got.Status)
	}
	if won, _ := h.repo.ClaimForExecution(h.ctx, b.ActionID, h.now); won {
		t.Fatal("an expired row must never be claimed")
	}
}

func brokerActionSweepsFollowTheTaskStatus(t *testing.T, h *brokerActionHarness) {
	completed, failed, running := uniqueID("task"), uniqueID("task"), uniqueID("task")
	h.setTaskStatus(t, completed, "broker-p", string(persistence.TaskStatusCompleted))
	h.setTaskStatus(t, failed, "broker-p", string(persistence.TaskStatusFailed))
	h.setTaskStatus(t, running, "broker-p", string(persistence.TaskStatusRunning))
	ac, _ := h.stage(t, completed, persistence.BrokerActionStaged, `{}`)
	af, _ := h.stage(t, failed, persistence.BrokerActionStaged, `{}`)
	ar, _ := h.stage(t, running, persistence.BrokerActionStaged, `{}`)
	if _, err := h.repo.PromoteStagedOfCompletedTasks(h.ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.DiscardStagedOrphans(h.ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{ac.ActionID: persistence.BrokerActionPending, af.ActionID: persistence.BrokerActionDiscarded, ar.ActionID: persistence.BrokerActionStaged} {
		got, _ := h.repo.Get(h.ctx, id)
		if got.Status != want {
			t.Errorf("%s: status %s, want %s", id, got.Status, want)
		}
	}
}

func brokerActionCountstuck(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"f"}`)
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	_ = h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now)
	rows, err := h.repo.CountStuck(h.ctx, h.now.Add(10*time.Minute), 15*time.Minute, 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	var approved int64
	for _, r := range rows {
		if r.Status == persistence.BrokerActionApproved {
			approved += r.Count
		}
	}
	if approved < 1 {
		t.Fatalf("an approved row older than 5 minutes must count as stuck: %+v", rows)
	}
	// The row-level predicate the doctor check uses agrees with the count,
	// on the row as this driver reads it back.
	got, err := h.repo.Get(h.ctx, a.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	if !persistence.BrokerActionIsStuck(got, h.now.Add(10*time.Minute)) {
		t.Fatalf("BrokerActionIsStuck disagrees with CountStuck at +10m: %+v", got)
	}
	if persistence.BrokerActionIsStuck(got, h.now.Add(time.Minute)) {
		t.Fatalf("an approved row one minute old is not stuck: %+v", got)
	}
}

func brokerActionListbystatus(t *testing.T, h *brokerActionHarness) {
	rows, err := h.repo.ListByStatus(h.ctx, "broker-p", persistence.BrokerActionPending, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Status != persistence.BrokerActionPending || r.ProjectID != "broker-p" {
			t.Fatalf("ListByStatus returned %+v", r)
		}
	}
}

// review-20260930-ac20: SQLite stores TEXT timestamps; a variable-width
// format ("…:00Z" vs "…:00.5Z") orders wrongly at a sub-second boundary, so
// an approval could land after its expiry.
func brokerActionExpiryIsExactBelowASecond(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	a := &persistence.BrokerAction{
		ActionID: uniqueID("bact"), ProjectID: "broker-p", TaskID: task, ActionKind: "reply", Tool: "mcp__w__send",
		WorkflowID: "mail-reply", ArgsJSON: []byte(`{}`), ArgsSHA256: "sha-exp", Status: persistence.BrokerActionStaged,
		CreatedAt: h.now, ExpiresAt: h.now, // expires on a whole second
	}
	if _, err := h.repo.Stage(h.ctx, a); err != nil {
		t.Fatal(err)
	}
	_, _ = h.repo.PromoteStaged(h.ctx, task)
	half := h.now.Add(500 * time.Millisecond)
	if err := h.repo.Approve(h.ctx, a.ActionID, "sha-exp", "op", half); !errors.Is(err, persistence.ErrBrokerActionNoTransition) {
		t.Fatalf("an approval half a second after expiry must be refused, got %v", err)
	}
	if n, err := h.repo.ExpireDue(h.ctx, half); err != nil || n < 1 {
		t.Fatalf("ExpireDue half a second after expiry = %d, %v", n, err)
	}
}

// review-20260930-ac20: a paused or waiting task's staged rows are
// discarded by the orphan sweep; when the task resumes and re-stages, the
// discarded row (never shown, never approvable) must be replaced, not block
// the new proposal on the unique key.
func brokerActionDiscardedRowIsReplaced(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task")
	h.setTaskStatus(t, task, "broker-p", string(persistence.TaskStatusPaused))
	h.stage(t, task, persistence.BrokerActionStaged, `{"v":1}`)
	if _, err := h.repo.DiscardStagedOrphans(h.ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.only(t, task); got.Status != persistence.BrokerActionDiscarded {
		t.Fatalf("status = %s, want discarded", got.Status)
	}
	if _, replaced := h.stage(t, task, persistence.BrokerActionStaged, `{"v":2}`); !replaced {
		t.Fatal("a resumed task's proposal must replace its discarded row")
	}
	if got := h.only(t, task); got.Status != persistence.BrokerActionStaged || string(got.ArgsJSON) != `{"v":2}` {
		t.Fatalf("row = %s %s", got.Status, got.ArgsJSON)
	}
}

// review-20260930-1334 F1: executing and unknown rows must age from the claim
// and from the terminal write, not from the approval. Aged from the approval,
// an action approved 20 minutes ago and claimed a minute ago (the worker was
// down) read as stuck while its call was in flight, and doctor handed the
// operator a resolve command for it.
func brokerActionStuckAgesFromTheClaim(t *testing.T, h *brokerActionHarness) {
	task := uniqueID("task-age")
	h.setTaskStatus(t, task, "broker-p", "COMPLETED")
	a, _ := h.stage(t, task, persistence.BrokerActionStaged, `{"to":"age"}`)
	if _, err := h.repo.PromoteStaged(h.ctx, task); err != nil {
		t.Fatal(err)
	}
	if err := h.repo.Approve(h.ctx, a.ActionID, a.ArgsSHA256, "op", h.now); err != nil {
		t.Fatal(err)
	}
	claimAt := h.now.Add(20 * time.Minute)
	if won, err := h.repo.ClaimForExecution(h.ctx, a.ActionID, claimAt); err != nil || !won {
		t.Fatalf("claim: %v %v", won, err)
	}
	stuckCount := func(at time.Time, status string) int64 {
		rows, err := h.repo.CountStuck(h.ctx, at, persistence.BrokerActionStuckAfter, persistence.BrokerActionApprovedStuckAfter)
		if err != nil {
			t.Fatal(err)
		}
		var n int64
		for _, r := range rows {
			if r.ProjectID == "broker-p" && r.Status == status {
				n += r.Count
			}
		}
		return n
	}
	stuck := func(at time.Time) bool {
		got, err := h.repo.Get(h.ctx, a.ActionID)
		if err != nil {
			t.Fatal(err)
		}
		return persistence.BrokerActionIsStuck(got, at)
	}
	base := stuckCount(claimAt.Add(time.Minute), persistence.BrokerActionExecuting)
	if stuck(claimAt.Add(time.Minute)) {
		t.Fatal("an action claimed a minute ago is stuck (aged from the approval)")
	}
	if !stuck(claimAt.Add(16 * time.Minute)) {
		t.Fatal("an action executing for 16 minutes is not stuck")
	}
	if n := stuckCount(claimAt.Add(16*time.Minute), persistence.BrokerActionExecuting); n != base+1 {
		t.Fatalf("CountStuck executing at claim+16m = %d, want %d", n, base+1)
	}
	unknownAt := claimAt.Add(time.Minute)
	if err := h.repo.Finish(h.ctx, a.ActionID, persistence.BrokerActionUnknown, persistence.BrokerOutcomeTimeout, []byte(`{}`), unknownAt); err != nil {
		t.Fatal(err)
	}
	if stuck(unknownAt.Add(time.Minute)) {
		t.Fatal("an action unknown for a minute is stuck")
	}
	if !stuck(unknownAt.Add(16 * time.Minute)) {
		t.Fatal("an action unknown for 16 minutes is not stuck")
	}
}
