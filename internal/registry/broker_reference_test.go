package registry

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"gopkg.in/yaml.v3"
)

// The shipped reference broker (mail-digest workflow + broker-swarm +
// configs/examples/broker-mail.yaml) must be runnable as a broker workflow.
// A reference that fails its own boundary check teaches operators the wrong
// shape — broker design §10.
func TestReferenceMailDigestBrokerIsRunnable(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..", "configs")

	wfs, err := LoadWorkflows(root)
	if err != nil {
		t.Fatalf("load bundled workflows: %v", err)
	}
	wf := wfs["mail-digest"]
	if wf == nil || wf.Broker == nil {
		t.Fatal("configs/workflows/mail-digest.md must load as a broker workflow")
	}

	swarms, err := LoadSwarms(root)
	if err != nil {
		t.Fatalf("load bundled swarms: %v", err)
	}
	swarm := swarms["broker-swarm"]
	if swarm == nil {
		t.Fatal("configs/swarms/broker-swarm.md missing")
	}

	raw, err := os.ReadFile(filepath.Join(root, "examples", "broker-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p Project
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatalf("parse example: %v", err)
	}
	if err := CheckBrokerRunnable(&p, wf, swarm); err != nil {
		t.Fatalf("reference broker is not runnable: %v", err)
	}
}

// The shipped reference WRITE workflow (mail-reply) must be runnable, its
// proposals must pass the delegate-time check with broker.writes on, and it
// must be refused with writes off — broker write-actions design §12 (3a).
func TestReferenceMailReplyBrokerProposesAWrite(t *testing.T) {
	_, here, _, _ := runtime.Caller(0)
	root := filepath.Join(filepath.Dir(here), "..", "..", "configs")
	wfs, err := LoadWorkflows(root)
	if err != nil {
		t.Fatalf("load bundled workflows: %v", err)
	}
	wf := wfs["mail-reply"]
	if wf == nil || wf.Broker == nil || len(wf.Broker.Proposes) != 1 {
		t.Fatal("configs/workflows/mail-reply.md must load as a broker workflow proposing one write")
	}
	if err := wf.Broker.Validate(); err != nil {
		t.Fatalf("mail-reply broker block: %v", err)
	}
	swarms, err := LoadSwarms(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "examples", "broker-mail.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var p Project
	if err := yaml.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	if err := CheckBrokerRunnable(&p, wf, swarms["broker-swarm"]); err != nil {
		t.Fatalf("mail-reply is not runnable: %v", err)
	}
	if err := CheckBrokerProposals(&p, wf, true); err != nil {
		t.Fatalf("mail-reply proposals refused with writes on: %v", err)
	}
	if err := CheckBrokerProposals(&p, wf, false); !errors.Is(err, ErrBrokerWritesDisabled) {
		t.Fatalf("writes off: want ErrBrokerWritesDisabled, got %v", err)
	}
	// The recipient is read out of third-party mail, and it is the field an
	// injection most wants to change: /inbox must flag it
	// (review-20260930-8b18 F2), with the subject and the body.
	flagged := map[string]bool{}
	for _, path := range wf.Broker.Proposes[0].UntrustedArgPaths() {
		flagged[path] = true
	}
	// Pinned so a rename that keeps the server binding valid is still seen
	// (review-20260930-21ca): the docs and the grant example name these.
	if got := wf.Broker.Proposes[0]; got.Action != "send_reply" || got.Tool != "mcp__gmail-send__gmail_send" {
		t.Errorf("mail-reply proposes %q via %q; the docs name send_reply via mcp__gmail-send__gmail_send", got.Action, got.Tool)
	}
	for _, f := range []string{"to", "subject", "body"} {
		if !flagged[f] {
			t.Errorf("mail-reply args %q must be x-untrusted so /inbox flags it", f)
		}
	}
}
