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
	"vornik.io/vornik/internal/registry"
)

// fakeAgentAdmin records verb calls.
type fakeAgentAdmin struct {
	calls []string
	// unavailable makes EnsureHome report a service not built yet.
	unavailable bool
	// approved is what ApprovedWorkflows answers.
	approved []string
}

func (f *fakeAgentAdmin) ApprovedWorkflows(context.Context, *persistence.APIKey) ([]string, error) {
	return f.approved, nil
}

func (f *fakeAgentAdmin) Do(_ context.Context, _ *persistence.APIKey, verb string, _ json.RawMessage) (agentadmin.Result, error) {
	f.calls = append(f.calls, verb)
	return agentadmin.Result{Effect: agentadmin.EffectApplied, ChangeID: "cpp_x"}, nil
}
func (f *fakeAgentAdmin) ListSetupJSON(context.Context, *persistence.APIKey) (any, error) {
	f.calls = append(f.calls, toolListMySetup)
	return map[string]string{"namespace": "hermes"}, nil
}
func (f *fakeAgentAdmin) ListRecipesJSON(context.Context, *persistence.APIKey) (any, error) {
	f.calls = append(f.calls, agentadmin.VerbListRecipes)
	return map[string]any{"recipes": []map[string]string{{"name": "inbox-digest"}}}, nil
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

func delegatePropsFor(t *testing.T, srv *Server, raw string) map[string]any {
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
	for _, d := range out.Tools {
		if d.Name == "delegate" {
			props, _ := d.InputSchema["properties"].(map[string]any)
			return props
		}
	}
	t.Fatal("no delegate tool")
	return nil
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
	for _, v := range []string{agentadmin.VerbCreateProject, agentadmin.VerbUpdateProject, agentadmin.VerbDefineWorkflow, toolListMySetup, toolDescribeInstallation} {
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
	// Issue #67: the metadata verb follows the same namespace admin gate.
	if resp := callAdminTool(t, srv, plainRaw, agentadmin.VerbUpdateProject); resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("plain key edited project metadata: %+v", resp.Error)
	}
	if text, isErr := decodeToolText(t, callAdminTool(t, srv, agentRaw, agentadmin.VerbUpdateProject)); isErr || !strings.Contains(text, `"applied"`) {
		t.Fatalf("agent metadata verb: %q (error %v)", text, isErr)
	}

	enabled = false
	if names := listTools(t, srv, agentRaw); names[agentadmin.VerbCreateProject] {
		t.Fatal("verbs offered while agent_admin.enabled is off")
	}
	if resp := callAdminTool(t, srv, agentRaw, agentadmin.VerbCreateProject); resp.Error == nil || resp.Error.Code != -32601 {
		t.Fatalf("a verb ran while disabled: %+v", resp.Error)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("the service was called %d times, want 2: %v", len(fake.calls), fake.calls)
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
	// Companion guidance design §10 (2026-10-03): the admin guidance, the
	// scoping sentence, then the companion text, joined by blank lines; a
	// plain companion key gets the companion text.
	want := agentadmin.AdminGuidance() + "\n\n" + adminDelegationScope + "\n\n" + CompanionGuidance()
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), agentRaw)); got != want {
		t.Fatalf("the agent admin key's instructions: %q", got)
	}
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), plainRaw)); got != CompanionGuidance() {
		t.Fatalf("a plain key's instructions: %q", got)
	}
	if got := instructions(mcpRequest(t, "initialize", nil)); got != "" {
		t.Fatalf("an unauthenticated initialize got instructions: %q", got)
	}
	// Review 20261002-3b9f F5: agent_admin.enabled: false stops the admin
	// part; the key is then an ordinary companion key.
	enabled = false
	if got := instructions(withCompanionBearer(mcpRequest(t, "initialize", nil), agentRaw)); strings.Contains(got, "describe_installation") {
		t.Fatalf("admin guidance while agent_admin is off: %q", got)
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

// The live MCP schema is the contract Hermes uses to construct add_api calls.
// Keep the query-param auth placement advertised alongside the implementation.
func TestCompanionAdmin_AddAPIOffersQueryParamAuth(t *testing.T) {
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbAddAPI {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		auth, _ := props["auth"].(map[string]any)
		authProps, _ := auth["properties"].(map[string]any)
		queryParam, ok := authProps["query_param"].(map[string]any)
		if !ok || !strings.Contains(queryParam["description"].(string), "legacy APIs") {
			t.Fatalf("add_api auth schema lacks query_param guidance: %v", auth)
		}
		return
	}
	t.Fatal("no add_api tool")
}

// Design §18.6 item 2: define_swarm offers an optional model per role,
// pointing at describe_installation's catalogue and saying what a remote one
// asks of the user. Control: the define_swarm tool definition.
func TestCompanionAdmin_DefineSwarmOffersARoleModel(t *testing.T) {
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbDefineSwarm {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		roles, _ := props["roles"].(map[string]any)
		items, _ := roles["items"].(map[string]any)
		rp, _ := items["properties"].(map[string]any)
		model, _ := rp["model"].(map[string]any)
		desc, _ := model["description"].(string)
		if !strings.Contains(desc, "describe_installation") || !strings.Contains(desc, "remote") {
			t.Fatalf("define_swarm role model: %v", model)
		}
		for _, r := range items["required"].([]string) {
			if r == "model" {
				t.Fatal("model is required; it must be optional")
			}
		}
		return
	}
	t.Fatal("no define_swarm tool")
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

// Design §18.10 (review 34c4 minor): define_workflow's steps description
// carries the same hand-off rule as the guidance, so an agent reading only
// the tool schema learns it too. Control: the steps description.
func TestCompanionAdmin_DefineWorkflowStepsStateTheHandoff(t *testing.T) {
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbDefineWorkflow {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		steps, _ := props["steps"].(map[string]any)
		desc, _ := steps["description"].(string)
		if !strings.Contains(desc, agentadmin.HandoffRule) {
			t.Fatalf("define_workflow steps description %q lacks %q", desc, agentadmin.HandoffRule)
		}
		return
	}
	t.Fatal("no define_workflow tool")
}

// Design §18.2 (review 3e94 F3): an agent admin key delegates only broker
// workflows, which refuse prompt and inputArtifacts, so the delegate schema
// it is shown omits both instead of inviting a refused call.
// Control: companionToolsFor's agent branch.
func TestCompanionAdmin_DelegateSchemaForAgentKeysOmitsRefusedFields(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	srv.agentAdmin, srv.agentAdminEnabled = &fakeAgentAdmin{}, func() bool { return true }
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))
	plainRaw, _ := seedCompanionKey(t, keys, "assistant", nil)
	delegateProps := func(raw string) map[string]any {
		rec := httptest.NewRecorder()
		srv.CompanionMCPHandler(rec, withCompanionBearer(mcpRequest(t, "tools/list", nil), raw))
		resp := decodeJSONRPC(t, rec.Body.Bytes())
		b, _ := json.Marshal(resp.Result)
		var out struct {
			Tools []mcpToolDef `json:"tools"`
		}
		require.NoError(t, json.Unmarshal(b, &out))
		for _, d := range out.Tools {
			if d.Name == "delegate" {
				props, _ := d.InputSchema["properties"].(map[string]any)
				return props
			}
		}
		t.Fatal("no delegate tool")
		return nil
	}
	brokerSrv, brokerKeys, _ := newBrokerMCPServer(t)
	brokerRaw, _ := seedCompanionKey(t, brokerKeys, "broker-acme", nil)
	broker := delegatePropsFor(t, brokerSrv, brokerRaw)
	for _, f := range []string{"prompt", "inputArtifacts"} {
		if _, ok := broker[f]; ok {
			t.Errorf("a broker-project key's delegate schema offers %s", f)
		}
	}
	if _, ok := broker["inputs"]; !ok {
		t.Error("a broker-project key's delegate schema lost inputs")
	}

	agent := delegateProps(agentRaw)
	for _, f := range []string{"prompt", "inputArtifacts"} {
		if _, ok := agent[f]; ok {
			t.Errorf("an agent admin key's delegate schema offers %s", f)
		}
	}
	if _, ok := agent["inputs"]; !ok {
		t.Error("an agent admin key's delegate schema lost inputs")
	}
	plain := delegateProps(plainRaw)
	if _, ok := plain["prompt"]; !ok {
		t.Error("a plain companion key's delegate schema lost prompt")
	}
	if _, ok := plain["inputs"]; ok {
		t.Error("a plain companion key's delegate schema offers broker-only inputs")
	}
}

