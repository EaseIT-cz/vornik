package api

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/stepoutcome"
)

// GitHub #665 (model-health attribution design, amendment 2026-11-07). The
// Ollama Cloud account hit its weekly usage limit on 2026-11-06; every call to
// seven models failed with HTTP 429, and model_health called each of them
// "degenerate output (median 0 completion tokens)". The agent posts a usage
// row for every step, so a step whose calls all failed posts 0 prompt and 0
// completion tokens: the zeros were failures, not empty answers.

// A failing model whose usage rows are all failed calls: flagged for the
// failure rate, never for degenerate output.
func TestEvalModelHealth665_FailedCallsAreNotDegenerateOutput(t *testing.T) {
	stats := []modelHealthStat{{model: "glm-5.2", samples: 89, failures: 89}}
	findings := evalModelHealth(stats, oneFallback("glm-5.2", "zai.glm-5"), defaultDoctorThresholds())
	if len(findings) != 1 {
		t.Fatalf("want one finding, got %v", findings)
	}
	if strings.Contains(findings[0].message, "degenerate") {
		t.Fatalf("a model that never answered was called degenerate: %q", findings[0].message)
	}
	if !strings.Contains(findings[0].message, "89/89") {
		t.Fatalf("the failure rate must still be reported: %q", findings[0].message)
	}
}

// A healthy model with no qualifying usage rows is not judged degenerate:
// before #665 the zero value of the median read as "0 tokens".
func TestEvalModelHealth665_NoUsageRowsIsNoDegenerateVerdict(t *testing.T) {
	stats := []modelHealthStat{{model: "quiet-model", samples: 40, failures: 0}}
	if findings := evalModelHealth(stats, nil, defaultDoctorThresholds()); len(findings) != 0 {
		t.Fatalf("a healthy model without usage rows was flagged: %v", findings)
	}
}

// The signal the check exists for still fires: a model that is reached and
// answers with (almost) nothing.
func TestEvalModelHealth665_AReachedModelAnsweringNothingIsStillDegenerate(t *testing.T) {
	if modelHealthDegenerateTokens <= 3 {
		t.Fatalf("the fixture's median of 3 must sit below the threshold (%d)", modelHealthDegenerateTokens)
	}
	stats := []modelHealthStat{{
		model: "empty-model", samples: 40, failures: 0,
		hasTokenMedian: true, medianCompletionTokens: 3,
	}}
	findings := evalModelHealth(stats, nil, defaultDoctorThresholds())
	if len(findings) != 1 || !strings.Contains(findings[0].message, "degenerate output (median 3 completion tokens)") {
		t.Fatalf("a model answering near-empty must still be flagged degenerate: %v", findings)
	}
}

// A provider refusing the account is named as such.
func TestEvalModelHealth665_TheProvidersLimitIsNamed(t *testing.T) {
	stats := []modelHealthStat{{
		model: "glm-5.3-flash", samples: 40, failures: 40, rateLimited: 38,
	}}
	findings := evalModelHealth(stats, oneFallback("glm-5.3-flash", "zai.glm-5"), defaultDoctorThresholds())
	if len(findings) != 1 {
		t.Fatalf("want one finding, got %v", findings)
	}
	m := findings[0].message
	for _, want := range []string{"38 of these failures", "UPSTREAM_RATE_LIMITED", "not the model", `recommend switching to the configured modelFallback "zai.glm-5" (1 role)`} {
		if !strings.Contains(m, want) {
			t.Fatalf("finding lacks %q: %q", want, m)
		}
	}
}

func newModelHealth665DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE execution_step_outcomes (
		model TEXT, outcome TEXT, error_class TEXT, error_detail TEXT, recorded_at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE task_llm_usage (
		model TEXT, prompt_tokens INTEGER, completion_tokens INTEGER, recorded_at TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	return db
}

// The outcome query counts the charged failures that carry the proxy's code.
func TestQueryModelHealthOutcomes665_CountsTheProvidersLimit(t *testing.T) {
	db := newModelHealth665DB(t)
	now := time.Now().UTC()
	seed := func(n int, outcome, class, detail string) {
		for i := 0; i < n; i++ {
			if _, err := db.Exec(`INSERT INTO execution_step_outcomes (model, outcome, error_class, error_detail, recorded_at) VALUES (?,?,?,?,?)`,
				"glm-5.2", outcome, class, detail, now.Add(-time.Duration(i+1)*time.Minute)); err != nil {
				t.Fatal(err)
			}
		}
	}
	seed(38, "failed", stepoutcome.ClassLLMCallFailed,
		"agent reported FAILED status: LLM call failed: UPSTREAM_RATE_LIMITED: the model provider refused the call for a usage or rate limit")
	seed(2, "failed", stepoutcome.ClassLLMCallFailed, "agent reported FAILED status: LLM call failed: upstream provider returned an error")
	seed(5, "ok", "", "")
	// Not charged to the model, so not counted as the provider's limit either.
	seed(4, "failed", stepoutcome.ClassContainerStartFailed, "UPSTREAM_RATE_LIMITED in a log tail")

	stats, err := queryModelHealthOutcomes(context.Background(), db, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	s := stats["glm-5.2"]
	if s == nil || s.samples != 45 || s.failures != 40 || s.rateLimited != 38 {
		t.Fatalf("got %+v, want samples 45, failures 40, rateLimited 38", s)
	}
}

// The median reads only calls that reached the model, and an even count takes
// the mean of the middle two, as PERCENTILE_CONT(0.5) did.
func TestQueryModelHealthTokenMedians665_DropsFailedCalls(t *testing.T) {
	db := newModelHealth665DB(t)
	now := time.Now().UTC()
	add := func(model string, prompt, completion int) {
		if _, err := db.Exec(`INSERT INTO task_llm_usage (model, prompt_tokens, completion_tokens, recorded_at) VALUES (?,?,?,?)`,
			model, prompt, completion, now.Add(-time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 30; i++ {
		add("glm-5.2", 0, 0) // failed calls
	}
	add("glm-5.2", 1000, 100)
	add("glm-5.2", 1000, 300)
	add("empty-model", 900, 0) // reached, answered nothing
	add("dead-model", 0, 0)
	add("", 500, 50) // no model: ignored

	medians, err := queryModelHealthTokenMedians(context.Background(), db, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got, ok := medians["glm-5.2"]; !ok || got != 200 {
		t.Fatalf("glm-5.2 median = %v (present %v), want 200 from the two answered calls", got, ok)
	}
	if got, ok := medians["empty-model"]; !ok || got != 0 {
		t.Fatalf("a reached model answering nothing must keep its 0 median, got %v (present %v)", got, ok)
	}
	if _, ok := medians["dead-model"]; ok {
		t.Fatal("a model whose calls all failed has no median")
	}
	if _, ok := medians[""]; ok {
		t.Fatal("rows without a model are ignored")
	}
}
