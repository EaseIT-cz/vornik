package api

// GitHub #440 (security audit 2026-10-17): GET .../tasks/{id}/logs and
// .../explain served the OPERATOR view of the held task log, unscrubbed, to a
// project-scoped key. A secret the write-time scan missed (or kept, under the
// detect action) sat in the execution's error_message and reached any holder
// of the project's key. Found at: review (code audit); a user observes it on
// the HTTP route, so these tests run the real AuthMiddleware and
// ProjectAuthMiddleware with a DB project key in front of the real project
// router. The admin-class caller keeps the operator view (process-spawn-law
// design, "Amendment 2026-10-21 (#439, #440)").

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/executor/livepubsub"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/postmortem"
	"vornik.io/vornik/internal/report"
)

// plantedToken440 is a GitHub-token-shaped value, built at run time so the
// source holds no literal credential.
var plantedToken440 = "ghp_" + strings.Repeat("Q7rT2xLm9K", 4)[:36]

const adminKey440 = "sk-admin-440"

// awsKey440 and slackKey440 are key-shaped values (built at run time) that the
// line scrubber catches when they follow a space, and escapedKeys440 puts each
// after a separator JSON escapes.
var (
	awsKey440      = "AKIA" + "IOSFODNN7EXAMPLQ"
	slackKey440    = "xoxb-" + "1234567890-abcdefghij"
	escapedKeys440 = map[string]string{
		"aws after newline":   "\n" + awsKey440,
		"slack after tab":     "\t" + slackKey440,
		"aws after less-than": "<" + awsKey440,
	}
)

// assertNoEscapedKey440 fails when any escapedKeys440 value reached body.
func assertNoEscapedKey440(t *testing.T, body string) {
	t.Helper()
	assert.NotContains(t, body, awsKey440, "a key after a JSON-escaped separator leaked")
	assert.NotContains(t, body, slackKey440, "a key after a JSON-escaped separator leaked")
}

type chain440 struct {
	handler    http.Handler
	projectKey string
	srv        *Server
}

// newChain440 builds the production middleware stack for /api/v1/projects/
// over a held-log fixture holding a failed task of project "alpha" whose
// error_message, last_error, step detail, tool output and hub event carry
// the planted token.
func newChain440(t *testing.T, opts ...ServerOption) *chain440 {
	t.Helper()
	f := newHeldLogFixture(50)
	f.addTask("task-440", "alpha", "exec-440", "step build failed\nexported GITHUB_TOKEN="+plantedToken440+"\nexit status 1")
	le := "provider said: bad credential " + plantedToken440
	f.tasks["task-440"].LastError = &le
	cls := persistence.TaskFailureClassToolError
	f.tasks["task-440"].LastErrorClass = &cls
	f.pub.Publish(context.Background(), "exec-440", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: "curl -H 'Authorization: Bearer " + plantedToken440 + "'"})
	// Adversarial review 2026-10-21 (W16-F, finding 1): a key at the start of
	// a line inside a payload. JSON marshalling turns the newline, tab or '<'
	// before it into \n, \t or <, so the key follows a letter and the
	// \b-anchored nets no longer match the marshalled line.
	// Security review 2026-10-21 (MEDIUM, parser differential): a key-shaped
	// secret used as an OBJECT KEY inside output_json, after a newline. The
	// leaf walk scrubbed values only, and the marshalled line hides the key
	// behind \n.
	f.pub.Publish(context.Background(), "exec-440", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "objkey", OutputJSON: json.RawMessage(`{"result:\n` + slackKey440 + `":1,"ok":"` + "fine" + `"}`)})
	for i, k := range escapedKeys440 {
		f.pub.Publish(context.Background(), "exec-440", livepubsub.KindToolCallFinished,
			livepubsub.ToolCallFinishedPayload{CallID: fmt.Sprintf("esc-%s", i), Err: "result:" + k})
	}

	outcomes := stubOutcomes{rows: []*persistence.ExecutionStepOutcome{
		{StepID: "build", Role: "coder", Outcome: "tool_error", ErrorDetail: "auth failed for " + plantedToken440},
	}}
	renderer := postmortem.NewRenderer(f.taskRepo(), f.execRepo(), outcomes, nil, f.reader())

	key, err := apikey.Generate("alpha")
	require.NoError(t, err)
	lookup := &multiKeyLookup{byHash: map[string]*persistence.APIKey{
		apikey.Hash(key): {ID: "akey-alpha", ProjectID: "alpha", KeyHash: apikey.Hash(key), CreatedAt: time.Now()},
	}}
	base := []ServerOption{
		WithLogger(zerolog.Nop()),
		WithTaskRepository(f.taskRepo()), WithExecutionRepository(f.execRepo()),
		WithLiveSubscriber(f.pub),
		WithExplainRenderer(renderer),
		WithAdminClassKeys(config.AdminConfig{Enabled: true, AllowedKeys: []string{adminKey440}}),
	}
	srv := NewServer(append(base, opts...)...)
	var h http.Handler = http.HandlerFunc(srv.apiV1ProjectsHandler)
	h = ProjectAuthMiddleware()(h)
	h = AuthMiddleware(AuthConfig{
		Enabled:       true,
		APIKeyLookup:  lookup,
		StaticAPIKeys: map[string][]string{adminKey440: nil},
	})(h)
	return &chain440{handler: h, projectKey: key, srv: srv}
}

