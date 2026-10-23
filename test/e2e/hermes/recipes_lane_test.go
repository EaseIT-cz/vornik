//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/test/standins"
)

// Agent-administered Vornik design §19.8 test 11 (recipes): each shipped
// recipe, installed through the admin verbs, approved and given its
// credentials on the scripted phone, runs against stub MCP servers offering
// the reference broker's tool names and returns an envelope-valid answer.
// The daemon's own egress check (the answering step and result) is the
// schema; this test checks the envelope's shape on what result returned.
//
// Amended 2026-10-12 (GitHub #337, Hermes lane design "Stand-in services for
// the recipe path"): the servers are the stateful stand-ins of
// test/standins, each holding two users' data; the recipe's credentials are
// alice's. The arm also calls test_connection, checks that nothing of bob's
// was read or answered, and that every connection the installs rendered
// points at loopback.

// CalendarTokenCanary is alice's calendar bearer.
const CalendarTokenCanary = "ghp_CANARYCALENDAR0123456789abcdefghijklm"

// Bob's bearers and the canary only bob's seeded message holds.
const (
	BobMailTokenCanary     = "ghp_CANARYBOBMAIL0123456789abcdefghijklmn"
	BobCalendarTokenCanary = "ghp_CANARYBOBCAL0123456789abcdefghijklmno"
	BobMailCanary          = "RAWMAIL-BOB-CANARY-4c2d"
)

// recipeMail is the mail stand-in offering the reference broker's read
// tools (configs/examples/broker-mail.yaml): alice's mailbox holds the
// invoice r-1 whose body carries RawMailCanary, behind MailTokenCanary;
// bob's holds his own message behind his own token.
func recipeMail() *standins.MCPServer {
	store := standins.NewMailStore()
	store.AddUser("alice", "alice@home.example")
	store.AddUser("bob", "bob@work.example")
	now := time.Now().UTC()
	store.Deliver("alice", standins.Message{ID: "r-1", From: "billing@acme.example", Subject: "Invoice 4411",
		Body: "Please pay invoice 4411. " + RawMailCanary, Received: now.Add(-2 * time.Hour), Unread: true})
	store.Deliver("bob", standins.Message{ID: "b-1", From: "boss@work.example", Subject: "Quarterly plan",
		Body: "Bob only. " + BobMailCanary, Received: now.Add(-time.Hour), Unread: true})
	s := standins.NewMailMCP(store, func() time.Time { return time.Now().UTC() })
	s.Token(MailTokenCanary, "alice")
	s.Token(BobMailTokenCanary, "bob")
	return s
}

// recipeCalendar is the calendar stand-in: alice's standup e-1 at noon UTC
// today (inside the day's window whenever the lane runs), bob's own event.
func recipeCalendar() *standins.MCPServer {
	store := standins.NewCalendarStore()
	now := time.Now().UTC()
	noon := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	store.AddEvent("alice", standins.Event{ID: "e-1", Title: "Standup", Start: noon, End: noon.Add(30 * time.Minute), Attendees: 4, Link: "https://meet.example/x"})
	store.AddEvent("bob", standins.Event{ID: "e-9", Title: "Bob review", Start: noon, End: noon.Add(time.Hour), Attendees: 2})
	s := standins.NewCalendarMCP(store, func() time.Time { return time.Now().UTC() })
	s.Token(CalendarTokenCanary, "alice")
	s.Token(BobCalendarTokenCanary, "bob")
	return s
}

// testConnectionOK calls test_connection for a connection and requires the
// closed answer {code: ok, http_status_class: 2xx, checked_at} and nothing
// else (agent-administered design §6.2), plus exactly the keys in extra (an
// OAuth connection names its account and that the provider reported the
// identity, design §22.2.9).
func testConnectionOK(t *testing.T, b *bridgeSession, project, name string, extra ...map[string]any) {
	t.Helper()
	text, isErr, err := b.Tool("test_connection", map[string]any{"project": project, "name": name})
	if err != nil || isErr {
		t.Fatalf("test_connection %s: %s %v %v", name, text, isErr, err)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(text), &res); err != nil {
		t.Fatalf("test_connection %s: not JSON: %s", name, text)
	}
	want := 3
	for _, e := range extra {
		for k, v := range e {
			want++
			if res[k] != v {
				t.Fatalf("test_connection %s: %s is %v, want %v (%s)", name, k, res[k], v, text)
			}
		}
	}
	if len(res) != want || res["code"] != "ok" || res["http_status_class"] != "2xx" || res["checked_at"] == nil {
		t.Fatalf("test_connection %s: %s", name, text)
	}
}

