//go:build e2e_hermes

package hermes

import (
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

// GitHub #314, the DoD lane case of agent-administered design §19.14.6
// (with §19.14.8 item 8 and §19.14.11): the scripted phone installs
// `agenda` as a connector from the device's Connectors page, in one
// confirm, and the actual Hermes, connected as an agent, delegates it and
// receives alice's agenda. The vetting is the lane's own: an operator
// connector.yaml written into the stack's deployed tree before boot,
// pointing at the calendar stand-in (no shipped recipe is vetted).
//
// Run it (podman, the lane's Hermes image, and an agent image built from
// this tree):
//
//	VORNIK_E2E_AGENT_IMAGE=localhost/vornik-agent:e2e-smoke \
//	  go test -tags=e2e_hermes -count=1 -v -timeout 30m \
//	  -run '^TestC1_ConnectorAgendaFromThePhone$' ./test/e2e/hermes/
//
// Like every arm of this lane it fails, never skips, when it cannot run
// (requirePodman, requireLaneAgentImage); VORNIK_E2E_HERMES_SKIP=1 skips it
// explicitly.

var shownField = regexp.MustCompile(`name="shown" value="([0-9a-f]+)"`)

// phoneBody posts a form and returns the status and the body.
func phoneBody(t *testing.T, p *Phone, path string, form url.Values) (int, string) {
	t.Helper()
	res, err := p.post(path, form)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	return res.StatusCode, string(b)
}

func TestC1_ConnectorAgendaFromThePhone(t *testing.T) {
	requireLaneAgentImage(t)
	cal := recipeCalendar()
	calURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", serveLoopback(t, cal))
	vetting := "title: \"Calendar (read)\"\nservers:\n  calendar: {url: \"" + calURL + "\"}\n" +
		"vetted: {by: \"Hermes lane\", at: \"2026-10-13\", recipe_version: 1}\n"
	s := startStackWith(t, nil, map[string]string{"agent-templates/recipes/agenda/connector.yaml": vetting})
	s.llm.SetScripts(recipeScripts()...)
	phone := pairPhone(t, s)
	c := accountsHermesClient(t, s)
	c.sweepQuiet = map[string]string{"broker_actions": "a connector reads; it proposes no write"}
	defer accountsSweep(t, s, c, []string{CalendarTokenCanary, BobCalendarTokenCanary})

	// The page lists the vetted connector with its exact tool.
	list, err := phone.get("/ui/approve/connectors")
	if err != nil || !strings.Contains(list, "Calendar (read)") || !strings.Contains(list, "calendar_list_events") {
		t.Fatalf("the Connectors page: %v\n%s", err, list)
	}
	// The connector's own project in Hermes's namespace, then the sentence.
	if code, body := phoneBody(t, phone, "/ui/approve/connectors/agenda/project", url.Values{"namespace": {c.ns}}); code != 200 && code != 303 {
		t.Fatalf("create the project: %d %s", code, body)
	}
	choose, err := phone.get("/ui/approve/connectors/agenda?" + url.Values{"namespace": {c.ns}, "project": {"agenda"}}.Encode())
	m := shownField.FindStringSubmatch(choose)
	if err != nil || m == nil || !strings.Contains(choose, "wants to install") {
		t.Fatalf("the choose page: %v\n%s", err, choose)
	}
	// One confirm: install, credential, tools, check.
	code, done := phoneBody(t, phone, "/ui/approve/connectors/agenda", url.Values{
		"namespace": {c.ns}, "project": {"agenda"}, "shown": {m[1]},
		"credential_CALENDAR_TOKEN": {CalendarTokenCanary},
		// A forged URL is ignored (§19.14.6 NeverAcceptsAURLOrTool).
		"var_calendar_server_url": {"https://evil.example/mcp"}, "url": {"https://evil.example/mcp"},
	})
	if code != 200 || !strings.Contains(done, "Installed and connected") || !strings.Contains(done, "<strong>ok</strong>") {
		t.Fatalf("the confirm: %d\n%s", code, done)
	}
	if pending, err := phone.Pending(); err != nil || len(pending) != 0 {
		t.Fatalf("requests left on the phone after the one confirm: %v %v", pending, err)
	}
	t.Logf("egress guard: %d connection URLs examined, all loopback", assertLoopbackOnly(t, s))

	// The actual Hermes sees it as installed by the device, and delegates it.
	catalog := c.call("catalog", map[string]any{}, "workflows")
	if !strings.Contains(mustJSON(catalog), `"installed_by":"device"`) {
		t.Fatalf("catalog does not mark the device install: %s", accountsSafe(mustJSON(catalog)))
	}
	wid := c.ns + "--agenda--agenda"
	out := c.call("delegate", map[string]any{"workflow": wid, "inputs": map[string]any{"range": "today"}}, "task_id")
	id, _ := out["task_id"].(string)
	if id == "" {
		t.Fatal("Hermes's delegate returned no task ID")
	}
	var answer map[string]any
	for deadline := time.Now().Add(slow(3 * time.Minute)); time.Now().Before(deadline) && answer == nil; {
		res := c.call("result", map[string]any{"task_id": id, "wait_seconds": 25}, "status")
		if res["complete"] != true {
			continue
		}
		if res["status"] != "COMPLETED" {
			t.Fatalf("the agenda failed: %s", accountsSafe(mustJSON(res)))
		}
		answer, _ = res["output"].(map[string]any)
		if answer == nil {
			t.Fatal("the agenda's result has no output")
		}
	}
	if answer == nil {
		t.Fatal("the agenda did not complete")
	}
	envelopeValid(t, "agenda", answer, []string{"calendar"}, []string{`calendar\.google\.com`})
	for _, it := range answer["items"].([]any) {
		if got := plain(it.(map[string]any)["id"]); got != "e-1" {
			t.Fatalf("the agenda answered %q, which is not alice's event", got)
		}
	}
	assertOnlyUser(t, "calendar", cal, "alice")
	if cal.Refused() != 0 {
		t.Fatalf("the calendar refused the credential %d times", cal.Refused())
	}
	t.Log("C1: the phone installed agenda as a connector in one confirm; Hermes delegated it and received alice's agenda")
}
