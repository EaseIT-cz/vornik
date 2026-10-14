//go:build e2e_hermes

package hermes

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/test/standins"
)

// A9 (Hermes lane design, amendment 2026-10-12 "Stand-in services for the
// recipe path", C3; GitHub #337): an MCP server behind OAuth, connected from
// the phone through the real redirect chain, tested with test_connection,
// used by an agent-defined workflow that proposes a write, and the write sent
// once after the phone approves it, under the signed-in user's token.

const notesMarker = "[A9-NOTES]"

// a9Note is the write the scripted agent proposes.
var a9Note = map[string]any{"contact_id": "c-1", "note": "Renewal call booked for Friday."}

func a9Scripts() []Script {
	return []Script{{Marker: notesMarker, Final: "Note proposed.", Steps: []ScriptStep{
		{ToolSuffix: "crm_search_contacts", Args: map[string]any{"query": "acme"}},
		{ToolSuffix: "crm_get_contact", Args: map[string]any{"id": "c-1"}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/propose-add_note.json",
			"content": mustJSON(map[string]any{"action": "add_note", "args": a9Note})}},
		{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(map[string]any{"proposed": 1})}},
	}}}
}

// withPublicOrigin sets server.public_base_url to the lane daemon's own
// address: the OAuth redirect URI is built from it (mcpconnect.CallbackPath).
func withPublicOrigin(cfg string) string {
	m := regexp.MustCompile(`(?m)^  address: "(127\.0\.0\.1:\d+)"$`).FindStringSubmatch(cfg)
	if m == nil {
		return cfg
	}
	return strings.Replace(cfg, m[0], m[0]+"\n  public_base_url: \"http://"+m[1]+"\"", 1)
}

// waitPending waits until the phone has at least one pending request whose
// sentence contains want, and returns its id.
func waitPending(t *testing.T, p *Phone, want string) string {
	t.Helper()
	var id string
	waitFor(t, "a pending request naming "+want, slow(time.Minute), func() bool {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range pending {
			if strings.Contains(r.Sentence, want) {
				id = r.ID
				return true
			}
		}
		return false
	})
	return id
}

