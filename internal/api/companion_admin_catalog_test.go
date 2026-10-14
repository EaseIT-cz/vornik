package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Design §18.3 (2026-10-02): for an agent admin key, catalog listed only
// the home project although delegate ran any approved workflow of the
// namespace (claudecode--docreview--review ran and was never listed). Now it
// lists every approved workflow of the key's namespace, each with its
// project and input schema, and nothing of another namespace even if the
// approval source named one. Control: companionToolCatalog's agent branch.
func TestCompanionCatalog_AgentKeyListsItsNamespacesApprovedWorkflows(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	reg := registry.New()
	broker := func(prop string) *registry.WorkflowBroker {
		return &registry.WorkflowBroker{
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{prop: map[string]any{"type": "string", "maxLength": 512, "x-untrusted": true}}},
			Egress:      registry.BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}},
		}
	}
	registry.SeedForTest(reg, map[string]*registry.Project{
		"hermes--home":    {ID: "hermes--home", Broker: true},
		"hermes--finance": {ID: "hermes--finance", Broker: true},
		"hermes--travel":  {ID: "hermes--travel", Broker: true},
		"her--x":          {ID: "her--x", Broker: true},
	})
	registry.SeedWorkflowsForTest(reg, map[string]*registry.Workflow{
		"hermes--finance--spend": {ID: "hermes--finance--spend", Description: "Sum a month.", Broker: broker("month")},
		"hermes--travel--plan":   {ID: "hermes--travel--plan", Description: "Plan a trip.", Broker: broker("city")},
		"her--x--leak":           {ID: "her--x--leak", Broker: broker("y")},
	})
	srv.projectRegistry = reg
	fake := &fakeAgentAdmin{approved: []string{"hermes--finance--spend", "hermes--travel--plan", "her--x--leak"}}
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return true }

	raw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(t.Context(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))

	resp := callAdminTool(t, srv, raw, "catalog")
	require.Nil(t, resp.Error)
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, text)
	var out struct {
		DelegateInputSchema map[string]any `json:"delegate_input_schema"`
		Workflows           []struct {
			ID          string         `json:"id"`
			Project     string         `json:"project"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	props, _ := out.DelegateInputSchema["properties"].(map[string]any)
	require.Contains(t, props, "inputs")
	require.NotContains(t, props, "prompt")
	reqs, _ := out.DelegateInputSchema["required"].([]any)
	require.Equal(t, []any{"workflow", "inputs"}, reqs)
	got := map[string]string{}
	for _, w := range out.Workflows {
		require.NotNil(t, w.InputSchema, "%s has no input_schema", w.ID)
		got[w.ID] = w.Project
	}
	require.Equal(t, map[string]string{
		"hermes--finance--spend": "hermes--finance",
		"hermes--travel--plan":   "hermes--travel",
	}, got)
}

// connectorInstallsOnly is a grants repository that answers only the
// connector-install read.
type connectorInstallsOnly struct {
	persistence.AgentGrantRepository
	rows []persistence.AgentConnectorInstall
}

func (c connectorInstallsOnly) ListConnectorInstalls(_ context.Context, ns string) ([]persistence.AgentConnectorInstall, error) {
	var out []persistence.AgentConnectorInstall
	for _, r := range c.rows {
		if r.Namespace == ns {
			out = append(out, r)
		}
	}
	return out, nil
}

// GitHub #314 (2026-10-13; agent-administered design §19.14.3, §19.14.9
// remaining item 3): catalog marks a workflow the person installed from
// their device as installed_by device, as list_my_setup does; others carry
// no marker, and another namespace's row marks nothing here.
func TestCompanionCatalog_MarksDeviceInstalledConnectors(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	reg := registry.New()
	broker := &registry.WorkflowBroker{
		InputSchema: map[string]any{"type": "object"},
		Egress:      registry.BrokerEgress{Output: "result.json", Schema: map[string]any{"type": "object"}},
	}
	registry.SeedForTest(reg, map[string]*registry.Project{
		"hermes--home":     {ID: "hermes--home", Broker: true},
		"hermes--personal": {ID: "hermes--personal", Broker: true},
	})
	registry.SeedWorkflowsForTest(reg, map[string]*registry.Workflow{
		"hermes--personal--agenda": {ID: "hermes--personal--agenda", Broker: broker},
		"hermes--personal--spend":  {ID: "hermes--personal--spend", Broker: broker},
	})
	srv.projectRegistry = reg
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{approved: []string{"hermes--personal--agenda", "hermes--personal--spend"}}, func() bool { return true }
	srv.agentGrants = connectorInstallsOnly{rows: []persistence.AgentConnectorInstall{
		{WorkflowID: "hermes--personal--agenda", Namespace: "hermes", ProjectID: "hermes--personal", Connector: "agenda", DeviceID: "dev_a"},
		{WorkflowID: "hermes--personal--spend", Namespace: "codex", ProjectID: "codex--personal", Connector: "x", DeviceID: "dev_a"},
	}}
	raw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(t.Context(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(raw),
		KeyPrefix: apikey.DisplayPrefix(raw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))
	resp := callAdminTool(t, srv, raw, "catalog")
	require.Nil(t, resp.Error)
	text, isErr := decodeToolText(t, resp)
	require.False(t, isErr, text)
	var out struct {
		Workflows []struct {
			ID          string `json:"id"`
			InstalledBy string `json:"installed_by"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
	got := map[string]string{}
	for _, w := range out.Workflows {
		got[w.ID] = w.InstalledBy
	}
	require.Equal(t, map[string]string{"hermes--personal--agenda": "device", "hermes--personal--spend": ""}, got)
}
