package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #119 item 8 (2026-10-08): a write action's phone request names the
// account it sends from (agent-administered design §22.2.6; Review Focus 1),
// in the sentence and in the canonical payload the device approves. A
// default account with no identity leaves both byte-identical; an account
// that cannot be confirmed (the approval row and the project file disagree)
// is said, never guessed.

func writeFixture(t *testing.T, name string, acct *agentadmin.AccountInput) *agentAdminFixture {
	t.Helper()
	f := newAgentAdminFixture(t)
	f.do(agentadmin.VerbCreateProject, agentadmin.CreateProjectInput{Slug: "comms", Purpose: "Mail"})
	f.approve(f.do(agentadmin.VerbAddMCPServer, agentadmin.AddMCPServerInput{Project: "comms", Name: name, URL: "https://mail.example/mcp",
		Auth: agentadmin.MCPAuthInput{Mode: "static", Credential: "MAIL_TOKEN"}, WriteTools: []string{"send"}, Account: acct}))
	return f
}

func goldenAction(t *testing.T, f *agentAdminFixture, tool string) *persistence.BrokerAction {
	t.Helper()
	a := &persistence.BrokerAction{ActionID: "act_golden", ProjectID: "hermes--comms", TaskID: "task_golden", WorkflowID: "hermes--comms--reply",
		ActionKind: "send_mail", Tool: tool, ArgsJSON: []byte(`{"subject":"hi","to":"bob@example.com"}`), ArgsSHA256: "sha-golden",
		Status: persistence.BrokerActionStaged, ExpiresAt: time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)}
	ctx := context.Background()
	if _, err := f.c.repos.BrokerActions.Stage(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.repos.BrokerActions.PromoteStaged(ctx, a.TaskID); err != nil {
		t.Fatal(err)
	}
	stored, err := f.c.repos.BrokerActions.Get(ctx, a.ActionID)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestWriteActionPayload_DefaultAccountUnchanged(t *testing.T) {
	f := writeFixture(t, "mail", nil)
	row, err := f.svc.actionApprovalRow(context.Background(), "hermes", goldenAction(t, f, "mcp__mail-write__send"))
	if err != nil {
		t.Fatal(err)
	}
	wantRendered, _ := os.ReadFile("testdata/accounts_golden/action_request.rendered.json")
	wantSentence, _ := os.ReadFile("testdata/accounts_golden/action_request.sentence.txt")
	if string(row.Rendered) != string(wantRendered) || row.Sentence != string(wantSentence) {
		t.Fatalf("a default-account write request changed:\n%s\n%s", row.Rendered, row.Sentence)
	}
}

func TestPlain_WriteActionNamesTheAccount(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(context.Background(), "hermes--comms", "mail-work", "alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	row, err := f.svc.actionApprovalRow(context.Background(), "hermes", goldenAction(t, f, "mcp__mail-work-write__send"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Sentence, "wants to send mail with mail-work (send) from your work account (alice@corp.example).") {
		t.Fatalf("sentence: %s", row.Sentence)
	}
	if !strings.Contains(string(row.Rendered), `"account":{"identity":"alice@corp.example","identity_source":"provider","label":"work"}`) {
		t.Fatalf("the approved payload does not carry the account: %s", row.Rendered)
	}
	// TestDevicePage_ShowsAccountLine: the device page leads with the
	// request's description, which carries the account.
	if d := describeAction(row); d == nil || !strings.Contains(d.Summary, "from your work account (alice@corp.example)") {
		t.Fatalf("device description: %+v", d)
	}
}

// C5 through the service: the state read says which credentials are stored,
// so a second request for a stored key says whose key it replaces.
func TestCredentialSentence_RotationReadFromTheStore(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	first := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: "MAIL_TOKEN", Purpose: "read mail", Kind: "secret"})
	if strings.Contains(first.Sentence, "replaces") {
		t.Fatalf("a first entry speaks of replacing: %s", first.Sentence)
	}
	slot, err := f.c.repos.ApproverDevices.GetRequest(context.Background(), requestIDOf(first))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.svc.enterCredential(context.Background(), f.device, *slot, slot.RenderedSHA256, []byte("v")); err != nil {
		t.Fatal(err)
	}
	f.svc.bg.Wait()
	again := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: "MAIL_TOKEN", Purpose: "rotate", Kind: "secret"})
	if !strings.Contains(again.Sentence, "This replaces the key now stored for your work account") {
		t.Fatalf("a rotation does not say whose key it replaces: %s", again.Sentence)
	}
}

func TestWriteActionAccount_MismatchFailsClosed(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	// A hand edit makes the file say another account than the approval.
	path := filepath.Join(f.cfgDir, "configs", "projects", "hermes--comms.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(b), `account: "work"`, `account: "personal"`)
	if edited == string(b) {
		t.Fatal("the edit did not apply")
	}
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.c.ConfigReloader.Reload(); err != nil {
		t.Fatal(err)
	}
	row, err := f.svc.actionApprovalRow(context.Background(), "hermes", goldenAction(t, f, "mcp__mail-work-write__send"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(row.Sentence, "; the account of mail-work could not be confirmed.") || strings.Contains(row.Sentence, "personal") ||
		strings.Contains(string(row.Rendered), `"account"`) {
		t.Fatalf("a mismatched account was not said: %s / %s", row.Sentence, row.Rendered)
	}
}