func (c *chain440) get(t *testing.T, path, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+bearer)
	rec := httptest.NewRecorder()
	c.handler.ServeHTTP(rec, req)
	return rec
}

func TestTaskLogs440_ProjectKeyReadsScrubbedTail(t *testing.T) {
	c := newChain440(t)

	rec := c.get(t, "/api/v1/projects/alpha/tasks/task-440/logs", c.projectKey)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.NotContains(t, body, plantedToken440, "a project-scoped key read the planted secret")
	assertNoEscapedKey440(t, body)
	assert.Contains(t, body, report.WithheldLine)
	assert.Contains(t, body, "exit status 1", "lines with no secret are still served")

	// The operator view is unchanged for an admin-class key.
	rec = c.get(t, "/api/v1/projects/alpha/tasks/task-440/logs", adminKey440)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), plantedToken440, "the operator view must stay byte-identical")
}

func TestExplain440_ProjectKeySeesNoSecret(t *testing.T) {
	c := newChain440(t)

	rec := c.get(t, "/api/v1/projects/alpha/tasks/task-440/explain", c.projectKey)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	body := rec.Body.String()
	assert.NotContains(t, body, plantedToken440, "explain served the planted secret to a project-scoped key")
	assertNoEscapedKey440(t, body)
	var resp explainResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Inputs["log_tail"], "the log tail is still served, scrubbed")
	assert.Contains(t, resp.Inputs["log_tail"], report.WithheldLine)
	assert.Equal(t, report.WithheldLine, resp.Inputs["last_error"])

	rec = c.get(t, "/api/v1/projects/alpha/tasks/task-440/explain", adminKey440)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), plantedToken440, "the operator view must stay unchanged")
}

// Fail closed: with no scrubber a project key gets nothing, never the
// operator view.
func TestTaskLogs440_NoScrubberRefuses(t *testing.T) {
	c := newChain440(t)
	c.srv.liveLogsOnce.Do(func() {}) // freeze the state: no scrubber is built
	c.srv.liveLogsState = &liveLogState{}

	for _, path := range []string{"/api/v1/projects/alpha/tasks/task-440/logs", "/api/v1/projects/alpha/tasks/task-440/explain"} {
		rec := c.get(t, path, c.projectKey)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code, path)
		assert.NotContains(t, rec.Body.String(), plantedToken440, path)
		assert.Contains(t, rec.Body.String(), "LOG_SCRUBBER_UNAVAILABLE", path)
	}
}

// Redact before truncate: a token straddling the 512-byte payload cap leaves
// a fragment in the operator view that no detector matches; the scrubbed
// view scrubs the whole payload first and withholds the line.
func TestHeldTaskLog440_ScrubbedViewScrubsBeforeTheCap(t *testing.T) {
	f := newHeldLogFixture(10)
	f.addTask("t1", "p1", "e1", "")
	f.pub.Publish(context.Background(), "e1", livepubsub.KindToolCallFinished,
		livepubsub.ToolCallFinishedPayload{CallID: "c1", Err: strings.Repeat("a", 470) + " " + plantedToken440})

	raw, err := f.reader().TaskLogs(context.Background(), "t1", 100)
	require.NoError(t, err)
	require.Contains(t, raw, "ghp_", "the fixture must put a fragment under the cap")

	scrub, err := report.NewLineScrubber("")
	require.NoError(t, err)
	view, err := f.reader().ScrubbedView(scrub)
	require.NoError(t, err)
	out, err := view.TaskLogs(context.Background(), "t1", 100)
	require.NoError(t, err)
	assert.NotContains(t, out, "ghp_")
	assert.Equal(t, report.WithheldLine, out)

	_, err = f.reader().ScrubbedView(nil)
	assert.Error(t, err, "a nil scrubber must not yield a view")
}
