package api

// GitHub #496 (P2, security): the step routes GET /api/v1/executions/{id}/
// steps/{step}/prompt, /input, /result and /exchanges served a failed step's
// stored text (its result.json carries the error) as stored to every caller
// the project check admitted, outside the one per-access mapping of #491
// (ErrorTextAccessFor): an unscoped static key that is not admin-class read
// every project's step text, and a project key read secrets #466 scrubs from
// the same error in last_error. Found at: review (the #491 code review); an
// attacker reaches it on the HTTP route, so these tests run the real router
// (NewRouter: AuthMiddleware with the configured static keys, a DB project
// key, the project gate) over real SQLite stores (process-spawn-law design,
// "Amendment 2026-10-21 (#496)").

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/report"
)

// The planted step texts. Each holds the four #466 shapes (a token after
// "Bearer", an AWS key on its own line inside a leaf, a Slack key as an
// object key, a token after a literal escaped \n) and the clean line
// "exit status 1" that every scrubbed view keeps.
var (
	stepError496 = "push failed: Authorization: Bearer " + plantedToken440 + "\n" + awsKey440 +
		"\nupstream said: {\"error\":\"denied\\n" + escapedToken466 + "\"}\nexit status 1"

	stepResult496 = mustJSON496(map[string]any{
		"status": "FAILED", "attempts": 3,
		"error": map[string]any{"message": stepError496, slackKey440: 1},
	})
	stepInput496 = mustJSON496(map[string]any{
		"taskId":  "t466",
		"context": map[string]any{"prompt": "deploy with " + awsKey440 + "\nexit status 1", slackKey440: "x"},
	})
	stepUserPrompt496  = "Fix the push.\nAuthorization: Bearer " + plantedToken440 + "\nexit status 1"
	stepToolsPrompt496 = mustJSON496([]any{map[string]any{"name": "run_shell", "description": "uses " + awsKey440 + "\nexit status 1"}})
	stepExchangeReq496 = mustJSON496(map[string]any{"messages": []any{
		map[string]any{"role": "tool", "content": stepError496},
	}})
	stepExchangeResp496 = mustJSON496(map[string]any{"choices": []any{
		map[string]any{"finish_reason": "stop", "message": map[string]any{"content": "The token " + plantedToken440 + " was refused.\nexit status 1"}},
	}})
)

func mustJSON496(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

const (
	stepBase496 = "/api/v1/executions/e466/steps/push"
)

var stepPaths496 = []string{
	stepBase496 + "/prompt",
	stepBase496 + "/input",
	stepBase496 + "/result",
	stepBase496 + "/exchanges",
	stepBase496 + "/exchanges?format=jsonl",
}

// newStepHarness496 is the #491 harness (a DB project key for alpha, an
// admin-class static key, an unscoped static key that is not admin-class)
// with real SQLite outcome, prompt and exchange stores holding a failed
// step of execution e466 whose texts carry the planted secrets.
func newStepHarness496(t *testing.T) *harness466 {
	t.Helper()
	ctx := context.Background()
	db := sqlitetest.Memory(t)
	prompts := sqlite.NewStepPromptRepository(db.DB)
	outcomes := sqlite.NewExecutionStepOutcomeRepository(db.DB)
	exchanges := sqlite.NewLLMExchangeRepository(db.DB)

	save := func(part persistence.StepPromptPart, body string) string {
		h, err := prompts.Save(ctx, part, body)
		require.NoError(t, err)
		return h
	}
	exit := 1
	require.NoError(t, outcomes.Record(ctx, &persistence.ExecutionStepOutcome{
		ProjectID: "alpha", TaskID: "t466", ExecutionID: "e466", StepID: "push",
		Outcome: "failed", RecordedAt: time.Now().UTC(), ContainerExitCode: &exit,
		PromptHashes: persistence.StepPromptHashes{
			User:   save(persistence.StepPromptUser, stepUserPrompt496),
			Tools:  save(persistence.StepPromptTools, stepToolsPrompt496),
			Input:  save(persistence.StepPromptInput, stepInput496),
			Result: save(persistence.StepPromptResult, stepResult496),
		},
	}))
	require.NoError(t, exchanges.Record(ctx, &persistence.LLMExchange{
		ExecutionID: "e466", StepID: "push", RequestHash: "rh496", Model: "m",
		RequestJSON: stepExchangeReq496, ResponseJSON: stepExchangeResp496,
	}))
	// Code review round 1 (review-20261022-257f, finding 2): a body that is
	// not JSON stays null on the scrubbed view, so the envelope stays JSON.
	require.NoError(t, exchanges.Record(ctx, &persistence.LLMExchange{
		ExecutionID: "e466", StepID: "push", RequestHash: "rh496b", Model: "m",
		RequestJSON: "not json: " + awsKey440, ResponseJSON: "upstream 502 for Bearer " + plantedToken440,
	}))
	return newHarness466With(t, []string{staticKey491},
		WithStepPromptRepository(prompts),
		WithExecutionStepOutcomeRepository(outcomes),
		WithLLMExchangeRepository(exchanges))
}

// getWithHeaders496 is harness466.get that also returns the response headers.
func (h *harness466) getWithHeaders496(t *testing.T, path, bearer string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, h.hs.URL+path, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, rerr := resp.Body.Read(buf)
		b.Write(buf[:n])
		if rerr != nil {
			break
		}
	}
	return resp.StatusCode, resp.Header, b.String()
}

