package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
	"vornik.io/vornik/internal/postmortem"
)

// GitHub #299 (2026-10-08): a broker task's error class comes from the stored,
// closed failure class column, never from the hidden last_error text. Before
// the change, status.error_class, result.egress_error and task_explain's
// error_class substring-matched last_error ("budget" -> budget, "timed out"
// -> timeout), a one-bit oracle per keyword on text the key may not read
// (review-20261008-a4ef F5). Broker design §5.2, amendment 2026-10-08 (D1).

// failedBrokerTask arranges a FAILED mail-digest task with the given hidden
// text and stored class ("" leaves the column empty).
func failedBrokerTask(t *testing.T, srv *Server, taskRepo *mocks.MockTaskRepository, text, class string) {
	t.Helper()
	brokerTask(t, srv, taskRepo, persistence.TaskStatusFailed, "", text)
	get := taskRepo.GetFunc
	taskRepo.GetFunc = func(ctx context.Context, id string) (*persistence.Task, error) {
		task, err := get(ctx, id)
		if err == nil && class != "" {
			c := class
			task.LastErrorClass = &c
		}
		return task, err
	}
}

// brokerClassFrom calls tool on a FAILED broker task and returns the named
// class field.
func brokerClassFrom(t *testing.T, tool, field, text, class string) any {
	t.Helper()
	srv, keyRepo, taskRepo := newBrokerMCPServer(t)
	raw, _ := seedCompanionKey(t, keyRepo, "broker-acme", nil)
	failedBrokerTask(t, srv, taskRepo, text, class)
	// task_explain answers only on a daemon with a renderer; the broker view
	// returns before the renderer is used, so it never reaches logs.
	srv.explainRenderer = postmortem.NewRenderer(srv.taskRepo, nil, nil, nil, failingTailFetcher{t})
	out, isErr := callTool(t, srv, raw, tool, map[string]any{"task_id": "t1"})
	require.False(t, isErr, out)
	assert.NotContains(t, out, brokerSentinel)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &m), out)
	return m[field]
}

// The hidden texts that the old substring rules classed as budget or timeout.
var hiddenTexts = []string{"boom", "budget exceeded " + brokerSentinel, "context deadline exceeded", "step timed out", "TIMEOUT"}

func TestBrokerStatus_ErrorClassIgnoresHiddenText(t *testing.T) {
	for _, text := range hiddenTexts {
		got := brokerClassFrom(t, "status", "error_class", text, persistence.TaskFailureClassUnknown)
		assert.Equal(t, "failed", got, "text %q must not choose the class", text)
	}
}

func TestBrokerStatus_ErrorClassFromStoredClass(t *testing.T) {
	cases := map[string]string{
		persistence.TaskFailureClassTimeout:       "timeout",
		persistence.TaskFailureClassLeaseExpired:  "timeout",
		persistence.TaskFailureClassBudgetBlocked: "budget",
		persistence.TaskFailureClassToolError:     "failed",
		"SOMETHING_UNLISTED":                      "failed",
		// An empty column with "budget" in the text: the given-up guess
		// (review-20261008-d60d item 7).
		"": "failed",
	}
	for stored, want := range cases {
		got := brokerClassFrom(t, "status", "error_class", "budget exceeded, no keyword matters", stored)
		assert.Equal(t, want, got, "stored class %q", stored)
	}
}

// When the hidden text and the stored class disagree, the stored class wins
// (review-20261009-da9f item 1).
func TestBrokerStatus_StoredClassWinsOverContradictingText(t *testing.T) {
	assert.Equal(t, "timeout", brokerClassFrom(t, "status", "error_class", "budget exceeded", persistence.TaskFailureClassTimeout))
	assert.Equal(t, "budget", brokerClassFrom(t, "status", "error_class", "step timed out", persistence.TaskFailureClassBudgetBlocked))
}

func TestBrokerResult_EgressErrorIgnoresHiddenText(t *testing.T) {
	for _, text := range hiddenTexts {
		got := brokerClassFrom(t, "result", "egress_error", text, persistence.TaskFailureClassUnknown)
		assert.Equal(t, "failed", got, "text %q must not choose the class", text)
	}
	assert.Equal(t, "budget", brokerClassFrom(t, "result", "egress_error", "boom", persistence.TaskFailureClassBudgetBlocked))
	assert.Equal(t, "timeout", brokerClassFrom(t, "result", "egress_error", "boom", persistence.TaskFailureClassTimeout))
}

func TestTaskExplain_BrokerClassIgnoresHiddenText(t *testing.T) {
	for _, text := range hiddenTexts {
		got := brokerClassFrom(t, "task_explain", "error_class", text, persistence.TaskFailureClassUnknown)
		assert.Equal(t, "failed", got, "text %q must not choose the class", text)
	}
	assert.Equal(t, "budget", brokerClassFrom(t, "task_explain", "error_class", "boom", persistence.TaskFailureClassBudgetBlocked))
}
