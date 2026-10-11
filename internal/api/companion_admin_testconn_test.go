package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

func (f *fakeAgentAdmin) TestConnectionJSON(_ context.Context, _ *persistence.APIKey, _ json.RawMessage) (any, error) {
	f.calls = append(f.calls, agentadmin.VerbTestConnection)
	return agentadmin.ConnectionResult{Code: agentadmin.ConnOK, HTTPStatusClass: "2xx"}, nil
}

// GitHub #101 (2026-10-08): test_connection is an admin verb: listed and
// callable only for an agent admin key while agent_admin.enabled is on, a
// read (not in agentAdminMutating), and its description states what ok
// does and does not mean.
func TestCompanionAdmin_TestConnectionOfferedOnlyToAdminKey(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	fake := &fakeAgentAdmin{}
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return true }
	plainRaw, _ := seedCompanionKey(t, keys, "assistant", nil)
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))

	if listTools(t, srv, plainRaw)[agentadmin.VerbTestConnection] {
		t.Fatal("a plain companion key was offered test_connection")
	}
	if !listTools(t, srv, agentRaw)[agentadmin.VerbTestConnection] {
		t.Fatal("the agent admin key was not offered test_connection")
	}
	if resp := callAdminTool(t, srv, plainRaw, agentadmin.VerbTestConnection); resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("a plain key called test_connection: %+v", resp.Error)
	}
	text, isErr := decodeToolText(t, callAdminTool(t, srv, agentRaw, agentadmin.VerbTestConnection))
	if isErr || !strings.Contains(text, `"code":"ok"`) {
		t.Fatalf("the agent admin key's call: %q (error %v)", text, isErr)
	}
	if agentAdminMutating[agentadmin.VerbTestConnection] {
		t.Fatal("test_connection is a read; it must not be a mutating verb")
	}
	if strings.Join(fake.calls, ",") != agentadmin.VerbTestConnection {
		t.Fatalf("calls = %v", fake.calls)
	}
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbTestConnection {
			continue
		}
		if !strings.Contains(d.Description, agentadmin.TestConnectionRule) {
			t.Fatalf("description does not state the rule: %s", d.Description)
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		if len(props) != 2 || props["project"] == nil || props["name"] == nil {
			t.Fatalf("schema properties: %v", props)
		}
		return
	}
	t.Fatal("no test_connection definition")
}
