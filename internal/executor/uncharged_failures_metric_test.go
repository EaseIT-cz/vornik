package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/pricing"
	"vornik.io/vornik/internal/stepoutcome"
)

// model_health removes failures that were not the model's from its rate and
// reports them only in doctor text (model-health attribution design). During
// the 2026-09-17/18 agent-image outage that was the only place the signal
// lived, so a dashboard showed nothing. These pin the persistent counter
// (design amendment "the excluded failures as a metric").

func TestRecordFinalOutcome_CountsEveryUnchargedClass(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	for _, class := range stepoutcome.NotAttributableToModelClasses() {
		m.RecordFinalOutcome("coder", "m1", string(stepoutcome.Failed), class)
		assert.Equal(t, 1.0, testutil.ToFloat64(m.StepFailuresNotChargedToModelTotal.WithLabelValues("coder", "m1", class)), class)
	}
	assert.Equal(t, float64(len(stepoutcome.NotAttributableToModelClasses())),
		testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "m1", string(stepoutcome.Failed))),
		"every uncharged failure is also an outcome-counter failure: a subset, counted once")
}

func TestRecordFinalOutcome_ChargedAndEmptyClassesAreNotUncharged(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	m.RecordFinalOutcome("coder", "m1", string(stepoutcome.Failed), stepoutcome.ClassLLMCallFailed)
	m.RecordFinalOutcome("coder", "m1", string(stepoutcome.Failed), "")
	assert.Equal(t, 0, testutil.CollectAndCount(m.StepFailuresNotChargedToModelTotal))
	assert.Equal(t, 2.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", "m1", string(stepoutcome.Failed))))
}

func TestRecordFinalOutcome_OkNeverCountsAsUncharged(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	m.RecordFinalOutcome("coder", "m1", string(stepoutcome.OK), stepoutcome.ClassAgentMountUnusable)
	m.RecordFinalOutcome("coder", "m1", string(stepoutcome.PendingValidation), stepoutcome.ClassAgentMountUnusable)
	assert.Equal(t, 0, testutil.CollectAndCount(m.StepFailuresNotChargedToModelTotal),
		"neither ok nor pending_validation is a terminal failure")
}

// The scraped name and label set are what dashboards and the design's PromQL
// read; pin them, not only the struct field (implementation review e82c F4).
func TestStepFailuresNotChargedToModel_ScrapedNameAndLabels(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewMetrics(reg)
	m.RecordFinalOutcome("coder", "m1", string(stepoutcome.Failed), stepoutcome.ClassContainerStartFailed)
	families, err := reg.Gather()
	require.NoError(t, err)
	for _, f := range families {
		if f.GetName() != "vornik_executor_step_failures_not_charged_to_model_total" {
			continue
		}
		require.Len(t, f.GetMetric(), 1)
		got := map[string]string{}
		for _, l := range f.GetMetric()[0].GetLabel() {
			got[l.GetName()] = l.GetValue()
		}
		assert.Equal(t, map[string]string{"role": "coder", "model": "m1", "class": stepoutcome.ClassContainerStartFailed}, got)
		return
	}
	t.Fatal("vornik_executor_step_failures_not_charged_to_model_total not registered")
}

func TestRecordFinalOutcome_EmptyRoleOrModelCountsNothing(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	m.RecordFinalOutcome("", "m1", string(stepoutcome.Failed), stepoutcome.ClassAgentMountUnusable)
	m.RecordFinalOutcome("coder", "", string(stepoutcome.Failed), stepoutcome.ClassAgentMountUnusable)
	assert.Equal(t, 0, testutil.CollectAndCount(m.StepFailuresNotChargedToModelTotal))
	assert.Equal(t, 0, testutil.CollectAndCount(m.AgentStepOutcomesTotal))
}

// The model label goes through the same catalogue guard as the outcome
// counter, so cardinality stays bounded (review be12 F2/F3).
func TestRecordFinalOutcome_UnknownModelIsOtherOnTheUnchargedCounter(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	path := filepath.Join(t.TempDir(), "pricing.yaml")
	require.NoError(t, os.WriteFile(path, []byte("models:\n  known-model:\n    input: 1\n    output: 1\n"), 0o644))
	table, err := pricing.Load(path)
	require.NoError(t, err)
	m.SetModelCatalog(table)

	m.RecordFinalOutcome("coder", "some-unlisted-model", string(stepoutcome.Failed), stepoutcome.ClassContainerKilled)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StepFailuresNotChargedToModelTotal.WithLabelValues("coder", modelLabelOther, stepoutcome.ClassContainerKilled)))
	assert.Equal(t, 1, testutil.CollectAndCount(m.StepFailuresNotChargedToModelTotal))
	assert.Equal(t, 1.0, testutil.ToFloat64(m.AgentStepOutcomesTotal.WithLabelValues("coder", modelLabelOther, string(stepoutcome.Failed))),
		"the same collapsed model on both counters, so the subtraction joins")
}

// Through the executor: a finalized pending row carries its class; a swept row
// carries none and never counts here.
func TestUnchargedFailures_FinalizeCountsSweepDoesNot(t *testing.T) {
	m := NewMetrics(prometheus.NewRegistry())
	repo := newStubStepOutcomeRepo()
	e := &Executor{outcomeRepo: repo, metrics: m, logger: zerolog.Nop()}
	task := &persistence.Task{ID: "t1", ProjectID: "p1"}
	exec := &persistence.Execution{ID: "e1"}
	ctx := context.Background()

	e.recordStepOutcome(ctx, task, exec, "a", "consumer", "m1", string(stepoutcome.PendingValidation), "", "", nil, nil)
	e.finalizePendingOutcome(ctx, exec.ID, "a", string(stepoutcome.Failed), stepoutcome.ClassMissingPrerequisite, "input absent", nil)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StepFailuresNotChargedToModelTotal.WithLabelValues("consumer", "m1", stepoutcome.ClassMissingPrerequisite)))

	e.recordStepOutcome(ctx, task, exec, "b", "consumer", "m1", string(stepoutcome.PendingValidation), "", "", nil, nil)
	e.sweepPendingOutcomes(ctx, exec.ID, string(stepoutcome.Failed))
	assert.Equal(t, 1, testutil.CollectAndCount(m.StepFailuresNotChargedToModelTotal), "a swept row has no class and must not count")

	e.recordStepOutcome(ctx, task, exec, "c", "consumer", "m1", string(stepoutcome.Failed), stepoutcome.ClassAgentMountUnusable, "mount", nil, nil)
	assert.Equal(t, 1.0, testutil.ToFloat64(m.StepFailuresNotChargedToModelTotal.WithLabelValues("consumer", "m1", stepoutcome.ClassAgentMountUnusable)),
		"a direct terminal write carries its class")
}
