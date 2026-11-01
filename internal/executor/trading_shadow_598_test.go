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

// GitHub #598 step 1: the rule engine decides each tick in shadow, from the
// strategist step's own tool calls, and its decision is recorded beside the
// model's. Measurement only: it never fails the step.

type shadowRepoStub struct {
	rows []persistence.ShadowDecision
	err  error
}

func (r *shadowRepoStub) Record(_ context.Context, rows []persistence.ShadowDecision) error {
	if r.err != nil {
		return r.err
	}
	r.rows = append(r.rows, rows...)
	return nil
}
func (r *shadowRepoStub) Since(context.Context, string, time.Time) ([]persistence.ShadowDecision, error) {
	return nil, nil
}

func TestShadow_RecordsEngineBesideModel_598(t *testing.T) {
	pinSession(t, true)
	shadow := &shadowRepoStub{}
	e := passingEvidenceExecutor(&funnelRepoStub{})
	e.tradingShadowRepo = shadow
	e.tradingOrderRepo = &stubTradingOrderRepo{rows: []*persistence.TradingOrder{
		// MSFT was sold yesterday: inside the three-session cooldown.
		{ID: "x", IdempotencyKey: "kx", Symbol: "MSFT", Action: "SELL", FilledQty: 5, Status: "filled",
			SubmittedAt: evidenceNow().Add(-24 * time.Hour), TerminalAt: ptrTime(evidenceNow().Add(-24 * time.Hour))},
	}}
	in := evidenceOutcome()
	in.ResultBytes = []byte(`{"proposals":[],"has_proposals":false,"holdings_review":[],"entry_funnel":[
	  {"symbol":"MSFT","intent":"open","status":"rejected","gate":"scorecard_total","reason":"x"}]}`)
	v := e.tradingFloorParticipant(context.Background(), in)
	require.False(t, v.Refused, v.Reason)
	require.NoError(t, in.Err)
	require.Len(t, shadow.rows, 1, "one row per entry-universe symbol (the project allows MSFT only)")
	r := shadow.rows[0]
	assert.Equal(t, "MSFT", r.Symbol)
	assert.Equal(t, "cooldown", r.EngineGate)
	assert.Equal(t, "rejected", r.ModelStatus)
	assert.Equal(t, "scorecard", r.ModelGate, "the model's gate is normalised")
	assert.True(t, r.Agree, "both rejected; the reasons differ, which the report shows")
	assert.Equal(t, "exec-1", r.ExecutionID)
	assert.Equal(t, "trade", r.ProjectID)
}

func TestShadow_NeverFailsTheStep_598(t *testing.T) {
	pinSession(t, true)
	e := passingEvidenceExecutor(&funnelRepoStub{})
	e.tradingShadowRepo = &shadowRepoStub{err: errors.New("db down")}
	in := evidenceOutcome()
	v := e.tradingFloorParticipant(context.Background(), in)
	assert.False(t, v.Refused, v.Reason)
	assert.NoError(t, in.Err)
}

func ptrTime(t time.Time) *time.Time { return &t }

var _ = registry.Project{}
var _ = zerolog.Nop
