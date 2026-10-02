package agentadmin

import (
	"encoding/json"
	"strings"
	"testing"
)

// treeWithServer is a tree whose project "fin" has a server bound to FIO.
func treeWithServer(t *testing.T) *tree {
	t.Helper()
	tr := newTree(t, "hermes")
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "fin", Purpose: "finance"}))
	tr.advert["https://bank.example/mcp"] = []string{"balance"}
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "fin", Name: "bank", URL: "https://bank.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	return tr
}

// Design §8.2, plan P4.1: request_credential is always widening, writes no
// file, and names the credential, its user and the purpose; no value field
// exists. Control: requestCredential.
func TestRequestCredential(t *testing.T) {
	tr := treeWithServer(t)
	c := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "fin", Name: "FIO", Purpose: "read the account balance", Kind: "secret"})
	if c.Class != Widening {
		t.Fatalf("class %v (%s), want widening", c.Class, c.Reason)
	}
	if len(c.Ops) != 0 {
		t.Fatalf("a credential request wrote files: %+v", c.Ops)
	}
	if c.Slot == nil || c.Slot.Name != "FIO" || c.Slot.Project != "hermes--fin" || c.Slot.Kind != "secret" || c.Slot.Namespace != "hermes" {
		t.Fatalf("slot = %+v", c.Slot)
	}
	for _, want := range []string{"FIO", "hermes--fin", "bank", "bank.example", "read the account balance", "never see it"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	if !contains(c.Locks, "credential:hermes/FIO") {
		t.Errorf("locks %v lack the credential", c.Locks)
	}
	var rendered map[string]any
	if err := json.Unmarshal(c.Rendered, &rendered); err != nil || rendered["slot"] == nil {
		t.Fatalf("rendered change lacks the slot: %s", c.Rendered)
	}

	for name, in := range map[string]RequestCredentialInput{
		"unknown project":  {Project: "nope", Name: "FIO", Purpose: "x", Kind: "secret"},
		"bad name":         {Project: "fin", Name: "fio", Purpose: "x", Kind: "secret"},
		"unbound name":     {Project: "fin", Name: "OTHER", Purpose: "x", Kind: "secret"},
		"no purpose":       {Project: "fin", Name: "FIO", Kind: "secret"},
		"two-line purpose": {Project: "fin", Name: "FIO", Purpose: "a\nb", Kind: "secret"},
		"unknown kind":     {Project: "fin", Name: "FIO", Purpose: "x", Kind: "password"},
	} {
		if c := tr.render(VerbRequestCredential, in); c.Class != Refused {
			t.Errorf("%s: accepted (%v)", name, c.Class)
		}
	}
	// A value field does not exist: strict decoding refuses it.
	raw := json.RawMessage(`{"project":"fin","name":"FIO","purpose":"x","kind":"secret","value":"hunter2"}`)
	c, err := tr.r.Render(tr.state(), VerbRequestCredential, raw)
	if err != nil || c.Class != Refused {
		t.Fatalf("a request carrying a value was not refused: %v %v", c.Class, err)
	}
	if strings.Contains(c.Reason, "hunter2") {
		t.Fatalf("the refusal echoed the value: %s", c.Reason)
	}
}
