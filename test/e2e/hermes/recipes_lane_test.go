//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Agent-administered Vornik design §19.8 test 11 (recipes): each shipped
// recipe, installed through the admin verbs, approved and given its
// credentials on the scripted phone, runs against stub MCP servers offering
// the reference broker's tool names and returns an envelope-valid answer.
// The daemon's own egress check (the answering step and result) is the
// schema; this test checks the envelope's shape on what result returned.

// CalendarTokenCanary is the calendar stub's bearer.
const CalendarTokenCanary = "ghp_CANARYCALENDAR0123456789abcdefghijklm"

// recipeMailStub offers the reference broker's read tools (configs/examples/
// broker-mail.yaml) behind MailTokenCanary.
func recipeMailStub() *MCPStub {
	s := NewMCPStub("mail",
		MCPTool{Name: "gmail_search", Handle: func(json.RawMessage) (string, bool) {
			return `{"messages":[{"id":"r-1","from":"billing@acme.example","subject":"Invoice 4411","received_at":"2026-10-03T06:04:00Z"}]}`, false
		}},
		MCPTool{Name: "gmail_get", Handle: func(json.RawMessage) (string, bool) {
			return `{"id":"r-1","from":"billing@acme.example","subject":"Invoice 4411","received_at":"2026-10-03T06:04:00Z","body":"Please pay invoice 4411. ` + RawMailCanary + `"}`, false
		}},
	)
	s.Token = MailTokenCanary
	return s
}

func recipeCalendarStub() *MCPStub {
	s := NewMCPStub("calendar",
		MCPTool{Name: "calendar_list_events", Handle: func(json.RawMessage) (string, bool) {
			return `{"events":[{"id":"e-1","start":"2026-10-03T09:00:00Z","end":"2026-10-03T09:30:00Z","summary":"Standup","attendees":4,"hangoutLink":"https://meet.example/x"}]}`, false
		}},
	)
	s.Token = CalendarTokenCanary
	return s
}

// The scripted model's plans, keyed on each recipe step's own opening line.
func recipeScripts() []Script {
	write := func(doc any) ScriptStep {
		return ScriptStep{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(doc)}}
	}
	search := ScriptStep{ToolSuffix: "gmail_search", Args: map[string]any{"query": "newer_than:1d"}}
	get := ScriptStep{ToolSuffix: "gmail_get", Args: map[string]any{"id": "r-1"}}
	events := ScriptStep{ToolSuffix: "calendar_list_events", Args: map[string]any{"range": "today"}}
	message := map[string]any{"source": "mail", "id": "r-1", "link": "https://mail.google.com/mail/u/0/#inbox/r-1",
		"received_at": "2026-10-03T06:04:00Z", "from_domain": "acme.example", "category": "invoice", "needs_reply": true,
		"one_line": "An invoice from acme.example is due."}
	event := map[string]any{"source": "calendar", "id": "e-1", "start": "2026-10-03T09:00:00Z", "end": "2026-10-03T09:30:00Z",
		"title": "Standup", "location_type": "online", "attendee_count": 4, "conflicts": false}
	return []Script{
		{Marker: "Build the inbox digest", Final: "Digest written.", Steps: []ScriptStep{search, get, write(map[string]any{
			"status": "ok", "as_of": "2026-10-03T07:00:00Z", "items": []any{message},
			"counts": map[string]any{"total": 1, "shown": 1, "suppressed": 0}})}},
		{Marker: "Build the agenda", Final: "Agenda written.", Steps: []ScriptStep{events, write(map[string]any{
			"status": "ok", "as_of": "2026-10-03T07:00:00Z", "items": []any{event}})}},
		{Marker: "Build the morning brief", Final: "Brief written.", Steps: []ScriptStep{events, search, get, write(map[string]any{
			"status": "partial", "as_of": "2026-10-03T07:00:00Z", "errors": []any{map[string]any{"source": "mail", "kind": "quota"}},
			"items": []any{
				map[string]any{"source": "calendar", "id": "e-1", "time": "2026-10-03T09:00:00Z", "title": "Standup", "location_type": "online", "attendee_count": 4, "conflicts": false},
				map[string]any{"source": "mail", "id": "r-1", "time": "2026-10-03T06:04:00Z", "from_domain": "acme.example", "one_line": "An invoice is due.", "needs_reply": true},
			}})}},
	}
}

// approveRecipes approves every pending request until none is left for a
// quiet spell, entering a credential only on the request that asks for it.
func approveRecipes(t *testing.T, p *Phone, values map[string]string) int {
	t.Helper()
	n, quiet := 0, time.Now()
	for time.Since(quiet) < slow(15*time.Second) {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, req := range pending {
			value := ""
			for name, v := range values {
				if strings.Contains(req.Sentence, "add the credential "+name) {
					value = v
				}
			}
			if err := p.Approve(req.ID, value); err != nil {
				t.Fatalf("approve %s (%s): %v", req.ID, req.Sentence, err)
			}
			n++
		}
		quiet = time.Now()
	}
	return n
}