// decodedBody496 is every string of a JSON body (or of each JSON line of a
// jsonl body), decoded, so neither the escaping of the raw bytes nor a
// nested JSON text hides a planted value from the absence checks: a string
// leaf that is itself JSON is decoded too.
func decodedBody496(t *testing.T, path, body string) string {
	t.Helper()
	var b strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch tv := x.(type) {
		case string:
			b.WriteString(tv)
			b.WriteByte('\n')
			var inner any
			if json.Unmarshal([]byte(tv), &inner) == nil {
				walk(inner)
			}
		case map[string]any:
			for k, y := range tv {
				b.WriteString(k)
				b.WriteByte('\n')
				walk(y)
			}
		case []any:
			for _, y := range tv {
				walk(y)
			}
		}
	}
	docs := []string{body}
	if strings.Contains(path, "format=jsonl") {
		docs = strings.Split(strings.TrimSpace(body), "\n")
	}
	for _, d := range docs {
		var v any
		require.NoError(t, json.Unmarshal([]byte(d), &v), "%s: the view must stay valid JSON: %s", path, d)
		walk(v)
	}
	return b.String()
}

func TestStepRoutes496_ProjectKeyReadsScrubbedView(t *testing.T) {
	h := newStepHarness496(t)
	for _, p := range stepPaths496 {
		code, hdr, body := h.getWithHeaders496(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assertNoSecret466(t, p, body)
		text := decodedBody496(t, p, body)
		assertNoSecret466(t, p+" (decoded)", text)
		assert.Contains(t, text, "exit status 1", "%s: the clean line was lost", p)
		assert.Contains(t, text, report.WithheldLine, "%s: no line was withheld", p)
		assert.Equal(t, "scrubbed", hdr.Get("X-Vornik-View"), p)
		// Code review round 1: the stored bytes' hashes would let a caller
		// confirm a guess of a replaced low-entropy value; the scrubbed
		// view carries none.
		assert.Empty(t, hdr.Get("X-Vornik-Content-Hash"), p)
		assert.NotRegexp(t, `[0-9a-f]{64}`, body, "%s: a stored body's hash reached the scrubbed view", p)
		assert.NotContains(t, body, "rh496", "%s: the exchange's request hash reached the scrubbed view", p)
	}
}

func TestStepRoutes496_UnscopedStaticKeyRefused(t *testing.T) {
	h := newStepHarness496(t)
	check := func(where string) {
		for _, p := range stepPaths496 {
			code, _, body := h.getWithHeaders496(t, p, staticKey491)
			assert.Equal(t, http.StatusForbidden, code, "%s %s: %s", where, p, body)
			assert.Contains(t, body, "PROJECT_SCOPE_REQUIRED", "%s %s", where, p)
			assertNoErrorLine491(t, where+" "+p, body)
		}
		// A denied caller learns nothing of the execution: an unknown one
		// answers the same 403, not 404.
		code, _, _ := h.getWithHeaders496(t, "/api/v1/executions/e-nope/steps/push/result", staticKey491)
		assert.Equal(t, http.StatusForbidden, code, "%s: an unknown execution", where)
	}
	check("with a scrubber")
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}
	check("with no scrubber")
}

func TestStepRoutes496_NoScrubberRefusesScopedKey(t *testing.T) {
	h := newStepHarness496(t)
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}
	for _, p := range stepPaths496 {
		code, _, body := h.getWithHeaders496(t, p, h.projectKey)
		assert.Equal(t, http.StatusServiceUnavailable, code, "%s: %s", p, body)
		assert.Contains(t, body, "LOG_SCRUBBER_UNAVAILABLE", p)
		assertNoErrorLine491(t, p, body)
	}
}

func TestStepRoutes496_AdminKeyReadsStoredBytes(t *testing.T) {
	h := newStepHarness496(t)
	for p, want := range map[string]string{
		stepBase496 + "/input":  stepInput496,
		stepBase496 + "/result": stepResult496,
	} {
		code, hdr, body := h.getWithHeaders496(t, p, adminKey466)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assert.Equal(t, want, body, "%s: the operator view is the stored bytes", p)
		assert.Equal(t, "operator", hdr.Get("X-Vornik-View"), p)
	}
	for _, p := range []string{stepBase496 + "/prompt", stepBase496 + "/exchanges"} {
		code, hdr, body := h.getWithHeaders496(t, p, adminKey466)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedBody496(t, p, body)
		assert.Contains(t, text, plantedToken440, "%s: the operator view must stay unchanged", p)
		assert.NotContains(t, text, report.WithheldLine, p)
		assert.Equal(t, "operator", hdr.Get("X-Vornik-View"), p)
	}
}

