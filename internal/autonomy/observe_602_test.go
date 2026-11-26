package autonomy

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #602, design 11 amendment 2026-11-20 section B: in observe mode a
// tick that passes every gate records WOULD_START with the prompt it would
// have submitted, and starts nothing. Repeats are checked against earlier
// WOULD_START rows (B3), and the budget gate reserves and settles under
// observe-<task ID> so it decides as act mode does (B2).

// listingEvalRepo records rows and answers List the way the real
// repositories do: filtered by project and outcome, newest first.
type listingEvalRepo struct {
	mu      sync.Mutex
	rows    []*persistence.AutonomyEvaluation
	listErr error
}

func (r *listingEvalRepo) Record(_ context.Context, e *persistence.AutonomyEvaluation) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *e
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *listingEvalRepo) List(_ context.Context, f persistence.AutonomyEvaluationFilter) ([]*persistence.AutonomyEvaluation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []*persistence.AutonomyEvaluation
	for i := len(r.rows) - 1; i >= 0; i-- {
		e := r.rows[i]
		if f.ProjectID != nil && e.ProjectID != *f.ProjectID {
			continue
		}
		if f.Outcome != nil && e.Outcome != *f.Outcome {
			continue
		}
		out = append(out, e)
		if f.PageSize > 0 && len(out) == f.PageSize {
			break
		}
	}
	return out, nil
}

func (r *listingEvalRepo) CountByOutcome(context.Context, string, time.Time, time.Time) (map[string]int64, error) {
	return nil, nil
}

func (r *listingEvalRepo) LatestByProject(context.Context) (map[string]*persistence.AutonomyEvaluation, error) {
	return nil, nil
}

func (r *listingEvalRepo) outcomes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, e := range r.rows {
		out = append(out, e.Outcome)
	}
	return out
}

func (r *listingEvalRepo) last() *persistence.AutonomyEvaluation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rows[len(r.rows)-1]
}

// fakeLedger is a budget reservation ledger that admits or blocks as told
// and records what was reserved and settled.
type fakeLedger struct {
	mu         sync.Mutex
	block      bool
	reserveErr error
	settleErr  error
	reserved   []string
	settled    []string
}

func (l *fakeLedger) Reserve(_ context.Context, req persistence.ReserveRequest) (persistence.ReserveResult, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.reserveErr != nil {
		return persistence.ReserveResult{}, l.reserveErr
	}
	if l.block {
		return persistence.ReserveResult{Blocked: true, Period: "daily"}, nil
	}
	l.reserved = append(l.reserved, req.TaskID)
	return persistence.ReserveResult{Reserved: true}, nil
}

func (l *fakeLedger) SettleByTask(_ context.Context, taskID string, _ time.Time) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.settleErr != nil {
		return 0, l.settleErr
	}
	l.settled = append(l.settled, taskID)
	return 1, nil
}

func (l *fakeLedger) SweepTerminalAndStale(context.Context, time.Time, time.Time) (int64, error) {
	return 0, nil
}

func (l *fakeLedger) UnsettledSumByProject(context.Context, string) (float64, error) { return 0, nil }

func observeProject(observe bool) *registry.Project {
	return &registry.Project{
		ID: "p1",
		Autonomy: registry.ProjectAutonomy{
			Enabled: true,
			Observe: observe,
			Goal:    "keep the backlog moving",
		},
	}
}

const observeArgs = `{"prompt":"Summarise the open issues labelled P1"}`

func TestObserve602_RecordsWouldStartAndStartsNothing(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))

	id, err := m.createAutonomousTask(context.Background(), observeProject(true), observeArgs, time.Now())
	require.NoError(t, err)
	assert.Empty(t, id, "observe mode returns the suppressed contract: an empty ID and no error")
	assert.Empty(t, repo.createdTasks(), "observe mode must not create a task")
	require.Equal(t, []string{persistence.AutonomyOutcomeWouldStart}, evals.outcomes())
	row := evals.last()
	assert.Equal(t, "Summarise the open issues labelled P1", row.ProposedPrompt)
	assert.NotEmpty(t, row.PromptHash)
	assert.Nil(t, row.TaskID)
}

func TestObserve602_RequireApprovalCreatesNothing(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))
	p := observeProject(true)
	p.Autonomy.RequireApproval = true

	_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
	require.NoError(t, err)
	assert.Empty(t, repo.createdTasks(), "observe wins over requireApproval: nothing to approve")
	assert.Equal(t, []string{persistence.AutonomyOutcomeWouldStart}, evals.outcomes())
}

func TestObserve602_RepeatInsideTheWindowIsDuplicate(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))
	p := observeProject(true)

	for range 2 {
		_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
		require.NoError(t, err)
	}
	// The same work, worded with other case and spacing, is still a repeat.
	_, err := m.createAutonomousTask(context.Background(), p, `{"prompt":"  summarise the OPEN issues   labelled p1 "}`, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{
		persistence.AutonomyOutcomeWouldStart,
		persistence.AutonomyOutcomeDuplicate,
		persistence.AutonomyOutcomeDuplicate,
	}, evals.outcomes())

	// Different work is proposed.
	_, err = m.createAutonomousTask(context.Background(), p, `{"prompt":"Triage the new bug reports"}`, time.Now())
	require.NoError(t, err)
	assert.Equal(t, persistence.AutonomyOutcomeWouldStart, evals.last().Outcome)
	assert.Empty(t, repo.createdTasks())
}