// envelopeValid checks the answer envelope (design §19.3) on a result's
// output: status, as_of, items with a source among want and a link only on
// one of hosts.
func envelopeValid(t *testing.T, name string, out map[string]any, sources []string, hosts []string) {
	t.Helper()
	if out == nil {
		t.Fatalf("%s: no output", name)
	}
	switch plain(out["status"]) {
	case "ok", "partial", "error":
	default:
		t.Fatalf("%s: status %v", name, out["status"])
	}
	if _, err := time.Parse(time.RFC3339, plain(out["as_of"])); err != nil {
		t.Fatalf("%s: as_of %v", name, out["as_of"])
	}
	items, ok := out["items"].([]any)
	if !ok || len(items) == 0 {
		t.Fatalf("%s: items %v", name, out["items"])
	}
	link := regexp.MustCompile(`^https://(` + strings.Join(hosts, "|") + `)/`)
	for _, it := range items {
		m := it.(map[string]any)
		if !containsStr(sources, plain(m["source"])) {
			t.Fatalf("%s: item source %v", name, m["source"])
		}
		if l, ok := m["link"]; ok && !link.MatchString(plain(l)) {
			t.Fatalf("%s: link %v", name, l)
		}
	}
}

// untrustedRe is the marking result() puts around every string an agent
// workflow returned: data, never instructions, to the harness.
var untrustedRe = regexp.MustCompile(`(?s)^<untrusted_content[^>]*>\s*(.*?)\s*</untrusted_content>$`)

// plain is a returned string without its untrusted-content marking.
func plain(v any) string {
	s := fmt.Sprint(v)
	if m := untrustedRe.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return s
}

func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func TestA4_Recipes(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStack(t)
	s.llm.SetScripts(recipeScripts()...)
	mail, cal := recipeMailStub(), recipeCalendarStub()
	mailURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", serveLoopback(t, mail))
	calURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", serveLoopback(t, cal))
	phone := pairPhone(t, s)

	env := newAgentEnv(t)
	connect := exec.Command(s.ctl, "agent", "connect", "claude-desktop", "--url", s.apiURL)
	connect.Env = env.vars("VORNIK_API_KEY=" + s.adminKey)
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	ns := "claudedesktop"
	b := startBridge(t, env, filepath.Join(env.cfg, "Claude", "claude_desktop_config.json"), "vornik-"+ns)
	if _, err := b.Call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "lane"}}); err != nil {
		t.Fatal(err)
	}
	_ = b.Notify("notifications/initialized")
	if text, _, _ := b.Tool("list_recipes", nil); !strings.Contains(text, "morning-brief") {
		t.Fatalf("list_recipes: %s", text)
	}
	b.mustEffect(t, "create_project", map[string]any{"slug": "personal", "purpose": "Mail and calendar"}, "applied")
	values := map[string]string{"MAIL_TOKEN": MailTokenCanary, "CALENDAR_TOKEN": CalendarTokenCanary}

	install := func(recipe string, vars map[string]any) {
		t.Helper()
		r := b.mustEffect(t, "install_recipe", map[string]any{"recipe": recipe, "project": "personal", "variables": vars, "schedule": nil}, "awaiting_approval")
		if !strings.Contains(r.Sentence, "Install") && !strings.Contains(r.Sentence, "install") {
			t.Fatalf("%s: sentence %q", recipe, r.Sentence)
		}
		// The install, then its credentials, then the tools the daemon
		// lists once a credential is stored.
		if n := approveRecipes(t, phone, values); n < 1 {
			t.Fatalf("%s: the phone saw no request", recipe)
		}
	}
	install("inbox-digest", map[string]any{"mail_server_url": mailURL})
	install("agenda", map[string]any{"calendar_server_url": calURL})
	// Both servers are reused: one approval, for the workflow alone.
	install("morning-brief", map[string]any{"mail_server_url": mailURL, "calendar_server_url": calURL})

	run := func(recipe string, inputs map[string]any) map[string]any {
		t.Helper()
		res := b.waitResult(t, b.delegate(t, ns+"--personal--"+recipe, inputs))
		out, _ := res["output"].(map[string]any)
		if out == nil {
			t.Fatalf("%s: %v", recipe, res)
		}
		return out
	}
	envelopeValid(t, "inbox-digest", run("inbox-digest", map[string]any{"since": "24h"}), []string{"mail"}, []string{`mail\.google\.com`})
	envelopeValid(t, "agenda", run("agenda", map[string]any{"range": "today"}), []string{"calendar"}, []string{`calendar\.google\.com`})
	envelopeValid(t, "morning-brief", run("morning-brief", map[string]any{}), []string{"mail", "calendar"}, []string{`mail\.google\.com`, `calendar\.google\.com`})

	// The credentials reached the servers they belong to.
	if countTool(mail, "gmail_get") == 0 || countTool(cal, "calendar_list_events") == 0 {
		t.Fatal("a recipe never read its server")
	}
	if mail.Refused() != 0 || cal.Refused() != 0 {
		t.Fatalf("a server refused the credential: mail %d, calendar %d", mail.Refused(), cal.Refused())
	}
	if text, _, _ := b.Tool("list_recipes", nil); strings.Count(text, `"project":"`+ns+`--personal"`) != 3 {
		t.Fatalf("list_recipes does not show the three installs: %s", text)
	}
	if strings.Contains(b.Transcript(), RawMailCanary) || strings.Contains(b.Transcript(), MailTokenCanary) || strings.Contains(b.Transcript(), CalendarTokenCanary) {
		t.Fatal("a mail body or a credential reached the harness")
	}
}
