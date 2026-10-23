package ui

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/html"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/mocks"
)

// GitHub #375 (found at: production, the operator in a browser on a laptop and
// a phone, 2026-10-14): the desktop layout of the task page had no Pause next to
// its Cancel, and the phone layout had no Cancel for a paused or parked task.
// Each layout typed its own status literal, and the older test
// (TestTaskDetail_MobileBarHasLifecycleActions) asserted on the whole body, so
// the desktop header satisfied its "mobile" assertions.
//
// This test renders the page through the real TaskDetail handler, splits the
// body into the desktop header row (class "hidden md:flex … justify-between")
// and the phone action bar (class "md:hidden fixed … z-30"; the bottom nav is
// z-50), and asserts each layout offers exactly the status controls the status
// allows (13-control-plane-web-ui.md "Update — 2026-10-16: task-detail status
// controls"). The expected sets are written out here, not derived from
// taskStatusControls, so the test can tell the broken page from the fixed one.
// Lower than found-at because no browser e2e lane exists on this host; the
// handler render is the highest level reachable.
func TestTaskDetail_BothLayoutsOfferTheStatusControls_375(t *testing.T) {
	type want struct{ pause, resume, cancel, requeue, live bool }
	cases := []struct {
		status persistence.TaskStatus
		want   want
	}{
		{persistence.TaskStatusRunning, want{pause: true, cancel: true, live: true}},
		{persistence.TaskStatusPaused, want{resume: true, cancel: true}},
		// Parked statuses: in the UI's cancellable set (05-scheduler §4.10).
		{persistence.TaskStatusWaitingForChildren, want{pause: true, cancel: true}},
		{persistence.TaskStatusAwaitingInput, want{pause: true, cancel: true}},
		// The pause handler refuses AWAITING_APPROVAL, so no Pause is offered;
		// the UI cancel set includes it.
		{persistence.TaskStatusAwaitingApproval, want{cancel: true}},
		// PENDING is both cancellable and requeueable (retryOne).
		{persistence.TaskStatusPending, want{pause: true, cancel: true, requeue: true, live: true}},
		// Terminal: only Requeue.
		{persistence.TaskStatusCompleted, want{requeue: true}},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			taskID := "task_375_" + strings.ToLower(string(tc.status))
			body := renderTaskDetailViaHandler(t, taskID, tc.status, true)
			assertLayoutControls(t, body, taskID, tc.status, map[string]bool{
				"pause": tc.want.pause, "resume": tc.want.resume, "cancel": tc.want.cancel,
				"retry": tc.want.requeue, "live": tc.want.live,
			})
		})
	}

	// Without a task-message repository the pause and resume handlers answer
	// 503, so neither layout offers them; Cancel still works and both layouts
	// must offer it (the phone bar used to vanish with the conversation block).
	t.Run("RUNNING without a task-message repository", func(t *testing.T) {
		taskID := "task_375_nolifecycle"
		body := renderTaskDetailViaHandler(t, taskID, persistence.TaskStatusRunning, false)
		assertLayoutControls(t, body, taskID, persistence.TaskStatusRunning, map[string]bool{
			"pause": false, "resume": false, "cancel": true, "retry": false, "live": true,
		})
	})
}

// TestTaskDetail_StatusControlsUseNoInlineHandlers_375: the shared controls
// confirm through data-confirm and the page's delegated submit listener, never
// an onsubmit attribute (console CSP, internal/ui/console_security.go).
func TestTaskDetail_StatusControlsUseNoInlineHandlers_375(t *testing.T) {
	for _, st := range []persistence.TaskStatus{persistence.TaskStatusRunning, persistence.TaskStatusFailed} {
		taskID := "task_375_csp_" + strings.ToLower(string(st))
		body := renderTaskDetailViaHandler(t, taskID, st, true)
		doc, err := html.Parse(strings.NewReader(body))
		require.NoError(t, err)
		for name, root := range layoutRoots(doc) {
			require.NotNil(t, root, name)
			for _, f := range elements(root, "form") {
				action := attr(f, "action")
				if !strings.HasPrefix(action, "/ui/tasks/"+taskID+"/") || strings.HasSuffix(action, "/close") {
					continue // Close is outside the partial (its data-confirm: csp_confirm_416_test.go).
				}
				assert.Empty(t, attr(f, "onsubmit"), "%s %s: %s carries an inline onsubmit", name, st, action)
				if strings.HasSuffix(action, "/cancel") || strings.HasSuffix(action, "/retry") {
					assert.NotEmpty(t, attr(f, "data-confirm"), "%s %s: %s must confirm via data-confirm", name, st, action)
				}
			}
		}
		assert.Contains(t, body, "getAttribute('data-confirm')", "the page must carry the delegated data-confirm listener")
	}
}

func layoutRoots(doc *html.Node) map[string]*html.Node {
	return map[string]*html.Node{
		"desktop header row": findByClasses(doc, "hidden", "md:flex", "justify-between"),
		"phone action bar":   findByClasses(doc, "md:hidden", "fixed", "z-30"),
	}
}

