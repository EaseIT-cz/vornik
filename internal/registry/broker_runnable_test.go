package registry

import (
	"strings"
	"testing"
)

// CheckBrokerRunnable — broker design §4.2 (read-only tools) and §5.3
// (first_party provenance). Runs at delegate time because the swarm that
// executes a workflow is only known per project.

func brokerFixture() (*Project, *Workflow, *Swarm) {
	p := &Project{
		ID:     "broker-acme",
		Broker: true,
		MCP: ProjectMCP{Servers: []MCPServerConfig{{
			Name:           "gmail",
			BrokerReadOnly: true,
			AllowedTools:   []string{"search_messages", "get_message"},
		}}},
	}
	wf := &Workflow{
		ID:         "mail-digest",
		Entrypoint: "read",
		Steps:      map[string]WorkflowStep{"read": {Type: "agent", Role: "reader", OnSuccess: "done"}},
		Terminals:  map[string]WorkflowTerminal{"done": {Status: "COMPLETED"}},
		Broker:     &WorkflowBroker{Egress: BrokerEgress{Output: "digest.json", Schema: map[string]any{"type": "object"}}},
	}
	sw := &Swarm{Roles: []SwarmRole{{
		Name: "reader",
		Permissions: SwarmRolePermissions{AllowedTools: []string{
			"file_write", "mcp__gmail__search_messages", "mcp__gmail__get_message",
		}},
	}}}
	return p, wf, sw
}

func TestCheckBrokerRunnable_AcceptsDeclaredReadOnlyTools(t *testing.T) {
	p, wf, sw := brokerFixture()
	if err := CheckBrokerRunnable(p, wf, sw); err != nil {
		t.Fatalf("reference broker must be runnable: %v", err)
	}
}

func TestCheckBrokerRunnable_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Project, wf *Workflow, sw *Swarm)
		want   string
	}{
		{"not a broker workflow", func(_ *Project, wf *Workflow, _ *Swarm) { wf.Broker = nil }, "not a broker workflow"},
		{"non-broker project", func(p *Project, _ *Workflow, _ *Swarm) { p.Broker = false }, "broker project"},
		{"server not declared read-only", func(p *Project, _ *Workflow, _ *Swarm) { p.MCP.Servers[0].BrokerReadOnly = false }, "broker_read_only"},
		{"tool missing from server allowed_tools", func(p *Project, _ *Workflow, _ *Swarm) {
			p.MCP.Servers[0].AllowedTools = []string{"search_messages"}
		}, "get_message"},
		{"server allowed_tools empty", func(p *Project, _ *Workflow, _ *Swarm) { p.MCP.Servers[0].AllowedTools = nil }, "allowed_tools"},
		{"unknown server", func(_ *Project, _ *Workflow, sw *Swarm) {
			sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "mcp__outlook__send")
		}, "outlook"},
		{"wildcard tool", func(_ *Project, _ *Workflow, sw *Swarm) {
			sw.Roles[0].Permissions.AllowedTools = []string{"mcp__gmail__*"}
		}, "mcp__gmail__*"},
		{"non-inert builtin", func(_ *Project, _ *Workflow, sw *Swarm) {
			sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "web_fetch")
		}, "web_fetch"},
		// Inert for the network-capability guard, but a task-kind reminder
		// spawns a task: an injected mail could schedule arbitrary work in
		// the broker project. Broker roles get the stricter set.
		{"set_reminder is not broker-safe", func(_ *Project, _ *Workflow, sw *Swarm) {
			sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "set_reminder")
		}, "set_reminder"},
		{"update_operator_profile is not broker-safe", func(_ *Project, _ *Workflow, sw *Swarm) {
			sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "update_operator_profile")
		}, "update_operator_profile"},
		{"role unrestricted", func(_ *Project, _ *Workflow, sw *Swarm) { sw.Roles[0].Permissions.AllowedTools = nil }, "unrestricted"},
		{"role unknown", func(_ *Project, wf *Workflow, _ *Swarm) {
			wf.Steps["read"] = WorkflowStep{Type: "agent", Role: "ghost", OnSuccess: "done"}
		}, "ghost"},
		{"opaque step type", func(_ *Project, wf *Workflow, _ *Swarm) {
			wf.Steps["read"] = WorkflowStep{Type: "system", OnSuccess: "done"}
		}, "step type"},
		{"first_party with non-operator-authored server", func(_ *Project, wf *Workflow, _ *Swarm) {
			wf.Broker.Egress.Provenance = BrokerProvenanceFirstParty
		}, "operator_authored"},
		{"nil swarm", func(_ *Project, _ *Workflow, sw *Swarm) { *sw = Swarm{} }, "reader"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, wf, sw := brokerFixture()
			tc.mutate(p, wf, sw)
			err := CheckBrokerRunnable(p, wf, sw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal mentioning %q, got %v", tc.want, err)
			}
		})
	}
}

func TestCheckBrokerRunnable_FirstPartyWhenEveryServerOperatorAuthored(t *testing.T) {
	p, wf, sw := brokerFixture()
	wf.Broker.Egress.Provenance = BrokerProvenanceFirstParty
	p.MCP.Servers[0].OperatorAuthored = true
	if err := CheckBrokerRunnable(p, wf, sw); err != nil {
		t.Fatalf("first_party with every server operator_authored must run: %v", err)
	}
}

