package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/persistence/sqlite/sqlitetest"
)

// Found-at-level regression for GitHub #408 (found in production,
// 2026-10-16, during incident #407). `vornikctl task list -p <project>`
// printed the API's first page of 20 rows followed by "Total: N" — no
// --limit/--all flag and no line saying the list was cut. The operator
// cancelled the visible queued tasks while 58 queued children, 11
// WAITING_FOR_CHILDREN parents and 22 further top-level tasks stayed
// invisible. Production is not reachable from a test, so this drives the
// operator's command (runTaskList) against the real API handler
// (api.Server.Routes over HTTP) and a real migrated SQLite database — the
// highest level a test reaches (cli-design.md "Amended 2026-10-16, GitHub
// #408").

type list408Fixture struct {
	parentID  string
	childIDs  []string
	queuedIDs []string
	doneIDs   []string
	approval  string
	all       []string
}

// seed408 writes 32 tasks to project p: an OLD parent in
// WAITING_FOR_CHILDREN with three QUEUED children (all older than every
// top-level row, so the newest-first first page hides them — the production
// shape), 25 QUEUED top-level tasks, one COMPLETED, one CANCELLED and one
// AWAITING_APPROVAL (listed by --active, refused by `task cancel`).
func seed408(t *testing.T, repo *sqlite.TaskRepository, p string) list408Fixture {
	t.Helper()
	ctx := context.Background()
	base := time.Now().UTC().Add(-3 * time.Hour)
	var f list408Fixture
	mk := func(id string, st persistence.TaskStatus, at time.Time, parent *string) {
		t.Helper()
		wf := "wf"
		if err := repo.Create(ctx, &persistence.Task{
			ID: id, ProjectID: p, WorkflowID: &wf, Status: st, ParentTaskID: parent,
			CreationSource: persistence.TaskCreationSourceUser, Priority: 50,
			Payload: []byte(`{"taskType":"headmatch"}`), CreatedAt: at, UpdatedAt: at,
			MaxAttempts: 3, Attempt: 1,
		}); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
		f.all = append(f.all, id)
	}
	f.parentID = "t408-parent"
	mk(f.parentID, persistence.TaskStatusWaitingForChildren, base, nil)
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("t408-child-%02d", i)
		mk(id, persistence.TaskStatusQueued, base.Add(time.Duration(i+1)*time.Minute), &f.parentID)
		f.childIDs = append(f.childIDs, id)
	}
	for i, st := range []persistence.TaskStatus{persistence.TaskStatusCompleted, persistence.TaskStatusCancelled} {
		id := fmt.Sprintf("t408-done-%02d", i)
		mk(id, st, base.Add(time.Duration(10+i)*time.Minute), nil)
		f.doneIDs = append(f.doneIDs, id)
	}
	f.approval = "t408-approval"
	mk(f.approval, persistence.TaskStatusAwaitingApproval, base.Add(12*time.Minute), nil)
	for i := 0; i < 25; i++ {
		id := fmt.Sprintf("t408-queued-%02d", i)
		mk(id, persistence.TaskStatusQueued, base.Add(time.Hour+time.Duration(i)*time.Minute), nil)
		f.queuedIDs = append(f.queuedIDs, id)
	}
	return f
}

// resetTaskListFlags puts every task-list flag back to its default so the
// subtests do not leak state into one another (cobra flags are globals).
func resetTaskListFlags(t *testing.T) {
	t.Helper()
	taskCov_reset()
	for name, def := range map[string]string{"limit": "20", "offset": "0", "all": "false", "active": "false", "status": "", "json": "false"} {
		f := taskListCmd.Flags().Lookup(name)
		if f == nil {
			continue
		}
		if err := f.Value.Set(def); err != nil {
			t.Fatalf("reset --%s: %v", name, err)
		}
		f.Changed = false
	}
}