func TestCompanionAdmin_DelegateSchemaShapingDoesNotMutateBaseDefinition(t *testing.T) {
	var base mcpToolDef
	for _, d := range companionToolDefs() {
		if d.Name == "delegate" {
			base = d
			break
		}
	}
	require.NotEmpty(t, base.Name)

	_ = brokerOnlyDelegate(base)
	_ = promptOnlyDelegate(base)

	props, _ := base.InputSchema["properties"].(map[string]any)
	require.Contains(t, props, "prompt")
	require.Contains(t, props, "inputs")
	require.Contains(t, props, "inputArtifacts")
}

// Design §18.2: define_workflow's inputs description states the input rules
// the validator enforces, from the same source.
func TestCompanionAdmin_DefineWorkflowInputsStateTheRules(t *testing.T) {
	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbDefineWorkflow {
			continue
		}
		props, _ := d.InputSchema["properties"].(map[string]any)
		inputs, _ := props["inputs"].(map[string]any)
		desc, _ := inputs["description"].(string)
		for _, r := range registry.BrokerInputRules() {
			if !strings.Contains(desc, r.Text) {
				t.Errorf("inputs description lacks rule %q", r.ID)
			}
		}
		return
	}
	t.Fatal("no define_workflow tool")
}

// Review 1843 item 2 (T7): the tool schemas tell the agent that a verb naming
// an existing project takes its slug or full id, and create_project a bare slug.
func TestAdminToolDefs_StateTheProjectReferenceRule(t *testing.T) {
	prop := func(tool, field string) string {
		for _, d := range companionAdminToolDefs() {
			if d.Name != tool {
				continue
			}
			p, _ := d.InputSchema["properties"].(map[string]any)
			f, _ := p[field].(map[string]any)
			s, _ := f["description"].(string)
			return s
		}
		return ""
	}
	for _, c := range [][2]string{{agentadmin.VerbDefineWorkflow, "project"}, {agentadmin.VerbSetBudget, "project"},
		{agentadmin.VerbAddAPI, "project"}, {agentadmin.VerbDefineSwarm, "slug"}, {agentadmin.VerbRemove, "id"}} {
		if got := prop(c[0], c[1]); !strings.Contains(got, agentadmin.ProjectRefRule) {
			t.Errorf("%s.%s description lacks the rule: %q", c[0], c[1], got)
		}
	}
	if got := prop(agentadmin.VerbCreateProject, "slug"); got != agentadmin.NewSlugRule {
		t.Errorf("create_project.slug = %q", got)
	}
}