func TestObserve602_CronStyleRecordsOnePerTick(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))
	p := observeProject(true)
	p.Autonomy.DuplicateWindow = "0s"

	for range 2 {
		_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
		require.NoError(t, err)
	}
	assert.Equal(t, []string{persistence.AutonomyOutcomeWouldStart, persistence.AutonomyOutcomeWouldStart}, evals.outcomes(),
		"a cron-style project has no duplicate check in either mode")
}

func TestObserve602_FailedWouldStartReadFailsClosed(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{listErr: errors.New("db down")}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))

	id, err := m.createAutonomousTask(context.Background(), observeProject(true), observeArgs, time.Now())
	require.Error(t, err)
	assert.Empty(t, id)
	assert.Equal(t, []string{persistence.AutonomyOutcomeDBError}, evals.outcomes())
}

func TestObserve602_ActModeIsUnchanged(t *testing.T) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil, WithEvaluationRepository(evals))

	id, err := m.createAutonomousTask(context.Background(), observeProject(false), observeArgs, time.Now())
	require.NoError(t, err)
	assert.NotEmpty(t, id)
	assert.Len(t, repo.createdTasks(), 1)
	assert.Equal(t, []string{persistence.AutonomyOutcomeCreated}, evals.outcomes())
	assert.Empty(t, evals.last().ProposedPrompt, "only WOULD_START carries the prompt")
}

func budgetedObserveManager(ledger *fakeLedger, spent float64) (*Manager, *mockTaskRepo, *listingEvalRepo, *registry.Project) {
	repo := &mockTaskRepo{}
	evals := &listingEvalRepo{}
	m := New(nil, &registry.Registry{}, repo, nil,
		WithEvaluationRepository(evals),
		WithLLMUsageRepository(&stubLLMUsageRepo{dailyCost: spent}),
		WithBudgetReservationRepository(ledger),
	)
	p := observeProject(true)
	p.Budget = registry.ProjectBudget{DailyHardUSD: 5, DailySoftUSD: 0.1}
	return m, repo, evals, p
}

func TestObserve602_BudgetReservesAndSettlesUnderAnObserveID(t *testing.T) {
	ledger := &fakeLedger{}
	// Spent is over the soft cap and under the hard cap: act mode's
	// Reserve admits it, so observe must too.
	m, repo, evals, p := budgetedObserveManager(ledger, 1)

	_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{persistence.AutonomyOutcomeWouldStart}, evals.outcomes())
	require.Len(t, ledger.reserved, 1)
	assert.True(t, strings.HasPrefix(ledger.reserved[0], "observe-"), "reserved under %q, want an observe- ID", ledger.reserved[0])
	assert.Equal(t, ledger.reserved, ledger.settled, "the booking must be settled at once")
	assert.Empty(t, repo.createdTasks())
}

func TestObserve602_BudgetBlockedAsActModeWouldBe(t *testing.T) {
	m, _, evals, p := budgetedObserveManager(&fakeLedger{block: true}, 4.9)

	_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{persistence.AutonomyOutcomeBudgetBlocked}, evals.outcomes())
}

func TestObserve602_LedgerErrorsFailOpen(t *testing.T) {
	m, _, evals, p := budgetedObserveManager(&fakeLedger{reserveErr: errors.New("ledger down")}, 1)
	_, err := m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{persistence.AutonomyOutcomeWouldStart}, evals.outcomes(), "a reserve error fails open, as in act mode")

	m, _, evals, p = budgetedObserveManager(&fakeLedger{settleErr: errors.New("settle failed")}, 1)
	_, err = m.createAutonomousTask(context.Background(), p, observeArgs, time.Now())
	require.NoError(t, err)
	assert.Equal(t, []string{persistence.AutonomyOutcomeWouldStart}, evals.outcomes(),
		"a failed settle is left to the stale sweep; the proposal still records")
}

func TestObserve602_BacklogItemStaysOpen(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  observe: true
  mode: "backlog"
  pollInterval: "1h"
`)
	m, repo, evalRepo, abs := seedBacklogTick(t, reg, "- [ ] fix the parser bug\n")
	project := reg.GetProject("p1")
	require.NotNil(t, project)
	require.True(t, project.Autonomy.Observe, "observe must decode from the project file")

	require.NoError(t, m.tickBacklog(context.Background(), project, time.Now()))
	assert.Empty(t, repo.createdTasks())
	got, err := os.ReadFile(abs)
	require.NoError(t, err)
	assert.Equal(t, "- [ ] fix the parser bug\n", string(got), "observe mode leaves the item open")
	entries := evalRepo.snapshot()
	require.NotEmpty(t, entries)
	assert.Equal(t, persistence.AutonomyOutcomeWouldStart, entries[len(entries)-1].Outcome)
}

func TestObserve602_MisspeltKeyIsRefused(t *testing.T) {
	reg := registryWithProject(t, "p1", `autonomy:
  enabled: true
  obsrve: true
  goal: "g"
`)
	assert.Nil(t, reg.GetProject("p1"), "the strict decoder must refuse a misspelt observe key")
}
