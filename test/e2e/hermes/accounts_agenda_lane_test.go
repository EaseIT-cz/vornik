//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func accountsCalendarStub(label, token string) *MCPStub {
	s := NewMCPStub("calendar-"+label, MCPTool{Name: "calendar_list_events", Handle: func(json.RawMessage) (string, bool) {
		return mustJSON(map[string]any{"events": []any{map[string]any{
			"id": label + "-event", "summary": label + " meeting", "start": "2026-10-03T09:00:00Z", "end": "2026-10-03T09:30:00Z", "attendees": 2,
		}}}), false
	}})
	s.Token = token
	return s
}

func accountsAgendaScripts() []Script {
	var scripts []Script
	for _, label := range []string{"work", "personal"} {
		item := map[string]any{"source": "calendar-" + label, "id": label + "-event", "title": label + " meeting",
			"start": "2026-10-03T09:00:00Z", "end": "2026-10-03T09:30:00Z", "location_type": "none", "attendee_count": 2, "conflicts": false}
		scripts = append(scripts, Script{Marker: "mcp__calendar-" + label + "__calendar_list_events", Final: "Agenda written.", Steps: []ScriptStep{
			{ToolSuffix: "calendar-" + label + "__calendar_list_events", Args: map[string]any{"range": "today"}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(map[string]any{
				"status": "ok", "as_of": "2026-10-03T07:00:00Z", "items": []any{item},
			})}},
		}})
	}
	return scripts
}

// Only the named credential's password form receives the canary. Connection
// and workflow approvals receive no value, even when grouped in one install.
func accountsApproveAgenda(t *testing.T, p *Phone, label, token string) {
	t.Helper()
	credential := "CALENDAR_TOKEN_" + strings.ToUpper(label)
	connectionSeen, credentialSeen := false, false
	quiet, deadline := time.Now(), time.Now().Add(slow(2*time.Minute))
	for time.Now().Before(deadline) && time.Since(quiet) < slow(15*time.Second) {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal(accountsSafe(err.Error()))
		}
		if len(pending) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, req := range pending {
			page, err := p.get("/ui/approve/" + req.ID)
			if err != nil {
				t.Fatal(accountsSafe(err.Error()))
			}
			value := ""
			isCredential := strings.Contains(req.Sentence, "add the credential "+credential+" ")
			isConnection := strings.Contains(req.Sentence, "through the server") && strings.Contains(req.Sentence, "calendar-"+label)
			if isCredential || isConnection {
				if !strings.Contains(page, "your "+label+" account") {
					t.Fatalf("agenda approval HTML omitted %s account", label)
				}
			}
			if isCredential {
				if !strings.Contains(page, `name="value"`) || !strings.Contains(page, `type="password"`) {
					t.Fatal("calendar credential approval has no credential input")
				}
				value, credentialSeen = token, true
			} else if strings.Contains(page, `name="value"`) {
				t.Fatal("unexpected credential form during account agenda install")
			}
			connectionSeen = connectionSeen || isConnection
			if err := p.Approve(req.ID, value); err != nil {
				t.Fatal(accountsSafe(err.Error()))
			}
		}
		quiet = time.Now()
	}
	if !connectionSeen || !credentialSeen {
		t.Fatalf("agenda %s install did not expose both connection and credential approvals", label)
	}
}

func accountsAgendaResult(t *testing.T, c *accountsClient, label string) map[string]any {
	t.Helper()
	out := c.call("delegate", map[string]any{"workflow": c.ns + "--comms--agenda-" + label, "inputs": map[string]any{"range": "today"}}, "task_id")
	id, _ := out["task_id"].(string)
	if id == "" {
		t.Fatal("actual Hermes agenda delegate returned no task ID")
	}
	for deadline := time.Now().Add(slow(3 * time.Minute)); time.Now().Before(deadline); {
		result := c.call("result", map[string]any{"task_id": id, "wait_seconds": 25}, "status")
		if result["complete"] != true {
			continue
		}
		if result["status"] != "COMPLETED" {
			t.Fatalf("agenda failed: %s", accountsSafe(mustJSON(result)))
		}
		output, ok := result["output"].(map[string]any)
		if !ok {
			t.Fatal("actual Hermes agenda receipt has no output object")
		}
		return output
	}
	t.Fatal("actual Hermes agenda result did not complete")
	return nil
}

