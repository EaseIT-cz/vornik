package api

import (
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
		Workflows []struct {
			ID          string         `json:"id"`
			Project     string         `json:"project"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"workflows"`
	}
	require.NoError(t, json.Unmarshal([]byte(text), &out))
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
