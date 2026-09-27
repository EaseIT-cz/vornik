package memory

import (
	"bytes"
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
)

// Optional work switched off for a model is a STATE, not a failure (breaker
// design §5.3d, 2026-09-26). Incident: the slow-hardware bench arm, where the
// operator asked for non-essential LLM features to be switchable off on slow
// backends. The sweep of these callers found a Warn on every failed call, so a
// refusal treated as a failure would replace a timeout flood with a refusal
// flood, count false failures and back the loops off.

var refused = titlerReply{err: chat.ErrOptionalWorkDisabled}

func TestAttemptLoops_DoNotRetryARefusal(t *testing.T) {
	for _, c := range memoryCallers() {
		t.Run(c.name, func(t *testing.T) {
			p := &ctxRecordingProvider{}
			p.replies = []titlerReply{refused, refused, refused}
			start := time.Now()
			_ = c.run(p, time.Minute)
			if got := p.calls(); got != 1 {
				t.Fatalf("a refusal will be refused again; calls=%d, want 1", got)
			}
			if time.Since(start) > 300*time.Millisecond {
				t.Fatal("no backoff sleep before giving up on a refusal")
			}
		})
	}
}

func TestTitleBackfill_StopsAtTheFirstRefusalAndIsNotAFailure(t *testing.T) {
	bf, mock, cleanup := newBackfiller(t, []titlerReply{refused, {content: "never asked"}})
	defer cleanup()
	mock.ExpectQuery("SELECT id, project_id, source_name, content").
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "content"}).
			AddRow("c1", "p", "s", "alpha").AddRow("c2", "p", "s", "beta"))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))

	res, err := bf.BackfillBatch(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paused || res.Failed != 0 || res.Succeeded != 0 {
		t.Fatalf("want a paused batch with no failures, got %+v", res)
	}
	if n := bf.Titler.Client.(*titlerFakeProvider).calls.Load(); n != 1 {
		t.Fatalf("the batch must stop calling the model at the first refusal; calls=%d", n)
	}
}

func TestClassifyBackfill_StopsCallingTheModelButKeepsTheRoleMap(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{refused, {content: "research"}})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("p", 10, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "a.md", "dispatcher", "needs the model").
			AddRow("c2", "p", "b.md", "dispatcher", "also needs the model").
			AddRow("c3", "p", "c.md", "researcher", "the role map answers this"))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1)) // c3, role map
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("dispatcher", 2))

	res, err := bf.BackfillBatch(context.Background(), "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Paused || res.Failed != 0 || res.Succeeded != 1 {
		t.Fatalf("want paused, no failures, the role-map row classified; got %+v", res)
	}
	if n := bf.Classifier.Client.(*classifyFakeProvider).calls.Load(); n != 1 {
		t.Fatalf("one refusal stops the model calls for the rest of the batch; calls=%d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestBackfillTick_PauseIsNotEvidenceAndLogsOncePerTransition(t *testing.T) {
	var buf bytes.Buffer
	var paused bool
	batch := func(p bool) func(context.Context, int) (backfillCounts, error) {
		return func(context.Context, int) (backfillCounts, error) {
			return backfillCounts{Processed: 1, Paused: p, Remaining: 1}, nil
		}
	}
	h := func(p bool) backfillTickHooks {
		hk := newBackfillHooks("title", zerolog.New(&buf), nil,
			func(context.Context) (int, error) { return 1, nil }, batch(p))
		hk.paused = &paused
		return hk
	}
	for i := 0; i < 3; i++ {
		if _, evidence := runBackfillTick(context.Background(), 1, h(true)); evidence {
			t.Fatal("a paused tick says nothing about the model and must not drive backoff")
		}
	}
	if n := strings.Count(buf.String(), "paused"); n != 1 {
		t.Fatalf("pause logged %d times over three paused ticks, want 1:\n%s", n, buf.String())
	}
	if _, evidence := runBackfillTick(context.Background(), 1, h(false)); !evidence {
		t.Fatal("a normal tick after resuming is evidence again")
	}
	if n := strings.Count(buf.String(), "resumed"); n != 1 {
		t.Fatalf("resume logged %d times, want 1:\n%s", n, buf.String())
	}
}

func TestConsolidateWorker_ARefusalEndsTheTickQuietly(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer func() { _ = db.Close() }()
	var buf bytes.Buffer
	fp := &titlerFakeProvider{replies: []titlerReply{refused, {content: "not reached"}}}
	w := &LLMConsolidateWorker{
		Writer:   NewNarrativeWriter(fp, "", llmspend.Disabled()),
		Repo:     NewRepository(db),
		Projects: &stubProjectLister{ids: []string{"p1", "p2"}},
		Interval: time.Hour,
		Logger:   zerolog.New(&buf),
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT project_id, terms_json, chunks_scanned")).
		WithArgs("p1").
		WillReturnRows(sqlmock.NewRows([]string{
			"project_id", "terms_json", "chunks_scanned", "generated_at",
			"duration_ms", "narrative", "narrative_model", "narrative_generated_at",
		}).AddRow("p1", `[{"Term":"x","Count":1}]`, 10, time.Now(), 1, nil, nil, nil))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT content")).
		WithArgs("p1", 8).
		WillReturnRows(sqlmock.NewRows([]string{"content"}).AddRow("y"))

	w.tick(context.Background())

	if n := fp.calls.Load(); n != 1 {
		t.Fatalf("every project would be refused alike; the tick must end at the first; calls=%d", n)
	}
	if strings.Contains(buf.String(), `"level":"warn"`) {
		t.Fatalf("a refusal is not a failure and must not warn:\n%s", buf.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestReranker_LogsARefusalAtDebug(t *testing.T) {
	var buf bytes.Buffer
	p := &titlerFakeProvider{replies: []titlerReply{refused}}
	rr := &LLMReranker{Client: p, Logger: zerolog.New(&buf)}
	in := []SearchResult{{ChunkID: "a"}, {ChunkID: "b"}}
	out, err := rr.Rerank(context.Background(), "q", in)
	if !errors.Is(err, ErrRerankDegraded) || len(out) != 2 {
		t.Fatalf("a refused rerank keeps the RRF order and reports the degrade: %v %v", out, err)
	}
	if strings.Contains(buf.String(), `"level":"warn"`) {
		t.Fatalf("a refusal is a state and logs at Debug, not Warn:\n%s", buf.String())
	}
}

func TestIngestInlineFallback_LogsARefusalAtDebug(t *testing.T) {
	p, _, _, mock, cleanup := newPipelineTestRig(t)
	defer cleanup()
	var buf bytes.Buffer
	p.logger = zerolog.New(&buf)
	p.SetClassifier(NewClassifier(newClassifyProvider(refused, refused), "", llmspend.Disabled()), true)
	mock.ExpectExec("INSERT INTO project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO memory_embed_queue").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("UPDATE project_memory_chunks").WillReturnResult(sqlmock.NewResult(0, 0))

	stats, err := p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "dispatcher", "exec", strings.Repeat("word ", 30), 0, "")
	if err != nil || stats.Admitted != 1 {
		t.Fatalf("the chunk still ingests, unclassified: %+v %v", stats, err)
	}
	if strings.Contains(buf.String(), `"level":"warn"`) {
		t.Fatalf("a refusal logs at Debug, not Warn:\n%s", buf.String())
	}
}

// The cross-project cursor ADVANCES past a page nothing classified (rag-ingest
// pipeline design, correction 2026-09-26). Incident: the 2026-07-30 livelock,
// whose fix documented this advance but never implemented it on the
// cross-project sweep the auto-loop runs; its test incremented the field by
// hand. This drives two sweeps through the repository instead.
func TestClassifyAcrossProjects_CursorAdvancesPastAnAllAbstainedPage(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{
		{content: "unclassified"}, {content: "unclassified"},
	})
	defer cleanup()
	page := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "a.md", "dispatcher", "one").
			AddRow("c2", "p", "b.md", "dispatcher", "two")
	}
	mock.ExpectQuery("SELECT id, project_id").WithArgs(2, 0).WillReturnRows(page())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	if _, err := bf.BackfillBatchAcrossProjects(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery("SELECT id, project_id").WithArgs(2, 2).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	mock.ExpectQuery("SELECT id, project_id").WithArgs(2, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}))
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	if _, err := bf.BackfillBatchAcrossProjects(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("the second sweep must read from offset 2, past the abstained page: %v", err)
	}
}

