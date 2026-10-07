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

// twoProjectTree is treeWithServer plus a second project "ops" whose server
// also uses FIO, in the same namespace.
func twoProjectTree(t *testing.T) *tree {
	t.Helper()
	tr := treeWithServer(t)
	tr.apply(tr.render(VerbCreateProject, CreateProjectInput{Slug: "ops", Purpose: "operations"}))
	tr.advert["https://ops.example/mcp"] = []string{"list"}
	tr.apply(tr.render(VerbAddMCPServer, AddMCPServerInput{Project: "ops", Name: "opsrv", URL: "https://ops.example/mcp",
		Auth: MCPAuthInput{Mode: "static", Credential: "FIO"}}))
	return tr
}

func renderLocked(t *testing.T, tr *tree, holder string, in RequestCredentialInput) Change {
	t.Helper()
	st := tr.state()
	st.Locked["credential:hermes/FIO"] = true
	if holder != "" {
		st.LockedBy = map[string]string{"credential:hermes/FIO": holder}
	}
	raw, _ := json.Marshal(in)
	c, err := tr.r.Render(st, VerbRequestCredential, raw)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// GitHub #80 (2026-10-03): the credential is namespace-scoped, so a second
// project's request while the first is pending said "part of a change waiting
// for approval". It is an applied no-op that names the credential, the request
// and the project.
func TestRequestCredential_SecondProjectWhilePending(t *testing.T) {
	tr := twoProjectTree(t)
	c := renderLocked(t, tr, "req-1 (credential_slot)", RequestCredentialInput{Project: "ops", Name: "FIO", Purpose: "x", Kind: "secret"})
	if c.Class != Inert || len(c.Ops) != 0 || c.Slot != nil {
		t.Fatalf("want an inert no-op, got class %v ops %d slot %v (%s)", c.Class, len(c.Ops), c.Slot, c.Reason)
	}
	for _, want := range []string{"FIO", "hermes--ops", "req-1", "already requested", "Nothing more to request"} {
		if !strings.Contains(c.Sentence, want) {
			t.Errorf("sentence lacks %q: %s", want, c.Sentence)
		}
	}
	if strings.Contains(c.Sentence, "waiting for approval") || strings.Contains(c.Sentence, "credential_slot") {
		t.Errorf("sentence leaks the old wording or the kind: %s", c.Sentence)
	}
}

// Validation runs before the lock: a name none of B's servers use is still
// refused for that reason, not answered with the no-op sentence.
func TestRequestCredential_LockedButUnboundNameStillRefused(t *testing.T) {
	tr := twoProjectTree(t)
	st := tr.state()
	st.Locked["credential:hermes/OTHER"] = true
	st.LockedBy = map[string]string{"credential:hermes/OTHER": "req-1 (credential_slot)"}
	raw, _ := json.Marshal(RequestCredentialInput{Project: "ops", Name: "OTHER", Purpose: "x", Kind: "secret"})
	c, err := tr.r.Render(st, VerbRequestCredential, raw)
	if err != nil || c.Class != Refused || !strings.Contains(c.Reason, "no server or API of hermes--ops uses") {
		t.Fatalf("got %v %q %v", c.Class, c.Reason, err)
	}
}

// Already entered: no lock is held, so B's request is the rotation path (a
// widening slot), neither the no-op sentence nor a refusal.
func TestRequestCredential_AlreadyEnteredIsRotation(t *testing.T) {
	tr := twoProjectTree(t)
	c := tr.render(VerbRequestCredential, RequestCredentialInput{Project: "ops", Name: "FIO", Purpose: "x", Kind: "secret"})
	if c.Class != Widening || c.Slot == nil || c.Slot.Project != "hermes--ops" {
		t.Fatalf("got %v %q", c.Class, c.Reason)
	}
}

// No known holder (not reachable from the service, which always sets
// LockedBy with a lock; defence in depth) keeps the generic refusal.
func TestRequestCredential_LockWithoutHolderStillRefused(t *testing.T) {
	tr := twoProjectTree(t)
	c := renderLocked(t, tr, "", RequestCredentialInput{Project: "ops", Name: "FIO", Purpose: "x", Kind: "secret"})
	if c.Class != Refused || !strings.Contains(c.Reason, "waiting for approval") {
		t.Fatalf("got %v %q", c.Class, c.Reason)
	}
}