// Design V5 (the local adversarial pass on the design): an execution's stored
// result is the last step's result.json, the bytes T3 serves, so GET
// execution and the execution list served the failed step's error raw to
// every admitted caller, the unscoped static key included, around T3's 403.
func TestExecutionResult496_FollowsTheMapping(t *testing.T) {
	h := newStepHarness496(t)
	require.NoError(t, h.srv.executionRepo.RecordCompletion(context.Background(), "e466", []byte(stepResult496)))
	paths := []string{"/api/v1/executions/e466", "/api/v1/projects/alpha/executions"}
	for _, p := range paths {
		code, body := h.get(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedBody496(t, p, body)
		assertNoSecret466(t, p, text)
		assert.Contains(t, text, "exit status 1", "%s: the clean line was lost", p)
		assert.Contains(t, text, report.WithheldLine, p)

		code, body = h.get(t, p, staticKey491)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text = decodedBody496(t, p, body)
		assertNoErrorLine491(t, p+" (unscoped key)", text)
		assert.Contains(t, text, UnscopedWithheldText, p)

		code, body = h.get(t, p, adminKey466)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		assert.Contains(t, decodedBody496(t, p, body), plantedToken440, "%s: the operator view must stay unchanged", p)
	}

	// Code review round 1 (review-20261022-257f, finding 1): with no
	// scrubber a scoped caller's read is still served, every line withheld
	// (#466 D1's structured-read rule), and the unscoped key reads the
	// constant.
	h.srv.liveLogsOnce.Do(func() {})
	h.srv.liveLogsState = &liveLogState{}
	for _, p := range paths {
		code, body := h.get(t, p, h.projectKey)
		require.Equal(t, http.StatusOK, code, "%s: %s", p, body)
		text := decodedBody496(t, p, body)
		assertNoSecret466(t, p+" (no scrubber)", text)
		assert.NotContains(t, text, "exit status 1", "%s: no scrubber must withhold every line", p)
		assert.Contains(t, text, report.WithheldLine, p)

		_, body = h.get(t, p, staticKey491)
		assert.Contains(t, decodedBody496(t, p, body), UnscopedWithheldText, p)
	}
}

// The scrubbed view of one stored body (design V2): JSON keeps its shape and
// its numbers as written, a secret object key is withheld, a non-JSON body
// is scrubbed by line, a top-level JSON string is scrubbed as text, and an
// earlier duplicate key's value is dropped, not served.
func TestScrubStoredText496(t *testing.T) {
	scrub, err := report.NewLineScrubber("")
	require.NoError(t, err)

	out, isJSON := ScrubStoredText(`{"n":12345678901234567890,"f":1.50,"s":"ok\nAuthorization: Bearer `+plantedToken440+`","`+slackKey440+`":true}`, scrub)
	require.True(t, isJSON)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	assert.Contains(t, out, `12345678901234567890`, "a number is kept as written")
	assert.Contains(t, out, `1.50`, "a number is kept as written")
	assert.Equal(t, "ok\n"+report.WithheldLine, m["s"])
	assert.Contains(t, m, report.WithheldLine, "a secret object key is withheld")
	assert.NotContains(t, out, plantedToken440)
	assert.NotContains(t, out, slackKey440)

	out, isJSON = ScrubStoredText("plain text\n"+awsKey440+"\nexit status 1", scrub)
	assert.False(t, isJSON)
	assert.Equal(t, "plain text\n"+report.WithheldLine+"\nexit status 1", out)

	// Two JSON values, or JSON followed by text, are not one document.
	out, isJSON = ScrubStoredText(`{"a":1} `+awsKey440, scrub)
	assert.False(t, isJSON)
	assert.NotContains(t, out, awsKey440)

	out, isJSON = ScrubStoredText(`"`+awsKey440+`"`, scrub)
	require.True(t, isJSON)
	assert.NotContains(t, out, awsKey440)
	var s string
	require.NoError(t, json.Unmarshal([]byte(out), &s), out)
	assert.Equal(t, report.WithheldLine, s)

	out, _ = ScrubStoredText(`{"k":"`+awsKey440+`","k":"clean"}`, scrub)
	assert.Equal(t, `{"k":"clean"}`, out)

	// An escaped HTML character is not re-escaped; the empty body stays empty.
	out, _ = ScrubStoredText(`{"a":"<b>"}`, scrub)
	assert.Equal(t, `{"a":"<b>"}`, out)
	out, _ = ScrubStoredText("", scrub)
	assert.Equal(t, "", out)

	// No scrubber withholds every line, keys included (ScrubLines'
	// fail-closed rule; the routes answer 503 before they get here).
	out, _ = ScrubStoredText(`{"a":"clean"}`, nil)
	assert.Equal(t, `{"`+report.WithheldLine+`":"`+report.WithheldLine+`"}`, out)
}
