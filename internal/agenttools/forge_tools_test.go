package agenttools

import (
	"encoding/json"
	"testing"
)

// Issue #319: forge issue capabilities must be declared, gated, and cannot
// silently widen the baseline or expose provider credentials to agents.
func TestForgeIssueToolsDeclaration(t *testing.T) {
	for _, op := range []string{"list_issues", "get_issue", "list_labels", "add_labels", "set_labels", "close_issue"} {
		t.Run(op, func(t *testing.T) {
			tool := Get("forge_" + op)
			if tool == nil {
				t.Fatal("forge issue declaration missing")
			}
			acts := op == "add_labels" || op == "set_labels" || op == "close_issue"
			if tool.Gate != GateAllowlist || tool.AlwaysGranted || tool.Advertise != AdvertiseWhenAPIURL || tool.Runtime != RuntimeShell || tool.Acts != acts {
				t.Fatalf("unexpected permission/runtime declaration: %+v", tool)
			}
			var schema struct {
				Function struct {
					Parameters struct {
						Properties           map[string]json.RawMessage
						Required             []string
						AdditionalProperties bool
					}
				}
			}
			if err := json.Unmarshal(tool.Schema, &schema); err != nil {
				t.Fatal(err)
			}
			params := schema.Function.Parameters
			if params.AdditionalProperties || len(params.Required) == 0 || params.Required[0] != "repo" {
				t.Fatal("schema must require repo and forbid extra properties")
			}
			for _, forbidden := range []string{"operation", "path", "token", "api_key", "provider"} {
				if _, ok := params.Properties[forbidden]; ok {
					t.Fatalf("schema exposes %s", forbidden)
				}
			}
		})
	}
}
