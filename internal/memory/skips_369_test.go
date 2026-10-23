package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/llmspend"
)

// GitHub #369, outcome-quality LLD §S8.11 (rows 6a, 6b, 7a, 7d, 13a, 16e): a
// memory step that failed and was skipped or allowed past left no metric
// (and for 6a no log above Debug), so "the step ran and found nothing" and
// "the step's dependency is down" read the same. Each now counts
// vornik_memory_skipped_total{site,reason}.

func skipped(m *Metrics, site, reason string) float64 {
	return testutil.ToFloat64(m.SkippedTotal.WithLabelValues(site, reason))
}

// 6a: the inline titler's failure.
func TestWorker_InlineTitleFailureIsCounted(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		reason string
	}{
		{"upstream error", errors.New("503"), "llm_error"},
		{"deadline", fmt.Errorf("call: %w", context.DeadlineExceeded), "llm_timeout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := freshMetrics()
			titler := NewTitler(&titlerFakeProvider{replies: []titlerReply{{err: c.err}, {err: c.err}, {err: c.err}}}, "", llmspend.Disabled())
			titler.MaxAttempts = 1
			w := &Worker{titler: titler, logger: zerolog.Nop(), metrics: m}
			w.titleChunk(context.Background(), "c1", "p1", "some content to title")
			if got := skipped(m, skipSiteInlineTitle, c.reason); got != 1 {
				t.Fatalf("memory_skipped_total{inline_title,%s} = %v, want 1: the failed titler left no signal", c.reason, got)
			}
		})
	}
}

// 6a: the optional-work switch is a state, not a failure: not counted.
func TestWorker_InlineTitleOptionalWorkOffIsNotCounted(t *testing.T) {
	m := freshMetrics()
	titler := NewTitler(&titlerFakeProvider{replies: []titlerReply{{err: chat.ErrOptionalWorkDisabled}}}, "", llmspend.Disabled())
	titler.MaxAttempts = 1
	w := &Worker{titler: titler, logger: zerolog.Nop(), metrics: m}
	w.titleChunk(context.Background(), "c1", "p1", "some content to title")
	for _, r := range []string{"llm_error", "llm_timeout", "llm_cancelled"} {
		if got := skipped(m, skipSiteInlineTitle, r); got != 0 {
			t.Fatalf("optional work off counted as %s", r)
		}
	}
}

// 13a: a quarantine row that was not written is not counted as quarantined,
// and the failed write is counted.
func TestIngestArtifact_FailedQuarantineWriteIsNotCountedQuarantined(t *testing.T) {
	p, q, _, _, cleanup := newPipelineTestRig(t)
	defer cleanup()
	q.err = errors.New("disk full")
	p.UpdateGates("", nil, []string{"deny"})
	body := "deny " + strings.Repeat("word ", 30)
	if _, err := p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "researcher", "exec", body, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(p.cfg.Metrics.PipelineQuarantinedTotal.WithLabelValues("p", string(GatePolicyMatch))); got != 0 {
		t.Fatalf("pipeline_quarantined_total = %v for a row that was never written, want 0", got)
	}
	if got := skipped(p.cfg.Metrics, skipSiteQuarantineWrite, "error"); got != 1 {
		t.Fatalf("memory_skipped_total{quarantine_write,error} = %v, want 1", got)
	}
}

// 13a, the control: a written quarantine row still counts.
func TestIngestArtifact_WrittenQuarantineIsCounted(t *testing.T) {
	p, _, _, _, cleanup := newPipelineTestRig(t)
	defer cleanup()
	p.UpdateGates("", nil, []string{"deny"})
	body := "deny " + strings.Repeat("word ", 30)
	if _, err := p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "researcher", "exec", body, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got := testutil.ToFloat64(p.cfg.Metrics.PipelineQuarantinedTotal.WithLabelValues("p", string(GatePolicyMatch))); got != 1 {
		t.Fatalf("pipeline_quarantined_total = %v, want 1", got)
	}
}

// 6b: a dedup lookup that failed allows the chunk (as built) and says so.
func TestIngestArtifact_FailedDedupLookupIsCounted(t *testing.T) {
	p, _, _, mock, cleanup := newPipelineTestRig(t)
	defer cleanup()
	p.cfg.ChunkExists = func(context.Context, string, string) (bool, error) { return false, errors.New("db gone") }
	// The allowed chunk then fails at the indexer; the count happens at
	// the gate, before admission.
	mock.ExpectExec("INSERT INTO project_memory_chunks").WillReturnError(errors.New("stop here"))
	_, _ = p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "researcher", "exec", strings.Repeat("word ", 30), 0, "")
	if got := skipped(p.cfg.Metrics, skipSiteDedupLookup, "error"); got != 1 {
		t.Fatalf("memory_skipped_total{dedup_lookup,error} = %v, want 1: the allow-on-error was invisible", got)
	}
}

