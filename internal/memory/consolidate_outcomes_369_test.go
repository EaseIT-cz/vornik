package memory

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/llmspend"
)

// GitHub #369, outcome-quality LLD §S8.11 rows 7c and 13b: the narrative
// worker counted every failure "errored" (a dead model and a DB blip
// alike), and both consolidation workers counted a leader-fenced project
// "ok" and the tick "progressed", though nothing was written. A missing gist
// and an empty narrative also counted "ok".

func gistRow(pid string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"project_id", "terms_json", "chunks_scanned", "generated_at",
		"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
	}).AddRow(pid, `[{"Term":"x","Count":1}]`, 1, time.Now(), 1, nil, nil, nil)
}

func staleGate() *fenceGate {
	g := &fenceGate{}
	g.leader.Store(true)
	g.epochOK.Store(false)
	return g
}

func TestLLMConsolidateWorker_AFencedProjectIsCountedFenced(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m := freshMetrics()
	w := &LLMConsolidateWorker{
		Writer:     NewNarrativeWriter(&titlerFakeProvider{}, "", llmspend.Disabled()),
		Repo:       NewRepository(db),
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		Metrics:    m,
		LeaderGate: staleGate(),
	}
	w.tick(context.Background())
	if got := testutil.ToFloat64(m.NarrativeProjectsTotal.WithLabelValues("proj-a", "fenced")); got != 1 {
		t.Fatalf("narrative_projects_total{fenced} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.NarrativeTicksTotal.WithLabelValues("progressed")); got != 0 {
		t.Fatalf("a tick that wrote nothing counted progressed")
	}
	if got := testutil.ToFloat64(m.NarrativeTicksTotal.WithLabelValues("fenced")); got != 1 {
		t.Fatalf("narrative_ticks_total{fenced} = %v, want 1", got)
	}
}

func TestLLMConsolidateWorker_EachOutcomeHasItsOwnLabel(t *testing.T) {
	cases := []struct {
		name    string
		reply   titlerReply
		expect  func(sqlmock.Sqlmock)
		outcome string
	}{
		{"no gist yet", titlerReply{}, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
				WithArgs("p").WillReturnError(sqlmockNoRows)
		}, "no_gist"},
		{"the narrative model fails", titlerReply{err: errors.New("upstream 503")}, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
				WithArgs("p").WillReturnRows(gistRow("p"))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
				WithArgs("p", 8).WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("y"))
		}, "llm_error"},
		{"the gist read fails", titlerReply{}, func(mock sqlmock.Sqlmock) {
			mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
				WithArgs("p").WillReturnError(errors.New("connection refused"))
		}, "errored"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			db, mock, _ := sqlmock.New()
			defer func() { _ = db.Close() }()
			c.expect(mock)
			m := freshMetrics()
			writer := NewNarrativeWriter(&titlerFakeProvider{replies: []titlerReply{c.reply, c.reply, c.reply}}, "", llmspend.Disabled())
			writer.MaxAttempts = 1
			w := &LLMConsolidateWorker{
				Writer:   writer,
				Repo:     NewRepository(db),
				Projects: &stubProjectLister{ids: []string{"p"}},
				Interval: time.Hour,
				Logger:   zerolog.Nop(),
				Metrics:  m,
			}
			w.tick(context.Background())
			if got := testutil.ToFloat64(m.NarrativeProjectsTotal.WithLabelValues("p", c.outcome)); got != 1 {
				t.Fatalf("narrative_projects_total{%s} = %v, want 1", c.outcome, got)
			}
		})
	}
}

func TestConsolidateWorker_AFencedProjectIsCountedFenced(t *testing.T) {
	db, _, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	m := freshMetrics()
	repo := NewRepository(db)
	w := &ConsolidateWorker{
		Consolid:   NewConsolidator(repo),
		Repo:       repo,
		Projects:   &stubProjectLister{ids: []string{"proj-a"}},
		Interval:   time.Hour,
		Logger:     zerolog.Nop(),
		Metrics:    m,
		LeaderGate: staleGate(),
	}
	w.tick(context.Background())
	if got := testutil.ToFloat64(m.ConsolidateProjectsTotal.WithLabelValues("proj-a", "fenced")); got != 1 {
		t.Fatalf("consolidate_projects_total{fenced} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.ConsolidateTicksTotal.WithLabelValues("progressed")); got != 0 {
		t.Fatalf("a fenced tick counted progressed")
	}
	if got := testutil.ToFloat64(m.ConsolidateTicksTotal.WithLabelValues("fenced")); got != 1 {
		t.Fatalf("consolidate_ticks_total{fenced} = %v, want 1", got)
	}
}
