package hermes

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Found at e2e-lane, 2026-10-14 (Hermes release lane at ef7fdffd7, progress
// step): VORNIK_E2E_DEDICATED_HOST=1 was refused because the lane's OWN
// daemon had started an agent container (vornik-broker-mail-mail-reader-
// task-...) for an H3g task. Agent containers carry the daemon's labels
// (vornik.managed, vornik.projectId, vornik.taskId), not the lane label,
// so foreignContainers counted them as foreign. A container is the lane's
// when it carries the lane label, or when it is vornik.managed and its task
// is a task of one of this lane's daemons; anything else, and any
// "cannot tell", stays foreign (lane design, "Memory guardrails",
// amendment 2026-10-14 W8-AJ).
func TestForeignContainers_LaneDaemonAgentIsTheLanes(t *testing.T) {
	agent := func(name, project, task string) podmanContainer {
		return podmanContainer{ID: name, State: "running", Names: []string{name}, Labels: map[string]string{
			"vornik.managed": "true", "vornik.projectId": project, "vornik.role": "mail-reader", "vornik.taskId": task,
		}}
	}
	cs := []podmanContainer{
		{ID: "pg", State: "running", Names: []string{"vornik-e2e-pg-x"}, Labels: map[string]string{laneLabelKey: laneLabelVal}},
		agent("lane-agent", "broker-mail", "task-lane"),
		agent("prod-agent", "broker-mail", "task-prod"),
		agent("unresolved-agent", "broker-mail", "task-err"),
		agent("no-task-agent", "broker-mail", ""),
		{ID: "scraper", State: "running", Names: []string{"vornik-scraper"}},
		// A task label without vornik.managed is not a daemon agent.
		{ID: "unmanaged", State: "running", Names: []string{"unmanaged"}, Labels: map[string]string{"vornik.projectId": "broker-mail", "vornik.taskId": "task-lane"}},
		{ID: "old", State: "exited", Names: []string{"old"}},
	}
	owns := func(project, task string) (bool, error) {
		switch task {
		case "task-lane":
			return project == "broker-mail", nil
		case "task-err":
			return false, errors.New("connection refused")
		}
		return false, nil
	}
	got := foreignContainers(cs, owns)
	joined := strings.Join(got, ",")
	if len(got) != 5 || strings.Contains(joined, "lane-agent") || !strings.Contains(joined, "unmanaged") || strings.Contains(joined, "vornik-e2e-pg") ||
		!strings.Contains(joined, "prod-agent") || !strings.Contains(joined, "unresolved-agent") ||
		!strings.Contains(joined, "no-task-agent") || !strings.Contains(joined, "vornik-scraper") {
		t.Fatalf("foreignContainers = %v; want the production agent, the unresolved one, the task-less one, the unmanaged one and the scraper, not the lane's agent", got)
	}
	// No way to ask (nil owner) is "cannot tell": every agent is foreign.
	if got := foreignContainers(cs, nil); len(got) != 6 {
		t.Fatalf("with no owner, foreignContainers = %v, want all six non-lane-labelled running containers", got)
	}
}

// The owner asks each registered lane daemon for the task by id: 200 with
// the same id is the lane's; 404 from every daemon is not; any other answer
// or a transport error is "cannot tell" (an error, so the caller fails closed).
func TestLaneTaskOwner_AsksTheLaneDaemons(t *testing.T) {
	const key = "sk-lane"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/api/v1/projects/broker-mail/tasks/task-lane":
			_, _ = w.Write([]byte(`{"taskId":"task-lane","projectId":"broker-mail"}`))
		case "/api/v1/projects/broker-mail/tasks/task-liar":
			_, _ = w.Write([]byte(`{"taskId":"task-other"}`))
		case "/api/v1/projects/broker-mail/tasks/task-500":
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	owns := laneTaskOwner(srv.Client(), []laneDaemon{{apiURL: srv.URL, adminKey: key}})
	for _, c := range []struct {
		task    string
		want    bool
		wantErr bool
	}{
		{"task-lane", true, false},
		{"task-prod", false, false},
		{"task-liar", false, true},
		{"task-500", false, true},
		{"../x", false, true},
	} {
		got, err := owns("broker-mail", c.task)
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("owns(%q) = %v, %v; want %v, err=%v", c.task, got, err, c.want, c.wantErr)
		}
	}
	if ok, err := laneTaskOwner(srv.Client(), []laneDaemon{{apiURL: srv.URL, adminKey: "wrong"}})("broker-mail", "task-lane"); ok || err == nil {
		t.Fatalf("a refused key must be cannot-tell, got %v, %v", ok, err)
	}
	if ok, err := laneTaskOwner(srv.Client(), nil)("broker-mail", "task-lane"); ok || err != nil {
		t.Fatalf("no lane daemon: not the lane's, got %v, %v", ok, err)
	}
	srv.Close()
	if ok, err := owns("broker-mail", "task-lane"); ok || err == nil {
		t.Fatalf("an unreachable daemon must be cannot-tell, got %v, %v", ok, err)
	}
}

func TestLaneDaemonRegistry(t *testing.T) {
	const url = "http://127.0.0.1:1/registry-test"
	count := func() int {
		n := 0
		for _, d := range registeredLaneDaemons() {
			if d.apiURL == url {
				n++
			}
		}
		return n
	}
	unreg := registerLaneDaemon(laneDaemon{apiURL: url, adminKey: "k"})
	if count() != 1 {
		t.Fatalf("registered = %+v", registeredLaneDaemons())
	}
	unreg()
	if count() != 0 {
		t.Fatalf("after unregister = %+v", registeredLaneDaemons())
	}
}
