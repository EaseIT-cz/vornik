package dispatcher

// GitHub #480 (P1, security; operator decision A5): get_task_status and
// wait_for_task gave the chat model a task's raw last_error and the
// execution's raw error_message on every channel. Found at: review (the #466
// adversarial code review, its open decision 1); an attacker reaches it
// through a channel's inbound turn, which the per-channel tests drive. These
// pin the dispatcher's own rule: the view is decided per turn and the zero
// value is scrubbed (process-spawn-law design, "Amendment 2026-10-21
// (#480)", J1 and J2).

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/dispatcher/dispatchertest"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
	"vornik.io/vornik/internal/report"
)

func agent480(t *testing.T, model *dispatchertest.StatusModel, scrub bool) *Agent {
	t.Helper()
	db := sqlitetest.Memory(t)
	tasks := sqlite.NewTaskRepository(db.DB)
	execs := sqlite.NewExecutionRepository(db.DB)
	dispatchertest.SeedFailedTask480(t, tasks, execs, "alpha")
	var opts []AgentOption
	if scrub {
		s, err := report.NewLineScrubber("")
		require.NoError(t, err)
		opts = append(opts, WithErrorTextScrubber(s))
	}
	return NewAgent(model.Provider(), tasks, execs, nil, nil, opts...)
}

func turn480(operatorView bool) Request {
	return Request{
		Messages:        []chat.Message{{Role: "user", Content: "why did the task fail?"}},
		Project:         "alpha",
		AllowedProjects: []string{"alpha"},
		OperatorView:    operatorView,
	}
}

func TestDispatcher480_UnclassifiedTurnIsScrubbed(t *testing.T) {
	model := dispatchertest.NewStatusModel(t)
	res := agent480(t, model, true).Process(context.Background(), turn480(false))
	require.NoError(t, res.Err)
	dispatchertest.AssertScrubbed(t, "a turn no channel classified", model.ToolResults())
}

func TestDispatcher480_OperatorViewReadsRaw(t *testing.T) {
	model := dispatchertest.NewStatusModel(t)
	res := agent480(t, model, true).Process(context.Background(), turn480(true))
	require.NoError(t, res.Err)
	dispatchertest.AssertRaw(t, "an operator-view turn", model.ToolResults())
}

func TestDispatcher480_StreamingTurnIsScrubbed(t *testing.T) {
	model := dispatchertest.NewStatusModel(t)
	res := agent480(t, model, true).ProcessStreaming(context.Background(), turn480(false), func(string) {})
	require.NoError(t, res.Err)
	dispatchertest.AssertScrubbed(t, "a streaming turn", model.ToolResults())
}

func TestDispatcher480_NoScrubberWithholdsEveryLine(t *testing.T) {
	model := dispatchertest.NewStatusModel(t)
	res := agent480(t, model, false).Process(context.Background(), turn480(false))
	require.NoError(t, res.Err)
	results := model.ToolResults()
	joined := strings.Join(results, "\n")
	for _, s := range dispatchertest.Secrets480() {
		assert.NotContains(t, joined, s)
	}
	assert.NotContains(t, joined, dispatchertest.CleanLine480, "with no scrubber every line is withheld, the clean one too")
	assert.Contains(t, joined, report.WithheldLine)
}

// The cut at 1500 bytes ran on the raw text: a token straddling it left a
// fragment that no net matches. Redact before truncate.
func TestWaitForTask480_ScrubsBeforeTheCut(t *testing.T) {
	tok := "ghp_" + strings.Repeat("Z5xV8bN1mQ", 4)[:36]
	le := strings.Repeat("a", 1500-20) + " Bearer " + tok
	repo := newPollingRepo(&persistence.Task{ID: "task_x", ProjectID: "p", Status: persistence.TaskStatusFailed, LastError: &le})
	s, err := report.NewLineScrubber("")
	require.NoError(t, err)
	te := &ToolExecutor{taskRepo: repo, errorScrub: s}
	res := te.waitForTask(context.Background(), `{"task_id":"task_x","timeout_seconds":1}`, []string{"p"})
	assert.NotContains(t, res.Content, tok[:12], "a fragment of the token survived the cut")
	assert.Contains(t, res.Content, report.WithheldLine)
}

