package service

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
)

// GitHub #119 item 8 (2026-10-08): test_connection names the connection's
// account label and where its identity came from, never a provider-reported
// identity (agent-administered design §22.2.7, §22.2.9): that is third-party
// data about the user, and no egress schema approved it for the agent. A
// default connection still answers exactly the three keys.
func TestTestConnection_ReturnsAccountNeverProviderIdentity(t *testing.T) {
	f := newFinance(t)
	api := newRecordingAPI(t, http.StatusOK)
	f.approve(f.do(agentadmin.VerbAddAPI, agentadmin.AddAPIInput{Project: "finance", Name: "bank", BaseURL: api.srv.URL,
		Auth: agentadmin.APIAuthInput{Credential: "BANK", Prefix: "Bearer "}, Methods: []string{"GET"},
		Account: &agentadmin.AccountInput{Label: "work", Identity: "alice@corp.example"}}))
	f.storeCredential("BANK", "v")

	raw, err := f.testConn("finance", "bank")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"account":"work"`) || !strings.Contains(raw, `"identity_source":"agent"`) || strings.Contains(raw, "alice@corp.example") {
		t.Fatalf("an agent-described account: %s", raw)
	}

	if err := f.c.repos.AgentGrants.SetIntegrationIdentity(context.Background(), "hermes--finance", "bank", "provider-alice@corp.example"); err != nil {
		t.Fatal(err)
	}
	raw, err = f.testConn("finance", "bank")
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"provider-alice@corp.example", "SECRET-BODY", "SECRET-HDR"} {
		if strings.Contains(raw, leak) {
			t.Fatalf("the answer carries %q: %s", leak, raw)
		}
	}
	if !strings.Contains(raw, `"account":"work"`) || !strings.Contains(raw, `"identity_source":"provider"`) {
		t.Fatalf("a provider-identified account: %s", raw)
	}
}
