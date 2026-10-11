package agentadmin

import (
	"encoding/json"
	"testing"
)

// GitHub #119 item 8 (2026-10-08): the reach signature binds each reached
// connection's account (agent-administered design §22.2.4). It is filled
// only when some label is not the default, so every existing approved hash
// is unchanged (operator decision F1/F4), and re-labelling a reached
// connection is widening.

// reachM6Golden is the hash the B0 measurement M6 recorded at the lane's
// base (§22.1.2) for a workflow reading mail-work with MAIL_WORK.
const reachM6Golden = "c817d0cb68ee70c639c3dc36dd86ebd190929e5e8c11d9bf0232f1e9b97411c4"

func accountWorkflowTree(t *testing.T, label string) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.project("comms")
	tr.advert["https://mail.example/mcp"] = []string{"read_inbox", "send"}
	in := AddMCPServerInput{Project: "comms", Name: "mail-work", URL: "https://mail.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "MAIL_WORK"}, WriteTools: []string{"send"}}
	if label != "" {
		in.Account = &AccountInput{Label: label}
	}
	tr.apply(tr.render(VerbAddMCPServer, in))
	tr.apply(tr.render(VerbDefineSwarm, DefineSwarmInput{Slug: "comms", Roles: []RoleInput{
		{Name: "reader", Instructions: "read", Tools: []string{"file_write", "mcp__mail-work__read_inbox"}}}}))
	return tr
}

func digestWF(tr *tree, proposes string) Change {
	tr.t.Helper()
	in := DefineWorkflowInput{Project: "comms", Slug: "digest", Purpose: "digest",
		Steps:  []StepInput{{Name: "read", Role: "reader", Instructions: "Read."}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"done":{"type":"boolean"}}}`)}
	if proposes != "" {
		in.Proposes = json.RawMessage(proposes)
	}
	return tr.render(VerbDefineWorkflow, in)
}

func TestReach_DefaultAccountsKeepTheHash(t *testing.T) {
	tr := accountWorkflowTree(t, "")
	c := digestWF(tr, "")
	if c.reach == nil || c.reach.Accounts != nil || c.reach.Hash() != reachM6Golden {
		t.Fatalf("a default-account workflow's reach moved: %+v", c.reach)
	}
	// The pre-document golden is unchanged too.
	if got := reachHashOf(t, ""); got != reachGoldenNoStanding {
		t.Fatalf("reach golden moved: %s", got)
	}
}

func TestReach_AccountChangeIsWidening(t *testing.T) {
	tr := accountWorkflowTree(t, "")
	c := digestWF(tr, "")
	tr.apply(c)
	// The same workflow over a connection re-added as the work account:
	// the signature names the account, its hash differs, and defining the
	// workflow again is widening.
	work := accountWorkflowTree(t, "work")
	cw := digestWF(work, "")
	if cw.reach == nil || cw.reach.Accounts["mail-work"] != "work" || cw.reach.Hash() == reachM6Golden {
		t.Fatalf("a labelled connection is not in the reach: %+v", cw.reach)
	}
	work.reach["hermes--comms--digest"] = c.reach.Hash() // approved while default
	if again := digestWF(work, ""); again.Class != Widening {
		t.Fatalf("re-pointing a workflow to another account is %s, want widening", again.Class)
	}
	// An integration reached only through proposes is bound too.
	p := digestWF(work, `[{"action":"send_mail","tool":"mcp__mail-work-write__send","args_schema":`+mailArgs+`}]`)
	if p.reach == nil || p.reach.Accounts["mail-work"] != "work" {
		t.Fatalf("a proposed write's account is not in the reach: %+v", p.reach)
	}
}
