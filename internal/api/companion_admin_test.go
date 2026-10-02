package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// fakeAgentAdmin records verb calls.
type fakeAgentAdmin struct {
	calls []string
	// unavailable makes EnsureHome report a service not built yet.
	unavailable bool
}

func (f *fakeAgentAdmin) Do(_ context.Context, _ *persistence.APIKey, verb string, _ json.RawMessage) (agentadmin.Result, error) {
	f.calls = append(f.calls, verb)
	return agentadmin.Result{Effect: agentadmin.EffectApplied, ChangeID: "cpp_x"}, nil
}
func (f *fakeAgentAdmin) ListSetupJSON(context.Context, *persistence.APIKey) (any, error) {
	f.calls = append(f.calls, toolListMySetup)
	return map[string]string{"namespace": "hermes"}, nil
}
func (f *fakeAgentAdmin) EnsureHome(_ context.Context, ns, _ string) (string, error) {
	f.calls = append(f.calls, "ensure_home:"+ns)
	if f.unavailable {
		return "", agentadmin.ErrUnavailable
	}
	return ns + "--home", nil
}
func (f *fakeAgentAdmin) DescribeJSON(context.Context, *persistence.APIKey) (any, error) {
	f.calls = append(f.calls, toolDescribeInstallation)
	return map[string]string{"namespace": "hermes"}, nil
}

func listTools(t *testing.T, srv *Server, raw string) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, withCompanionBearer(mcpRequest(t, "tools/list", nil), raw))
	resp := decodeJSONRPC(t, rec.Body.Bytes())
	require.Nil(t, resp.Error)
	b, _ := json.Marshal(resp.Result)
	var out struct {
		Tools []mcpToolDef `json:"tools"`
	}
	require.NoError(t, json.Unmarshal(b, &out))
	names := map[string]bool{}
	for _, d := range out.Tools {
		names[d.Name] = true
	}
	return names
}

func callAdminTool(t *testing.T, srv *Server, raw, tool string) jsonRPCResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.CompanionMCPHandler(rec, withCompanionBearer(mcpRequest(t, "tools/call", map[string]any{"name": tool, "arguments": map[string]any{}}), raw))
	return decodeJSONRPC(t, rec.Body.Bytes())
}

// Design §6: the admin verbs exist only for an agent admin key while
// agent_admin.enabled is on. Control: agentAdminOffered. Without it, any
// companion key could reconfigure Vornik.
func TestCompanionAdmin_OfferedOnlyToAgentAdminKeysWhenEnabled(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	fake := &fakeAgentAdmin{}
	enabled := true
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return enabled }

	plainRaw, _ := seedCompanionKey(t, keys, "assistant", nil)
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))

	if names := listTools(t, srv, plainRaw); names[agentadmin.VerbCreateProject] || names[toolListMySetup] {
		t.Fatal("a plain companion key was offered the admin verbs")
	}
	names := listTools(t, srv, agentRaw)
	for _, v := range []string{agentadmin.VerbCreateProject, agentadmin.VerbDefineWorkflow, toolListMySetup, toolDescribeInstallation} {
		if !names[v] {
			t.Errorf("the agent admin key was not offered %s", v)
		}
	}
	if !names["delegate"] {
		t.Error("the ordinary tools disappeared for the agent admin key")
	}

	resp := callAdminTool(t, srv, plainRaw, agentadmin.VerbCreateProject)
	if resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("a plain key calling an admin verb: %+v, want -32601 unknown tool", resp.Error)
	}
	resp = callAdminTool(t, srv, agentRaw, agentadmin.VerbCreateProject)
	text, isErr := decodeToolText(t, resp)
	if isErr || !strings.Contains(text, `"applied"`) {
		t.Fatalf("the agent admin key's verb: %q (error %v)", text, isErr)
	}

	enabled = false
	if names := listTools(t, srv, agentRaw); names[agentadmin.VerbCreateProject] {
		t.Fatal("verbs offered while agent_admin.enabled is off")
	}
	if resp := callAdminTool(t, srv, agentRaw, agentadmin.VerbCreateProject); resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("a verb ran while disabled: %+v", resp.Error)
	}
	if len(fake.calls) != 1 {
		t.Fatalf("the service was called %d times, want 1: %v", len(fake.calls), fake.calls)
	}
}

// The admin verbs never enter the broker allowlist: a broker-project key
// that is not an agent admin key cannot reach them by that route either.
func TestCompanionAdmin_BrokerAllowlistStaysClosed(t *testing.T) {
	for _, v := range []string{agentadmin.VerbCreateProject, agentadmin.VerbRemove, toolListMySetup} {
		if brokerProjectTools[v] {
			t.Errorf("%s is in the broker-project allowlist", v)
		}
	}
}