// assertLayoutControls checks, per layout, that each verb's control is
// present exactly when wanted. "live" is a link; the rest are POST forms.
func assertLayoutControls(t *testing.T, body, taskID string, status persistence.TaskStatus, wants map[string]bool) {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(body))
	require.NoError(t, err)
	for name, root := range layoutRoots(doc) {
		require.NotNil(t, root, "%s must render for %s", name, status)
		targets := map[string]bool{}
		for _, f := range elements(root, "form") {
			targets[attr(f, "action")] = true
		}
		for _, a := range elements(root, "a") {
			targets[attr(a, "href")] = true
		}
		for verb, wanted := range wants {
			got := targets["/ui/tasks/"+taskID+"/"+verb]
			assert.Equal(t, wanted, got,
				"%s, status %s: %s control present=%v, want %v (targets: %v)",
				name, status, verb, got, wanted, targets)
		}
	}
}

func renderTaskDetailViaHandler(t *testing.T, taskID string, status persistence.TaskStatus, lifecycle bool) string {
	t.Helper()
	taskRepo := &mocks.MockTaskRepository{
		GetFunc: func(_ context.Context, _ string) (*persistence.Task, error) {
			return &persistence.Task{ID: taskID, ProjectID: "p1", Status: status}, nil
		},
		ListFunc: func(context.Context, persistence.TaskFilter) ([]*persistence.Task, error) { return nil, nil },
	}
	opts := []ServerOption{WithTaskRepository(taskRepo)}
	if lifecycle {
		opts = append(opts, WithTaskMessageRepository(&fakeTaskMessageRepo{}))
	}
	srv := NewServer(opts...)
	req := httptest.NewRequest(http.MethodGet, "/tasks/"+taskID, nil)
	rec := httptest.NewRecorder()
	srv.TaskDetail(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	return rec.Body.String()
}

// findByClasses returns the first element whose class attribute carries every
// given token.
func findByClasses(n *html.Node, tokens ...string) *html.Node {
	if n.Type == html.ElementNode {
		have := map[string]bool{}
		for _, c := range strings.Fields(attr(n, "class")) {
			have[c] = true
		}
		all := true
		for _, tok := range tokens {
			if !have[tok] {
				all = false
				break
			}
		}
		if all {
			return n
		}
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findByClasses(c, tokens...); f != nil {
			return f
		}
	}
	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// elements returns every element named tag under n.
func elements(n *html.Node, tag string) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// allTaskStatusesFor375 lists every persistence.TaskStatus (there is no
// exported list); a new status must be added here to be covered.
var allTaskStatusesFor375 = []persistence.TaskStatus{
	persistence.TaskStatusPending, persistence.TaskStatusQueued, persistence.TaskStatusLeased,
	persistence.TaskStatusRunning, persistence.TaskStatusWaitingForChildren,
	persistence.TaskStatusCompleted, persistence.TaskStatusFailed, persistence.TaskStatusCancelled,
	persistence.TaskStatusAwaitingInput, persistence.TaskStatusAwaitingExternal,
	persistence.TaskStatusPaused, persistence.TaskStatusClosed, persistence.TaskStatusAwaitingApproval,
}

// TestTaskStatusControls_MatchesTheHandlerSets (GitHub #375): for every
// status, the page offers a control exactly when the handler behind it would
// act. Pause and Requeue are checked against what uiPauseTask and retryOne
// actually do with a scripted repository, not against the helper sets alone.
func TestTaskStatusControls_MatchesTheHandlerSets(t *testing.T) {
	for _, st := range allTaskStatusesFor375 {
		t.Run(string(st), func(t *testing.T) {
			off := taskStatusControls(st, false)
			assert.False(t, off.Pause, "no Pause without a task-message repository")
			assert.False(t, off.Resume, "no Resume without a task-message repository")

			on := taskStatusControls(st, true)
			assert.Equal(t, slices.Contains(persistence.CancellableTaskStatuses(), st), on.Cancel, "cancel = cancelOne's set")
			assert.Equal(t, on.Cancel, off.Cancel, "cancel does not depend on the lifecycle repo")
			assert.Equal(t, st == persistence.TaskStatusPaused, on.Resume, "resume = PAUSED")
			assert.Equal(t, slices.Contains(uiLiveTaskStatuses(), st), on.WatchLive)

			// Pause: what does uiPauseTask's conditional write accept?
			var pauseFrom []persistence.TaskStatus
			repo := &mocks.MockTaskRepository{
				TransitionConditionalFunc: func(_ context.Context, _ string, from []persistence.TaskStatus, _ persistence.TaskStatus, _ persistence.TransitionOpts) (bool, error) {
					pauseFrom = from
					return true, nil
				},
				GetFunc: func(_ context.Context, id string) (*persistence.Task, error) {
					return &persistence.Task{ID: id, ProjectID: "p1", Status: st}, nil
				},
				RequeueTerminalTaskFunc: func(context.Context, string, int, int) (bool, error) { return true, nil },
			}
			srv := NewServer(WithLogger(quietLogger()), WithTaskRepository(repo), WithTaskMessageRepository(&fakeTaskMessageRepo{}))
			srv.uiPauseTask(context.Background(), &persistence.Task{ID: "t", Status: st})
			require.NotNil(t, pauseFrom, "uiPauseTask must reach its conditional write")
			assert.Equal(t, slices.Contains(pauseFrom, st), on.Pause, "pause = uiPauseTask's from-set")

			// Requeue: does retryOne requeue this status?
			req := httptest.NewRequest(http.MethodPost, "/tasks/t/retry", nil)
			assert.Equal(t, srv.retryOne(context.Background(), req, "t"), on.Requeue, "requeue = retryOne's set")
		})
	}
}
