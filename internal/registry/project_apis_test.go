package registry

import (
	"strings"
	"testing"
)

func apiProject(apis ...ProjectAPI) *Project {
	return &Project{ID: "hermes--fin", SwarmID: "s", DefaultWorkflowID: "w", APIs: apis,
		Permissions: ProjectPermissions{Secrets: []string{"hermes/FIO"}}}
}

// Agent-administered Vornik plan P4.5: a project's REST providers are
// validated at load. Control: validateAPIs.
func TestProjectAPIs_Validate(t *testing.T) {
	good := ProjectAPI{Name: "fio", BaseURL: "https://api.fio.example/v1", Methods: []string{"GET"},
		Auth: ProjectAPIAuth{Header: "Authorization", ValueFrom: "secret://hermes/FIO", Prefix: "Bearer "}}
	if err := apiProject(good).Validate("p.yaml"); err != nil {
		t.Fatalf("a well-formed API was refused: %v", err)
	}
	writes := good
	writes.Methods, writes.Writes = []string{"GET", "POST"}, true
	if err := apiProject(writes).Validate("p.yaml"); err != nil {
		t.Fatalf("a writing API was refused: %v", err)
	}
	cases := map[string]func(a *ProjectAPI){
		"http to a public host":   func(a *ProjectAPI) { a.BaseURL = "http://api.example/v1" },
		"userinfo":                func(a *ProjectAPI) { a.BaseURL = "https://u:p@api.example" },
		"query in base":           func(a *ProjectAPI) { a.BaseURL = "https://api.example/?k=v" },
		"no methods":              func(a *ProjectAPI) { a.Methods = nil },
		"unknown method":          func(a *ProjectAPI) { a.Methods = []string{"TRACE"} },
		"lower-case method":       func(a *ProjectAPI) { a.Methods = []string{"get"} },
		"write without writes":    func(a *ProjectAPI) { a.Methods = []string{"GET", "DELETE"} },
		"writes without a write":  func(a *ProjectAPI) { a.Writes = true },
		"bad name":                func(a *ProjectAPI) { a.Name = "Fio API" },
		"write suffix":            func(a *ProjectAPI) { a.Name = "fio-write" },
		"reserved header":         func(a *ProjectAPI) { a.Auth.Header = "Host" },
		"header with a colon":     func(a *ProjectAPI) { a.Auth.Header = "X-A: b" },
		"value not a reference":   func(a *ProjectAPI) { a.Auth.ValueFrom = "hunter2" },
		"secret not granted":      func(a *ProjectAPI) { a.Auth.ValueFrom = "secret://hermes/OTHER" },
		"prefix with a line feed": func(a *ProjectAPI) { a.Auth.Prefix = "Bearer\n" },
	}
	for name, mutate := range cases {
		a := good
		a.Methods = append([]string(nil), good.Methods...)
		mutate(&a)
		if err := apiProject(a).Validate("p.yaml"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := apiProject(good, good).Validate("p.yaml"); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Errorf("a duplicate API name: %v", err)
	}
}

// Plan P4.5: apis are agent-project only in this release; an operator
// project uses gateway.providers.
func TestAgentRules_APIsOnOperatorProjectRefused(t *testing.T) {
	files := baseAgentTree()
	files["projects/ops.yaml"] = agentProjectYAML("ops", "hermes--fin", "hermes--fin--start", "") // borrows; refused anyway
	files["projects/ops.yaml"] = "projectId: ops\nswarmId: s\ndefaultWorkflowId: w\napis:\n  - name: x\n    base_url: https://x.example\n    methods: [GET]\n"
	files["swarms/s.md"] = strings.Replace(agentSwarm, "hermes--fin", "s", 1)
	files["workflows/w.md"] = agentWorkflowMD("w", "agent", "")
	if rs := rejectedFor(t, files); !hasRejection(rs, "apis") {
		t.Fatalf("an operator project with apis was not rejected: %+v", rs)
	}
}

// Plan P4.5 (review 6e86 F2): query_api is a broker reader only for an
// agent project that declares apis; an operator broker project stays
// refused (it would reach Kong's unrestricted query_api). Control: the
// project-aware branch of checkBrokerTool.
func TestCheckBrokerRunnable_QueryAPI(t *testing.T) {
	p, wf, sw := brokerFixture()
	sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "query_api")
	if err := CheckBrokerRunnable(p, wf, sw); err == nil {
		t.Fatal("an operator broker role was given query_api")
	}
	p.ID = "hermes--fin"
	if err := CheckBrokerRunnable(p, wf, sw); err == nil {
		t.Fatal("an agent project without apis was given query_api")
	}
	p.APIs = []ProjectAPI{{Name: "fio", BaseURL: "https://a.example", Methods: []string{"GET"}}}
	if err := CheckBrokerRunnable(p, wf, sw); err != nil {
		t.Fatalf("an agent project with apis: %v", err)
	}
	p.ID = "broker-acme" // operator again, even with apis
	if err := CheckBrokerRunnable(p, wf, sw); err == nil {
		t.Fatal("an operator project with apis was given query_api")
	}
}

// Plan P4.8: a broker proposal may name an API write as api:<name>:<METHOD>,
// declared when the project's API lists that write method; anything else
// is refused. Control: APITool, validateProposes and BrokerWriteToolDeclared.
func TestBrokerProposals_APIWrites(t *testing.T) {
	for tool, ok := range map[string]bool{
		"api:pay:POST:/payments": true, "api:pay:DELETE:/x/1": true, "api:pay:GET:/x": false, "api:pay:POST": false,
		"api::POST:/x": false, "api:Pay:POST:/x": false, "api:pay:POST:x": false, "api:pay:POST:/a/../b": false,
		"api:pay:POST://evil": false, "api:pay:POST:/a?b=1": false, "mcp__a__b": false,
	} {
		_, _, _, got := BrokerProposal{Tool: tool}.APITool()
		if got != ok {
			t.Errorf("APITool(%q) = %v, want %v", tool, got, ok)
		}
	}
	b := &WorkflowBroker{Egress: BrokerEgress{Output: "r.json"}, Proposes: []BrokerProposal{{Action: "pay", Tool: "api:pay:POST:/payments",
		Output: "pay.json", ArgsSchema: map[string]any{"type": "object", "additionalProperties": false,
			"properties": map[string]any{"amount": map[string]any{"type": "number"}}}}}}
	if err := b.validateProposes(); err != nil {
		t.Fatalf("an API write proposal was refused at load: %v", err)
	}
	p := &Project{ID: "hermes--fin", APIs: []ProjectAPI{{Name: "pay", BaseURL: "https://p.example", Methods: []string{"GET", "POST"}, Writes: true}}}
	if err := BrokerWriteToolDeclared(p, "api:pay:POST:/payments"); err != nil {
		t.Fatalf("a declared API write: %v", err)
	}
	for _, tool := range []string{"api:pay:DELETE:/x", "api:other:POST:/x", "api:pay:GET:/x"} {
		if err := BrokerWriteToolDeclared(p, tool); err == nil {
			t.Errorf("%s was declared", tool)
		}
	}
}
