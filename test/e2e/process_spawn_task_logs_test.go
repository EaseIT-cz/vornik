//go:build e2e_http
// +build e2e_http

// Process-spawn law, GitHub #294 (2026-10-08) — black-box e2e on the REAL
// daemon binary with the logging podman shim first on its PATH
// (process_spawn_law_http_test.go).
//
// Before #294, GET .../tasks/{id}/logs, .../explain, the support report for a
// task, the UI log panel and the UI post-mortem reached Executor.TaskLogs,
// which ran `podman ps --filter label=…task_id=<id>` and then `podman logs`
// on the request. They now read the daemon-held task log. The proof is the
// shim log: none of the requests may add a `podman logs` line or a podman
// line naming the task, and each must still answer with the persisted error.
package e2e_test

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	sqliterepo "vornik.io/vornik/internal/persistence/sqlite"
)

// seedFailedTask writes a terminal task and its failed execution through the
// real SQLite repositories, so the scheduler never picks it up.
func seedFailedTask(t *testing.T, taskID, errText string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()
	wf := e2eStaticProject + "-wf"
	if err := sqliterepo.NewTaskRepository(db).Create(ctx, &persistence.Task{
		ID: taskID, ProjectID: e2eStaticProject, WorkflowID: &wf,
		Status: persistence.TaskStatusFailed, LastError: &errText,
	}); err != nil {
		t.Fatalf("seed task: %v", err)
	}
	if err := sqliterepo.NewExecutionRepository(db).Create(ctx, &persistence.Execution{
		ID: "exec_" + taskID, TaskID: taskID, ProjectID: e2eStaticProject, WorkflowID: wf,
		Status: persistence.ExecutionStatusFailed, ErrorMessage: &errText,
	}); err != nil {
		t.Fatalf("seed execution: %v", err)
	}
}

// taskLogSpawns are the shim lines a task-log read would leave: any
// `podman logs`, and any podman call naming the task (the container lookup).
func taskLogSpawns(lines []string, taskID string) []string {
	var out []string
	for _, l := range lines {
		if strings.HasPrefix(l, "podman logs") || (strings.HasPrefix(l, "podman ") && strings.Contains(l, taskID)) {
			out = append(out, l)
		}
	}
	return out
}

// uiStreamFirstEvent opens the SSE log panel and returns its first event.
func uiStreamFirstEvent(t *testing.T, path string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpBase+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	var b strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(b.String(), "\n\n") {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

func TestSpawnLaw_TaskLogRoutesRunNoPodman(t *testing.T) {
	const taskID = "task_e2e294_logs"
	const errText = "step build failed: exit status 7 (e2e #294 marker)"
	seedFailedTask(t, taskID, errText)
	base := "/api/v1/projects/" + e2eStaticProject + "/tasks/" + taskID

	cases := []struct {
		name string
		do   func(t *testing.T) string
	}{
		{"GET logs", func(t *testing.T) string {
			resp, body := doReq(t, http.MethodGet, base+"/logs?tail=50", "", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET logs = %d: %s", resp.StatusCode, body)
			}
			return body
		}},
		{"GET explain", func(t *testing.T) string {
			resp, body := doReq(t, http.MethodGet, base+"/explain", "", nil)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("GET explain = %d: %s", resp.StatusCode, body)
			}
			return body
		}},
		{"POST support-report for the task", func(t *testing.T) string {
			resp, body := doReq(t, http.MethodPost, "/api/v1/support-report", "application/json",
				strings.NewReader(`{"task_id":"`+taskID+`"}`))
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("POST support-report = %d: %s", resp.StatusCode, body)
			}
			return "" // gzip; the spawn log is the evidence
		}},
		{"UI log stream", func(t *testing.T) string {
			return uiStreamFirstEvent(t, "/ui/tasks/"+taskID+"/logs/stream")
		}},
		// The post-mortem gathers its evidence (the task log among it) before
		// it calls the model; the e2e model endpoint is unreachable, so the
		// answer is an error redirect, and the spawn log is the evidence.
		{"UI post-mortem", func(t *testing.T) string {
			resp, body := doReq(t, http.MethodPost, "/ui/tasks/"+taskID+"/post-mortem", "application/x-www-form-urlencoded", strings.NewReader(""))
			if resp.StatusCode >= 500 && resp.StatusCode != http.StatusBadGateway {
				t.Fatalf("POST post-mortem = %d: %s", resp.StatusCode, body)
			}
			return ""
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body string
			spawned := spawnsDuring(t, func() { body = c.do(t) })
			if s := taskLogSpawns(spawned, taskID); len(s) > 0 {
				t.Errorf("GitHub #294: %s ran podman on the request: %q", c.name, s)
			}
			showsLog := c.name == "GET logs" || c.name == "GET explain" || c.name == "UI log stream"
			if showsLog && !strings.Contains(body, "e2e #294 marker") {
				t.Errorf("%s no longer shows the persisted error:\n%s", c.name, body)
			}
		})
	}
}
