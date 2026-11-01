package executor

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// GitHub #596: the strategist's entry funnel is recorded by the daemon when
// the evidence gate passes the accepted output. Measurement only: a missing
// or malformed funnel, or a failed write, never fails the step.

type funnelRepoStub struct {
	rows []persistence.FunnelCandidate
	err  error
}

func (r *funnelRepoStub) Record(_ context.Context, rows []persistence.FunnelCandidate) error {
	if r.err != nil {
		return r.err
	}
	r.rows = append(r.rows, rows...)
	return nil
}
func (r *funnelRepoStub) Pending(context.Context, string, string) ([]persistence.FunnelCandidate, error) {
	return nil, nil
}
func (r *funnelRepoStub) SetOutcome(context.Context, string, string, string, int, float64, *float64, *float64) error {
	return nil
}

func passingEvidenceExecutor(funnel persistence.FunnelRepository) *Executor {
	repo := &evidenceAuditRepo{entries: []*persistence.ToolAuditEntry{
		{ToolName: "mcp__broker__get_account_summary", ToolOutput: `{"positions":[]}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"SPY","region":"us"}`, ToolOutput: `{"total":1,"trend":1,"momentum":0,"macro":0,"last_close":500,"sma50":490}`, Outcome: "ok"},
		{ToolName: "mcp__ta__scorecard", ToolInput: `{"symbol":"MSFT","region":"us"}`, ToolOutput: `{"total":2,"trend":1,"momentum":1,"macro":0,"last_close":400,"sma50":390}`, Outcome: "ok"},
	}}
	return &Executor{logger: zerolog.Nop(), workflows: &MockWorkflowResolver{projects: map[string]*registry.Project{"trade": evidenceProject()}},
		auditRepo: repo, tradingFunnelRepo: funnel}
}