func runList408(t *testing.T, project string, flags map[string]string) string {
	t.Helper()
	resetTaskListFlags(t)
	t.Cleanup(func() { resetTaskListFlags(t) })
	taskListProject = project
	for k, v := range flags {
		if err := taskListCmd.Flags().Set(k, v); err != nil {
			t.Fatalf("set --%s=%s: %v", k, v, err)
		}
	}
	out, err := captureStdoutFunc(t, func() error { return runTaskList(taskListCmd, nil) })
	if err != nil {
		t.Fatalf("runTaskList %v: %v\n%s", flags, err, out)
	}
	return out
}

func countIDs(out string, ids []string) int {
	n := 0
	for _, id := range ids {
		if strings.Contains(out, id) {
			n++
		}
	}
	return n
}

func TestTaskList408_CLIAgainstRealAPIAndDB(t *testing.T) {
	db := sqlitetest.Open(t, filepath.Join(t.TempDir(), "vornik.db"))
	repo := sqlite.NewTaskRepository(db.DB)
	const project = "headmatch"
	f := seed408(t, repo, project)

	srv := httptest.NewServer(api.NewServer(
		api.WithLogger(zerolog.Nop()),
		api.WithTaskRepository(repo),
	).Routes())
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	t.Setenv("VORNIK_API_KEY", "test-key-408")

	t.Run("default output states the truncation", func(t *testing.T) {
		out := runList408(t, project, nil)
		if got := countIDs(out, f.all); got != 20 {
			t.Errorf("default page: %d task rows, want 20\n%s", got, out)
		}
		if !strings.Contains(out, "Showing 20 of 32 tasks") || !strings.Contains(out, "--all") {
			t.Errorf("default output does not state the cut (want \"Showing 20 of 32 tasks\" and a --all hint):\n%s", out)
		}
		if countIDs(out, []string{f.parentID}) != 0 {
			t.Errorf("fixture shape wrong: the old waiting parent should be off the first page")
		}
	})

	t.Run("--all shows every task and no cut", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"all": "true"})
		if got := countIDs(out, f.all); got != 32 {
			t.Errorf("--all: %d of 32 task rows\n%s", got, out)
		}
		if strings.Contains(out, "Showing") {
			t.Errorf("--all must not claim a cut:\n%s", out)
		}
		if !strings.Contains(out, "Total: 32") {
			t.Errorf("--all footer: want \"Total: 32\":\n%s", out)
		}
	})

	t.Run("--all and --limit page through the API", func(t *testing.T) {
		// Shrink the CLI's page so the walk crosses several real API pages.
		old := taskListAPIPage
		taskListAPIPage = 7
		defer func() { taskListAPIPage = old }()
		out := runList408(t, project, map[string]string{"all": "true"})
		if got := countIDs(out, f.all); got != 32 || !strings.Contains(out, "Total: 32") {
			t.Errorf("--all over 7-row pages: %d of 32 rows\n%s", got, out)
		}
		out = runList408(t, project, map[string]string{"limit": "25"})
		if got := countIDs(out, f.all); got != 25 || !strings.Contains(out, "Showing 25 of 32 tasks") {
			t.Errorf("--limit 25 over 7-row pages: %d rows\n%s", got, out)
		}
	})

	t.Run("--offset with --all says how many it showed", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"all": "true", "offset": "10"})
		if got := countIDs(out, f.all); got != 22 || !strings.Contains(out, "Total: 32 (22 shown from offset 10)") {
			t.Errorf("--offset 10 --all: %d rows\n%s", got, out)
		}
	})

	t.Run("--limit 25 shows 25 and states the cut", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"limit": "25"})
		if got := countIDs(out, f.all); got != 25 {
			t.Errorf("--limit 25: %d rows\n%s", got, out)
		}
		if !strings.Contains(out, "Showing 25 of 32 tasks") {
			t.Errorf("--limit 25: want \"Showing 25 of 32 tasks\":\n%s", out)
		}
	})

	t.Run("--active includes the waiting parent and its children", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"active": "true", "all": "true"})
		if countIDs(out, []string{f.parentID}) != 1 || !strings.Contains(out, "WAITING_FOR_CHILDREN") {
			t.Errorf("--active is missing the WAITING_FOR_CHILDREN parent:\n%s", out)
		}
		if got := countIDs(out, f.childIDs); got != 3 {
			t.Errorf("--active: %d of 3 children\n%s", got, out)
		}
		if got := countIDs(out, f.doneIDs); got != 0 {
			t.Errorf("--active listed %d finished tasks\n%s", got, out)
		}
		if countIDs(out, []string{f.approval}) != 1 {
			t.Errorf("--active is missing the AWAITING_APPROVAL task:\n%s", out)
		}
		if !strings.Contains(out, "Total: 30") {
			t.Errorf("--active --all footer: want \"Total: 30\":\n%s", out)
		}
		// The PARENT column names each child's parent.
		for _, line := range strings.Split(out, "\n") {
			if strings.HasPrefix(line, f.childIDs[0]) && !strings.Contains(line, f.parentID) {
				t.Errorf("child row does not name its parent: %q", line)
			}
		}
	})

	t.Run("--active without --all still states its cut", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"active": "true"})
		if !strings.Contains(out, "Showing 20 of 30 tasks") {
			t.Errorf("--active default: want \"Showing 20 of 30 tasks\":\n%s", out)
		}
	})

	t.Run("--json carries total, shown and the next offset", func(t *testing.T) {
		out := runList408(t, project, map[string]string{"json": "true"})
		var got struct {
			Tasks      []json.RawMessage `json:"tasks"`
			Total      int               `json:"total"`
			Shown      int               `json:"shown"`
			Truncated  bool              `json:"truncated"`
			NextOffset *int              `json:"nextOffset"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("json: %v\n%s", err, out)
		}
		if got.Total != 32 || got.Shown != 20 || len(got.Tasks) != 20 || !got.Truncated ||
			got.NextOffset == nil || *got.NextOffset != 20 {
			t.Errorf("json = total %d shown %d rows %d truncated %v next %v; want 32/20/20/true/20",
				got.Total, got.Shown, len(got.Tasks), got.Truncated, got.NextOffset)
		}
	})

	// --active lists what an operator must consider, not only what
	// `task cancel` accepts: an AWAITING_APPROVAL task is listed and the
	// cancel refuses it (answer it with approve/reject). Pinned so the
	// divergence stays deliberate (design review 2026-10-16, P1/P2).
	t.Run("task cancel refuses the AWAITING_APPROVAL task --active lists", func(t *testing.T) {
		taskCov_reset()
		taskCancelProject = project
		_, err := captureStdoutFunc(t, func() error { return runTaskCancel(taskCancelCmd, []string{f.approval}) })
		if err == nil || !strings.Contains(err.Error(), "AWAITING_APPROVAL") {
			t.Errorf("cancel of an AWAITING_APPROVAL task: err = %v, want a refusal naming the status", err)
		}
	})

	t.Run("--all and --limit are mutually exclusive", func(t *testing.T) {
		resetTaskListFlags(t)
		t.Cleanup(func() { resetTaskListFlags(t) })
		taskListProject = project
		for k, v := range map[string]string{"all": "true", "limit": "5"} {
			if err := taskListCmd.Flags().Set(k, v); err != nil {
				t.Fatalf("set --%s: %v", k, err)
			}
		}
		if _, err := captureStdoutFunc(t, func() error { return runTaskList(taskListCmd, nil) }); err == nil {
			t.Error("--all with --limit: want an error")
		}
	})
}

// Flag validation needs no server: each is refused before any request.
func TestTaskList408_FlagValidation(t *testing.T) {
	t.Setenv("VORNIK_API_URL", "http://127.0.0.1:1")
	for name, flags := range map[string]map[string]string{
		"active with status": {"active": "true", "status": "QUEUED"},
		"zero limit":         {"limit": "0"},
		"negative offset":    {"offset": "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			resetTaskListFlags(t)
			t.Cleanup(func() { resetTaskListFlags(t) })
			taskListProject = "p"
			for k, v := range flags {
				if err := taskListCmd.Flags().Set(k, v); err != nil {
					t.Fatalf("set --%s: %v", k, err)
				}
			}
			if _, err := captureStdoutFunc(t, func() error { return runTaskList(taskListCmd, nil) }); err == nil {
				t.Errorf("%v: want an error", flags)
			}
		})
	}
}

// A daemon whose count failed sends totalUnknown: the CLI must not print
// "Total: <page length>" (the silent cut, under load) but keep paging until
// a short page and say the total is unknown.
func TestTaskList408_UnknownTotalIsNeverATotal(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		offset := r.URL.Query().Get("offset")
		switch offset {
		case "0":
			_, _ = w.Write([]byte(`{"tasks":[{"taskId":"u-1"},{"taskId":"u-2"}],"total":2,"totalUnknown":true}`))
		default:
			_, _ = w.Write([]byte(`{"tasks":[{"taskId":"u-3"}],"total":1,"totalUnknown":true}`))
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	old := taskListAPIPage
	taskListAPIPage = 2
	defer func() { taskListAPIPage = old }()

	out := runList408(t, "p", map[string]string{"all": "true"})
	if strings.Contains(out, "Total:") {
		t.Errorf("an unknown total must never print as Total:\n%s", out)
	}
	if countIDs(out, []string{"u-1", "u-2", "u-3"}) != 3 || calls != 2 {
		t.Errorf("want 3 rows over 2 pages, got %d calls:\n%s", calls, out)
	}
	if !strings.Contains(out, "could not count the total") || !strings.Contains(out, "reached its end") {
		t.Errorf("want the unknown total stated:\n%s", out)
	}

	calls = 0
	out = runList408(t, "p", map[string]string{"limit": "2"})
	if !strings.Contains(out, "more may match") || calls != 1 {
		t.Errorf("--limit 2 with an unknown total: want \"more may match\" after one call (%d):\n%s", calls, out)
	}
}

// A real count from an earlier page survives a later page whose count
// failed: the JSON total is never replaced by a fallback page length
// (code review 2026-10-16, F1).
func TestTaskList408_KnownTotalSurvivesALaterFailedCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("offset") {
		case "0":
			_, _ = w.Write([]byte(`{"tasks":[{"taskId":"k-1"},{"taskId":"k-2"}],"total":5}`))
		case "2":
			_, _ = w.Write([]byte(`{"tasks":[{"taskId":"k-3"},{"taskId":"k-4"}],"total":2,"totalUnknown":true}`))
		default:
			_, _ = w.Write([]byte(`{"tasks":[{"taskId":"k-5"}],"total":1,"totalUnknown":true}`))
		}
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	old := taskListAPIPage
	taskListAPIPage = 2
	defer func() { taskListAPIPage = old }()

	out := runList408(t, "p", map[string]string{"all": "true", "json": "true"})
	var got taskListOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if got.Total != 5 || got.Shown != 5 || !got.TotalUnknown || got.Truncated {
		t.Errorf("got total %d shown %d unknown %v truncated %v; want 5/5/true/false",
			got.Total, got.Shown, got.TotalUnknown, got.Truncated)
	}
}

// An older daemon sends neither parentTaskId nor nextOffset: the CLI still
// states the cut from total and shows PARENT as "-".
func TestTaskList408_OlderDaemonStillStatesTheCut(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tasks":[{"taskId":"t-old","status":"QUEUED","priority":50}],"total":40}`))
	}))
	defer srv.Close()
	t.Setenv("VORNIK_API_URL", srv.URL)
	out := runList408(t, "p", map[string]string{"limit": "1"})
	if !strings.Contains(out, "Showing 1 of 40 tasks") {
		t.Errorf("want the cut stated:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "t-old" {
			// TASK ID, STATUS, (empty TYPE), PRIORITY, PARENT.
			if len(f) < 4 || f[3] != "-" {
				t.Errorf("want PARENT shown as -: %q", line)
			}
			return
		}
	}
	t.Errorf("row t-old missing:\n%s", out)
}
