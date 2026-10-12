package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
)

// Lane B implementation review 25bf (2026-10-09, GitHub #119 item 8).

// Finding 3: a provider-reported identity is in the device's sentences (the
// person must see which mailbox), but the same sentences went back to the
// agent key, in a verb's result and in list_my_setup's request lists. A
// provider identity is third-party data about the user and reaches the
// agent through no approved egress (design §22.2.7).
func TestAgentKey_NeverSeesAProviderIdentity(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	ctx := context.Background()
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(ctx, "hermes--comms", "mail-work", "alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: "MAIL_TOKEN", Purpose: "read mail", Kind: "secret"})
	if res.Effect != agentadmin.EffectAwaiting || strings.Contains(res.Sentence, "alice@corp.example") {
		t.Fatalf("the verb's result carries the provider identity: %+v", res)
	}
	if !strings.Contains(res.Sentence, "your work account (verified by the provider)") {
		t.Fatalf("the result does not say the account is verified: %s", res.Sentence)
	}
	req, err := f.c.repos.ApproverDevices.GetRequest(ctx, requestIDOf(res))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(req.Sentence, "alice@corp.example") {
		t.Fatalf("the device's sentence lost the identity: %s", req.Sentence)
	}
	raw, _ := json.Marshal(setupOf(t, f))
	if strings.Contains(string(raw), "alice@corp.example") {
		t.Fatalf("list_my_setup carries the provider identity: %s", raw)
	}
}

// audit 2026-10-12: finding C, a request sentence is stored once and kept;
// after the stored identity changed (re-sign-in as another account, or a
// Disconnect that cleared it), the old sentence still named the previous
// provider identity and list_my_setup passed it to the agent key.
func TestAgentKey_NeverSeesAPreviousProviderIdentity(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work"})
	ctx := context.Background()
	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(ctx, "hermes--comms", "mail-work", "alice+news@corp.example"); err != nil {
		t.Fatal(err)
	}
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: "MAIL_TOKEN", Purpose: "read mail", Kind: "secret"})
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("fixture did not file a request: %+v", res)
	}
	for _, next := range []string{"bob@corp.example", ""} {
		if err := f.c.repos.AgentGrants.SetIntegrationIdentity(ctx, "hermes--comms", "mail-work", next); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(setupOf(t, f))
		for _, leaked := range []string{"alice+news@corp.example", "bob@corp.example"} {
			if strings.Contains(string(raw), leaked) {
				t.Fatalf("identity now %q: list_my_setup carries %s: %s", next, leaked, raw)
			}
		}
	}
}

// audit 2026-10-12: finding C, an identity the agent itself described stays
// visible to it: it is the agent's own input, not third-party data.
func TestAgentKey_SeesTheIdentityItDescribed(t *testing.T) {
	f := writeFixture(t, "mail-work", &agentadmin.AccountInput{Label: "work", Identity: "me@agent.example"})
	res := f.do(agentadmin.VerbRequestCredential, agentadmin.RequestCredentialInput{Project: "comms", Name: "MAIL_TOKEN", Purpose: "read mail", Kind: "secret"})
	if !strings.Contains(res.Sentence, "me@agent.example") {
		t.Fatalf("the agent's own identity was masked in its result: %s", res.Sentence)
	}
	raw, _ := json.Marshal(setupOf(t, f))
	if !strings.Contains(string(raw), "me@agent.example") {
		t.Fatalf("the agent's own identity was masked in list_my_setup: %s", raw)
	}
}

// Finding 2: two device sign-ins for one server ran their exchange and their
// identity writes unserialised, so the stored token and the recorded
// identity could come from different sign-ins. Completions now hold one
// lock from the prior-sign-in read to the identity write; a completion that
// cannot take it waits.
func TestDeviceOAuth_CompletionsAreSerialised(t *testing.T) {
	f, vendor := oauthWriteFixture(t)
	vendor.idToken = idTokenFor(map[string]any{"email": "alice@corp.example", "email_verified": true})
	f.svc.signInMu.Lock()
	done := make(chan struct{})
	go func() {
		signInFromPhone(t, f, "c1")
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("a sign-in completed while another held the sign-in lock")
	case <-time.After(300 * time.Millisecond):
	}
	f.svc.signInMu.Unlock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the sign-in did not complete after the lock was released")
	}
	if got := identityOf(t, f); got != [2]string{"alice@corp.example", "provider"} {
		t.Fatalf("identity %q", got)
	}
}