// The receiver hands the channel's decision to the dispatcher. A channel
// with no OperatorView hook (e-mail, GitHub: no identity authority) is
// scrubbed; a hook that says true yields the raw view; the hook sees the
// inbound message the channel built (J4).
func TestChannelReceiver480_OperatorViewHook(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(context.Context, conversation.ChannelMessage) bool
		raw  bool
	}{
		{"no hook (e-mail, GitHub)", nil, false},
		{"hook says false", func(context.Context, conversation.ChannelMessage) bool { return false }, false},
		{"hook says true for this speaker", func(_ context.Context, m conversation.ChannelMessage) bool { return m.SpeakerID == "admin-1" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := dispatchertest.NewStatusModel(t)
			r := &ChannelReceiver{
				Channel:      &stubChannel{name: "email"},
				Agent:        agent480(t, model, true),
				Sessions:     &stubSessionStore{session: Session{ActiveProject: "alpha", AllowedProjects: []string{"alpha"}}},
				OperatorView: tc.hook,
			}
			require.NoError(t, r.Receive(context.Background(), conversation.ChannelMessage{
				Source: "email", SessionID: "s1", SpeakerID: "admin-1", Text: "why did it fail?",
			}))
			if tc.raw {
				dispatchertest.AssertRaw(t, tc.name, model.ToolResults())
			} else {
				dispatchertest.AssertScrubbed(t, tc.name, model.ToolResults())
			}
		})
	}
}

// Design review round 1 (suggestion 5): the dispatcher writes each tool
// result to tool_audit_log, which a project-scoped key reads through the
// project audit CSV export. An operator-view turn's raw result there would
// hand the admin's raw text to every project key, so the row is always the
// scrubbed view (J2b).
func TestDispatcher480_AuditRowIsScrubbedOnOperatorView(t *testing.T) {
	for _, withScrubber := range []bool{true, false} {
		t.Run(map[bool]string{true: "scrubber", false: "no scrubber (design review round 2, F4)"}[withScrubber], func(t *testing.T) {
			model := dispatchertest.NewStatusModel(t)
			db := sqlitetest.Memory(t)
			tasks := sqlite.NewTaskRepository(db.DB)
			execs := sqlite.NewExecutionRepository(db.DB)
			audit := sqlite.NewToolAuditRepository(db.DB)
			dispatchertest.SeedFailedTask480(t, tasks, execs, "alpha")
			opts := []AgentOption{WithAuditRepository(audit)}
			if withScrubber {
				s, err := report.NewLineScrubber("")
				require.NoError(t, err)
				opts = append(opts, WithErrorTextScrubber(s))
			}
			agent := NewAgent(model.Provider(), tasks, execs, nil, nil, opts...)

			res := agent.Process(context.Background(), turn480(true))
			require.NoError(t, res.Err)
			dispatchertest.AssertRaw(t, "the operator-view turn itself", model.ToolResults())

			project := "alpha"
			rows, err := audit.List(context.Background(), persistence.ToolAuditFilter{ProjectID: &project, PageSize: 50})
			require.NoError(t, err)
			var seen int
			for _, r := range rows {
				if r.ToolName != "get_task_status" && r.ToolName != "wait_for_task" {
					continue
				}
				seen++
				for _, sec := range dispatchertest.Secrets480() {
					assert.NotContains(t, r.ToolOutput, sec, "%s audit row holds a planted secret", r.ToolName)
				}
				assert.Contains(t, r.ToolOutput, report.WithheldLine)
			}
			require.Equal(t, 2, seen, "both status tools must have written an audit row")
		})
	}
}

// testLineScrubber480 builds the production line scrubber for tests that
// exercise the non-admin view of a clean error text.
func testLineScrubber480(t *testing.T) report.LineScrubber {
	t.Helper()
	s, err := report.NewLineScrubber("")
	require.NoError(t, err)
	return s
}
