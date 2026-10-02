package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/apigateway"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
)

func agentAPIServer(t *testing.T, roleTools []string) (*Server, *fakeQueryGateway, *fakeQueryGateway) {
	t.Helper()
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{
		"hermes--fin": {ID: "hermes--fin", Broker: true, APIs: []registry.ProjectAPI{{Name: "fio", BaseURL: "https://a.example", Methods: []string{"GET"}}}},
		"proj":        {ID: "proj"},
	})
	agentClient := &fakeQueryGateway{resp: apigateway.Response{Status: 200, Body: `{"ok":1}`}}
	kong := &fakeQueryGateway{resp: apigateway.Response{Status: 200, Body: "kong"}}
	srv := &Server{logger: zerolog.Nop(), projectRegistry: reg, toolAuditRepo: &stubAuditRepo{}, apiGatewayClient: kong,
		agentGrants: memGrants{rows: map[string]persistence.AgentIntegrationApproval{
			"hermes--fin/fio": {ProjectID: "hermes--fin", Integration: "fio", Kind: "api", ReadTools: []string{"GET"}},
		}},
		agentAPIClients: func(string) apigateway.Client { return agentClient },
		egress:          &EgressScan{Detector: mustDetector(t)},
	}
	srv.agentRoleAllowlistForTest = func(_ context.Context, _ string) ([]string, mcpGapReason) { return roleTools, mcpGapNone }
	return srv, agentClient, kong
}

// Plan P4.5: an agent project's query_api goes to its own API client, never
// Kong; only a role that holds query_api may call it, and only to read
// (writes are proposals). Control: the agent branch of the query route.
// Before it, this route was outside the agent tool gate entirely (the P3b
// route table missed it).
func TestAgentQueryAPI_AgentProject(t *testing.T) {
	srv, agentClient, kong := agentAPIServer(t, []string{"query_api"})
	call := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		srv.AgentQueryAPI(rec, agentTaskReq(http.MethodPost, "/api/v1/projects/hermes--fin/api/query", body, "hermes--fin"))
		return rec
	}
	rec := call(`{"provider":"fio","path":"x"}`)
	if rec.Code != http.StatusOK || decodeQueryResp(t, rec).Refusal != "" || len(agentClient.calls) != 1 || len(kong.calls) != 0 {
		t.Fatalf("an approved read: %d %s (agent %d, kong %d)", rec.Code, rec.Body.String(), len(agentClient.calls), len(kong.calls))
	}
	rec = call(`{"provider":"fio","method":"POST","path":"x","body":{"a":1}}`)
	if resp := decodeQueryResp(t, rec); resp.Refusal == "" || len(agentClient.calls) != 1 {
		t.Fatalf("a write from a role was not refused: %+v", resp)
	}

	srv2, agentClient2, _ := agentAPIServer(t, []string{"file_read"})
	rec = httptest.NewRecorder()
	srv2.AgentQueryAPI(rec, agentTaskReq(http.MethodPost, "/api/v1/projects/hermes--fin/api/query", `{"provider":"fio","path":"x"}`, "hermes--fin"))
	if rec.Code == http.StatusOK && decodeQueryResp(t, rec).Refusal == "" || len(agentClient2.calls) != 0 {
		t.Fatalf("a role without query_api reached the API: %d %s", rec.Code, rec.Body.String())
	}

	// Operator projects keep Kong.
	srv3, agentClient3, kong3 := agentAPIServer(t, nil)
	rec = httptest.NewRecorder()
	srv3.AgentQueryAPI(rec, agentTaskReq(http.MethodPost, "/api/v1/projects/proj/api/query", `{"provider":"weather","path":"x"}`, "proj"))
	if len(kong3.calls) != 1 || len(agentClient3.calls) != 0 {
		t.Fatalf("an operator project left Kong: kong %d agent %d (%s)", len(kong3.calls), len(agentClient3.calls), rec.Body.String())
	}
}

func mustDetector(t *testing.T) secrets.Detector {
	t.Helper()
	d, err := secrets.NewMultiDetector(secrets.Config{})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Plan P5.2: an agent project's query_api path and query are scanned before
// the client is called; a credential-shaped value refuses the call naming
// the field. Without a scanner the agent route refuses (fail closed).
// Control: the egress scan on the agent branch.
func TestAgentQueryAPI_EgressScan(t *testing.T) {
	srv, agentClient, _ := agentAPIServer(t, []string{"query_api"})
	call := func(body string) AgentQueryResponse {
		rec := httptest.NewRecorder()
		srv.AgentQueryAPI(rec, agentTaskReq(http.MethodPost, "/api/v1/projects/hermes--fin/api/query", body, "hermes--fin"))
		return decodeQueryResp(t, rec)
	}
	resp := call(`{"provider":"fio","path":"x","query":{"token":"AKIAQWERTYUIOPASDFGH"}}`)
	if !strings.Contains(resp.Refusal, "$.query.token") || strings.Contains(resp.Refusal, "AKIAQWERTYUIOPASDFGH") || len(agentClient.calls) != 0 {
		t.Fatalf("a key in a query value: %+v (%d calls)", resp, len(agentClient.calls))
	}
	if resp := call(`{"provider":"fio","path":"accounts/AKIAQWERTYUIOPASDFGH"}`); !strings.Contains(resp.Refusal, "$.path") || len(agentClient.calls) != 0 {
		t.Fatalf("a key in the path: %+v", resp)
	}
	srv.egress = nil
	if resp := call(`{"provider":"fio","path":"x"}`); resp.Refusal == "" || len(agentClient.calls) != 0 {
		t.Fatalf("no scanner, agent route: %+v", resp)
	}
}