// Design §5: an agent admin key covers every project of its namespace and
// nothing else; an ordinary key covers its own project only. Control:
// keyCoversProject, which status, result and cancel ask.
func TestKeyCoversProject(t *testing.T) {
	plain := &persistence.APIKey{ProjectID: "assistant"}
	agent := &persistence.APIKey{ProjectID: "hermes--home", AgentAdmin: true, AgentNamespace: "hermes"}
	for _, c := range []struct {
		key     *persistence.APIKey
		project string
		want    bool
	}{
		{plain, "assistant", true}, {plain, "hermes--fin", false},
		{agent, "hermes--home", true}, {agent, "hermes--fin", true},
		{agent, "codex--fin", false}, {agent, "hermesx--fin", false}, {agent, "assistant", false},
		{nil, "assistant", false},
	} {
		if got := keyCoversProject(c.key, c.project); got != c.want {
			t.Errorf("keyCoversProject(%+v, %q) = %v, want %v", c.key, c.project, got, c.want)
		}
	}
}

// Plan P6.3 (review 20261002-6f6b F3): an agent admin key's initialize
// carries the admin guidance as instructions; a plain key's does not, and
// neither does an unauthenticated initialize. Control: the instructions
// branch of initialize.
func TestCompanionAdmin_InitializeInstructions(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	enabled := true
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{}, func() bool { return enabled }
	plainRaw, _ := seedCompanionKey(t, keys, "assistant", nil)
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))
	instructions := func(req *http.Request) string {
		rec := httptest.NewRecorder()
		srv.CompanionMCPHandler(rec, req)
		resp := decodeJSONRPC(t, rec.Body.Bytes())
		require.Nil(t, resp.Error)
		m, _ := resp.Result.(map[string]any)
		s, _ := m["instructions"].(string)
		return s
	}
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), agentRaw)); got != agentadmin.AdminGuidance() {
		t.Fatalf("the agent admin key's instructions: %q", got)
	}
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), plainRaw)); got != "" {
		t.Fatalf("a plain key got instructions: %q", got)
	}
	if got := instructions(mcpRequest(t, "initialize", nil)); got != "" {
		t.Fatalf("an unauthenticated initialize got instructions: %q", got)
	}
	// Review 20261002-3b9f F5: agent_admin.enabled: false stops it too.
	enabled = false
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), agentRaw)); got != "" {
		t.Fatalf("instructions while agent_admin is off: %q", got)
	}
}

// Design §17.1: define_workflow offers schedule {cron, timezone, inputs},
// so an agent can ask for "every month" without guessing a field name.
// Control: the define_workflow tool definition.
func TestCompanionAdmin_DefineWorkflowOffersSchedule(t *testing.T) {
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbDefineWorkflow {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		sched, _ := props["schedule"].(map[string]any)
		inner, _ := sched["properties"].(map[string]any)
		for _, f := range []string{"cron", "timezone", "inputs"} {
			if inner[f] == nil {
				t.Errorf("schedule has no %s: %v", f, sched)
			}
		}
		if !strings.Contains(sched["description"].(string), "hourly") {
			t.Errorf("schedule description: %v", sched["description"])
		}
		return
	}
	t.Fatal("no define_workflow tool")
}

// Regression (DoD lane bring-up, 2026-10-02): an agent admin key was offered
// recall, remember and the skill tools, which its broker home project
// refuses on every call; design §5 says the key has no memory or skills,
// and an offered tool that always fails misleads the agent. tools/list now
// offers exactly what gateCompanionTool admits. Control: the gate filter in
// companionToolsFor.
func TestCompanionToolsFor_OffersOnlyWhatTheKeyMayCall(t *testing.T) {
	srv, keys, _ := newBrokerMCPServer(t)
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{}, func() bool { return true }
	brokerRaw, _ := seedCompanionKey(t, keys, "broker-acme", nil)
	memRaw, _ := seedCompanionKey(t, keys, "memory-acme", nil)
	keys.rows[len(keys.rows)-1].DelegateDisabled = true

	broker := listTools(t, srv, brokerRaw)
	for _, banned := range []string{"recall", "remember", "skill_search", "memory_correct"} {
		if broker[banned] {
			t.Errorf("a broker-project key was offered %s", banned)
		}
	}
	for _, want := range []string{"delegate", "result", "status", "catalog", "whoami"} {
		if !broker[want] {
			t.Errorf("a broker-project key was not offered %s", want)
		}
	}
	mem := listTools(t, srv, memRaw)
	if mem["delegate"] || !mem["whoami"] {
		t.Errorf("a memory-only key: delegate %v, whoami %v", mem["delegate"], mem["whoami"])
	}
}