// A PAUSED page was never put to the model, so the cursor stays put.
func TestClassifyAcrossProjects_CursorStaysOnAPausedPage(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{refused})
	defer cleanup()
	page := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "a.md", "dispatcher", "one").
			AddRow("c2", "p", "b.md", "dispatcher", "two")
	}
	mock.ExpectQuery("SELECT id, project_id").WithArgs(2, 0).WillReturnRows(page())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	res, err := bf.BackfillBatchAcrossProjects(context.Background(), 2)
	if err != nil || !res.Paused {
		t.Fatalf("want a paused batch: %+v %v", res, err)
	}
	mock.ExpectQuery("SELECT id, project_id").WithArgs(2, 0).WillReturnRows(page())
	mock.ExpectQuery("SELECT COUNT").WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(2))
	if _, err := bf.BackfillBatchAcrossProjects(context.Background(), 2); err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("after a paused page the cursor must not move: %v", err)
	}
}

// The per-project sweep keeps its cursor on a paused page too.
func TestClassifyPerProject_CursorStaysOnAPausedPage(t *testing.T) {
	bf, mock, cleanup := newClassifyBackfiller(t, []titlerReply{refused})
	defer cleanup()
	mock.ExpectQuery("FROM project_memory_chunks").WithArgs("p", 2, 0).
		WillReturnRows(sqlmock.NewRows([]string{"id", "project_id", "source_name", "producer_role", "content"}).
			AddRow("c1", "p", "a.md", "dispatcher", "one").AddRow("c2", "p", "b.md", "dispatcher", "two"))
	mock.ExpectQuery("FROM project_memory_chunks").WillReturnRows(sqlmock.NewRows([]string{"role", "n"}).AddRow("dispatcher", 2))
	if _, err := bf.BackfillBatch(context.Background(), "p", 2); err != nil {
		t.Fatal(err)
	}
	if got := bf.projectOffsets["p"]; got != 0 {
		t.Fatalf("a paused page must not advance the per-project cursor; offset=%d", got)
	}
}

// The tick records the paused outcome positively (review-20260925-a332 #5).
func TestBackfillTick_CountsThePausedOutcome(t *testing.T) {
	var got []string
	h := newBackfillHooks("classify", zerolog.Nop(), nil,
		func(context.Context) (int, error) { return 1, nil },
		func(context.Context, int) (backfillCounts, error) {
			return backfillCounts{Processed: 1, Paused: true}, nil
		})
	h.tickOutcome = func(o string) { got = append(got, o) }
	runBackfillTick(context.Background(), 1, h)
	if len(got) != 1 || got[0] != "paused" {
		t.Fatalf("tick outcomes = %v, want [paused]", got)
	}
}
