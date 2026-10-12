package agentadmin

import (
	"fmt"
	"testing"
)

// GitHub #286 (2026-10-11): API and MCP connections share approval keys.
// Exercise rendered files and the real registry reload between both verbs.
func TestIntegrationNamesAreSharedAcrossKinds(t *testing.T) {
	for _, apiFirst := range []bool{true, false} {
		t.Run(fmt.Sprintf("api_first_%t", apiFirst), func(t *testing.T) {
			tr := newTree(t, "hermes")
			tr.project("finance")
			api := fioAPI()
			mcp := AddMCPServerInput{Project: "finance", Name: api.Name, URL: "https://mcp.example/mcp"}
			tr.advert[mcp.URL] = []string{"read"}
			first, second := VerbAddAPI, VerbAddMCPServer
			var firstIn, secondIn any = api, mcp
			if !apiFirst {
				first, second = second, first
				firstIn, secondIn = secondIn, firstIn
			}
			tr.apply(tr.render(first, firstIn))
			c := tr.render(second, secondIn)
			want := second + ` refused: hermes--finance already has a server or API named "fio"; remove it first`
			if c.Class != Refused || c.Reason != want || len(c.Ops) != 0 {
				t.Fatalf("collision must be refused without file operations: %+v", c)
			}
		})
	}
}
