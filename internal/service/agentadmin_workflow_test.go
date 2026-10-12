package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #283: an administering assistant must inspect the active definition
// before replacing it. Exercise real apply, reload and scoped read-back.
func TestShowWorkflow_OwnedLiveDefinition(t *testing.T) {
	f := newFinance(t)
	f.approve(f.do(agentadmin.VerbDefineWorkflow, agentadmin.DefineWorkflowInput{
		Project: "finance", Slug: "digest", Purpose: "Digest",
		Steps:  []agentadmin.StepInput{{Name: "sum", Role: "worker", Instructions: "Include every row"}},
		Egress: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"rows":{"type":"array","maxItems":7,"items":{"type":"string","maxLength":40}}}}`),
	}))
	for _, project := range []string{"finance", "hermes--finance"} {
		out, err := f.svc.ShowWorkflowJSON(context.Background(), f.key, json.RawMessage(`{"project":"`+project+`","slug":"digest"}`))
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(out)
		for _, want := range []string{`"id":"hermes--finance--digest"`, `Include every row`, `"maxItems":7`, `"effective_max_bytes":`, `"input_schema":`} {
			if !strings.Contains(string(b), want) {
				t.Fatalf("readback lacks %s: %s", want, b)
			}
		}
	}
	for _, raw := range []string{`{"project":"codex--finance","slug":"digest"}`, `{"project":"finance","slug":"missing"}`, `{"project":"finance","slug":"digest","namespace":"codex"}`, `{"project":"finance","slug":"../digest"}`} {
		if _, err := f.svc.ShowWorkflowJSON(context.Background(), f.key, json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	for _, key := range []*persistence.APIKey{nil, {AgentAdmin: false}, {AgentAdmin: true, AgentNamespace: "codex"}} {
		if _, err := f.svc.ShowWorkflowJSON(context.Background(), key, json.RawMessage(`{"project":"finance","slug":"digest"}`)); err == nil {
			t.Fatal("foreign/plain key accepted")
		}
	}
}