func TestA9_OAuthConnectorWrite(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStackWith(t, withPublicOrigin, nil)
	s.llm.SetScripts(a9Scripts()...)

	store := standins.NewCRMStore()
	store.AddContact("alice", standins.Contact{ID: "c-1", Name: "Jana Novak", Company: "Acme", Email: "jana@acme.example"})
	store.AddContact("bob", standins.Contact{ID: "c-9", Name: "Bob's contact", Company: "Acme"})
	as := standins.NewOAuthServer("alice", "alice@home.example")
	crm := standins.NewCRMMCP(store)
	crm.Protect(as)
	crmURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", serveLoopback(t, as.Handler(crm)))
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

	// 1. The server, then its OAuth slot; the phone signs in with Connect.
	b.mustEffect(t, "create_project", map[string]any{"slug": "sales", "purpose": "Customer notes"}, "applied")
	b.mustEffect(t, "add_mcp_server", map[string]any{"project": "sales", "name": "crm", "url": crmURL,
		"auth": map[string]any{"mode": "oauth", "scopes": []string{"read", "write"}}, "write_tools": []string{"crm_add_note"}}, "awaiting_approval")
	if n := approveAll(t, phone, nil); n < 1 {
		t.Fatal("the phone saw no request for the server")
	}
	b.mustEffect(t, "request_credential", map[string]any{"project": "sales", "name": "OAUTH_CRM", "purpose": "Read contacts and add notes", "kind": "oauth"}, "awaiting_approval")
	page, err := phone.Connect(waitPending(t, phone, "OAUTH_CRM"))
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if !strings.Contains(page, "Signed in as alice@home.example") {
		t.Fatalf("the callback page does not name the account: %s", page)
	}
	if as.Exchanges() != 1 {
		t.Fatalf("token exchanges = %d, want 1", as.Exchanges())
	}
	// The tools the daemon lists with the new token.
	waitPending(t, phone, "crm_search_contacts")
	approveAll(t, phone, nil)
	// The provider's id_token named the account, so the answer says whose.
	testConnectionOK(t, b, "sales", "crm", map[string]any{"account": "default", "identity_source": "provider"})
	t.Logf("egress guard: %d connection URLs examined, all loopback", assertLoopbackOnly(t, s))

	// 2. The workflow reads and proposes one note; nothing is written yet.
	b.mustEffect(t, "define_swarm", map[string]any{"slug": "sales", "roles": []map[string]any{
		{"name": "reader", "instructions": "Read contacts.", "tools": []string{"mcp__crm__crm_search_contacts", "mcp__crm__crm_get_contact", "file_write"}}}}, "applied")
	b.mustEffect(t, "define_workflow", map[string]any{"project": "sales", "slug": "notes", "purpose": "Note a customer call",
		"steps":  []map[string]any{{"name": "note", "role": "reader", "instructions": notesMarker + " Propose a note for the Acme contact."}},
		"inputs": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"proposed": map[string]any{"type": "integer"}}},
		"proposes": []map[string]any{{"action": "add_note", "tool": "mcp__crm-write__crm_add_note", "args_schema": map[string]any{
			"type": "object", "additionalProperties": false, "required": []string{"contact_id", "note"}, "properties": map[string]any{
				"contact_id": map[string]any{"type": "string", "pattern": "^c-[0-9]+$", "maxLength": 20},
				"note":       map[string]any{"type": "string", "maxLength": 200, "x-untrusted": true}}}}},
	}, "awaiting_approval")
	approveAll(t, phone, nil)
	res := b.waitResult(t, b.delegate(t, ns+"--sales--notes", map[string]any{}))
	if !strings.Contains(mustJSON(res), "add_note") {
		t.Fatalf("the proposal is not in the result: %v", res)
	}
	if n := len(crm.Writes()); n != 0 {
		t.Fatalf("%d writes before the phone approved", n)
	}
	if len(crm.CallsBy("alice")) == 0 {
		t.Fatal("the workflow never read the CRM: the OAuth token was not exercised")
	}

	// 3. The phone approves; the write goes once, as alice, with exactly the
	// proposed arguments (the daemon may add its own project_id).
	approveAll(t, phone, nil)
	waitFor(t, "the approved note", slow(2*time.Minute), func() bool { return len(crm.Writes()) == 1 })
	w := crm.Writes()[0]
	if w.User != "alice" || w.Tool != "crm_add_note" {
		t.Fatalf("write %+v", w)
	}
	var sent map[string]any
	if err := json.Unmarshal(w.Args, &sent); err != nil {
		t.Fatal(err)
	}
	delete(sent, "project_id")
	if !reflect.DeepEqual(sent, a9Note) {
		t.Fatalf("sent %v, proposed %v", sent, a9Note)
	}
	if c, _ := store.Contact("alice", "c-1"); len(c.Notes) != 1 {
		t.Fatalf("alice's contact notes: %v", c.Notes)
	}
	for _, c := range crm.Calls() {
		if c.User != "alice" {
			t.Fatalf("a call with %s's credential", c.User)
		}
	}

	// Every request was decided by the phone with the hash it was shown; no
	// agent secret was stored (an OAuth grant lives in the token store).
	b.stop()
	assertDeviceApprovals(t, s, phone, 0)
	sweep := NewSweep(as.IssuedTokens(), nil)
	sweep.AddText("bridge transcript", true, b.Transcript())
	sweep.AddText("the callback page the phone saw", true, page)
	// No credential-shaped value is put in front of the model in this arm
	// (A1's hostile probe is what fills the redaction audit), so the table
	// must stay empty and is swept the moment it is not.
	sweepVornik(t, s, sweep, map[string]string{"secret_redaction_audit": "this arm puts no credential-shaped value in front of the model"})
	t.Logf("canary sweep:\n%s", sweep.Report())
	if v := sweep.Violations(); len(v) != 0 {
		t.Fatalf("canary sweep:\n%s", strings.Join(v, "\n"))
	}
}