// assertLoopbackOnly reads every project file under the lane's config tree
// and fails when an MCP server or API URL is not loopback: a run that
// reached a real provider cannot pass (GitHub #337, C5). It returns how many
// URLs it examined, and fails on zero (a check over nothing proves nothing).
func assertLoopbackOnly(t *testing.T, s *stack) int {
	t.Helper()
	dir := filepath.Join(filepath.Dir(s.cfgPath), "configs", "projects")
	urlRe := regexp.MustCompile(`(?m)^\s*(?:-\s*)?(?:url|base_url|baseUrl|baseURL):\s*"?([^"\s]+)"?`)
	n := 0
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, m := range urlRe.FindAllStringSubmatch(string(b), -1) {
			n++
			u, err := url.Parse(m[1])
			if err != nil || (u.Hostname() != "127.0.0.1" && u.Hostname() != "localhost" && u.Hostname() != "::1") {
				t.Errorf("%s names a non-loopback connection %q", path, m[1])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatalf("no connection URL found under %s: the egress guard examined nothing", dir)
	}
	return n
}

// assertOnlyUser fails when a stand-in recorded a call made with another
// user's credential, or none at all with user's.
func assertOnlyUser(t *testing.T, name string, s *standins.MCPServer, user string) {
	t.Helper()
	calls := s.Calls()
	if len(calls) == 0 {
		t.Fatalf("%s: never called: the credential was not exercised", name)
	}
	for _, c := range calls {
		if c.User != user {
			t.Fatalf("%s: a %s call with %s's credential", name, c.Tool, c.User)
		}
	}
}

// todayEventsArgs are the reference calendar.listEvents arguments for
// today UTC, the window the seeded e-1 sits in (GitHub #404: the stand-in
// takes calendarId, timeMin and timeMax, as the reference does, not range).
func todayEventsArgs() map[string]any {
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return map[string]any{"calendarId": "primary", "timeMin": day.Format(time.RFC3339),
		"timeMax": day.Add(24 * time.Hour).Format(time.RFC3339)}
}

// assertNoFailedCall fails when a stand-in answered a call with a tool
// error. GitHub #404: the scripted model writes the same answer whether a
// read worked or not, so without this the lane passed with argument names
// the stand-in refused.
func assertNoFailedCall(t *testing.T, name string, s *standins.MCPServer) {
	t.Helper()
	for _, c := range s.Failed() {
		t.Fatalf("%s: %s %s was refused: %s", name, c.Tool, c.Args, c.Err)
	}
}

// The scripted model's plans, keyed on each recipe step's own opening line.
func recipeScripts() []Script {
	write := func(doc any) ScriptStep {
		return ScriptStep{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(doc)}}
	}
	search := ScriptStep{ToolSuffix: "gmail_search", Args: map[string]any{"query": "newer_than:1d"}}
	get := ScriptStep{ToolSuffix: "gmail_get", Args: map[string]any{"messageId": "r-1"}}
	events := ScriptStep{ToolSuffix: "calendar_list_events", Args: todayEventsArgs()}
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
	mail, cal := recipeMail(), recipeCalendar()
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
	// GitHub #337: the provider accepts each stored credential before any
	// workflow relies on it, and every rendered connection is loopback.
	testConnectionOK(t, b, "personal", "mail")
	testConnectionOK(t, b, "personal", "calendar")
	t.Logf("egress guard: %d connection URLs examined, all loopback", assertLoopbackOnly(t, s))

	answers := map[string]map[string]any{
		"inbox-digest":  run("inbox-digest", map[string]any{"since": "24h"}),
		"agenda":        run("agenda", map[string]any{"range": "today"}),
		"morning-brief": run("morning-brief", map[string]any{}),
	}
	envelopeValid(t, "inbox-digest", answers["inbox-digest"], []string{"mail"}, []string{`mail\.google\.com`})
	envelopeValid(t, "agenda", answers["agenda"], []string{"calendar"}, []string{`calendar\.google\.com`})
	envelopeValid(t, "morning-brief", answers["morning-brief"], []string{"mail", "calendar"}, []string{`mail\.google\.com`, `calendar\.google\.com`})
	// Every answered id is one of alice's seeded items; none is bob's, and no
	// answer carries a seeded body.
	alice := map[string]bool{"r-1": true, "e-1": true}
	for name, out := range answers {
		for _, it := range out["items"].([]any) {
			if id := plain(it.(map[string]any)["id"]); !alice[id] {
				t.Fatalf("%s answered item %q, which is not alice's", name, id)
			}
		}
		if j := mustJSON(out); strings.Contains(j, RawMailCanary) || strings.Contains(j, BobMailCanary) {
			t.Fatalf("%s: a seeded body reached the answer", name)
		}
	}

	// The credentials reached the servers they belong to, and selected
	// alice's data only: no call was made with bob's.
	if len(mail.CallsBy("alice")) == 0 || len(cal.CallsBy("alice")) == 0 {
		t.Fatal("a recipe never read its server")
	}
	assertOnlyUser(t, "mail", mail, "alice")
	assertOnlyUser(t, "calendar", cal, "alice")
	assertNoFailedCall(t, "mail", mail)
	assertNoFailedCall(t, "calendar", cal)
	if mail.Refused() != 0 || cal.Refused() != 0 {
		t.Fatalf("a server refused the credential: mail %d, calendar %d", mail.Refused(), cal.Refused())
	}
	if text, _, _ := b.Tool("list_recipes", nil); strings.Count(text, `"project":"`+ns+`--personal"`) != 3 {
		t.Fatalf("list_recipes does not show the three installs: %s", text)
	}
	if tr := b.Transcript(); strings.Contains(tr, RawMailCanary) || strings.Contains(tr, BobMailCanary) ||
		strings.Contains(tr, MailTokenCanary) || strings.Contains(tr, CalendarTokenCanary) {
		t.Fatal("a mail body or a credential reached the harness")
	}
}