func TestAccountsH_Agenda(t *testing.T) {
	requireLaneAgentImage(t)
	s, producerTrace := accountsStartStack(t)
	p := pairPhone(t, s)
	c := accountsHermesClient(t, s)
	c.producerTrace = producerTrace
	c.sweepQuiet = map[string]string{"broker_actions": "agenda workflows are read-only and propose no write"}
	defer accountsSweep(t, s, c, []string{accountsWorkCalendarToken, accountsPersonalCalendarToken})
	work := accountsCalendarStub("work", accountsWorkCalendarToken)
	personal := accountsCalendarStub("personal", accountsPersonalCalendarToken)
	mux := http.NewServeMux()
	mux.Handle("/calendar-work", work)
	mux.Handle("/calendar-personal", personal)
	base := fmt.Sprintf("http://127.0.0.1:%d", serveLoopback(t, mux))
	s.llm.SetScripts(accountsAgendaScripts()...)
	c.effect(t, "create_project", map[string]any{"slug": "comms", "purpose": "Two account agendas"}, "applied")
	for _, account := range []struct{ label, token string }{{"work", accountsWorkCalendarToken}, {"personal", accountsPersonalCalendarToken}} {
		c.effect(t, "install_recipe", map[string]any{"recipe": "agenda", "project": "comms", "account": account.label,
			"variables": map[string]any{"calendar_server_url": base + "/calendar-" + account.label}, "schedule": nil}, "awaiting_approval")
		accountsApproveAgenda(t, p, account.label, account.token)
	}
	db := laneDB(t, s)
	hashes := make(map[string]string)
	for _, label := range []string{"work", "personal"} {
		var hash, device string
		if err := db.QueryRow(`SELECT reach_hash, approved_by_device FROM agent_workflow_approvals WHERE workflow_id=$1 AND namespace=$2 AND project_id=$3`,
			c.ns+"--comms--agenda-"+label, c.ns, c.ns+"--comms").Scan(&hash, &device); err != nil {
			t.Fatal(accountsSafe(err.Error()))
		}
		if hash == "" || device == "" {
			t.Fatal("agenda workflow lacks persisted reach or device approval")
		}
		hashes[label] = hash
	}
	if hashes["work"] == hashes["personal"] {
		t.Fatal("two account agenda workflows have the same approved reach hash")
	}
	for _, label := range []string{"work", "personal"} {
		out := accountsAgendaResult(t, c, label)
		// Existing envelope diagnostics print fields; sanitize their copy while
		// retaining the original receipt in the complete canary sweep.
		var safeOut map[string]any
		if err := json.Unmarshal([]byte(accountsSafe(mustJSON(out))), &safeOut); err != nil {
			t.Fatal("could not prepare safe agenda envelope diagnostics")
		}
		envelopeValid(t, "agenda-"+label, safeOut, []string{"calendar-" + label}, []string{"calendar.google.com"})
		items := out["items"].([]any)
		if len(items) != 1 {
			t.Fatal("agenda did not return exactly one bounded fixture event")
		}
		item, ok := items[0].(map[string]any)
		if !ok || plain(item["id"]) != label+"-event" || plain(item["title"]) != label+" meeting" || plain(out["status"]) != "ok" {
			t.Fatal("agenda receipt did not contain its account's distinct fixture event")
		}
	}
	if countTool(work, "calendar_list_events") != 1 || countTool(personal, "calendar_list_events") != 1 || work.Refused() != 0 || personal.Refused() != 0 {
		t.Fatal("agenda calendar counters differ from work=1 personal=1 refused=0")
	}
	accountsAssertDevice(t, s, p, "", []string{accountsWorkCalendarToken, accountsPersonalCalendarToken})
	t.Log("B11 H: actual Hermes received distinct work/personal agendas; separate device approvals, reach hashes and authenticated calendar calls")
}
