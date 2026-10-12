package api

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

func (f *fakeAgentAdmin) ShowWorkflowJSON(_ context.Context, _ *persistence.APIKey, _ json.RawMessage) (any, error) {
	f.calls = append(f.calls, agentadmin.VerbShowWorkflow)
	return map[string]any{"id": "hermes--finance--digest"}, nil
}

func TestShowWorkflow_AdminOfferingAndDispatch(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	fake := &fakeAgentAdmin{}
	enabled := true
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return enabled }
	plain, _ := seedCompanionKey(t, keys, "assistant", nil)
	agent, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{ID: "akey-show-workflow", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agent), KeyPrefix: apikey.DisplayPrefix(agent), CreatedAt: time.Now(), ClientKind: "hermes", AgentAdmin: true, AgentNamespace: "hermes"}))
	require.False(t, listTools(t, srv, plain)[agentadmin.VerbShowWorkflow])
	require.True(t, listTools(t, srv, agent)[agentadmin.VerbShowWorkflow])
	if r := callAdminTool(t, srv, plain, agentadmin.VerbShowWorkflow); r.Error == nil || r.Error.Code != -32601 {
		t.Fatalf("plain key called read: %+v", r.Error)
	}
	text, isErr := decodeToolText(t, callAdminTool(t, srv, agent, agentadmin.VerbShowWorkflow))
	require.False(t, isErr, text)
	require.True(t, strings.Contains(text, "hermes--finance--digest"))
	enabled = false
	require.False(t, listTools(t, srv, agent)[agentadmin.VerbShowWorkflow])
	if r := callAdminTool(t, srv, agent, agentadmin.VerbShowWorkflow); r.Error == nil || r.Error.Code != -32601 {
		t.Fatal("read offered with admin disabled")
	}
	require.Equal(t, []string{agentadmin.VerbShowWorkflow}, fake.calls)
}

// GitHub #283: workflow read-back uses exactly the admin tool boundary.
func TestShowWorkflow_AdminSchema(t *testing.T) {
	if !isAgentAdminTool(agentadmin.VerbShowWorkflow) || brokerProjectTools[agentadmin.VerbShowWorkflow] {
		t.Fatal("wrong tool boundary")
	}
	for _, d := range companionAdminToolDefs() {
		if d.Name == agentadmin.VerbShowWorkflow {
			if d.InputSchema["additionalProperties"] != false {
				t.Fatal("unknown fields allowed")
			}
			return
		}
	}
	t.Fatal("show_workflow absent")
}