func TestFunnel_RecordedWhenTheGatePasses_596(t *testing.T) {
	pinSession(t, true)
	funnel := &funnelRepoStub{}
	e := passingEvidenceExecutor(funnel)
	in := evidenceOutcome()
	in.StepID = "strategize_model_fallback"
	in.ResultBytes = []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[],"entry_funnel":[
	  {"symbol":"MSFT","intent":"open","status":"rejected","gate":"already_held","reason":"held","inputs":{"last_close":400,"atr14":8}},
	  {"symbol":"SHEL","intent":"open","status":"rejected","gate":"made_up","reason":"x","inputs":{}}]}`)
	v := e.tradingFloorParticipant(context.Background(), in)
	require.False(t, v.Refused, v.Reason)
	require.NoError(t, in.Err)
	require.Len(t, funnel.rows, 2)
	m := funnel.rows[0]
	assert.Equal(t, "exec-1", m.ExecutionID)
	assert.Equal(t, "t", m.TaskID)
	assert.Equal(t, "trade", m.ProjectID)
	assert.Equal(t, "strategize_model_fallback", m.StepID, "the accepted attempt's step id")
	assert.Equal(t, "held", m.Gate)
	assert.Equal(t, "already_held", m.GateRaw)
	require.NotNil(t, m.RefClose)
	assert.Equal(t, 400.0, *m.RefClose)
	require.NotNil(t, m.RefATR)
	assert.Equal(t, 8.0, *m.RefATR)
	assert.Equal(t, "other", funnel.rows[1].Gate)
	assert.Nil(t, funnel.rows[1].RefClose, "no inputs: no reference close")
	assert.False(t, m.ObservedAt.IsZero())
}

func TestFunnel_NeverFailsTheStep_596(t *testing.T) {
	pinSession(t, true)
	for name, tc := range map[string]struct {
		result string
		repo   *funnelRepoStub
	}{
		"no funnel":   {`{"proposals":[],"has_proposals":false,"holdings_review":[]}`, &funnelRepoStub{}},
		"write fails": {`{"proposals":[],"has_proposals":false,"holdings_review":[],"entry_funnel":[{"symbol":"MSFT","intent":"open","status":"rejected","gate":"regime"}]}`, &funnelRepoStub{err: errors.New("db down")}},
		"funnel junk": {`{"proposals":[],"has_proposals":false,"holdings_review":[],"entry_funnel":"nope"}`, &funnelRepoStub{}},
	} {
		t.Run(name, func(t *testing.T) {
			e := passingEvidenceExecutor(tc.repo)
			in := evidenceOutcome()
			in.ResultBytes = []byte(tc.result)
			v := e.tradingFloorParticipant(context.Background(), in)
			assert.False(t, v.Refused, v.Reason)
			assert.NoError(t, in.Err)
		})
	}
}

func (r *funnelRepoStub) Since(context.Context, string, time.Time) ([]persistence.FunnelCandidate, error) {
	return nil, nil
}

// outcomeRepo is a funnel store that holds pending candidates and records
// the outcomes written.
type outcomeRepo struct {
	funnelRepoStub
	pending []persistence.FunnelCandidate
	set     map[string][3]float64 // key "exec/i" -> ret, mfe, mae
}

func (r *outcomeRepo) Pending(context.Context, string, string) ([]persistence.FunnelCandidate, error) {
	return r.pending, nil
}

func (r *outcomeRepo) SetOutcome(_ context.Context, exec, _, _ string, i int, ret float64, mfe, mae *float64) error {
	if r.set == nil {
		r.set = map[string][3]float64{}
	}
	v := [3]float64{ret, -999, -999}
	if mfe != nil {
		v[1] = *mfe
	}
	if mae != nil {
		v[2] = *mae
	}
	r.set[exec+"/"+string(rune('0'+i))] = v
	return nil
}

func ny(m time.Month, d, h int) time.Time {
	tz, _ := time.LoadLocation("America/New_York")
	return time.Date(2026, m, d, h, 0, 0, 0, tz)
}

// A candidate observed Thu 2026-10-29 gets its 1- and 3-session outcomes on
// Wed 2026-11-04 after the close (sessions 30, 2, 3, 4 are final); the
// 5-session one stays pending. One without an ATR gets the return only.
func TestFunnelOutcomes_FilledFromPrewarmBars_596(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	repo := &outcomeRepo{pending: []persistence.FunnelCandidate{
		{ExecutionID: "e1", Symbol: "MSFT", Intent: "open", ObservedAt: ny(10, 29, 14), RefClose: f(100), RefATR: f(2)},
		{ExecutionID: "e2", Symbol: "MSFT", Intent: "open", ObservedAt: ny(10, 29, 15), RefClose: f(100)},
	}}
	e := &Executor{logger: zerolog.Nop(), tradingFunnelRepo: repo}
	bars := []barClose{
		{TS: ny(10, 29, 0), High: 101, Low: 99, Close: 100},
		{TS: ny(10, 30, 0), High: 104, Low: 98, Close: 103},
		{TS: ny(11, 2, 0), High: 106, Low: 101, Close: 105},
		{TS: ny(11, 3, 0), High: 102, Low: 95, Close: 96},
		{TS: ny(11, 4, 0), High: 99, Low: 94, Close: 97},
	}
	e.fillFunnelOutcomes(context.Background(), "trade", "MSFT", bars, ny(11, 4, 17))
	assert.InDelta(t, 0.03, repo.set["e1/0"][0], 1e-9)
	assert.Equal(t, [2]float64{2, -1}, [2]float64{repo.set["e1/0"][1], repo.set["e1/0"][2]})
	assert.InDelta(t, -0.04, repo.set["e1/1"][0], 1e-9)
	_, five := repo.set["e1/2"]
	assert.False(t, five, "the 5th session has not happened: pending")
	assert.InDelta(t, 0.03, repo.set["e2/0"][0], 1e-9)
	assert.Equal(t, [2]float64{-999, -999}, [2]float64{repo.set["e2/0"][1], repo.set["e2/0"][2]}, "no ATR: return only")
}

type benchRepoStub struct{ got []persistence.BenchmarkClose }

func (r *benchRepoStub) Record(_ context.Context, c []persistence.BenchmarkClose) error {
	r.got = append(r.got, c...)
	return nil
}
func (r *benchRepoStub) Range(context.Context, string, time.Time, time.Time) ([]persistence.BenchmarkClose, error) {
	return nil, nil
}

// GitHub #597: a benchmark symbol's completed sessions are recorded from the
// pre-warm's bars; the in-progress session is not.
func TestBenchmarkCloses_RecordedFromPrewarmBars_597(t *testing.T) {
	repo := &benchRepoStub{}
	e := &Executor{logger: zerolog.Nop(), tradingBenchmarkRepo: repo}
	bars := []barClose{
		{TS: ny(time.October, 29, 0), Close: 580},
		{TS: ny(time.October, 30, 0), Close: 583},
	}
	e.recordBenchmarkCloses(context.Background(), []string{"SPY"}, "SPY", bars, ny(time.October, 30, 15))
	require.Len(t, repo.got, 1, "15:00 ET on the 30th: only the 29th is final")
	assert.Equal(t, 580.0, repo.got[0].Close)
	e.recordBenchmarkCloses(context.Background(), []string{"SPY"}, "MSFT", bars, ny(time.October, 30, 17))
	assert.Len(t, repo.got, 1, "MSFT is not a benchmark")
}
