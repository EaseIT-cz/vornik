//go:build e2e_hermes

package hermes

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Broker write-actions design, "Tier 2: standing grants" (as revised,
// rounds 3 and 4, review 61a5), end to end in an agent namespace: the
// phone approves one write and, with it, future writes to the same address;
// a second write to that address is approved under the grant and sent with
// no phone approval; a write to another address still files a per-write
// approval and is not sent.

const (
	a6Landlord = "a6-landlord"
	a6Other    = "a6-other"
)

func standingScripts() []Script {
	draft := func(marker, to string) Script {
		proposal := map[string]any{"action": "create_draft", "args": map[string]any{"to": to, "subject": "Re: Rent", "body": "Confirmed."}}
		return Script{Marker: marker, Final: "Draft proposed.", Steps: []ScriptStep{
			{ToolSuffix: "mail_search", Args: map[string]any{"query": "rent"}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/propose-create_draft.json", "content": mustJSON(proposal)}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(map[string]any{"drafted": 1})}},
		}}
	}
	return []Script{draft(a6Landlord, "landlord@flat.example"), draft(a6Other, "someone@else.example")}
}

// pendingOnPhone waits up to d for n requests on the phone (n == 0: that
// none appears for the whole of d).
func pendingOnPhone(t *testing.T, p *Phone, n int, d time.Duration) []PendingRequest {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal(err)
		}
		if n > 0 && len(pending) >= n {
			return pending
		}
		if n == 0 && len(pending) > 0 {
			return pending
		}
		if time.Now().After(deadline) {
			return pending
		}
		time.Sleep(500 * time.Millisecond)
	}
}

func TestA6_StandingGrants(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStack(t)
	s.llm.SetScripts(standingScripts()...)
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

	b.mustEffect(t, "create_project", map[string]any{"slug": "mail", "purpose": "Draft replies"}, "applied")
	b.mustEffect(t, "add_mcp_server", map[string]any{"project": "mail", "name": "mail", "url": s.agentMailURL,
		"auth": map[string]any{"mode": "static", "credential": "MAIL_TOKEN"}, "write_tools": []string{"drafts_create"}}, "awaiting_approval")
	approveAll(t, phone, nil)
	b.mustEffect(t, "request_credential", map[string]any{"project": "mail", "name": "MAIL_TOKEN", "purpose": "Read the mailbox and save drafts", "kind": "secret"}, "awaiting_approval")
	approveAll(t, phone, map[string]string{"MAIL_TOKEN": MailTokenCanary})
	b.mustEffect(t, "define_swarm", map[string]any{"slug": "mail", "roles": []map[string]any{
		{"name": "reader", "instructions": "Read mail.", "tools": []string{"mcp__mail__mail_search", "file_write"}}}}, "applied")
	b.mustEffect(t, "define_workflow", map[string]any{"project": "mail", "slug": "drafts", "purpose": "Draft replies",
		"steps": []map[string]any{{"name": "draft", "role": "reader", "instructions": "Draft a reply to the newest mail."}},
		"inputs": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"who"},
			"properties": map[string]any{"who": map[string]any{"enum": []string{a6Landlord, a6Other}}}},
		"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"drafted": map[string]any{"type": "integer"}}},
		"proposes": []map[string]any{{"action": "create_draft", "tool": "mcp__mail-write__drafts_create",
			"standing": map[string]any{"key": []string{"to"}, "max_days": 7, "max_uses": 20},
			"args_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"to":      map[string]any{"type": "string", "pattern": "^[^@ ]+@[^@ ]+$", "maxLength": 100, "x-destination": true},
				"subject": map[string]any{"type": "string", "maxLength": 200, "x-untrusted": true},
				"body":    map[string]any{"type": "string", "maxLength": 512, "x-untrusted": true}}}}},
	}, "awaiting_approval")
	approveAll(t, phone, nil)
	wf := ns + "--mail--drafts"

	// 1. The seed: a per-write approval on the phone, approved with a grant.
	b.waitResult(t, b.delegate(t, wf, map[string]any{"who": a6Landlord}))
	pending := pendingOnPhone(t, phone, 1, slow(time.Minute))
	if len(pending) != 1 {
		t.Fatalf("the seed write filed %d phone requests, want 1", len(pending))
	}
	if err := phone.ApproveWithGrant(pending[0].ID, 7, 20); err != nil {
		t.Fatalf("approve with a grant: %v", err)
	}
	waitFor(t, "the seed draft", slow(2*time.Minute), func() bool { return countTool(s.agentMail, "drafts_create") == 1 })

	// 2. The same address again: no phone request, and it is sent.
	b.waitResult(t, b.delegate(t, wf, map[string]any{"who": a6Landlord}))
	waitFor(t, "the covered draft", slow(2*time.Minute), func() bool { return countTool(s.agentMail, "drafts_create") == 2 })
	if p := pendingOnPhone(t, phone, 0, slow(10*time.Second)); len(p) != 0 {
		t.Fatalf("a covered write filed a phone request: %+v", p)
	}

	// 3. Another address: a per-write approval, nothing sent.
	b.waitResult(t, b.delegate(t, wf, map[string]any{"who": a6Other}))
	other := pendingOnPhone(t, phone, 1, slow(time.Minute))
	if len(other) != 1 || !strings.Contains(other[0].Sentence, "create draft") {
		t.Fatalf("another recipient: phone shows %+v, want one per-write approval", other)
	}
	time.Sleep(slow(5 * time.Second))
	if n := countTool(s.agentMail, "drafts_create"); n != 2 {
		t.Fatalf("a write to another address was sent without approval (%d drafts)", n)
	}
	if err := phone.Reject(other[0].ID); err != nil {
		t.Fatal(err)
	}

	// The Standing approvals page lists the grant with its two writes.
	page, err := phone.get("/ui/approve/standing")
	if err != nil || !strings.Contains(page, "landlord@flat.example") || !strings.Contains(page, "19 of 20 uses left") {
		t.Fatalf("standing page: %v\n%s", err, page)
	}
	b.stop()
}
