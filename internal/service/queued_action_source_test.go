package service

import (
	"os"
	"strings"
	"testing"
)

// #119 the closed physical-send/context denominator prevents an unguarded new route.
func TestQueuedActionPhysicalSendSources(t *testing.T) {
	if got := sitesString(callSites(t, `requestguard\.Validate\(`)); got != "agentapi/agentapi.go=| mcp/client.go=|||" {
		t.Fatalf("physical guard call sites: %s", got)
	}
	for _, file := range []string{"../mcp/client.go", "../agentapi/agentapi.go"} {
		raw, e := os.ReadFile(file)
		if e != nil {
			t.Fatal(e)
		}
		s := string(raw)
		for _, part := range strings.Split(s, ".Do(httpReq)")[0:strings.Count(s, ".Do(httpReq)")] {
			i := strings.LastIndex(part, "requestguard.Validate(httpReq.Context())")
			if i < 0 || len(part)-i > 450 {
				t.Fatalf("%s send lacks final request-context guard", file)
			}
		}
	}
	raw, e := os.ReadFile("agentadmin_actions.go")
	if e != nil {
		t.Fatal(e)
	}
	s := string(raw)
	start := strings.Index(s, "func (a actionCaller) CallToolOnce(")
	if start < 0 {
		t.Fatal("missing actionCaller method")
	}
	s = s[start:]
	selection := strings.Index(s, ".APITool()")
	if selection < 0 {
		t.Fatal("missing route selection")
	}
	for _, marker := range []string{"brokeractions.ActionContext(ctx)", "a.c.checkActionAccount(ctx, projectID, vector)", "requestguard.With(ctx,", "requestguard.Present(ctx)"} {
		i := strings.Index(s, marker)
		if i < 0 || i > selection {
			t.Fatalf("mandatory fixed-context check missing/beyond route selection: %s", marker)
		}
	}

}