func TestCheckBrokerRunnable_NilInputs(t *testing.T) {
	_, wf, sw := brokerFixture()
	if err := CheckBrokerRunnable(nil, wf, sw); err == nil {
		t.Fatal("nil project must refuse")
	}
	p, _, _ := brokerFixture()
	if err := CheckBrokerRunnable(p, wf, nil); err == nil {
		t.Fatal("nil swarm must refuse")
	}
}

// Broker write-actions design §4.2.

func proposingFixture() (*Project, *Workflow, *Swarm) {
	p, wf, sw := brokerFixture()
	p.MCP.Servers = append(p.MCP.Servers, MCPServerConfig{
		Name: "gmail-write", BrokerWrite: true, AllowedTools: []string{"gmail_send"},
	})
	wf.Broker.Proposes = []BrokerProposal{{
		Action: "gmail_reply", Tool: "mcp__gmail-write__gmail_send", Output: "proposal.json",
		ArgsSchema: map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
	}}
	return p, wf, sw
}

func TestCheckBrokerProposals_AcceptsDeclaredWriteServer(t *testing.T) {
	p, wf, sw := proposingFixture()
	if err := CheckBrokerRunnable(p, wf, sw); err != nil {
		t.Fatalf("runnable: %v", err)
	}
	if err := CheckBrokerProposals(p, wf, true); err != nil {
		t.Fatalf("proposals: %v", err)
	}
}

func TestCheckBrokerProposals_Refusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(p *Project, wf *Workflow)
		writes bool
		want   string
	}{
		{"writes off", func(*Project, *Workflow) {}, false, "BROKER_WRITES_DISABLED"},
		{"server not broker_write", func(p *Project, _ *Workflow) { p.MCP.Servers[1].BrokerWrite = false }, true, "broker_write"},
		{"tool not in allowed_tools", func(p *Project, _ *Workflow) { p.MCP.Servers[1].AllowedTools = []string{"other"} }, true, "allowed_tools"},
		{"unknown server", func(_ *Project, wf *Workflow) { wf.Broker.Proposes[0].Tool = "mcp__nope__gmail_send" }, true, "nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, wf, _ := proposingFixture()
			tc.mutate(p, wf)
			err := CheckBrokerProposals(p, wf, tc.writes)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// No proposals: nothing to check, whatever broker.writes says.
	p, wf, _ := brokerFixture()
	if err := CheckBrokerProposals(p, wf, false); err != nil {
		t.Fatalf("a read-only workflow is not affected by broker.writes: %v", err)
	}
}

// THE load-bearing rule (§4.2, §13): an agent that can call the write tool
// directly bypasses approval entirely, so a role holding it makes the
// workflow unrunnable.
func TestCheckBrokerRunnable_RoleHoldingTheWriteToolIsRefused(t *testing.T) {
	p, wf, sw := proposingFixture()
	sw.Roles[0].Permissions.AllowedTools = append(sw.Roles[0].Permissions.AllowedTools, "mcp__gmail-write__gmail_send")
	err := CheckBrokerRunnable(p, wf, sw)
	if err == nil || !strings.Contains(err.Error(), "gmail_send") {
		t.Fatalf("a role holding the write tool must make the workflow unrunnable, got %v", err)
	}
}

func TestCheckBrokerRunnable_ServerBothReadOnlyAndWriteIsRefused(t *testing.T) {
	p, wf, sw := proposingFixture()
	p.MCP.Servers[1].BrokerReadOnly = true
	err := CheckBrokerRunnable(p, wf, sw)
	if err == nil || !strings.Contains(err.Error(), "both") {
		t.Fatalf("a server cannot be both broker_read_only and broker_write, got %v", err)
	}
}

// BrokerWriteToolDeclared is the worker's call-time re-check (§5.4 step 2);
// CheckBrokerProposals uses the same function, so the two cannot disagree.
func TestBrokerWriteToolDeclared(t *testing.T) {
	p, _, _ := proposingFixture()
	if err := BrokerWriteToolDeclared(p, "mcp__gmail-write__gmail_send"); err != nil {
		t.Fatalf("declared tool refused: %v", err)
	}
	for tool, want := range map[string]string{
		"mcp__gmail-write__gmail_delete": "allowed_tools",
		"mcp__nope__gmail_send":          "nope",
		"gmail_send":                     "mcp__<server>__<tool>",
	} {
		if err := BrokerWriteToolDeclared(p, tool); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want %q, got %v", tool, want, err)
		}
	}
	p.MCP.Servers[1].BrokerWrite = false
	if err := BrokerWriteToolDeclared(p, "mcp__gmail-write__gmail_send"); err == nil || !strings.Contains(err.Error(), "broker_write") {
		t.Fatalf("server no longer broker_write: %v", err)
	}
	if err := BrokerWriteToolDeclared(nil, "mcp__gmail-write__gmail_send"); err == nil {
		t.Fatal("nil project accepted")
	}
}
