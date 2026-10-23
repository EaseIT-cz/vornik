package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ratelimit"
	"vornik.io/vornik/internal/taskcreate"
	"vornik.io/vornik/internal/ui"
)

// GitHub #369 (comment 2026-10-14; outcome-quality LLD §S8.11 row D). The
// operator's 2026.10.6 test: task_20261014093717_631b29047b3cdfd6, a ui-form
// task, passed the distiller's eligibility gate and then proposed nothing,
// with no log line and no metric, so "the model said skip" could not be told
// apart from "the run never reached the model". Each post-gate return now
// counts skill_distill_skipped_total with its own reason. These cases run in
// the real loop (the #372 harness): a ui-form task completes, the distiller
// model answers, and the test waits for the reason that answer must produce.
//
// Found at: production (operator test). Regression test at: executor-e2e.

func e2e369SubmitUITask(t *testing.T, f *e2e372) *persistence.Task {
	t.Helper()
	creator := taskcreate.New(taskcreate.WithTaskRepository(f.tasks), taskcreate.WithProjectRegistry(f.reg),
		taskcreate.WithRateLimiter(ratelimit.New()))
	srv := ui.NewServer(ui.WithProjectRegistry(f.reg), ui.WithTaskRepository(f.tasks), ui.WithTaskCreator(creator))
	form := url.Values{}
	form.Set("prompt", "Write the release checklist")
	form.Set("workflowId", "wf372")
	form.Set("taskType", "research")
	req := httptest.NewRequest(http.MethodPost, "/ui/projects/"+e2e372Project+"/tasks/new", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ProjectCreateTaskSubmit(rec, req, e2e372Project)
	require.Equal(t, http.StatusSeeOther, rec.Code, rec.Body.String())
	return f.onlyTask(t)
}

func TestDistillE2E_EveryPostGateSkipIsCountedByItsCause(t *testing.T) {
	cases := []struct {
		name   string
		reply  string
		seed   bool // a same-named skill exists before the run
		reason string
	}{
		{"the model says skip", `{"skip":true}`, false, "model_skip"},
		{"an unparseable reply", `I think this task was great!`, false, "unparseable"},
		{"a candidate without a body", `{"skip":false,"name":"e2e-369-lesson","description":"when","body":"  "}`, false, "incomplete_candidate"},
		{"a candidate whose name exists", "", true, "duplicate_name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newE2E372(t)
			f.llm.reply = c.reply
			if c.seed {
				require.NoError(t, f.skills.Create(context.Background(), &persistence.Skill{
					ID: persistence.GenerateID("skill"), ProjectID: e2e372Project, Name: "E2E-372-Lesson",
					Description: "already here", Body: "# old", BodySHA256: "x",
					Maturity: persistence.SkillMaturityActive, Version: 1, OriginClient: "test",
				}))
			}
			task := e2e369SubmitUITask(t, f)
			f.start(t)
			f.waitCompleted(t, task.ID)
			require.Eventually(t, func() bool {
				return testutil.ToFloat64(f.metrics.SkillDistillSkippedTotal.WithLabelValues(c.reason)) == 1
			}, 20*time.Second, 10*time.Millisecond,
				"skill_distill_skipped_total{reason=%q} never reached 1: the post-gate return left no signal", c.reason)
			require.Equal(t, int32(1), f.llm.calls.Load(), "the run must have reached the model")
			want := 0
			if c.seed {
				want = 1
			}
			require.Equal(t, want, f.drafts(t), "no new draft may be written")
		})
	}
}