// 16e: a claim-audit lookup that failed lets the gate allow (as built) and
// is counted.
func TestIngestArtifact_FailedClaimAuditLookupIsCounted(t *testing.T) {
	p, _, _, mock, cleanup := newPipelineTestRig(t)
	defer cleanup()
	p.cfg.AuditLookup = func(context.Context, string, []Claim) ([]ClaimMatch, error) {
		return nil, errors.New("audit boom")
	}
	mock.ExpectExec("INSERT INTO project_memory_chunks").WillReturnError(errors.New("stop here"))
	body := "Ran `go test ./...` to verify the fix works end to end, " + strings.Repeat("word ", 20)
	_, _ = p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "researcher", "exec-1", body, 0, "")
	if got := skipped(p.cfg.Metrics, skipSiteClaimAudit, "lookup_error"); got != 1 {
		t.Fatalf("memory_skipped_total{claim_audit,lookup_error} = %v, want 1", got)
	}
}

// 7a: the inline classifier's failures are told apart by cause.
func TestClassifierSkipReason(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: %q", ErrClassifierUnparseable, "blah"), "unparseable"},
		{ErrClassifierNoChoices, "no_choices"},
		{context.DeadlineExceeded, "llm_timeout"},
		{context.Canceled, "llm_cancelled"},
		{errors.New("503"), "llm_error"},
	}
	for _, c := range cases {
		if got := classifierSkipReason(ctx, c.err); got != c.want {
			t.Errorf("classifierSkipReason(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestClassifier_UnrecognisedClassWrapsTheSentinel(t *testing.T) {
	c := NewClassifier(newClassifyProvider(titlerReply{content: "banana"}, titlerReply{content: "banana"}), "", llmspend.Disabled())
	_, err := c.Classify(context.Background(), "some content", "s.md", "researcher", "", "")
	if !errors.Is(err, ErrClassifierUnparseable) {
		t.Fatalf("err = %v, want ErrClassifierUnparseable", err)
	}
}

func TestPipeline_InlineClassifierFailureIsCounted(t *testing.T) {
	p, _, _, mock, cleanup := newPipelineTestRig(t)
	defer cleanup()
	cl := NewClassifier(newClassifyProvider(titlerReply{content: "banana"}, titlerReply{content: "banana"}), "", llmspend.Disabled())
	p.SetClassifier(cl, true)
	mock.ExpectExec("INSERT INTO project_memory_chunks").WillReturnError(errors.New("stop here"))
	_, _ = p.IngestArtifact(context.Background(), "p", "t", "a", "s.md", "no-such-role", "exec", strings.Repeat("word ", 30), 0, "")
	if got := skipped(p.cfg.Metrics, skipSiteClassifier, "unparseable"); got != 1 {
		t.Fatalf("memory_skipped_total{classifier,unparseable} = %v, want 1", got)
	}
}

// 7d: a degraded rerank is counted by its cause in the searcher.
func TestSearcher_DegradedRerankIsCountedByCause(t *testing.T) {
	cases := []struct {
		name   string
		reply  titlerReply
		reason string
	}{
		{"failed call", titlerReply{err: errors.New("upstream 503")}, RerankCauseLLM},
		{"unparsable scores", titlerReply{content: "not a score list"}, RerankCauseParse},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, mock, cleanup := newRepo(t)
			defer cleanup()
			m := freshMetrics()
			s := NewSearcher(Config{}, r, nil)
			s.setMetrics(m)
			s.SetReranker(&LLMReranker{Client: &titlerFakeProvider{replies: []titlerReply{c.reply}}})
			mock.ExpectQuery("ts_rank").
				WithArgs("p", "q", 15, "q").
				WillReturnRows(makeRR([]string{"a", "b", "c"}, []float64{0.9, 0.7, 0.5}))
			if _, err := s.SearchWithOptions(context.Background(), "p", "q", SearchOptions{Limit: 5, Rerank: true}); err != nil {
				t.Fatal(err)
			}
			if got := skipped(m, skipSiteRerank, c.reason); got != 1 {
				t.Fatalf("memory_skipped_total{rerank,%s} = %v, want 1: the degrade had no metric", c.reason, got)
			}
		})
	}
}
